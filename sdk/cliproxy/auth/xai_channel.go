package auth

import (
	"strings"
	"time"
)

const (
	XAIChannelBuild   = "build"
	XAIChannelConsole = "console"

	xaiChannelKey                 = "xai_channel"
	xaiConsoleQuotaExhaustedKey   = "console_quota_exhausted"
	xaiBuildQuotaRetryAfterKey    = "build_quota_retry_after"
	xaiConsoleSSOKey              = "sso"
	xaiConsoleSSOTokenKey         = "console_sso_token"
)

// XAIConsoleSSO returns the Console SSO token stored on a Build auth file.
func XAIConsoleSSO(auth *Auth) string {
	if auth == nil {
		return ""
	}
	auth.mapsMu.RLock()
	defer auth.mapsMu.RUnlock()
	if token := metadataString(auth.Metadata, xaiConsoleSSOTokenKey); token != "" {
		return token
	}
	return metadataString(auth.Metadata, xaiConsoleSSOKey)
}

// XAIHasConsoleChannel reports whether Console can still be used on this account.
func XAIHasConsoleChannel(auth *Auth) bool {
	if auth == nil || !isXAIProvider(auth.Provider) {
		return false
	}
	if XAIConsoleQuotaExhausted(auth) {
		return false
	}
	return XAIConsoleSSO(auth) != ""
}

// XAIConsoleQuotaExhausted reports a permanent Console quota mark (no 24h window).
func XAIConsoleQuotaExhausted(auth *Auth) bool {
	if auth == nil {
		return false
	}
	auth.mapsMu.RLock()
	defer auth.mapsMu.RUnlock()
	return metadataTruthy(auth.Metadata, xaiConsoleQuotaExhaustedKey)
}

// XAIUsingConsoleChannel reports whether the next request should hit Console.
func XAIUsingConsoleChannel(auth *Auth) bool {
	if !XAIHasConsoleChannel(auth) {
		return false
	}
	auth.mapsMu.RLock()
	defer auth.mapsMu.RUnlock()
	return strings.EqualFold(metadataString(auth.Metadata, xaiChannelKey), XAIChannelConsole)
}

func setXAIChannel(auth *Auth, channel string) {
	if auth == nil {
		return
	}
	auth.mapsMu.Lock()
	defer auth.mapsMu.Unlock()
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		channel = XAIChannelBuild
	}
	auth.Metadata[xaiChannelKey] = channel
}

func markXAIConsoleQuotaExhausted(auth *Auth) {
	if auth == nil {
		return
	}
	auth.mapsMu.Lock()
	defer auth.mapsMu.Unlock()
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[xaiConsoleQuotaExhaustedKey] = true
	auth.Metadata[xaiChannelKey] = XAIChannelBuild
}

func setXAIBuildQuotaRetryAfter(auth *Auth, at time.Time) {
	if auth == nil {
		return
	}
	auth.mapsMu.Lock()
	defer auth.mapsMu.Unlock()
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if at.IsZero() {
		delete(auth.Metadata, xaiBuildQuotaRetryAfterKey)
		return
	}
	auth.Metadata[xaiBuildQuotaRetryAfterKey] = at.UTC().Format(time.RFC3339Nano)
}

func xaiBuildQuotaRetryAfter(auth *Auth) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	auth.mapsMu.RLock()
	defer auth.mapsMu.RUnlock()
	return parseFlexibleTime(auth.Metadata[xaiBuildQuotaRetryAfterKey])
}

func xaiBuildWindowActive(auth *Auth, now time.Time) bool {
	at, ok := xaiBuildQuotaRetryAfter(auth)
	if !ok || at.IsZero() {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	return now.Before(at)
}
