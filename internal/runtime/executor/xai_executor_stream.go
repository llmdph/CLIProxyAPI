package executor

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

func (e *XAIExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	if xaiInputHasItemType(req.Payload, "compaction_trigger") {
		return e.executeCompactionTriggerStream(ctx, auth, req, opts)
	}

	token, _ := xaiCreds(auth)
	baseURL := xaiChatBaseURL(auth)
	logXAIResolvedBaseURL(ctx, baseURL)

	prepared, err := e.prepareResponsesRequest(ctx, req, opts, true)
	if err != nil {
		return nil, err
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, prepared.baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(prepared.body, e.Identifier())

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	ctx = helps.WithWarpDialRecorder(ctx)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(prepared.body))
	if err != nil {
		return nil, err
	}
	applyXAIChatHeaders(httpReq, auth, token, true, prepared.sessionID, opts.Headers)
	e.recordXAIRequest(ctx, auth, url, httpReq.Header.Clone(), prepared.body)

	helps.PrepareUpstreamForProxy(ctx, e.cfg, auth)
	httpClient := helps.NewFreshXAIHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		return nil, xaiStatusErr(httpResp.StatusCode, data)
	}

	expectThink := xaiRequestExpectsThink(prepared.body) &&
		!cliproxyexecutor.RequestClassUsesAuxPool(cliproxyexecutor.RequestClassFromContext(ctx))
	out := make(chan cliproxyexecutor.StreamChunk, 32)
	ready := make(chan error, 1)
	var readyOnce sync.Once
	signalReady := func(readyErr error) {
		readyOnce.Do(func() { ready <- readyErr })
	}

	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("xai executor: close response body error: %v", errClose)
			}
		}()
		live, runErr := e.pumpXAIChatStream(ctx, auth, req, prepared, reporter, httpResp, out, expectThink, signalReady)
		if runErr != nil {
			if live {
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: runErr}:
				case <-ctx.Done():
				}
			} else {
				reporter.PublishFailure(ctx, runErr)
			}
			signalReady(runErr)
			return
		}
		signalReady(nil)
	}()

	select {
	case readyErr := <-ready:
		if readyErr != nil {
			err = readyErr
			return nil, readyErr
		}
		return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
	case <-ctx.Done():
		err = ctx.Err()
		return nil, err
	}
}

func sendXAIStreamPayloads(ctx context.Context, out chan<- cliproxyexecutor.StreamChunk, payloads [][]byte) error {
	for _, payload := range payloads {
		if len(payload) == 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- cliproxyexecutor.StreamChunk{Payload: payload}:
		}
	}
	return nil
}

func (e *XAIExecutor) pumpXAIChatStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	prepared *xaiPreparedRequest,
	reporter *helps.UsageReporter,
	httpResp *http.Response,
	out chan<- cliproxyexecutor.StreamChunk,
	expectThink bool,
	signalReady func(error),
) (live bool, err error) {
	if !expectThink {
		live = true
		signalReady(nil)
	}

	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800)
	claudeInputTokens := helps.NewClaudeInputTokenState(prepared.from, prepared.to, prepared.responseFormat, prepared.originalPayload)
	var param any
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	responseFilter := newXAIInternalXSearchResponseFilter(prepared.filterInternalXSearch, prepared.clientDeclaredTools)
	var pendingEventLine []byte
	var pending [][]byte
	var translatedChunks [][]byte
	var rawLines [][]byte
	var completedData []byte
	var think xaiThinkEvidence
	sawIncomplete := false
	headers := httpResp.Header.Clone()

	failNoThink := func() error {
		rawSSE := bytes.Join(rawLines, []byte("\n"))
		errThink := xaiGateThinkStream(ctx, auth, prepared.body, rawSSE, completedData, cliproxyexecutor.Response{}, headers, translatedChunks)
		if errThink == nil {
			return nil
		}
		authID := ""
		if auth != nil {
			authID = strings.TrimSpace(auth.ID)
		}
		log.Warnf("xai: no Think before completed, switching auth=%s err=%v", authID, errThink)
		helps.QuarantineWarpAfterNoThink(ctx, e.cfg, auth)
		return errThink
	}

	becomeLive := func() error {
		if live {
			return nil
		}
		live = true
		if think.ok() {
			xaiNotifyThinkOK(ctx, auth, think)
		}
		signalReady(nil)
		if errSend := sendXAIStreamPayloads(ctx, out, pending); errSend != nil {
			return errSend
		}
		pending = nil
		return nil
	}

	emitTranslatedLine := func(translatedLine []byte) error {
		chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, prepared.to, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body, translatedLine, &param, claudeInputTokens)
		translatedChunks = append(translatedChunks, chunks...)
		if live {
			return sendXAIStreamPayloads(ctx, out, chunks)
		}
		pending = append(pending, chunks...)
		return nil
	}

	for scanner.Scan() {
		line := bytes.Clone(scanner.Bytes())
		helps.AppendAPIResponseChunk(ctx, e.cfg, line)
		rawLines = append(rawLines, line)
		if bytes.HasPrefix(line, xaiEventTag) {
			if pendingEventLine != nil {
				if errEmit := emitTranslatedLine(xaiNormalizeReasoningSummaryEventLine(pendingEventLine, "")); errEmit != nil {
					return live, errEmit
				}
			}
			pendingEventLine = bytes.Clone(line)
			continue
		}

		if bytes.HasPrefix(line, xaiDataTag) {
			eventDataList := xaiNormalizeReasoningSummaryDataEvents(bytes.TrimSpace(line[len(xaiDataTag):]))
			hasPendingEventLine := pendingEventLine != nil
			for i, eventData := range eventDataList {
				eventData = restoreXAINamespaceToolCalls(eventData, prepared.namespaceTools)
				eventData = responseFilter.apply(eventData)
				if len(eventData) == 0 {
					if hasPendingEventLine && i == 0 {
						pendingEventLine = nil
					}
					continue
				}
				if errEvt, okErr := xaiStreamEventStatusErr(eventData); okErr {
					return live, errEvt
				}
				ingestXAIThinkEvent(&think, eventData)
				if think.ok() {
					if errLive := becomeLive(); errLive != nil {
						return live, errLive
					}
				} else if expectThink && !live && xaiStreamEventIsAnswer(eventData) {
					if errThink := failNoThink(); errThink != nil {
						return false, errThink
					}
				}
				normalizedEventName := gjson.GetBytes(eventData, "type").String()
				switch normalizedEventName {
				case "response.output_item.done":
					xaiCollectOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
				case "response.completed", "response.incomplete":
					if detail, ok := helps.ParseCodexUsage(eventData); ok {
						reporter.Publish(ctx, detail)
					}
					eventData = xaiPatchCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)
					eventData = xaiNormalizeReasoningSummaryData(eventData)
					if normalizedEventName == "response.completed" {
						cacheXAIReasoningReplayFromCompleted(ctx, prepared.replayScope, eventData)
						completedData = bytes.Clone(eventData)
					} else {
						sawIncomplete = true
					}
					normalizedEventName = gjson.GetBytes(eventData, "type").String()
				}

				if hasPendingEventLine {
					eventLine := []byte("event: " + normalizedEventName)
					if i == 0 {
						eventLine = xaiNormalizeReasoningSummaryEventLine(pendingEventLine, normalizedEventName)
						pendingEventLine = nil
					}
					if errEmit := emitTranslatedLine(eventLine); errEmit != nil {
						return live, errEmit
					}
				}
				if errEmit := emitTranslatedLine(append([]byte("data: "), eventData...)); errEmit != nil {
					return live, errEmit
				}
			}
			continue
		}

		if pendingEventLine != nil {
			if errEmit := emitTranslatedLine(xaiNormalizeReasoningSummaryEventLine(pendingEventLine, "")); errEmit != nil {
				return live, errEmit
			}
			pendingEventLine = nil
		}
		if errEmit := emitTranslatedLine(bytes.Clone(line)); errEmit != nil {
			return live, errEmit
		}
	}
	if pendingEventLine != nil {
		if errEmit := emitTranslatedLine(xaiNormalizeReasoningSummaryEventLine(pendingEventLine, "")); errEmit != nil {
			return live, errEmit
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errScan)
		return live, errScan
	}

	if live {
		if think.ok() {
			xaiNotifyThinkOK(ctx, auth, think)
		}
		if len(completedData) == 0 && !sawIncomplete {
			return live, statusErr{code: http.StatusRequestTimeout, msg: "xai stream error: stream disconnected before response.completed"}
		}
		return live, nil
	}

	if len(completedData) == 0 {
		if sawIncomplete {
			if errLive := becomeLive(); errLive != nil {
				return live, errLive
			}
			return live, nil
		}
		return false, statusErr{code: http.StatusRequestTimeout, msg: "xai stream error: stream disconnected before response.completed"}
	}

	if errThink := failNoThink(); errThink != nil {
		return false, errThink
	}
	if errLive := becomeLive(); errLive != nil {
		return live, errLive
	}
	return live, nil
}
