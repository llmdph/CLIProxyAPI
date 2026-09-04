package management

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// thinkEvidence mirrors the LLM request-log "Think流" column semantics:
// hasThink means a think channel was observed; length is UTF-8 rune count of think text.
// Bad evidence (disable): !hasThink || length == 0. Good: hasThink && length > 0.
type thinkEvidence struct {
	HasThink bool
	Length   int
}

func (e thinkEvidence) ok() bool {
	return e.HasThink && e.Length > 0
}

func (e thinkEvidence) bad() bool {
	return !e.ok()
}

// parseThinkFromResponse extracts interleaved think text from an xAI / OpenAI-shaped body.
// Same signals as CPA llmreqlog Think sniffer (reasoning output items / reasoning_content).
func parseThinkFromResponse(body string) thinkEvidence {
	body = strings.TrimSpace(body)
	if body == "" {
		return thinkEvidence{}
	}
	var root any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		// SSE / multi-line stream blob
		return parseThinkFromSSE(body)
	}
	ev := thinkEvidence{}
	walkThinkJSON(root, &ev)
	return ev
}

func parseThinkFromSSE(body string) thinkEvidence {
	ev := thinkEvidence{}
	for _, line := range strings.Split(body, "\n") {
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
		walkThinkJSON(root, &ev)
	}
	return ev
}

func walkThinkJSON(node any, ev *thinkEvidence) {
	switch typed := node.(type) {
	case map[string]any:
		walkThinkObject(typed, ev)
	case []any:
		for _, item := range typed {
			walkThinkJSON(item, ev)
		}
	}
}

func walkThinkObject(obj map[string]any, ev *thinkEvidence) {
	eventType := thinkProbeString(obj["type"])
	switch eventType {
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if text := thinkProbeString(obj["delta"]); text != "" {
			addThinkText(ev, text)
		}
		return
	case "response.reasoning_text.done", "response.reasoning_summary_text.done":
		if text := firstNonEmpty(thinkProbeString(obj["text"]), nestedString(obj, "part", "text")); text != "" {
			addThinkText(ev, text)
		} else {
			ev.HasThink = true
		}
		return
	case "response.output_item.done":
		if item, ok := obj["item"].(map[string]any); ok && thinkProbeString(item["type"]) == "reasoning" {
			ingestReasoningItem(item, ev)
		}
	}

	// Non-stream Responses: output[].type == reasoning
	if output, ok := obj["output"].([]any); ok {
		for _, item := range output {
			itemObj, ok := item.(map[string]any)
			if !ok || thinkProbeString(itemObj["type"]) != "reasoning" {
				continue
			}
			ingestReasoningItem(itemObj, ev)
		}
	}

	// Chat completions: choices[].message/delta.reasoning_content
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
					ingestReasoningContent(rc, ev)
				}
			}
		}
	}

	// Claude-style content[].type == thinking (defensive; not primary for xAI)
	if content, ok := obj["content"].([]any); ok {
		for _, part := range content {
			partObj, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if thinkProbeString(partObj["type"]) == "thinking" {
				if text := thinkProbeString(partObj["thinking"]); text != "" {
					addThinkText(ev, text)
				} else {
					ev.HasThink = true
				}
			}
		}
	}

	// Recurse into nested objects that may wrap the payload.
	for key, value := range obj {
		switch key {
		case "output", "choices", "content", "item", "delta", "message", "summary":
			walkThinkJSON(value, ev)
		}
	}
}

func ingestReasoningItem(item map[string]any, ev *thinkEvidence) {
	saw := false
	if summary, ok := item["summary"].([]any); ok {
		for _, part := range summary {
			partObj, ok := part.(map[string]any)
			if !ok {
				continue
			}
			partType := thinkProbeString(partObj["type"])
			if partType == "summary_text" || partType == "reasoning_text" || partType == "" {
				if text := thinkProbeString(partObj["text"]); text != "" {
					addThinkText(ev, text)
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
			if thinkProbeString(partObj["type"]) == "reasoning_text" {
				if text := thinkProbeString(partObj["text"]); text != "" {
					addThinkText(ev, text)
					saw = true
				}
			}
		}
	}
	if !saw {
		// reasoning item present but empty → 有(0字)
		ev.HasThink = true
	}
}

func ingestReasoningContent(value any, ev *thinkEvidence) {
	switch typed := value.(type) {
	case string:
		if typed != "" {
			addThinkText(ev, typed)
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
			partType := thinkProbeString(itemObj["type"])
			if partType == "summary_text" || partType == "reasoning_text" || partType == "text" || partType == "" {
				builder.WriteString(thinkProbeString(itemObj["text"]))
			}
		}
		if text := builder.String(); text != "" {
			addThinkText(ev, text)
		} else {
			ev.HasThink = true
		}
	case map[string]any:
		if text := thinkProbeString(typed["text"]); text != "" {
			addThinkText(ev, text)
		} else {
			ev.HasThink = true
		}
	case nil:
		ev.HasThink = true
	default:
		ev.HasThink = true
	}
}

func addThinkText(ev *thinkEvidence, text string) {
	if ev == nil || text == "" {
		return
	}
	ev.HasThink = true
	ev.Length += utf8.RuneCountInString(text)
}

func nestedString(obj map[string]any, keys ...string) string {
	cur := any(obj)
	for _, key := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[key]
	}
	return thinkProbeString(cur)
}

func thinkProbeString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}
