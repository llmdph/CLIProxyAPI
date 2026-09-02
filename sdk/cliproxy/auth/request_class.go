package auth

import (
	"net/http"
	"strings"
	"unicode/utf8"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func classifyLLMRequest(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	bodies := requestClassBodies(req, opts)
	if isCompactionRequest(opts, bodies) {
		return cliproxyexecutor.RequestClassCompaction
	}
	if isSessionNameRequest(bodies) {
		return cliproxyexecutor.RequestClassSessionName
	}
	if isInternalRequest(bodies) {
		return cliproxyexecutor.RequestClassInternal
	}
	return cliproxyexecutor.RequestClassNormal
}

func requestClassBodies(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) [][]byte {
	out := make([][]byte, 0, 2)
	if len(opts.OriginalRequest) > 0 {
		out = append(out, opts.OriginalRequest)
	}
	if len(req.Payload) > 0 {
		out = append(out, req.Payload)
	}
	return out
}

func isCompactionRequest(opts cliproxyexecutor.Options, bodies [][]byte) bool {
	if strings.Contains(strings.ToLower(strings.TrimSpace(opts.Alt)), "compact") {
		return true
	}
	path := metadataString(opts.Metadata, cliproxyexecutor.RequestPathMetadataKey)
	if strings.Contains(strings.ToLower(path), "compact") {
		return true
	}
	if turnMetadataLooksLikeCompaction(headerTurnMetadata(opts.Headers)) {
		return true
	}
	for _, body := range bodies {
		if len(body) == 0 {
			continue
		}
		if requestKindIsCompaction(gjson.GetBytes(body, "request_kind").String()) {
			return true
		}
		if requestKindIsCompaction(gjson.GetBytes(body, "client_metadata.request_kind").String()) {
			return true
		}
		if bodyTurnMetadataLooksLikeCompaction(body) {
			return true
		}
		// Historical type=compaction items stay in later user turns after a
		// compact. Only an explicit compact-now trigger is this request class.
		if inputHasItemType(body, "compaction_trigger") {
			return true
		}
		// Codex memento compact uses POST /v1/responses with a checkpoint
		// prompt, not /responses/compact or type=compaction_trigger.
		// Claude Code /compact is a /v1/messages summarizer turn.
		if looksLikeCompactionPrompt(lastUserText(body)) ||
			looksLikeCompactionPrompt(systemText(body)) ||
			looksLikeCompactionPrompt(gjson.GetBytes(body, "instructions").String()) ||
			looksLikeCompactionPrompt(latestNonToolText(body)) {
			return true
		}
	}
	return false
}

func headerTurnMetadata(headers http.Header) string {
	if headers == nil {
		return ""
	}
	return strings.TrimSpace(headers.Get("X-Codex-Turn-Metadata"))
}

func bodyTurnMetadataLooksLikeCompaction(body []byte) bool {
	for _, path := range []string{
		"client_metadata.x-codex-turn-metadata",
		"response.client_metadata.x-codex-turn-metadata",
	} {
		node := gjson.GetBytes(body, path)
		if !node.Exists() {
			continue
		}
		if requestKindIsCompaction(node.Get("request_kind").String()) {
			return true
		}
		if turnMetadataLooksLikeCompaction(node.String()) {
			return true
		}
		if turnMetadataLooksLikeCompaction(node.Raw) {
			return true
		}
	}
	return false
}

func requestKindIsCompaction(kind string) bool {
	return strings.EqualFold(strings.TrimSpace(kind), "compaction")
}

func turnMetadataLooksLikeCompaction(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	if requestKindIsCompaction(gjson.Get(raw, "request_kind").String()) {
		return true
	}
	compaction := gjson.Get(raw, "compaction")
	if !compaction.Exists() || compaction.Type == gjson.Null {
		return false
	}
	if strings.TrimSpace(compaction.Get("strategy").String()) != "" {
		return true
	}
	if strings.TrimSpace(compaction.Get("trigger").String()) != "" {
		return true
	}
	if strings.TrimSpace(compaction.Get("implementation").String()) != "" {
		return true
	}
	return false
}

func looksLikeCompactionPrompt(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	lower := strings.ToLower(text)
	needles := []string{
		"context checkpoint compaction",
		"handoff summary for another llm",
		"you are performing a context checkpoint compaction",
		"detailed summary of the conversation so far",
		"detailed summary of our conversation so far",
		"summary of the conversation so far",
		"summarize the conversation so far",
		"summarize this conversation",
		"compact the conversation",
		"compact this conversation",
		"asked you to compact",
		"you are a conversation summarizer",
		"tasked with summarizing conversations",
		"the conversation has grown too long",
		"the conversation is getting too long",
		"paying close attention to the user's explicit requests and your previous actions",
		"this summary should be thorough in capturing technical details",
		"create a comprehensive summary of this conversation",
		"compress this conversation",
		"conversation compaction",
	}
	for _, needle := range needles {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func inputHasItemType(body []byte, want string) bool {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	want = strings.ToLower(strings.TrimSpace(want))
	for _, item := range input.Array() {
		if strings.ToLower(strings.TrimSpace(item.Get("type").String())) == want {
			return true
		}
	}
	return false
}

func isSessionNameRequest(bodies [][]byte) bool {
	for _, body := range bodies {
		if len(body) == 0 {
			continue
		}
		if looksLikeClaudeCodeTitleRequest(body) {
			return true
		}
		if reasoningLooksUserFacing(body) {
			continue
		}
		// Codex Desktop task-title helpers embed the original user prompt,
		// so they are not small but are still title requests. Keep this after
		// the user-facing-effort skip so a normal xhigh chat that mentions
		// "generate a title" is not parked into the aux pool.
		if looksLikeSessionName(lastUserText(body)) {
			return true
		}
		if !requestLooksSmall(body) {
			continue
		}
		if looksLikeSessionName(sessionNameText(body)) {
			return true
		}
	}
	return false
}

func looksLikeClaudeCodeTitleRequest(body []byte) bool {
	if messageCount(body) > 6 {
		return false
	}
	// Claude Code 2.1.246+ names sessions with a title-only JSON schema and a
	// "naming a coding session" system prompt. Haiku is often remapped onto
	// grok-4.6, so thinking may be medium instead of disabled.
	if jsonSchemaIsTitleOnly(body) {
		return true
	}
	sys := systemText(body)
	instr := gjson.GetBytes(body, "instructions").String()
	if looksLikeSessionName(sys) || looksLikeSessionName(instr) {
		return true
	}
	return hasClaudeSessionTitleWrapper(sys + "\n" + instr + "\n" + lastUserText(body))
}

func hasClaudeSessionTitleWrapper(text string) bool {
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "<session>") || !strings.Contains(lower, "</session>") {
		return false
	}
	return strings.Contains(lower, "naming a coding session") ||
		strings.Contains(lower, "write the title in") ||
		strings.Contains(lower, `single "title" field`)
}

func thinkingDisabled(body []byte) bool {
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String())) {
	case "disabled", "none":
		return true
	}
	switch requestEffort(body) {
	case "none", "minimal":
		return true
	default:
		return false
	}
}

func jsonSchemaIsTitleOnly(body []byte) bool {
	for _, path := range []string{
		"output_config.format.schema",
		"output_config.format.json_schema.schema",
		"output_format.schema",
		"outputFormat.schema",
		"response_format.json_schema.schema",
		"text.format.schema",
		"text.format.json_schema.schema",
	} {
		schema := gjson.GetBytes(body, path)
		if !schema.Exists() || schema.Type == gjson.Null {
			continue
		}
		props := schema.Get("properties")
		if !props.Exists() {
			continue
		}
		n := 0
		hasTitle := false
		props.ForEach(func(key, _ gjson.Result) bool {
			n++
			if key.String() == "title" {
				hasTitle = true
			}
			return true
		})
		if !hasTitle || n == 0 || n > 2 {
			continue
		}
		required := schema.Get("required")
		if required.IsArray() && len(required.Array()) == 1 && required.Get("0").String() == "title" {
			return true
		}
		if n == 1 {
			return true
		}
	}
	return false
}

func systemText(body []byte) string {
	node := gjson.GetBytes(body, "system")
	if node.Type == gjson.String {
		return node.String()
	}
	if !node.IsArray() {
		return ""
	}
	var b strings.Builder
	for _, part := range node.Array() {
		if part.Type == gjson.String {
			b.WriteString(part.String())
			b.WriteByte('\n')
			continue
		}
		b.WriteString(part.Get("text").String())
		b.WriteByte('\n')
	}
	return b.String()
}

func messageCount(body []byte) int {
	count := 0
	for _, path := range []string{"input", "messages"} {
		node := gjson.GetBytes(body, path)
		if node.IsArray() {
			count += len(node.Array())
		}
	}
	return count
}

func isInternalRequest(bodies [][]byte) bool {
	for _, body := range bodies {
		if len(body) == 0 {
			continue
		}
		if bytesContainsFold(body, "ambient_suggestions") || bytesContainsFold(body, "ambient suggestion") {
			if requestLooksSmall(body) {
				return true
			}
			continue
		}
		if reasoningLooksUserFacing(body) || !requestLooksSmall(body) {
			continue
		}
		if isCodexPersona(body) && isLowEffort(body) {
			return true
		}
	}
	return false
}

func isCodexPersona(body []byte) bool {
	instructions := gjson.GetBytes(body, "instructions").String()
	if strings.Contains(instructions, "You are Codex") || strings.Contains(strings.ToLower(instructions), "commentary channel") {
		return true
	}
	return false
}

func isLowEffort(body []byte) bool {
	switch requestEffort(body) {
	case "low", "none", "minimal":
		return true
	default:
		return false
	}
}

func requestEffort(body []byte) string {
	effort := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String()))
	if effort == "" {
		effort = strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "reasoning_effort").String()))
	}
	return effort
}

func reasoningLooksUserFacing(body []byte) bool {
	switch requestEffort(body) {
	case "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

func requestLooksSmall(body []byte) bool {
	if utf8.RuneCountInString(sessionNameText(body)) > 4000 {
		return false
	}
	count := 0
	for _, path := range []string{"input", "messages"} {
		node := gjson.GetBytes(body, path)
		if !node.IsArray() {
			continue
		}
		count += len(node.Array())
	}
	return count <= 6
}

func looksLikeSessionName(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	lower := strings.ToLower(text)
	needles := []string{
		"title for this conversation",
		"title for the conversation",
		"title for this session",
		"generate a title",
		"generate a short title",
		"generate a concise title",
		"session title",
		"conversation title",
		"name this conversation",
		"name this session",
		"concise title",
		"short title for",
		"short title for a task",
		"title for a task",
		"title for this task",
		"concise ui title",
		"generate a concise ui title",
		"structured title field",
		"reply with the title",
		"title only",
		"3-6 word title",
		"3 to 6 word",
		"5-10 word title",
		"return a short title",
		"short title.",
		"naming a coding session",
		"pick it out of a long list of sessions",
		"return json with a single \"title\" field",
		"write the title in the predominant language",
		"write the title in the language",
		"会话名称",
		"会话标题",
		"对话标题",
		"生成标题",
		"给这次对话起",
		"为这次对话起名",
	}
	for _, needle := range needles {
		if strings.Contains(lower, needle) || strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func sessionNameText(body []byte) string {
	var b strings.Builder
	write := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || b.Len() >= 8*1024 {
			return
		}
		remain := 8*1024 - b.Len()
		if len(s) > remain {
			s = s[:remain]
		}
		b.WriteString(s)
		b.WriteByte('\n')
	}
	write(gjson.GetBytes(body, "instructions").String())
	write(systemText(body))
	write(lastUserText(body))
	return b.String()
}

func latestNonToolText(body []byte) string {
	for _, path := range []string{"input", "messages"} {
		node := gjson.GetBytes(body, path)
		if !node.IsArray() {
			continue
		}
		items := node.Array()
		for i := len(items) - 1; i >= 0; i-- {
			item := items[i]
			typ := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
			role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
			if role == "tool" || typ == "function_call" || typ == "function_call_output" || typ == "tool_result" || strings.HasPrefix(typ, "tool") {
				continue
			}
			if typ == "compaction" || typ == "compaction_trigger" || typ == "compaction_summary" {
				continue
			}
			text := messageItemText(item)
			if strings.TrimSpace(text) == "" || strings.Contains(text, "<total_tokens>") {
				continue
			}
			return text
		}
	}
	return ""
}

func messageItemText(item gjson.Result) string {
	content := item.Get("content")
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		var b strings.Builder
		for _, part := range content.Array() {
			if part.Type == gjson.String {
				b.WriteString(part.String())
				b.WriteByte('\n')
				continue
			}
			b.WriteString(part.Get("text").String())
			b.WriteByte('\n')
		}
		return b.String()
	}
	return item.Get("text").String()
}

func lastUserText(body []byte) string {
	for _, path := range []string{"input", "messages"} {
		node := gjson.GetBytes(body, path)
		if node.Type == gjson.String {
			return node.String()
		}
		if !node.IsArray() {
			continue
		}
		items := node.Array()
		for i := len(items) - 1; i >= 0; i-- {
			item := items[i]
			typ := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
			role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
			if typ == "compaction" || typ == "compaction_trigger" || typ == "compaction_summary" {
				continue
			}
			if role != "" && role != "user" {
				continue
			}
			if text := item.Get("content").String(); strings.TrimSpace(text) != "" {
				return text
			}
			content := item.Get("content")
			if content.IsArray() {
				var b strings.Builder
				for _, part := range content.Array() {
					b.WriteString(part.Get("text").String())
					b.WriteByte('\n')
				}
				if strings.TrimSpace(b.String()) != "" {
					return b.String()
				}
			}
		}
	}
	return ""
}

func bytesContainsFold(body []byte, needle string) bool {
	if len(body) == 0 || needle == "" {
		return false
	}
	return strings.Contains(strings.ToLower(string(body)), strings.ToLower(needle))
}
