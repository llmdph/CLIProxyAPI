package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type fillFirstStubExecutor struct{}

func (fillFirstStubExecutor) Identifier() string { return "xai" }
func (fillFirstStubExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (fillFirstStubExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}
func (fillFirstStubExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}
func (fillFirstStubExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (fillFirstStubExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func registerFillFirstAuth(t *testing.T, m *Manager, id string) {
	t.Helper()
	auth := &Auth{
		ID:       id,
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "xai", "email": id},
	}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

func TestAcquireFillFirstRetryDoesNotExpandBucket(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	m.RegisterExecutor(fillFirstStubExecutor{})
	registerFillFirstAuth(t, m, "a@outlook.com")
	registerFillFirstAuth(t, m, "b@outlook.com")
	registerFillFirstAuth(t, m, "c@outlook.com")

	opts := cliproxyexecutor.Options{}
	ctx, finish := m.beginFillFirstHold(context.Background(), nil, cliproxyexecutor.Request{Payload: []byte(`{"input":"hello"}`)}, &opts)
	t.Cleanup(finish)
	hold := fillFirstHoldFrom(ctx)

	first, _, _, err := m.acquireFillFirstAuth(ctx, []string{"xai"}, "grok-4.6", nil, hold)
	if err != nil || first == nil {
		t.Fatalf("first acquire: auth=%v err=%v", first, err)
	}
	if got := m.fillFirst.memberIDs(); len(got) != 1 || got[0] != first.ID {
		t.Fatalf("members after first = %v", got)
	}

	second, _, _, err := m.acquireFillFirstAuth(ctx, []string{"xai"}, "grok-4.6", nil, hold)
	if err != nil || second == nil {
		t.Fatalf("retry acquire: auth=%v err=%v", second, err)
	}
	if second.ID != first.ID {
		t.Fatalf("retry switched %s -> %s", first.ID, second.ID)
	}
	if got := m.fillFirst.memberIDs(); len(got) != 1 || got[0] != first.ID {
		t.Fatalf("members after retry = %v", got)
	}

	tried := map[string]struct{}{first.ID: {}}
	if auth, _, _, err := m.acquireFillFirstAuth(ctx, []string{"xai"}, "grok-4.6", tried, hold); err == nil && auth != nil {
		t.Fatalf("excluded retry expanded to %s members=%v", auth.ID, m.fillFirst.memberIDs())
	}
	if got := m.fillFirst.memberIDs(); len(got) != 1 {
		t.Fatalf("members after excluded retry = %v", got)
	}
}

func TestAcquireFillFirstNewHoldExpandsWhenBusy(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	m.RegisterExecutor(fillFirstStubExecutor{})
	registerFillFirstAuth(t, m, "a@outlook.com")
	registerFillFirstAuth(t, m, "b@outlook.com")

	opts1 := cliproxyexecutor.Options{}
	ctx1, finish1 := m.beginFillFirstHold(context.Background(), nil, cliproxyexecutor.Request{Payload: []byte(`{"input":"one"}`)}, &opts1)
	t.Cleanup(finish1)
	hold1 := fillFirstHoldFrom(ctx1)
	first, _, _, err := m.acquireFillFirstAuth(ctx1, []string{"xai"}, "grok-4.6", nil, hold1)
	if err != nil || first == nil {
		t.Fatalf("first: %v %v", first, err)
	}

	opts2 := cliproxyexecutor.Options{}
	ctx2, finish2 := m.beginFillFirstHold(context.Background(), nil, cliproxyexecutor.Request{Payload: []byte(`{"input":"two"}`)}, &opts2)
	t.Cleanup(finish2)
	hold2 := fillFirstHoldFrom(ctx2)
	second, _, _, err := m.acquireFillFirstAuth(ctx2, []string{"xai"}, "grok-4.6", nil, hold2)
	if err != nil || second == nil {
		t.Fatalf("second: %v %v", second, err)
	}
	if second.ID == first.ID {
		t.Fatalf("new connection reused busy %s", first.ID)
	}
	if got := m.fillFirst.memberIDs(); len(got) != 2 {
		t.Fatalf("members after new connection = %v", got)
	}
}

func TestBeginFillFirstHoldSkipsGemini(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	opts := cliproxyexecutor.Options{}
	ctx, finish := m.beginFillFirstHold(context.Background(), []string{"xai", "antigravity"}, cliproxyexecutor.Request{Model: "gemini-3.8-flash-high", Payload: []byte(`{"model":"gemini-3.8-flash-high"}`)}, &opts)
	t.Cleanup(finish)
	if fillFirstHoldFrom(ctx) != nil {
		t.Fatal("gemini request must not enter xAI fill-first bucket")
	}
}

func TestBeginFillFirstHoldKeepsGrok(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	opts := cliproxyexecutor.Options{}
	ctx, finish := m.beginFillFirstHold(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: "grok-4.6", Payload: []byte(`{"model":"grok-4.6"}`)}, &opts)
	t.Cleanup(finish)
	if fillFirstHoldFrom(ctx) == nil {
		t.Fatal("grok request should enter fill-first")
	}
}

func TestAcquireFillFirstRetryExpandsAfterDroppedAccount(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	m.RegisterExecutor(fillFirstStubExecutor{})
	registerFillFirstAuth(t, m, "a@outlook.com")
	registerFillFirstAuth(t, m, "b@outlook.com")
	registerFillFirstAuth(t, m, "c@outlook.com")

	opts1 := cliproxyexecutor.Options{}
	ctx1, finish1 := m.beginFillFirstHold(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: "grok-4.6", Payload: []byte(`{"model":"grok-4.6"}`)}, &opts1)
	t.Cleanup(finish1)
	hold1 := fillFirstHoldFrom(ctx1)
	first, _, _, err := m.acquireFillFirstAuth(ctx1, []string{"xai"}, "grok-4.6", nil, hold1)
	if err != nil || first == nil {
		t.Fatalf("first: %v %v", first, err)
	}

	opts2 := cliproxyexecutor.Options{}
	ctx2, finish2 := m.beginFillFirstHold(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: "grok-4.6", Payload: []byte(`{"model":"grok-4.6"}`)}, &opts2)
	t.Cleanup(finish2)
	hold2 := fillFirstHoldFrom(ctx2)
	second, _, _, err := m.acquireFillFirstAuth(ctx2, []string{"xai"}, "grok-4.6", nil, hold2)
	if err != nil || second == nil {
		t.Fatalf("second: %v %v", second, err)
	}
	if second.ID == first.ID {
		t.Fatalf("second reused first %s", first.ID)
	}

	m.dropFillFirstMember(first.ID)
	tried := map[string]struct{}{first.ID: {}}
	type acqResult struct {
		auth *Auth
		err  error
	}
	got := make(chan acqResult, 1)
	go func() {
		replacement, _, _, err := m.acquireFillFirstAuth(ctx1, []string{"xai"}, "grok-4.6", tried, hold1)
		got <- acqResult{auth: replacement, err: err}
	}()
	var replacement *Auth
	select {
	case res := <-got:
		replacement, err = res.auth, res.err
	case <-time.After(2 * time.Second):
		t.Fatalf("quota retry blocked waiting on busy member members=%v", m.fillFirst.memberIDs())
	}
	if err != nil || replacement == nil {
		t.Fatalf("quota retry should add a replacement, got %v %v members=%v", replacement, err, m.fillFirst.memberIDs())
	}
	if replacement.ID == first.ID || replacement.ID == second.ID {
		t.Fatalf("quota retry reused busy/dropped auth %s members=%v", replacement.ID, m.fillFirst.memberIDs())
	}
}
