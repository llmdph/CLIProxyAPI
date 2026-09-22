package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestBuildQuotaSwitchesToConsole(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, &FillFirstSelector{}, nil)
	auth := &Auth{
		ID:       "xai-dual@outlook.com",
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":  "xai",
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

func TestBuildQuotaDoesNotSwitchToConsoleWhenDisabled(t *testing.T) {
	SetXAIConsoleEnabled(false)
	t.Cleanup(func() { SetXAIConsoleEnabled(true) })
	m := NewManager(nil, &FillFirstSelector{}, nil)
	auth := &Auth{
		ID:       "xai-no-console@outlook.com",
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":  "xai",
			"email": "xai-no-console@outlook.com",
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
	if !updated.Disabled {
		t.Fatal("build quota should disable this account instead of switching to console")
	}
	if XAIUsingConsoleChannel(updated) {
		t.Fatal("console must stay off")
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
			"type":                    "xai",
			"email":                   "xai-both@outlook.com",
			"sso":                     "console-sso-token",
			"xai_channel":             XAIChannelConsole,
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

func TestConsoleQuotaResourceExhaustedDisablesUntilBuildWindow(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, &FillFirstSelector{}, nil)
	auth := &Auth{
		ID:       "xai-console-resource@outlook.com",
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":                    "xai",
			"email":                   "xai-console-resource@outlook.com",
			"sso":                     "console-sso-token",
			"xai_channel":             XAIChannelConsole,
			"build_quota_retry_after": time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339Nano),
		},
	}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	quotaErr := errors.New(`{"code":"resource-exhausted","error":"Free usage quota exceeded. Purchase credits or provision an API key at https://console.x.ai/"}`)
	if !m.disableXAIAuthIfQuotaExhausted(context.Background(), auth, "xai", quotaErr) {
		t.Fatal("expected console quota handler")
	}
	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("missing auth")
	}
	if !updated.Disabled || updated.Status != StatusDisabled {
		t.Fatalf("console quota should disable while build window is active: disabled=%v status=%s", updated.Disabled, updated.Status)
	}
	if !XAIConsoleQuotaExhausted(updated) {
		t.Fatal("expected permanent console mark")
	}
}

type buildThenConsoleExecutor struct {
	mu    sync.Mutex
	calls []string
}

func (e *buildThenConsoleExecutor) Identifier() string { return "xai" }
func (e *buildThenConsoleExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}
func (e *buildThenConsoleExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	id := ""
	if auth != nil {
		id = auth.ID
	}
	channel := XAIChannelOf(auth)
	e.mu.Lock()
	e.calls = append(e.calls, id+":"+channel)
	e.mu.Unlock()
	if channel != XAIChannelConsole {
		return nil, errors.New(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.6"}`)
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"ok":true}`)}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}
func (e *buildThenConsoleExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}
func (e *buildThenConsoleExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}
func (e *buildThenConsoleExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestMarkResultSkipsCooldownAfterBuildToConsoleSwitch(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, &FillFirstSelector{}, nil)
	auth := &Auth{
		ID:       "xai-failover@outlook.com",
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":  "xai",
			"email": "xai-failover@outlook.com",
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
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "xai",
		Model:    "grok-4.6",
		Success:  false,
		Error:    resultErrorFromError(quotaErr),
	})
	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("missing auth")
	}
	if !XAIUsingConsoleChannel(updated) {
		t.Fatal("expected console channel")
	}
	blocked, reason, _ := isAuthBlockedForModel(updated, "grok-4.6", time.Now())
	if blocked {
		t.Fatalf("channel failover cooled the account: reason=%v", reason)
	}
}

func TestExecuteStreamBuildQuotaRetriesSameAccountWithoutExpandingBucket(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	exec := &buildThenConsoleExecutor{}
	m.RegisterExecutor(exec)
	keepID := "xai-keep@outlook.com"
	otherID := "xai-other@outlook.com"
	model := "grok-4.6"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(keepID, "xai", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(otherID, "xai", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(keepID)
		reg.UnregisterClient(otherID)
	})
	for _, id := range []string{keepID, otherID} {
		auth := &Auth{
			ID:       id,
			Provider: "xai",
			Status:   StatusActive,
			Metadata: map[string]any{"type": "xai", "email": id, "sso": "console-sso-token"},
		}
		if _, err := m.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	if !m.fillFirst.addIdle(keepID) {
		t.Fatal("seed keep account")
	}
	result, err := m.ExecuteStream(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hello"}`)}, cliproxyexecutor.Options{Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if result == nil {
		t.Fatal("missing stream result")
	}
	exec.mu.Lock()
	calls := append([]string(nil), exec.calls...)
	exec.mu.Unlock()
	if len(calls) != 2 || calls[0] != keepID+":"+XAIChannelBuild || calls[1] != keepID+":"+XAIChannelConsole {
		t.Fatalf("calls = %v, want [%s:build %s:console]", calls, keepID, keepID)
	}
	if got := m.fillFirst.memberIDs(); len(got) != 1 || got[0] != keepID {
		t.Fatalf("bucket members = %v, want [%s]", got, keepID)
	}
	updated, ok := m.GetByID(keepID)
	if !ok || updated == nil || !XAIUsingConsoleChannel(updated) {
		t.Fatal("keep account should stay on console")
	}
}
