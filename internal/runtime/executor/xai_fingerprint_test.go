package executor

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	xaiauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/xai"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestXAIClientIdentityRotatesPerRequest(t *testing.T) {
	first := newXAIClientIdentity()
	second := newXAIClientIdentity()
	if first.AgentID == "" || first.MACAddress == "" || first.DeviceID == "" {
		t.Fatalf("incomplete identity: %+v", first)
	}
	if first.AgentID == second.AgentID {
		t.Fatalf("agent id reused: %q", first.AgentID)
	}
	if first.MACAddress == second.MACAddress {
		t.Fatalf("mac reused: %q", first.MACAddress)
	}
	if first.RequestID == second.RequestID {
		t.Fatalf("request id reused: %q", first.RequestID)
	}
	if !strings.HasPrefix(first.UserAgent, "grok-shell/"+xaiClientVersionValue+" (") {
		t.Fatalf("unexpected UA: %q", first.UserAgent)
	}
	if first.AcceptLanguage == "" || first.TraceParent == "" {
		t.Fatalf("missing secondary headers: lang=%q trace=%q", first.AcceptLanguage, first.TraceParent)
	}
	if !strings.HasPrefix(first.TraceParent, "00-") || len(first.TraceParent) != 55 {
		t.Fatalf("bad traceparent: %q", first.TraceParent)
	}
	if first.TraceParent == second.TraceParent {
		t.Fatalf("traceparent reused: %q", first.TraceParent)
	}
}

func TestXAIIdentityForAuthStickyDeviceFingerprint(t *testing.T) {
	authID := "auth-sticky-device"
	InvalidateXAIClientIdentity(authID)
	t.Cleanup(func() { InvalidateXAIClientIdentity(authID) })

	first := xaiIdentityForAuth(authID)
	second := xaiIdentityForAuth(authID)
	if first.AgentID != second.AgentID || first.MACAddress != second.MACAddress || first.DeviceID != second.DeviceID {
		t.Fatalf("device fingerprint changed for same auth: %+v vs %+v", first, second)
	}
	if first.RequestID == second.RequestID {
		t.Fatalf("request id should rotate per HTTP call: %q", first.RequestID)
	}
	if first.TraceParent == second.TraceParent {
		t.Fatalf("traceparent should rotate per HTTP call: %q", first.TraceParent)
	}

	other := xaiIdentityForAuth("auth-other-device")
	t.Cleanup(func() { InvalidateXAIClientIdentity("auth-other-device") })
	if other.AgentID == first.AgentID {
		t.Fatalf("different auth reused agent id: %q", other.AgentID)
	}

	InvalidateXAIClientIdentity(authID)
	third := xaiIdentityForAuth(authID)
	if third.AgentID == first.AgentID {
		t.Fatalf("invalidate did not mint a new device profile")
	}
}

func TestApplyXAIChatHeadersStickyDeviceFingerprintPerAuth(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID: "auth-fp-1",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"base_url":  xaiauth.DefaultAPIBaseURL,
		},
	}
	InvalidateXAIClientIdentity(auth.ID)
	t.Cleanup(func() { InvalidateXAIClientIdentity(auth.ID) })

	req1 := httptest.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	req2 := httptest.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	applyXAIChatHeaders(req1, auth, "tok", true, "sess-stable")
	applyXAIChatHeaders(req2, auth, "tok", true, "sess-stable")

	if req1.Header.Get("x-grok-conv-id") != "sess-stable" || req2.Header.Get("x-grok-conv-id") != "sess-stable" {
		t.Fatalf("session affinity broken: %q / %q", req1.Header.Get("x-grok-conv-id"), req2.Header.Get("x-grok-conv-id"))
	}
	if req1.Header.Get("x-grok-agent-id") != req2.Header.Get("x-grok-agent-id") {
		t.Fatalf("agent id should stick for same auth")
	}
	if req1.Header.Get("x-device-mac") != req2.Header.Get("x-device-mac") {
		t.Fatalf("mac should stick for same auth")
	}
	if req1.Header.Get("traceparent") == "" || req1.Header.Get("Accept-Language") == "" {
		t.Fatalf("missing secondary headers: trace=%q lang=%q", req1.Header.Get("traceparent"), req1.Header.Get("Accept-Language"))
	}
	if req1.Header.Get("traceparent") == req2.Header.Get("traceparent") {
		t.Fatalf("traceparent should rotate per HTTP call")
	}
	if req1.Header.Get("x-grok-req-id") == req2.Header.Get("x-grok-req-id") {
		t.Fatalf("req-id should rotate per HTTP call")
	}
	if req1.Header.Get("Connection") == "close" || req2.Header.Get("Connection") == "close" {
		t.Fatalf("same account must not force Connection: close")
	}
	auth2 := &cliproxyauth.Auth{
		ID: "auth-fp-2",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"base_url":  xaiauth.DefaultAPIBaseURL,
		},
	}
	InvalidateXAIClientIdentity(auth2.ID)
	t.Cleanup(func() { InvalidateXAIClientIdentity(auth2.ID) })
	req3 := httptest.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	applyXAIChatHeaders(req3, auth2, "tok", true, "sess-stable")
	if req3.Header.Get("x-grok-agent-id") == req1.Header.Get("x-grok-agent-id") {
		t.Fatalf("next account should mint a new agent id")
	}
	if req3.Header.Get("x-grok-req-id") == req1.Header.Get("x-grok-req-id") {
		t.Fatalf("next account should mint a new req-id")
	}
	if req3.Header.Get("traceparent") == req1.Header.Get("traceparent") {
		t.Fatalf("next account should mint a new traceparent")
	}
}
