package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestGrokConversationSessionIDPrefersConvHeader(t *testing.T) {
	t.Parallel()
	req := cliproxyexecutor.Request{
		Payload:  []byte(`{"prompt_cache_key":"pck-1"}`),
		Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "exec-1"},
	}
	opts := cliproxyexecutor.Options{
		Headers: http.Header{
			"X-Grok-Conv-Id":    []string{"conv-header"},
			"X-Grok-Session-Id": []string{"session-header"},
			"Session-Id":        []string{"codex-session"},
		},
		Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "exec-opts"},
	}
	if got := grokConversationSessionID(req, opts); got != "conv-header" {
		t.Fatalf("grokConversationSessionID() = %q, want conv-header", got)
	}
}

func TestGrokSessionNoThinkTrackerMarksOnSecondHit(t *testing.T) {
	t.Parallel()
	tr := newGrokSessionNoThinkTracker()
	if tr.note("conv-1") {
		t.Fatal("first no-think should not mark")
	}
	if !tr.note("conv-1") {
		t.Fatal("second consecutive no-think should mark")
	}
	if !tr.marked("conv-1") {
		t.Fatal("session should stay marked")
	}
	tr.clear("conv-1")
	if tr.marked("conv-1") {
		t.Fatal("cleared session should not stay marked")
	}
}

func TestHandleGrokSessionNoThinkIgnoresNonNoThinkErrors(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	req := cliproxyexecutor.Request{Model: "grok-4.6"}
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "conv-ignore"},
	}
	consecutive := 0
	quotaErr := errors.New(`{"code":"subscription:free-usage-exhausted"}`)
	if err := m.handleGrokSessionNoThink(req, opts, &consecutive, quotaErr); err != nil {
		t.Fatalf("quota error must not mark session: %v", err)
	}
	if consecutive != 0 {
		t.Fatalf("consecutive = %d, want 0", consecutive)
	}
	disconnect := errors.New("xai stream error: stream disconnected before response.completed")
	if err := m.handleGrokSessionNoThink(req, opts, &consecutive, disconnect); err != nil {
		t.Fatalf("disconnect must not mark session: %v", err)
	}
	if m.grokSessionNoThink.marked("conv-ignore") {
		t.Fatal("non no-think errors marked the session")
	}
}

type noThinkCountingExecutor struct {
	calls atomic.Int32
	err   error
}

func (e *noThinkCountingExecutor) Identifier() string { return "xai" }
func (e *noThinkCountingExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.calls.Add(1)
	return cliproxyexecutor.Response{}, e.err
}
func (e *noThinkCountingExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.calls.Add(1)
	return nil, e.err
}
func (e *noThinkCountingExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}
func (e *noThinkCountingExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}
func (e *noThinkCountingExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func registerXAITestAuths(t *testing.T, m *Manager, model string, ids ...string) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	for _, id := range ids {
		reg.RegisterClient(id, "xai", []*registry.ModelInfo{{ID: model}})
		auth := &Auth{ID: id, Provider: "xai", Status: StatusActive, Metadata: map[string]any{"type": "xai", "email": id}}
		if _, err := m.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	t.Cleanup(func() {
		for _, id := range ids {
			reg.UnregisterClient(id)
		}
	})
}

func TestExecuteStreamMarksSessionAfterTwoHTTP200NoThinks(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, 0)
	exec := &noThinkCountingExecutor{err: &cliproxyexecutor.NoThinkStreamError{Detail: "无Think流"}}
	m.RegisterExecutor(exec)
	model := "grok-4.6"
	registerXAITestAuths(t, m, model, "xai-a@outlook.com", "xai-b@outlook.com", "xai-c@outlook.com")

	opts := cliproxyexecutor.Options{
		Stream:   true,
		Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "conv-marked"},
	}
	_, err := m.ExecuteStream(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: model}, opts)
	if err == nil {
		t.Fatal("expected session marked error")
	}
	if !strings.Contains(err.Error(), grokSessionMarkedMessage) {
		t.Fatalf("error = %v, want %q", err, grokSessionMarkedMessage)
	}
	if got := exec.calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2 HTTP 200 no-think attempts", got)
	}
	disabled := 0
	for _, id := range []string{"xai-a@outlook.com", "xai-b@outlook.com", "xai-c@outlook.com"} {
		auth, ok := m.GetByID(id)
		if !ok || auth == nil {
			t.Fatalf("missing auth %s", id)
		}
		if auth.Disabled {
			disabled++
		}
	}
	if disabled != 2 {
		t.Fatalf("disabled auths = %d, want 2 so the unused account is spared", disabled)
	}

	exec.calls.Store(0)
	_, err = m.ExecuteStream(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: model}, opts)
	if err == nil || !strings.Contains(err.Error(), grokSessionMarkedMessage) {
		t.Fatalf("marked session should be rejected immediately, err=%v", err)
	}
	if got := exec.calls.Load(); got != 0 {
		t.Fatalf("marked session still executed %d times", got)
	}
}

func TestExecuteStreamQuotaErrorsDoNotMarkSession(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, 0)
	exec := &noThinkCountingExecutor{err: errors.New(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage"}`)}
	m.RegisterExecutor(exec)
	model := "grok-4.6"
	registerXAITestAuths(t, m, model, "xai-q1@outlook.com", "xai-q2@outlook.com", "xai-q3@outlook.com")

	opts := cliproxyexecutor.Options{
		Stream:   true,
		Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "conv-quota"},
	}
	_, err := m.ExecuteStream(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: model}, opts)
	if err == nil {
		t.Fatal("expected quota error")
	}
	if strings.Contains(err.Error(), grokSessionMarkedMessage) {
		t.Fatalf("quota errors marked the session: %v", err)
	}
	if got := exec.calls.Load(); got != 3 {
		t.Fatalf("quota should still rotate accounts, calls=%d want 3", got)
	}
	if m.grokSessionNoThink.marked("conv-quota") {
		t.Fatal("quota errors must not mark the grok session")
	}
}
