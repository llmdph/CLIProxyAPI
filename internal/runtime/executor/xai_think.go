package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/llmreqlog"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// xaiThinkEvidence mirrors the LLM request-log "Think流" column:
// good = hasThink && length > 0, or Console encrypted reasoning.
// bad = missing stream or 有(0字).
type xaiThinkEvidence struct {
	HasThink  bool
	Length    int
	Encrypted bool
}

func (e xaiThinkEvidence) ok() bool  { return e.Encrypted || (e.HasThink && e.Length > 0) }
func (e xaiThinkEvidence) bad() bool { return !e.ok() }

func (e xaiThinkEvidence) detail() string {
	if e.Encrypted && e.Length == 0 {
		return "有Think流"
	}
	if !e.HasThink {
		return "无Think流"
	}
	if e.Length == 0 {
		return "Think流有(0字)"
	}
	return fmt.Sprintf("Think流有(%d字)", e.Length)
}

// xaiRequestExpectsThink reports whether the upstream Responses body asked for
// a reasoning/Think stream. Only those requests participate in the gate.
func xaiRequestExpectsThink(upstreamBody []byte) bool {
	effort := strings.ToLower(strings.TrimSpace(gjson.GetBytes(upstreamBody, "reasoning.effort").String()))
	switch effort {
	case "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

func parseXAIThinkEvidence(body []byte) xaiThinkEvidence {
	body = bytesTrimSpace(body)
	if len(body) == 0 {
		return xaiThinkEvidence{}
	}
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return parseXAIThinkFromSSE(body)
	}
	ev := xaiThinkEvidence{}
	walkXAIThinkJSON(root, &ev)
	return ev
}

func parseXAIThinkFromSSE(body []byte) xaiThinkEvidence {
	ev := xaiThinkEvidence{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "[DONE]" {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "" || line == "[DONE]" || !strings.HasPrefix(line, "{") {
			continue
		}
		var root any
		if err := json.Unmarshal([]byte(line), &root); err != nil {
			continue
		}
		walkXAIThinkJSON(root, &ev)
	}
	return ev
}

func walkXAIThinkJSON(node any, ev *xaiThinkEvidence) {
	switch typed := node.(type) {
	case map[string]any:
		walkXAIThinkObject(typed, ev)
	case []any:
		for _, item := range typed {
			walkXAIThinkJSON(item, ev)
		}
	}
}

func walkXAIThinkObject(obj map[string]any, ev *xaiThinkEvidence) {
	eventType := xaiAsString(obj["type"])
	switch eventType {
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if text := xaiAsString(obj["delta"]); text != "" {
			addXAIThinkText(ev, text)
		} else {
			ev.HasThink = true
		}
		return
	case "response.reasoning_text.done", "response.reasoning_summary_text.done":
		if text := firstNonEmptyString(xaiAsString(obj["text"]), xaiNestedString(obj, "part", "text")); text != "" {
			addXAIThinkText(ev, text)
		} else {
			ev.HasThink = true
		}
		return
	case "response.output_item.done", "response.output_item.added":
		if item, ok := obj["item"].(map[string]any); ok && xaiAsString(item["type"]) == "reasoning" {
			ingestXAIReasoningItem(item, ev)
		}
	}

	if output, ok := obj["output"].([]any); ok {
		for _, item := range output {
			itemObj, ok := item.(map[string]any)
			if !ok || xaiAsString(itemObj["type"]) != "reasoning" {
				continue
			}
			ingestXAIReasoningItem(itemObj, ev)
		}
	}

	if choices, ok := obj["choices"].([]any); ok {
		for _, choice := range choices {
			choiceObj, ok := choice.(map[string]any)
			if !ok {
				continue
			}
			for _, key := range []string{"message", "delta"} {
				part, ok := choiceObj[key].(map[string]any)
				if !ok {
					continue
				}
				if rc, exists := part["reasoning_content"]; exists {
					ingestXAIReasoningContent(rc, ev)
				}
			}
		}
	}

	if content, ok := obj["content"].([]any); ok {
		for _, part := range content {
			partObj, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch xaiAsString(partObj["type"]) {
			case "thinking", "reasoning", "reasoning_text":
				if text := firstNonEmptyString(xaiAsString(partObj["thinking"]), xaiAsString(partObj["text"])); text != "" {
					addXAIThinkText(ev, text)
				} else {
					ev.HasThink = true
				}
			}
		}
	}

	for key, value := range obj {
		switch key {
		case "output", "choices", "content", "item", "delta", "message", "summary":
			walkXAIThinkJSON(value, ev)
		}
	}
}

func ingestXAIReasoningItem(item map[string]any, ev *xaiThinkEvidence) {
	saw := false
	if summary, ok := item["summary"].([]any); ok {
		for _, part := range summary {
			partObj, ok := part.(map[string]any)
			if !ok {
				continue
			}
			partType := xaiAsString(partObj["type"])
			if partType == "summary_text" || partType == "reasoning_text" || partType == "" {
				if text := xaiAsString(partObj["text"]); text != "" {
					addXAIThinkText(ev, text)
					saw = true
				}
			}
		}
	}
	if content, ok := item["content"].([]any); ok {
		for _, part := range content {
			partObj, ok := part.(map[string]any)
			if !ok {
				continue
			}
			partType := xaiAsString(partObj["type"])
			if partType == "reasoning_text" {
				if text := xaiAsString(partObj["text"]); text != "" {
					addXAIThinkText(ev, text)
					saw = true
				}
			}
			if partType == "encrypted_content" || strings.TrimSpace(xaiAsString(partObj["encrypted_content"])) != "" {
				markXAIEncryptedThink(ev)
				saw = true
			}
		}
	}
	if blob := strings.TrimSpace(xaiAsString(item["encrypted_content"])); blob != "" {
		markXAIEncryptedThink(ev)
		saw = true
	}
	if !saw {
		ev.HasThink = true
	}
}

func ingestXAIReasoningContent(value any, ev *xaiThinkEvidence) {
	switch typed := value.(type) {
	case string:
		if typed != "" {
			addXAIThinkText(ev, typed)
		} else {
			ev.HasThink = true
		}
	case []any:
		var builder strings.Builder
		for _, item := range typed {
			itemObj, ok := item.(map[string]any)
			if !ok {
				if s, ok := item.(string); ok {
					builder.WriteString(s)
				}
				continue
			}
			partType := xaiAsString(itemObj["type"])
			if partType == "summary_text" || partType == "reasoning_text" || partType == "text" || partType == "" {
				builder.WriteString(xaiAsString(itemObj["text"]))
			}
		}
		if text := builder.String(); text != "" {
			addXAIThinkText(ev, text)
		} else {
			ev.HasThink = true
		}
	default:
		ev.HasThink = true
	}
}

func addXAIThinkText(ev *xaiThinkEvidence, text string) {
	if ev == nil || text == "" {
		return
	}
	ev.HasThink = true
	ev.Length += utf8.RuneCountInString(text)
}

func markXAIEncryptedThink(ev *xaiThinkEvidence) {
	if ev == nil {
		return
	}
	ev.HasThink = true
	ev.Encrypted = true
}

func xaiAsString(v any) string {
	s, _ := v.(string)
	return s
}

func xaiNestedString(obj map[string]any, keys ...string) string {
	cur := any(obj)
	for _, key := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[key]
	}
	return xaiAsString(cur)
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func ingestXAIThinkEvent(ev *xaiThinkEvidence, eventData []byte) {
	if ev == nil {
		return
	}
	eventData = bytesTrimSpace(eventData)
	if len(eventData) == 0 || eventData[0] != '{' {
		return
	}
	var root any
	if err := json.Unmarshal(eventData, &root); err != nil {
		return
	}
	walkXAIThinkJSON(root, ev)
}

// xaiStreamEventIsAnswer reports that the model started visible output
// without needing response.completed. Used to fail-fast no-think retries.
func xaiStreamEventIsAnswer(eventData []byte) bool {
	eventType := gjson.GetBytes(eventData, "type").String()
	switch eventType {
	case "response.output_text.delta", "response.output_text.done":
		return strings.TrimSpace(gjson.GetBytes(eventData, "delta").String()) != "" ||
			strings.TrimSpace(gjson.GetBytes(eventData, "text").String()) != ""
	case "response.content_part.added", "response.content_part.delta", "response.content_part.done":
		partType := gjson.GetBytes(eventData, "part.type").String()
		if partType == "reasoning_text" || partType == "summary_text" {
			return false
		}
		if partType == "output_text" || partType == "text" {
			return true
		}
	case "response.output_item.added", "response.output_item.done":
		itemType := gjson.GetBytes(eventData, "item.type").String()
		return itemType == "message"
	}
	return false
}

func xaiStreamEventStatusErr(eventData []byte) (error, bool) {
	eventType := gjson.GetBytes(eventData, "type").String()
	status := int(gjson.GetBytes(eventData, "status").Int())
	if status == 0 {
		status = int(gjson.GetBytes(eventData, "error.status").Int())
	}
	if eventType != "error" && status < 400 {
		return nil, false
	}
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return xaiStatusErr(status, eventData), true
}

func xaiInspectThinkStream(rawSSE, completedJSON, fallbackPayload []byte) xaiThinkEvidence {
	ev := parseXAIThinkEvidence(completedJSON)
	if ev.bad() && len(rawSSE) > 0 {
		ev = preferXAIThinkEvidence(ev, parseXAIThinkFromSSE(rawSSE))
	}
	if ev.bad() && len(fallbackPayload) > 0 {
		ev = preferXAIThinkEvidence(ev, parseXAIThinkEvidence(fallbackPayload))
	}
	return ev
}

func preferXAIThinkEvidence(current, next xaiThinkEvidence) xaiThinkEvidence {
	if next.ok() && !current.ok() {
		return next
	}
	if next.Length > current.Length {
		return next
	}
	if next.Encrypted && !current.Encrypted {
		return next
	}
	if next.HasThink && !current.HasThink {
		return next
	}
	return current
}

func xaiNotifyThinkOK(ctx context.Context, auth *cliproxyauth.Auth, ev xaiThinkEvidence) {
	if !ev.ok() {
		return
	}
	authID := ""
	if auth != nil {
		authID = strings.TrimSpace(auth.ID)
	}
	cliproxyexecutor.NotifyXAIThinkOK(ctx, authID)
	xaiRecordThinkEvidence(ctx, ev)
}

func xaiRecordThinkEvidence(ctx context.Context, ev xaiThinkEvidence) {
	if !ev.HasThink && !ev.Encrypted {
		return
	}
	llmreqlog.RecordThinkEvidence(ctx, true, int64(ev.Length))
}

func xaiGateThinkStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	upstreamBody []byte,
	rawSSE []byte,
	completedJSON []byte,
	fallback cliproxyexecutor.Response,
	streamHeader http.Header,
	streamChunks [][]byte,
) error {
	ev := xaiInspectThinkStream(rawSSE, completedJSON, fallback.Payload)
	if ev.ok() {
		xaiNotifyThinkOK(ctx, auth, ev)
		return nil
	}
	if cliproxyexecutor.RequestClassUsesAuxPool(cliproxyexecutor.RequestClassFromContext(ctx)) {
		return nil
	}
	if !xaiRequestExpectsThink(upstreamBody) {
		return nil
	}
	authID := ""
	if auth != nil {
		authID = strings.TrimSpace(auth.ID)
	}
	return &cliproxyexecutor.NoThinkStreamError{
		AuthID:       authID,
		Detail:       ev.detail(),
		HasThink:     ev.HasThink,
		ThinkingLen:  ev.Length,
		Fallback:     fallback,
		StreamHeader: streamHeader,
		StreamChunks: streamChunks,
	}
}
