package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBuildQuotaSwitchesToConsole(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, &FillFirstSelector{}, nil)
	auth := &Auth{
		ID:       "xai-dual@outlook.com",
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type": "xai",
			"email": "xai-dual@outlook.com",
			"sso":   "console-sso-token",
		},
	}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	quotaErr := errors.New(`{"code":"subscription:free-usage-exhausted"}`)
	if !m.disableXAIAuthIfQuotaExhausted(context.Background(), auth, "xai", quotaErr) {
		t.Fatal("expected quota handler")
	}
	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("missing auth")
	}
	if updated.Disabled || updated.Status == StatusDisabled {
		t.Fatalf("should stay active for console: disabled=%v status=%s", updated.Disabled, updated.Status)
	}
	if !XAIUsingConsoleChannel(updated) {
		t.Fatal("expected console channel")
	}
	if !xaiBuildWindowActive(updated, time.Now().Add(time.Minute)) {
		t.Fatal("expected build 24h window")
	}
}

func TestConsoleQuotaReturnsToBuildWhenWindowExpired(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, &FillFirstSelector{}, nil)
	auth := &Auth{
		ID:       "xai-console-done@outlook.com",
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":        "xai",
			"email":       "xai-console-done@outlook.com",
			"sso":         "console-sso-token",
			"xai_channel": XAIChannelConsole,
		},
	}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	quotaErr := errors.New(`{"code":"subscription:free-usage-exhausted"}`)
	if !m.disableXAIAuthIfQuotaExhausted(context.Background(), auth, "xai", quotaErr) {
		t.Fatal("expected quota handler")
	}
	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("missing auth")
	}
	if updated.Disabled {
		t.Fatal("build available, should not disable")
	}
	if XAIUsingConsoleChannel(updated) {
		t.Fatal("console should be marked exhausted")
	}
	if !XAIConsoleQuotaExhausted(updated) {
		t.Fatal("expected permanent console mark")
	}
}

func TestConsoleQuotaDisablesUntilBuildWindow(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, &FillFirstSelector{}, nil)
	auth := &Auth{
		ID:       "xai-both@outlook.com",
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":                   "xai",
			"email":                  "xai-both@outlook.com",
			"sso":                    "console-sso-token",
			"xai_channel":            XAIChannelConsole,
			"build_quota_retry_after": time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339Nano),
		},
	}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	quotaErr := errors.New(`{"code":"subscription:free-usage-exhausted"}`)
	if !m.disableXAIAuthIfQuotaExhausted(context.Background(), auth, "xai", quotaErr) {
		t.Fatal("expected quota handler")
	}
	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("missing auth")
	}
	if !updated.Disabled || updated.Status != StatusDisabled {
		t.Fatalf("both exhausted should disable: disabled=%v status=%s", updated.Disabled, updated.Status)
	}
	if !XAIConsoleQuotaExhausted(updated) {
		t.Fatal("expected permanent console mark")
	}
}
