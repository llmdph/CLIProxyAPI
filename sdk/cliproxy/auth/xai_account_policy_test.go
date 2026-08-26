package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
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
	if updated.Metadata == nil {
		t.Fatal("expected metadata for auto-disable persistence")
	}
	if got, _ := updated.Metadata["disabled"].(bool); !got {
		t.Fatalf("metadata disabled = %#v, want true", updated.Metadata["disabled"])
	}
	if got := metadataString(updated.Metadata, xaiAutoDisableStatusMessageKey); !isXAIAutoDisabledStatusMessage(got) {
		t.Fatalf("metadata status_message = %q", got)
	}
	if _, ok := parseFlexibleTime(updated.Metadata[xaiAutoDisableNextRetryAfterKey]); !ok {
		t.Fatalf("metadata next_retry_after missing: %#v", updated.Metadata[xaiAutoDisableNextRetryAfterKey])
	}
}

func TestReenableExpiredXAIDisabledAuths(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	now := time.Now()
	auth := &Auth{
		ID:             "xai-reenable@outlook.com",
		Provider:       "xai",
		Disabled:       true,
		Status:         StatusDisabled,
		StatusMessage:  "quota_exhausted: free-usage-exhausted",
		UpdatedAt:      now.Add(-25 * time.Hour),
		NextRetryAfter: now.Add(-time.Minute),
	}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	if n := m.reenableExpiredXAIDisabledAuths(context.Background(), now); n != 1 {
		t.Fatalf("reenabled = %d, want 1", n)
	}
	updated, ok := m.GetByID("xai-reenable@outlook.com")
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

func TestRestoreXAIAutoDisableStateFromLegacyMetadata(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Add(-30 * time.Hour)
	auth := &Auth{
		ID:        "xai-legacy@outlook.com",
		Provider:  "xai",
		Disabled:  true,
		Status:    StatusDisabled,
		UpdatedAt: now,
		Metadata:  map[string]any{"disabled": true, "type": "xai", "email": "legacy@outlook.com"},
	}
	RestoreXAIAutoDisableStateFromMetadata(auth)
	if !isXAIAutoDisabledStatusMessage(auth.StatusMessage) {
		t.Fatalf("status message = %q", auth.StatusMessage)
	}
	if auth.NextRetryAfter.IsZero() {
		t.Fatal("expected NextRetryAfter from UpdatedAt+24h")
	}
	want := now.Add(xaiDisabledAutoReenableAfter)
	if auth.NextRetryAfter.Sub(want) > time.Second || want.Sub(auth.NextRetryAfter) > time.Second {
		t.Fatalf("NextRetryAfter = %v, want %v", auth.NextRetryAfter, want)
	}
	m := NewManager(nil, nil, nil)
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	if n := m.reenableExpiredXAIDisabledAuths(context.Background(), time.Now()); n != 1 {
		t.Fatalf("reenabled = %d, want 1", n)
	}
}

func TestDisableAuthForQuotaExhaustedKeepsNextRetryAfterThroughUpdate(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "xai-keep-retry", Provider: "xai", Status: StatusActive, Metadata: map[string]any{"type": "xai"}}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	m.disableAuthForQuotaExhausted(context.Background(), auth, errors.New(`subscription:free-usage-exhausted`))
	updated, ok := m.GetByID("xai-keep-retry")
	if !ok || updated == nil {
		t.Fatal("missing auth")
	}
	if updated.NextRetryAfter.IsZero() {
		t.Fatal("NextRetryAfter cleared by Update()")
	}
	if !updated.Disabled {
		t.Fatal("expected disabled")
	}
}

func TestReenableScanDoesNotMutateLiveAuthMetadataConcurrently(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	now := time.Now()
	for i := 0; i < 64; i++ {
		id := "xai-race-" + strconv.Itoa(i)
		auth := &Auth{
			ID:             id,
			Provider:       "xai",
			Disabled:       true,
			Status:         StatusDisabled,
			StatusMessage:  "quota_exhausted",
			UpdatedAt:      now.Add(-30 * time.Hour),
			NextRetryAfter: now.Add(-time.Minute),
			Metadata: map[string]any{
				"type":     "xai",
				"disabled": true,
				"email":    "race@example.com",
			},
		}
		if _, err := m.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_ = m.List()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				_ = m.reenableExpiredXAIDisabledAuths(context.Background(), now)
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	wg.Wait()
}

func TestMarkResultPreservesXAIQuotaAutoDisableSchedule(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "xai-mark-keep@outlook.com",
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "xai", "email": "xai-mark-keep@outlook.com"},
	}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	quotaErr := errors.New(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.6"}`)
	m.disableAuthForQuotaExhausted(context.Background(), auth, quotaErr)
	before, ok := m.GetByID("xai-mark-keep@outlook.com")
	if !ok || before == nil {
		t.Fatal("missing auth after disable")
	}
	scheduled := before.NextRetryAfter
	if scheduled.IsZero() {
		t.Fatal("expected 24h NextRetryAfter after disable")
	}
	m.MarkResult(context.Background(), Result{
		AuthID:   before.ID,
		Provider: "xai",
		Model:    "grok-4.6",
		Success:  false,
		Error:    resultErrorFromError(quotaErr),
	})
	updated, ok := m.GetByID("xai-mark-keep@outlook.com")
	if !ok || updated == nil {
		t.Fatal("missing auth after MarkResult")
	}
	if !updated.Disabled || updated.Status != StatusDisabled {
		t.Fatalf("MarkResult dropped auto-disable: disabled=%v status=%s", updated.Disabled, updated.Status)
	}
	if updated.NextRetryAfter.IsZero() || updated.NextRetryAfter.Sub(scheduled) > time.Second || scheduled.Sub(updated.NextRetryAfter) > time.Second {
		t.Fatalf("MarkResult cleared 24h schedule: before=%v after=%v", scheduled, updated.NextRetryAfter)
	}
}

type quotaStreamFailExecutor struct {
	failID string
}

func (e quotaStreamFailExecutor) Identifier() string { return "xai" }
func (e quotaStreamFailExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}
func (e quotaStreamFailExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if auth != nil && auth.ID == e.failID {
		return nil, errors.New(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.6"}`)
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"ok":true}`)}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}
func (e quotaStreamFailExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}
func (e quotaStreamFailExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}
func (e quotaStreamFailExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestExecuteStreamDisablesOnQuotaExhausted(t *testing.T) {
	m := NewManager(nil, nil, nil)
	failID := "xai-stream-quota@outlook.com"
	model := "grok-4.6"
	m.RegisterExecutor(quotaStreamFailExecutor{failID: failID})
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(failID, "xai", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(failID) })
	auth := &Auth{
		ID:       failID,
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "xai", "email": failID},
	}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register %s: %v", failID, err)
	}
	_, err := m.ExecuteStream(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
	if err == nil {
		t.Fatal("ExecuteStream() error = nil, want quota exhausted")
	}
	if !isXAIQuotaExhaustedError(err) {
		t.Fatalf("ExecuteStream() error = %v, want quota exhausted", err)
	}
	failed, ok := m.GetByID(failID)
	if !ok || failed == nil {
		t.Fatal("missing exhausted auth")
	}
	if !failed.Disabled || failed.Status != StatusDisabled {
		t.Fatalf("stream quota did not disable auth: disabled=%v status=%s message=%q", failed.Disabled, failed.Status, failed.StatusMessage)
	}
	if failed.NextRetryAfter.IsZero() {
		t.Fatal("expected 24h re-enable schedule after stream quota disable")
	}
	delta := time.Until(failed.NextRetryAfter)
	if delta < 23*time.Hour || delta > 25*time.Hour {
		t.Fatalf("NextRetryAfter delta = %v, want ~24h", delta)
	}
}

func TestReenableExpiredXAIDisabledAuthsSkipsNonOutlook(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	now := time.Now()
	auth := &Auth{
		ID:             "xai-skip@llmdph.site",
		Provider:       "xai",
		Disabled:       true,
		Status:         StatusDisabled,
		StatusMessage:  "quota_exhausted: free-usage-exhausted",
		UpdatedAt:      now.Add(-25 * time.Hour),
		NextRetryAfter: now.Add(-time.Minute),
		Metadata:       map[string]any{"email": "skip@llmdph.site", "type": "xai"},
	}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	if n := m.reenableExpiredXAIDisabledAuths(context.Background(), now); n != 0 {
		t.Fatalf("reenabled = %d, want 0", n)
	}
	updated, ok := m.GetByID("xai-skip@llmdph.site")
	if !ok || updated == nil {
		t.Fatal("missing auth")
	}
	if !updated.Disabled || updated.Status != StatusDisabled {
		t.Fatalf("non-outlook auth was re-enabled: disabled=%v status=%s", updated.Disabled, updated.Status)
	}
}
