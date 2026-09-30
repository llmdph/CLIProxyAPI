package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestGrok47AccountPoolModelNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"grok-4.7", "xai/grok-4.7", "grok-4.7-build-fast", "grok-4.7-fast", "grok-4.7(high)"} {
		if !isGrok47AccountPoolModel(name) {
			t.Fatalf("%s should match", name)
		}
	}
	for _, name := range []string{"grok-4.6", "grok-4", "gpt-4.7", ""} {
		if isGrok47AccountPoolModel(name) {
			t.Fatalf("%s should not match", name)
		}
	}
}

func TestXAIGrok47Eligible(t *testing.T) {
	t.Parallel()
	now := time.Now()
	outlook := &Auth{ID: "outlook", Provider: "xai", Status: StatusActive, Metadata: map[string]any{"type": "xai", "email": "a@outlook.com"}}
	outlookDown := &Auth{ID: "outlook-down", Provider: "xai", Status: StatusActive, Metadata: map[string]any{"type": "xai", "email": "b@outlook.com", "xai_downrank_pool": true}}
	other := &Auth{ID: "other", Provider: "xai", Disabled: true, Status: StatusDisabled, StatusMessage: "disabled non-outlook by operator", Metadata: map[string]any{"type": "xai", "email": "c@use84.lysk.de5.net", "status_message": "disabled non-outlook by operator"}}
	otherDown := &Auth{ID: "other-down", Provider: "xai", Disabled: true, Status: StatusDisabled, StatusMessage: "disabled non-outlook by operator", Metadata: map[string]any{"type": "xai", "email": "d@gmail.com", "status_message": "disabled non-outlook by operator", "xai_downrank_pool": true}}
	quota := &Auth{ID: "quota", Provider: "xai", Disabled: true, Status: StatusDisabled, StatusMessage: "quota_exhausted: free-usage-exhausted", Metadata: map[string]any{"type": "xai", "email": "e@gmail.com"}}
	enabledOther := &Auth{ID: "enabled-other", Provider: "xai", Status: StatusActive, Metadata: map[string]any{"type": "xai", "email": "f@lysk.de5.net"}}

	checks := []struct {
		auth *Auth
		mode string
		want bool
	}{
		{outlook, internalconfig.Grok47AccountsOutlookClean, true},
		{outlookDown, internalconfig.Grok47AccountsOutlookClean, false},
		{other, internalconfig.Grok47AccountsOutlookClean, false},
		{outlook, internalconfig.Grok47AccountsOutlookAll, true},
		{outlookDown, internalconfig.Grok47AccountsOutlookAll, true},
		{other, internalconfig.Grok47AccountsOutlookAll, false},
		{outlook, internalconfig.Grok47AccountsNonOutlook, false},
		{enabledOther, internalconfig.Grok47AccountsNonOutlook, true},
		{other, internalconfig.Grok47AccountsNonOutlook, false},
		{otherDown, internalconfig.Grok47AccountsNonOutlook, false},
		{quota, internalconfig.Grok47AccountsNonOutlook, false},
		{outlook, internalconfig.Grok47AccountsAll, true},
		{outlookDown, internalconfig.Grok47AccountsAll, true},
		{enabledOther, internalconfig.Grok47AccountsAll, true},
		{other, internalconfig.Grok47AccountsAll, false},
		{otherDown, internalconfig.Grok47AccountsAll, false},
		{quota, internalconfig.Grok47AccountsAll, false},
	}
	for _, check := range checks {
		if got := xaiGrok47Eligible(check.auth, check.mode, "grok-4.7", now); got != check.want {
			t.Fatalf("mode=%s auth=%s got %v want %v", check.mode, check.auth.ID, got, check.want)
		}
	}
}

func TestBeginFillFirstHoldGrok47Pool(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, &FillFirstSelector{}, nil)
	m.SetConfigSnapshot(&internalconfig.Config{XAI: internalconfig.XAIConfig{Grok47Accounts: internalconfig.Grok47AccountsNonOutlook}})
	opts := cliproxyexecutor.Options{}
	ctx, finish := m.beginFillFirstHold(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: "grok-4.7-build-fast"}, &opts)
	t.Cleanup(finish)
	hold := fillFirstHoldFrom(ctx)
	if hold == nil || hold.pool != m.fillFirstGrok47 {
		t.Fatal("grok-4.7 did not use its own account pool")
	}

	ctx46, finish46 := m.beginFillFirstHold(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: "grok-4.6"}, &opts)
	t.Cleanup(finish46)
	hold46 := fillFirstHoldFrom(ctx46)
	if hold46 == nil || hold46.pool != m.fillFirst {
		t.Fatal("grok-4.6 changed pools")
	}
}

func TestMoveParkedNonOutlookKeepsDisabled(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:            "xai-parked@lysk.de5.net",
		Provider:      "xai",
		Disabled:      true,
		Status:        StatusDisabled,
		StatusMessage: "disabled non-outlook by operator",
		Metadata: map[string]any{
			"type":           "xai",
			"email":          "parked@lysk.de5.net",
			"status_message": "disabled non-outlook by operator",
		},
	}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	if !m.fillFirstGrok47.addIdle(auth.ID) {
		t.Fatal("seed grok47 pool")
	}
	m.disableAuthForNoThink(context.Background(), auth, &cliproxyexecutor.NoThinkStreamError{Detail: "missing think"})
	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("missing auth")
	}
	if !updated.Disabled || updated.Status != StatusDisabled {
		t.Fatalf("parked auth was re-enabled: disabled=%v status=%s", updated.Disabled, updated.Status)
	}
	if !isXAIDownrankAuth(updated) {
		t.Fatal("expected downrank mark")
	}
	if m.fillFirstDownrank.hasMember(auth.ID) {
		t.Fatal("parked non-outlook entered the shared secondary pool")
	}
	if m.fillFirstGrok47.hasMember(auth.ID) {
		t.Fatal("parked non-outlook stayed in the grok-4.7 bucket after downgrade")
	}
}

type grok47StubExecutor struct{}

func (grok47StubExecutor) Identifier() string { return "xai" }
func (grok47StubExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (grok47StubExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return &cliproxyexecutor.StreamResult{}, nil
}
func (grok47StubExecutor) Refresh(context.Context, *Auth) (*Auth, error) { return nil, nil }
func (grok47StubExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (grok47StubExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestPickGrok47AvailableAuthNonOutlookOnly(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, &FillFirstSelector{}, nil)
	m.RegisterExecutor(grok47StubExecutor{})
	m.SetConfigSnapshot(&internalconfig.Config{XAI: internalconfig.XAIConfig{Grok47Accounts: internalconfig.Grok47AccountsNonOutlook}})
	accounts := []*Auth{
		{ID: "xai-a@outlook.com", Provider: "xai", Status: StatusActive, Metadata: map[string]any{"type": "xai", "email": "a@outlook.com"}},
		{ID: "xai-b@outlook.com", Provider: "xai", Status: StatusActive, Metadata: map[string]any{"type": "xai", "email": "b@outlook.com", "xai_downrank_pool": true}},
		{ID: "xai-c@lysk.de5.net", Provider: "xai", Disabled: true, Status: StatusDisabled, StatusMessage: "disabled non-outlook by operator", Metadata: map[string]any{"type": "xai", "email": "c@lysk.de5.net", "status_message": "disabled non-outlook by operator"}},
		{ID: "xai-d@lysk.de5.net", Provider: "xai", Status: StatusActive, Metadata: map[string]any{"type": "xai", "email": "d@lysk.de5.net"}},
	}
	for _, auth := range accounts {
		if _, err := m.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
	seen := map[string]int{}
	for i := 0; i < 20; i++ {
		picked, _, provider, err := m.pickGrok47AvailableAuth("grok-4.7", map[string]struct{}{"xai": {}}, nil)
		if err != nil {
			t.Fatalf("pick: %v", err)
		}
		if provider != "xai" || picked.ID != "xai-d@lysk.de5.net" {
			t.Fatalf("picked %s provider %s", picked.ID, provider)
		}
		seen[picked.ID]++
	}
	if seen["xai-d@lysk.de5.net"] != 20 {
		t.Fatalf("seen %#v", seen)
	}
}

func TestPreparedModelsSkipDisabledAccounts(t *testing.T) {
	t.Parallel()
	parked := &Auth{
		ID:            "xai-parked@lysk.de5.net",
		Provider:      "xai",
		Disabled:      true,
		Status:        StatusDisabled,
		StatusMessage: "disabled non-outlook by operator",
		Metadata: map[string]any{
			"type":           "xai",
			"email":          "parked@lysk.de5.net",
			"status_message": "disabled non-outlook by operator",
		},
	}
	m := NewManager(nil, nil, nil)
	m.SetConfigSnapshot(&internalconfig.Config{XAI: internalconfig.XAIConfig{Grok47Accounts: internalconfig.Grok47AccountsNonOutlook}})
	if got, _ := m.preparedExecutionModels(parked, "grok-4.7-build-fast"); len(got) != 0 {
		t.Fatalf("disabled account should not execute for grok-4.7: %#v", got)
	}
	enabled := &Auth{ID: "xai-enabled@lysk.de5.net", Provider: "xai", Status: StatusActive, Metadata: map[string]any{"type": "xai", "email": "enabled@lysk.de5.net"}}
	if got, _ := m.preparedExecutionModels(enabled, "grok-4.7-build-fast"); len(got) == 0 {
		t.Fatal("enabled non-outlook account was not executable")
	}
}
