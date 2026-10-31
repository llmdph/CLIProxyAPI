package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	// xaiConsoleCursorMergedKey caches grok.com session.cursorMerged.
	// Accounts registered on or before 2026-10-12 are not linked to Cursor:
	// the field is false or absent, and Console never receives credits.
	// Accounts registered after 2026-10-13 report cursorMerged=true.
	xaiConsoleCursorMergedKey = "console_cursor_merged"
	xaiGrokSessionURL         = "https://grok.com/api/auth/session"
	xaiCursorMergedUserAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	xaiCursorMergedBodyLimit  = 1 << 20
	xaiCursorMergedTimeout    = 12 * time.Second
)

// fetchXAIConsoleCursorMerged reads session.cursorMerged. Tests replace it.
var fetchXAIConsoleCursorMerged = fetchXAIConsoleCursorMergedHTTP

func xaiConsoleCursorMergedState(auth *Auth) (merged bool, known bool) {
	if auth == nil {
		return false, false
	}
	auth.mapsMu.RLock()
	defer auth.mapsMu.RUnlock()
	return metadataBoolKnown(auth.Metadata, xaiConsoleCursorMergedKey)
}

func setXAIConsoleCursorMerged(auth *Auth, merged bool) {
	if auth == nil {
		return
	}
	auth.mapsMu.Lock()
	defer auth.mapsMu.Unlock()
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[xaiConsoleCursorMergedKey] = merged
}

func xaiMarkedConsoleChannel(auth *Auth) bool {
	if auth == nil {
		return false
	}
	auth.mapsMu.RLock()
	defer auth.mapsMu.RUnlock()
	return strings.EqualFold(metadataString(auth.Metadata, xaiChannelKey), XAIChannelConsole)
}

// allowXAIConsoleSwitch reports whether this account may enter Console.
// Only Cursor-linked accounts (registered after 2026-10-13) may.
// A failed lookup does not cache a result, so a later quota event can retry.
func (m *Manager) allowXAIConsoleSwitch(ctx context.Context, auth *Auth) bool {
	merged, known := xaiConsoleCursorMergedState(auth)
	if !known {
		var err error
		merged, known, err = fetchXAIConsoleCursorMerged(ctx, auth)
		if err != nil || !known {
			if err != nil {
				log.WithError(err).Warnf("xai: console eligibility check failed for %s", authID(auth))
			}
			return false
		}
		setXAIConsoleCursorMerged(auth, merged)
	}
	if merged {
		return true
	}
	setXAIChannel(auth, XAIChannelBuild)
	log.Warnf("xai: auth %s is not linked to Cursor (registered on or before 2026-10-12); skipping console", authID(auth))
	return false
}

// alignXAIConsoleChannel drops Console on accounts that are not Cursor-linked.
// Build-channel auths are left alone so ordinary requests do not call grok.com.
func (m *Manager) alignXAIConsoleChannel(ctx context.Context, auth *Auth) *Auth {
	if m == nil || auth == nil || !isXAIProvider(auth.Provider) || !xaiMarkedConsoleChannel(auth) {
		return auth
	}
	merged, known := xaiConsoleCursorMergedState(auth)
	if known && merged {
		return auth
	}
	if !known {
		var err error
		merged, known, err = fetchXAIConsoleCursorMerged(ctx, auth)
		if err != nil || !known {
			if err != nil {
				log.WithError(err).Warnf("xai: console eligibility check failed for %s", authID(auth))
			}
			return auth
		}
		setXAIConsoleCursorMerged(auth, merged)
	}
	if merged {
		updated, errUpdate := m.Update(ctx, auth.Clone())
		if errUpdate != nil || updated == nil {
			return auth
		}
		return updated
	}
	setXAIChannel(auth, XAIChannelBuild)
	log.Warnf("xai: auth %s left console; registered on or before 2026-10-12 (cursorMerged=false)", authID(auth))
	updated, errUpdate := m.Update(ctx, auth.Clone())
	if errUpdate != nil || updated == nil {
		return auth
	}
	return updated
}

func authID(auth *Auth) string {
	if auth == nil {
		return ""
	}
	return strings.TrimSpace(auth.ID)
}

func metadataBoolKnown(meta map[string]any, key string) (bool, bool) {
	if meta == nil {
		return false, false
	}
	raw, ok := meta[key]
	if !ok || raw == nil {
		return false, false
	}
	switch v := raw.(type) {
	case bool:
		return v, true
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes":
			return true, true
		case "0", "false", "no":
			return false, true
		default:
			return false, false
		}
	case float64:
		return v != 0, true
	case int:
		return v != 0, true
	case json.Number:
		n, err := v.Float64()
		if err != nil {
			return false, false
		}
		return n != 0, true
	default:
		switch strings.ToLower(strings.TrimSpace(fmt.Sprint(v))) {
		case "1", "true", "yes":
			return true, true
		case "0", "false", "no":
			return false, true
		default:
			return false, false
		}
	}
}

func fetchXAIConsoleCursorMergedHTTP(ctx context.Context, auth *Auth) (bool, bool, error) {
	sso := XAIConsoleSSO(auth)
	if sso == "" {
		return false, false, fmt.Errorf("xai console: missing sso")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, xaiCursorMergedTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, xaiGrokSessionURL, nil)
	if err != nil {
		return false, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", "https://grok.com/")
	req.Header.Set("User-Agent", xaiCursorMergedUserAgent)
	req.Header.Set("Cookie", xaiSessionCookie(sso, xaiClearanceCookie(auth)))
	resp, err := xaiCursorMergedClient(xaiCursorMergedProxy(ctx, auth)).Do(req)
	if err != nil {
		return false, false, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, xaiCursorMergedBodyLimit))
	if err != nil {
		return false, false, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return false, false, fmt.Errorf("xai console: grok session unauthorized")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, false, fmt.Errorf("xai console: grok session status %d", resp.StatusCode)
	}
	return parseXAICursorMerged(data)
}

func xaiCursorMergedProxy(ctx context.Context, auth *Auth) string {
	if proxyURL := strings.TrimSpace(cliproxyexecutor.RequestProxyURL(ctx)); proxyURL != "" {
		return proxyURL
	}
	if auth == nil {
		return ""
	}
	return strings.TrimSpace(auth.ProxyURL)
}

func xaiCursorMergedClient(proxyURL string) *http.Client {
	client := &http.Client{Timeout: xaiCursorMergedTimeout}
	if strings.TrimSpace(proxyURL) == "" {
		return client
	}
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.Warnf("xai: console eligibility proxy: %v", errBuild)
		return client
	}
	if transport != nil {
		client.Transport = transport
	}
	return client
}

func xaiClearanceCookie(auth *Auth) string {
	if auth == nil {
		return ""
	}
	auth.mapsMu.RLock()
	defer auth.mapsMu.RUnlock()
	return metadataString(auth.Metadata, "cf_clearance")
}

func xaiSessionCookie(sso, clearance string) string {
	sso = strings.TrimSpace(sso)
	if strings.HasPrefix(strings.ToLower(sso), "sso=") {
		sso = strings.TrimSpace(sso[len("sso="):])
	}
	if value, _, found := strings.Cut(sso, ";"); found {
		sso = strings.TrimSpace(value)
	}
	cookie := "sso=" + sso + "; sso-rw=" + sso
	clearance = strings.TrimSpace(clearance)
	if clearance != "" && clearance != "<nil>" {
		cookie += "; cf_clearance=" + clearance
	}
	return cookie
}

// parseXAICursorMerged matches grok2api parseCursorMerged.
// A missing cursorMerged field is treated as not linked, which is how accounts
// created before the 2026-10-13 Cursor unification are reported.
func parseXAICursorMerged(data []byte) (bool, bool, error) {
	var payload struct {
		Status  string `json:"status"`
		Session struct {
			CursorMerged *bool `json:"cursorMerged"`
		} `json:"session"`
		CursorMerged *bool `json:"cursorMerged"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return false, false, fmt.Errorf("xai console: parse grok session: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(payload.Status), "unauthenticated") {
		return false, false, fmt.Errorf("xai console: grok session unauthenticated")
	}
	if payload.Session.CursorMerged != nil {
		return *payload.Session.CursorMerged, true, nil
	}
	if payload.CursorMerged != nil {
		return *payload.CursorMerged, true, nil
	}
	return false, true, nil
}
