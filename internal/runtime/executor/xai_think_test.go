package executor

import (
	"context"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestParseXAIThinkEvidenceZeroReasoning(t *testing.T) {
	body := []byte(`{"output":[{"type":"reasoning","summary":[],"content":[]}]}`)
	ev := parseXAIThinkEvidence(body)
	if !ev.HasThink || ev.Length != 0 || !ev.bad() {
		t.Fatalf("want 有(0字), got has=%v len=%d", ev.HasThink, ev.Length)
	}
}

func TestParseXAIThinkEvidenceGoodSSE(t *testing.T) {
	body := []byte("data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"hello\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"output\":[]}\n\n")
	ev := parseXAIThinkFromSSE(body)
	if !ev.ok() || ev.Length != 5 {
		t.Fatalf("want good think len=5, got has=%v len=%d", ev.HasThink, ev.Length)
	}
}

func TestXAIRequestExpectsThink(t *testing.T) {
	if !xaiRequestExpectsThink([]byte(`{"reasoning":{"effort":"high"}}`)) {
		t.Fatal("high should expect think")
	}
	if xaiRequestExpectsThink([]byte(`{"reasoning":{"effort":"none"}}`)) {
		t.Fatal("none should not expect think")
	}
	if xaiRequestExpectsThink([]byte(`{}`)) {
		t.Fatal("missing effort should not expect think")
	}
}

func TestXAIGateThinkStream(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-1"}
	upstream := []byte(`{"reasoning":{"effort":"high"}}`)
	raw := []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"summary\":[]}}\n")
	err := xaiGateThinkStream(context.Background(), auth, upstream, raw, nil, cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil, nil)
	if err == nil {
		t.Fatal("expected no_think_stream error")
	}
	noThink, ok := cliproxyexecutor.AsNoThinkStream(err)
	if !ok || noThink.AuthID != "auth-1" {
		t.Fatalf("unexpected error: %#v", err)
	}
}

func TestXAIGateThinkStreamSkipsAuxPool(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-aux"}
	upstream := []byte(`{"reasoning":{"effort":"xhigh"}}`)
	raw := []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"summary\":[]}}\n")
	ctx := cliproxyexecutor.WithRequestClass(context.Background(), cliproxyexecutor.RequestClassCompaction)
	if err := xaiGateThinkStream(ctx, auth, upstream, raw, nil, cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil, nil); err != nil {
		t.Fatalf("aux pool should skip think gate: %v", err)
	}
}

func TestXAIGateThinkStreamNotifiesOnThink(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-ok"}
	upstream := []byte(`{"reasoning":{"effort":"high"}}`)
	raw := []byte("data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"hello\"}\n")
	var got string
	ctx := cliproxyexecutor.WithXAIThinkOK(context.Background(), func(authID string) { got = authID })
	if err := xaiGateThinkStream(ctx, auth, upstream, raw, nil, cliproxyexecutor.Response{}, nil, nil); err != nil {
		t.Fatalf("good think should pass: %v", err)
	}
	if got != "auth-ok" {
		t.Fatalf("NotifyXAIThinkOK auth=%q, want auth-ok", got)
	}
}

func TestXAIGateThinkStreamAuxWithThinkNotifies(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-aux"}
	upstream := []byte(`{"reasoning":{"effort":"xhigh"}}`)
	raw := []byte("data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"hello\"}\n")
	var got string
	ctx := cliproxyexecutor.WithRequestClass(context.Background(), cliproxyexecutor.RequestClassCompaction)
	ctx = cliproxyexecutor.WithXAIThinkOK(ctx, func(authID string) { got = authID })
	if err := xaiGateThinkStream(ctx, auth, upstream, raw, nil, cliproxyexecutor.Response{}, nil, nil); err != nil {
		t.Fatalf("aux good think should pass: %v", err)
	}
	if got != "auth-aux" {
		t.Fatalf("aux NotifyXAIThinkOK auth=%q, want auth-aux", got)
	}
}

func TestXAIStreamEventIsAnswer(t *testing.T) {
	if !xaiStreamEventIsAnswer([]byte(`{"type":"response.output_text.delta","delta":"hi"}`)) {
		t.Fatal("output_text.delta should be answer")
	}
	if !xaiStreamEventIsAnswer([]byte(`{"type":"response.output_item.added","item":{"type":"message"}}`)) {
		t.Fatal("message item should be answer")
	}
	if xaiStreamEventIsAnswer([]byte(`{"type":"response.reasoning_text.delta","delta":"think"}`)) {
		t.Fatal("reasoning delta is not answer")
	}
	if xaiStreamEventIsAnswer([]byte(`{"type":"response.created"}`)) {
		t.Fatal("created is not answer")
	}
}

func TestIngestXAIThinkEventAccumulates(t *testing.T) {
	var ev xaiThinkEvidence
	ingestXAIThinkEvent(&ev, []byte(`{"type":"response.reasoning_text.delta","delta":"ab"}`))
	ingestXAIThinkEvent(&ev, []byte(`{"type":"response.reasoning_text.delta","delta":"c"}`))
	if !ev.ok() || ev.Length != 3 {
		t.Fatalf("got has=%v len=%d", ev.HasThink, ev.Length)
	}
}

func TestXAIStreamEventStatusErrQuota(t *testing.T) {
	err, ok := xaiStreamEventStatusErr([]byte(`{"type":"error","status":429,"error":{"code":"subscription:free-usage-exhausted","message":"You've used all the included free usage for now."}}`))
	if !ok || err == nil {
		t.Fatal("want quota status error")
	}
	if _, isNoThink := cliproxyexecutor.AsNoThinkStream(err); isNoThink {
		t.Fatalf("quota must not be no_think_stream: %v", err)
	}
	if !xaiStreamEventIsAnswer([]byte(`{"type":"response.output_text.delta","delta":"hi"}`)) {
		t.Fatal("sanity")
	}
	if err2, ok2 := xaiStreamEventStatusErr([]byte(`{"type":"response.output_text.delta","delta":"hi"}`)); ok2 || err2 != nil {
		t.Fatalf("answer is not status err: %v", err2)
	}
}

func TestParseXAIThinkEvidenceEncryptedContent(t *testing.T) {
	body := []byte(`{"output":[{"type":"reasoning","summary":[],"encrypted_content":"abc123"}]}`)
	ev := parseXAIThinkEvidence(body)
	if !ev.Encrypted || ev.ok() {
		t.Fatalf("encrypted blob is console-only think, got has=%v enc=%v len=%d ok=%v", ev.HasThink, ev.Encrypted, ev.Length, ev.ok())
	}
}

func TestXAIGateThinkStreamAcceptsEncryptedContentOnConsole(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "auth-console",
		Provider: "xai",
		Metadata: map[string]any{"sso": "token", "xai_channel": "console"},
	}
	upstream := []byte(`{"reasoning":{"effort":"high"}}`)
	raw := []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"opaque\"}}\n")
	if err := xaiGateThinkStream(context.Background(), auth, upstream, raw, nil, cliproxyexecutor.Response{}, nil, nil); err != nil {
		t.Fatalf("encrypted console think should pass: %v", err)
	}
}

func TestXAIGateThinkStreamBuildEncryptedIsNoThink(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "auth-build", Provider: "xai"}
	upstream := []byte(`{"reasoning":{"effort":"high"}}`)
	raw := []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"opaque\"}}\n")
	err := xaiGateThinkStream(context.Background(), auth, upstream, raw, nil, cliproxyexecutor.Response{}, nil, nil)
	if err == nil {
		t.Fatal("build encrypted-only stream must not count as think")
	}
}

func TestEnsureXAIConsoleReasoningInclude(t *testing.T) {
	got := ensureXAIConsoleReasoningInclude([]byte(`{"model":"grok-4.6"}`))
	if gjson.GetBytes(got, "include.0").String() != "reasoning.encrypted_content" {
		t.Fatalf("include = %s", got)
	}
	got = ensureXAIConsoleReasoningInclude([]byte(`{"include":["reasoning.encrypted_content"]}`))
	if len(gjson.GetBytes(got, "include").Array()) != 1 {
		t.Fatalf("should not duplicate include: %s", got)
	}
}

func TestEnsureXAIConsoleReasoningSummary(t *testing.T) {
	got := ensureXAIConsoleReasoningSummary([]byte(`{"model":"grok-4.6","reasoning":{"effort":"xhigh"}}`))
	if gjson.GetBytes(got, "reasoning.summary").String() != "auto" {
		t.Fatalf("summary = %s", got)
	}
	got = ensureXAIConsoleReasoningSummary([]byte(`{"reasoning":{"effort":"high","summary":"detailed"}}`))
	if gjson.GetBytes(got, "reasoning.summary").String() != "detailed" {
		t.Fatalf("should keep client summary: %s", got)
	}
	got = ensureXAIConsoleReasoningSummary([]byte(`{"reasoning":{"effort":"none"}}`))
	if gjson.GetBytes(got, "reasoning.summary").Exists() {
		t.Fatalf("none should not add summary: %s", got)
	}
}
