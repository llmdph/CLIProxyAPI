package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestIsXAIQuotaExhaustedError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "free usage code", err: errors.New(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage"}`), want: true},
		{name: "spending limit", err: errors.New(`{"code":"personal-team-blocked:spending-limit","error":"out of credits"}`), want: true},
		{name: "bare 429", err: errors.New(`status 429: rate limit`), want: false},
		{name: "network", err: errors.New(`dial tcp: i/o timeout`), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isXAIQuotaExhaustedError(tc.err); got != tc.want {
				t.Fatalf("isXAIQuotaExhaustedError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShouldRotateXAICredential(t *testing.T) {
	t.Parallel()
	if shouldRotateXAICredential(errors.New("temporary upstream blip")) {
		t.Fatal("transient error should not rotate")
	}
	if !shouldRotateXAICredential(errors.New(`subscription:free-usage-exhausted`)) {
		t.Fatal("quota exhausted should rotate")
	}
	if !shouldRotateXAICredential(&cliproxyexecutor.NoThinkStreamError{Detail: "missing think"}) {
		t.Fatal("no-think should rotate")
	}
}

func TestIsCloudflareChallengeErrorMessage_XAIAttentionRequired(t *testing.T) {
	t.Parallel()
	msg := `<!DOCTYPE html><html><title>Attention Required! | Cloudflare</title><div id="cf-error-details">Sorry, you have been blocked</div>`
	if !isCloudflareChallengeErrorMessage(msg) {
		t.Fatal("expected cloudflare attention-required HTML to match")
	}
	if !isCloudflareChallengeError(errors.New(msg)) {
		t.Fatal("expected cloudflare error wrapper to match")
	}
	if isRequestInvalidError(errors.New(msg)) {
		t.Fatal("cloudflare HTML must not be treated as request-invalid")
	}
}

func TestDisableAuthForQuotaExhaustedSchedules24hReenable(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "xai-quota", Provider: "xai", Status: StatusActive}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	before := time.Now()
	m.disableAuthForQuotaExhausted(context.Background(), auth, errors.New(`{"code":"subscription:free-usage-exhausted"}`))
	updated, ok := m.GetByID("xai-quota")
	if !ok || updated == nil {
		t.Fatal("missing updated auth")
	}
	if !updated.Disabled || updated.Status != StatusDisabled {
		t.Fatalf("auth not disabled: disabled=%v status=%s", updated.Disabled, updated.Status)
	}
	if updated.NextRetryAfter.IsZero() {
		t.Fatal("expected NextRetryAfter for auto re-enable")
	}
	delta := updated.NextRetryAfter.Sub(before)
	if delta < 23*time.Hour || delta > 25*time.Hour {
		t.Fatalf("NextRetryAfter delta = %v, want ~24h", delta)
	}
	if !isXAIAutoDisabledStatusMessage(updated.StatusMessage) {
		t.Fatalf("status message not auto-disable marked: %q", updated.StatusMessage)
	}
}

func TestReenableExpiredXAIDisabledAuths(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	now := time.Now()
	auth := &Auth{
		ID:            "xai-reenable",
		Provider:      "xai",
		Disabled:      true,
		Status:        StatusDisabled,
		StatusMessage: "quota_exhausted: free-usage-exhausted",
		UpdatedAt:     now.Add(-25 * time.Hour),
		NextRetryAfter: now.Add(-time.Minute),
	}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	if n := m.reenableExpiredXAIDisabledAuths(context.Background(), now); n != 1 {
		t.Fatalf("reenabled = %d, want 1", n)
	}
	updated, ok := m.GetByID("xai-reenable")
	if !ok || updated == nil {
		t.Fatal("missing auth")
	}
	if updated.Disabled || updated.Status != StatusActive {
		t.Fatalf("auth not re-enabled: disabled=%v status=%s", updated.Disabled, updated.Status)
	}
	if updated.StatusMessage != "" || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("unexpected leftover state message=%q next=%v", updated.StatusMessage, updated.NextRetryAfter)
	}
}
