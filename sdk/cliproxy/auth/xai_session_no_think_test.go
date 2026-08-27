package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestGrokSessionNoThinkTrackerDoesNotMark(t *testing.T) {
	t.Parallel()
	tr := newGrokSessionNoThinkTracker()
	tr.sessions["conv-old"] = grokSessionNoThinkEntry{marked: true, expiresAt: time.Now().Add(time.Hour)}
	if n := tr.clearAll(); n != 1 {
		t.Fatalf("clearAll = %d, want 1", n)
	}
	if tr.note("conv-1") {
		t.Fatal("session tracker must not mark conversations")
	}
	if tr.note("conv-1") || tr.note("conv-1") {
		t.Fatal("repeated no-think must not mark the session")
	}
	if tr.marked("conv-1") {
		t.Fatal("session should not be marked")
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
	if m.handleGrokSessionNoThink(context.Background(), req, opts, &consecutive, quotaErr) {
		t.Fatal("quota error must not accept no-think fallback")
	}
	if consecutive != 0 {
		t.Fatalf("consecutive = %d, want 0", consecutive)
	}
	disconnect := errors.New("xai stream error: stream disconnected before response.completed")
	if m.handleGrokSessionNoThink(context.Background(), req, opts, &consecutive, disconnect) {
		t.Fatal("disconnect must not accept no-think fallback")
	}
	if m.grokSessionNoThink.marked("conv-ignore") {
		t.Fatal("non no-think errors marked the session")
	}
}

func TestHandleGrokSessionNoThinkAcceptsOnThird(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	req := cliproxyexecutor.Request{Model: "grok-4.6"}
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "conv-third"},
	}
	consecutive := 0
	noThink := &cliproxyexecutor.NoThinkStreamError{Detail: "无Think流"}
	if m.handleGrokSessionNoThink(context.Background(), req, opts, &consecutive, noThink) {
		t.Fatal("first no-think should keep rotating")
	}
	if m.handleGrokSessionNoThink(context.Background(), req, opts, &consecutive, noThink) {
		t.Fatal("second no-think should keep rotating")
	}
	if !m.handleGrokSessionNoThink(context.Background(), req, opts, &consecutive, noThink) {
		t.Fatal("third no-think should return content")
	}
	if consecutive != 3 {
		t.Fatalf("consecutive = %d, want 3", consecutive)
	}
	if m.grokSessionNoThink.marked("conv-third") {
		t.Fatal("third no-think must not mark the session")
	}
}

type noThinkCountingExecutor struct {
	calls atomic.Int32
	err   error
}

func (e *noThinkCountingExecutor) Identifier() string { return "xai" }
func (e *noThinkCountingExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.calls.Add(1)
	noThink, _ := cliproxyexecutor.AsNoThinkStream(e.err)
	if noThink == nil {
		return cliproxyexecutor.Response{}, e.err
	}
	cloned := *noThink
	if len(cloned.Fallback.Payload) == 0 {
		cloned.Fallback = cliproxyexecutor.Response{Payload: []byte(`{"id":"no-think-fallback"}`)}
	}
	return cliproxyexecutor.Response{}, &cloned
}
func (e *noThinkCountingExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.calls.Add(1)
	noThink, _ := cliproxyexecutor.AsNoThinkStream(e.err)
	if noThink == nil {
		return nil, e.err
	}
	cloned := *noThink
	if len(cloned.StreamChunks) == 0 {
		cloned.StreamChunks = [][]byte{[]byte(`data: {"id":"no-think-fallback"}`)}
		cloned.StreamHeader = http.Header{"Content-Type": []string{"text/event-stream"}}
	}
	return nil, &cloned
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

func TestExecuteStreamReturnsThirdNoThinkContent(t *testing.T) {
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
	result, err := m.ExecuteStream(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: model}, opts)
	if err != nil {
		t.Fatalf("third no-think should return content, err=%v", err)
	}
	if result == nil {
		t.Fatal("missing fallback stream")
	}
	if got := exec.calls.Load(); got != 3 {
		t.Fatalf("calls = %d, want 3 HTTP 200 no-think attempts", got)
	}
	downranked := 0
	for _, id := range []string{"xai-a@outlook.com", "xai-b@outlook.com", "xai-c@outlook.com"} {
		auth, ok := m.GetByID(id)
		if !ok || auth == nil {
			t.Fatalf("missing auth %s", id)
		}
		if auth.Disabled {
			t.Fatalf("no-think should move %s to secondary pool, not disable it", id)
		}
		if isXAIDownrankAuth(auth) {
			downranked++
		}
	}
	if downranked != 3 {
		t.Fatalf("downrank auths = %d, want 3", downranked)
	}
	if m.grokSessionNoThink.marked("conv-marked") {
		t.Fatal("session must not be marked")
	}

	exec.calls.Store(0)
	_, err = m.ExecuteStream(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: model}, opts)
	if err != nil && strings.Contains(err.Error(), grokSessionMarkedMessage) {
		t.Fatalf("must not reject the session: %v", err)
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

func TestHandleGrokSessionNoThinkSkipsAuxPool(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	req := cliproxyexecutor.Request{Model: "grok-4.6"}
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "conv-aux",
			cliproxyexecutor.RequestClassMetadataKey:     cliproxyexecutor.RequestClassCompaction,
		},
	}
	consecutive := 0
	noThink := &cliproxyexecutor.NoThinkStreamError{Detail: "无Think流"}
	ctx := cliproxyexecutor.WithRequestClass(context.Background(), cliproxyexecutor.RequestClassCompaction)
	if m.handleGrokSessionNoThink(ctx, req, opts, &consecutive, noThink) {
		t.Fatal("aux no-think must not accept main-pool fallback")
	}
	if m.handleGrokSessionNoThink(ctx, req, opts, &consecutive, noThink) {
		t.Fatal("second aux no-think must not accept main-pool fallback")
	}
	if consecutive != 0 {
		t.Fatalf("aux consecutive = %d, want 0", consecutive)
	}
	if m.grokSessionNoThink.marked("conv-aux") {
		t.Fatal("downrank/aux pool must not mark grok sessions")
	}
	m.grokSessionNoThink.sessions["conv-aux"] = grokSessionNoThinkEntry{marked: true, expiresAt: time.Now().Add(time.Hour)}
	if err := m.errIfGrokSessionMarked(ctx, req, opts); err != nil {
		t.Fatalf("aux request rejected marked session: %v", err)
	}
	if m.grokSessionNoThink.marked("conv-aux") {
		t.Fatal("leftover session marks must be cleared")
	}
}

func TestExecuteStreamCompactionNoThinkDoesNotMarkSession(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	m.SetRetryConfig(0, 0, 0)
	exec := &noThinkCountingExecutor{err: &cliproxyexecutor.NoThinkStreamError{Detail: "无Think流"}}
	m.RegisterExecutor(exec)
	model := "grok-4.6"
	registerXAITestAuths(t, m, model, "xai-c1@outlook.com", "xai-c2@outlook.com")

	opts := cliproxyexecutor.Options{
		Stream: true,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "conv-compact",
		},
	}
	req := cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(`{"input":[{"type":"compaction_trigger"}],"reasoning":{"effort":"xhigh"}}`),
	}
	_, err := m.ExecuteStream(context.Background(), []string{"xai"}, req, opts)
	if err != nil && strings.Contains(err.Error(), grokSessionMarkedMessage) {
		t.Fatalf("compaction no-think marked the session: %v", err)
	}
	if m.grokSessionNoThink.marked("conv-compact") {
		t.Fatal("compaction request marked the grok session")
	}
	if got := exec.calls.Load(); got != 1 {
		t.Fatalf("compaction should not rotate credentials, calls=%d want 1", got)
	}
}

