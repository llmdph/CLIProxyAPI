package executor

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

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
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close response body error: %v", errClose)
		}
	}()

	// Buffer the full upstream SSE before returning to the client so we can
	// reject missing/zero Think streams and retry another credential.
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800)
	var rawLines [][]byte
	for scanner.Scan() {
		line := bytes.Clone(scanner.Bytes())
		helps.AppendAPIResponseChunk(ctx, e.cfg, line)
		rawLines = append(rawLines, line)
	}
	if errScan := scanner.Err(); errScan != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errScan)
		reporter.PublishFailure(ctx, errScan)
		return nil, errScan
	}

	claudeInputTokens := helps.NewClaudeInputTokenState(prepared.from, prepared.to, prepared.responseFormat, prepared.originalPayload)
	var param any
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	responseFilter := newXAIInternalXSearchResponseFilter(prepared.filterInternalXSearch, prepared.clientDeclaredTools)
	var pendingEventLine []byte
	var translatedChunks [][]byte
	var completedData []byte
	emitTranslatedLine := func(translatedLine []byte) {
		chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, prepared.to, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body, translatedLine, &param, claudeInputTokens)
		translatedChunks = append(translatedChunks, chunks...)
	}

	for _, line := range rawLines {
		if bytes.HasPrefix(line, xaiEventTag) {
			if pendingEventLine != nil {
				emitTranslatedLine(xaiNormalizeReasoningSummaryEventLine(pendingEventLine, ""))
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
					}
					normalizedEventName = gjson.GetBytes(eventData, "type").String()
				}

				if hasPendingEventLine {
					eventLine := []byte("event: " + normalizedEventName)
					if i == 0 {
						eventLine = xaiNormalizeReasoningSummaryEventLine(pendingEventLine, normalizedEventName)
						pendingEventLine = nil
					}
					emitTranslatedLine(eventLine)
				}
				emitTranslatedLine(append([]byte("data: "), eventData...))
			}
			continue
		}

		if pendingEventLine != nil {
			emitTranslatedLine(xaiNormalizeReasoningSummaryEventLine(pendingEventLine, ""))
			pendingEventLine = nil
		}
		emitTranslatedLine(bytes.Clone(line))
	}
	if pendingEventLine != nil {
		emitTranslatedLine(xaiNormalizeReasoningSummaryEventLine(pendingEventLine, ""))
	}

	rawSSE := bytes.Join(rawLines, []byte("\n"))
	headers := httpResp.Header.Clone()
	if errThink := xaiGateThinkStream(auth, prepared.body, rawSSE, completedData, cliproxyexecutor.Response{}, headers, translatedChunks); errThink != nil {
		return nil, errThink
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		for _, chunk := range translatedChunks {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil
}
