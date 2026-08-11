package executor

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
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
	err := xaiGateThinkStream(auth, upstream, raw, nil, cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil, nil)
	if err == nil {
		t.Fatal("expected no_think_stream error")
	}
	noThink, ok := cliproxyexecutor.AsNoThinkStream(err)
	if !ok || noThink.AuthID != "auth-1" {
		t.Fatalf("unexpected error: %#v", err)
	}
}
