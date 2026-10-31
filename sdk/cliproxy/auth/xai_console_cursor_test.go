package auth

import (
	"context"
	"errors"
	"testing"
)

func TestParseXAICursorMerged(t *testing.T) {
	t.Parallel()
	merged, known, err := parseXAICursorMerged([]byte(`{"status":"authenticated","session":{"userId":"u1","cursorMerged":true}}`))
	if err != nil || !known || !merged {
		t.Fatalf("merged session = %v %v %v", merged, known, err)
	}
	merged, known, err = parseXAICursorMerged([]byte(`{"status":"authenticated","session":{"userId":"u1","cursorMerged":false}}`))
	if err != nil || !known || merged {
		t.Fatalf("unlinked session = %v %v %v", merged, known, err)
	}
	merged, known, err = parseXAICursorMerged([]byte(`{"status":"authenticated","session":{"userId":"u1"}}`))
	if err != nil || !known || merged {
		t.Fatalf("pre-unification session = %v %v %v", merged, known, err)
	}
	if _, _, err = parseXAICursorMerged([]byte(`{"status":"unauthenticated"}`)); err == nil {
		t.Fatal("expired session should not be treated as unlinked")
	}
}

func TestBuildQuotaSkipsConsoleWhenCursorUnlinked(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, &FillFirstSelector{}, nil)
	auth := &Auth{
		ID:       "xai-old@outlook.com",
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":                  "xai",
			"email":                 "xai-old@outlook.com",
			"sso":                   "console-sso-token",
			"console_cursor_merged": false,
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
		t.Fatal("pre-2026-10-13 account should disable instead of switching to console")
	}
	if XAIUsingConsoleChannel(updated) {
		t.Fatal("console must stay off")
	}
}

func TestBuildQuotaProbesCursorMergedBeforeConsole(t *testing.T) {
	orig := fetchXAIConsoleCursorMerged
	t.Cleanup(func() { fetchXAIConsoleCursorMerged = orig })
	fetchXAIConsoleCursorMerged = func(ctx context.Context, auth *Auth) (bool, bool, error) {
		return false, true, nil
	}
	m := NewManager(nil, &FillFirstSelector{}, nil)
	auth := &Auth{
		ID:       "xai-probe@outlook.com",
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":  "xai",
			"email": "xai-probe@outlook.com",
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
	if XAIUsingConsoleChannel(updated) || !updated.Disabled {
		t.Fatalf("unlinked probe should disable on build: disabled=%v channel=%s", updated.Disabled, XAIChannelOf(updated))
	}
	merged, known := xaiConsoleCursorMergedState(updated)
	if !known || merged {
		t.Fatalf("cached cursorMerged = %v known=%v", merged, known)
	}
}

func TestAlignXAIConsoleChannelDropsUnlinkedAccount(t *testing.T) {
	orig := fetchXAIConsoleCursorMerged
	t.Cleanup(func() { fetchXAIConsoleCursorMerged = orig })
	fetchXAIConsoleCursorMerged = func(ctx context.Context, auth *Auth) (bool, bool, error) {
		return false, true, nil
	}
	m := NewManager(nil, &FillFirstSelector{}, nil)
	auth := &Auth{
		ID:       "xai-stale-console@outlook.com",
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":        "xai",
			"email":       "xai-stale-console@outlook.com",
			"sso":         "console-sso-token",
			"xai_channel": XAIChannelConsole,
		},
	}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	updated := m.alignXAIConsoleChannel(context.Background(), auth)
	if updated == nil || XAIUsingConsoleChannel(updated) {
		t.Fatal("stale console channel should return to build")
	}
	stored, ok := m.GetByID(auth.ID)
	if !ok || stored == nil || xaiMarkedConsoleChannel(stored) {
		t.Fatal("unlinked console mark was not persisted")
	}
}
