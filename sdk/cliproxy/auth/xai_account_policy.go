package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const xaiDisabledAutoReenableAfter = 24 * time.Hour

const (
	xaiAutoDisableStatusMessageKey  = "status_message"
	xaiAutoDisableNextRetryAfterKey = "next_retry_after"
	xaiAutoDisableUpdatedAtKey      = "disabled_updated_at"
	xaiAutoDisableKindKey           = "xai_auto_disable"
)

// xaiAutoDisableKindValue marks auth files disabled by xAI quota / no-think policy.
const xaiAutoDisableKindValue = "quota_window"

// xaiCloudflareSameAuthMaxAttempts is the total number of upstream attempts on the
// same credential when Cloudflare returns a challenge / blocked HTML (often as 400).
const xaiCloudflareSameAuthMaxAttempts = 3

// xaiAutoReenableEmailSuffix limits automatic quota-window recovery to Outlook
// accounts only. Operator-disabled non-Outlook credentials stay disabled.
const xaiAutoReenableEmailSuffix = "@outlook.com"

func xaiAuthEmail(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if email := strings.TrimSpace(authAttribute(auth, "email")); email != "" {
		return strings.ToLower(email)
	}
	auth.mapsMu.RLock()
	defer auth.mapsMu.RUnlock()
	if email := strings.ToLower(strings.TrimSpace(metadataString(auth.Metadata, "email"))); email != "" {
		return email
	}
	for _, raw := range []string{auth.ID, auth.FileName, auth.Label} {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		s = strings.TrimSuffix(s, ".json")
		s = strings.TrimPrefix(s, "xai-")
		if at := strings.LastIndex(s, "@"); at > 0 && at < len(s)-1 {
			return strings.ToLower(s)
		}
	}
	return ""
}

func isOutlookXAIAuth(auth *Auth) bool {
	email := xaiAuthEmail(auth)
	return email != "" && strings.HasSuffix(email, xaiAutoReenableEmailSuffix)
}

func isXAIProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "xai", "x-ai", "grok":
		return true
	default:
		return false
	}
}

// isXAIQuotaExhaustedError reports Grok free-tier / spending-limit exhaustion
// (aligned with grok-inspection classify.go). Bare HTTP 429 / temp rate limits
// must not match so the sticky account is not burned.
func isXAIQuotaExhaustedError(err error) bool {
	if err == nil {
		return false
	}
	blob := strings.ToLower(err.Error())
	if strings.Contains(blob, "free-usage-exhausted") ||
		strings.Contains(blob, "included free usage") ||
		strings.Contains(blob, "used all the included free usage") {
		return true
	}
	return strings.Contains(blob, "personal-team-blocked:spending-limit")
}

// shouldRotateXAICredential reports whether the conductor may try the next xAI
// account. Only no-think (risk-control downrank) and quota/spending exhaustion rotate;
// transient upstream failures stick to the current account.
func shouldRotateXAICredential(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := cliproxyexecutor.AsNoThinkStream(err); ok {
		return true
	}
	return isXAIQuotaExhaustedError(err)
}

// recordXAIAwareExecutionResult records failures that should burn/rotate an xAI
// account (no-think / quota). Transient xAI failures are intentionally not
// recorded so fill-first keeps sticking to the same credential.
func (m *Manager) recordXAIAwareExecutionResult(ctx context.Context, result Result, auth *Auth, provider string, err error, ephemeral bool) {
	if m == nil {
		return
	}
	if isXAIProvider(provider) && err != nil && !shouldRotateXAICredential(err) && !isRequestInvalidError(err) {
		return
	}
	m.recordExecutionResult(ctx, result, auth, ephemeral)
}

func (m *Manager) disableXAIAuthIfQuotaExhausted(ctx context.Context, auth *Auth, provider string, err error) bool {
	if m == nil || auth == nil || err == nil {
		return false
	}
	if !isXAIProvider(provider) || !isXAIQuotaExhaustedError(err) {
		return false
	}
	m.disableAuthForQuotaExhausted(ctx, auth, err)
	return true
}

func (m *Manager) disableAuthForQuotaExhausted(ctx context.Context, auth *Auth, err error) {
	if m == nil || auth == nil {
		return
	}
	if !isXAIProvider(auth.Provider) {
		return
	}
	clone := auth.Clone()
	if clone == nil {
		return
	}
	now := time.Now()
	clone.Disabled = true
	clone.Status = StatusDisabled
	clone.UpdatedAt = now
	// Schedule automatic re-enable after the free-tier rolling window.
	clone.NextRetryAfter = now.Add(xaiDisabledAutoReenableAfter)
	detail := "quota_exhausted"
	if err != nil {
		msg := strings.TrimSpace(err.Error())
		if msg != "" {
			if len(msg) > 240 {
				msg = msg[:240] + "…"
			}
			detail = "quota_exhausted: " + msg
		}
	}
	clone.StatusMessage = detail
	syncXAIAutoDisableMetadata(clone)
	if _, errUpdate := m.Update(ctx, clone); errUpdate != nil {
		log.WithError(errUpdate).Warnf("xai: failed to disable auth %s after %s", auth.ID, detail)
		return
	}
	if m.fillFirst != nil {
		m.fillFirst.drop(auth.ID)
	}
	log.Warnf("xai: disabled auth %s after %s", auth.ID, detail)
}

func (m *Manager) disableAuthForNoThink(ctx context.Context, auth *Auth, noThink *cliproxyexecutor.NoThinkStreamError) {
	if m == nil || auth == nil {
		return
	}
	clone := auth.Clone()
	if clone == nil {
		return
	}
	now := time.Now()
	clone.Disabled = true
	clone.Status = StatusDisabled
	clone.UpdatedAt = now
	clone.NextRetryAfter = now.Add(xaiDisabledAutoReenableAfter)
	detail := "no_think_stream"
	if noThink != nil && strings.TrimSpace(noThink.Detail) != "" {
		detail = "no_think_stream: " + strings.TrimSpace(noThink.Detail)
	}
	clone.StatusMessage = detail
	syncXAIAutoDisableMetadata(clone)
	if _, errUpdate := m.Update(ctx, clone); errUpdate != nil {
		log.WithError(errUpdate).Warnf("xai: failed to disable auth %s after %s", auth.ID, detail)
		return
	}
	if m.fillFirst != nil {
		m.fillFirst.drop(auth.ID)
	}
	log.Warnf("xai: disabled auth %s after %s", auth.ID, detail)
}

func isXAIAutoDisabledStatusMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "quota_exhausted") ||
		strings.Contains(lower, "no_think_stream") ||
		strings.Contains(lower, "free-usage-exhausted") ||
		strings.Contains(lower, "included free usage") ||
		strings.Contains(lower, "personal-team-blocked:spending-limit")
}

func shouldPreserveXAIAutoDisableSchedule(auth *Auth) bool {
	_, ok := xaiDisabledReenableAt(auth)
	return ok
}

// shouldKeepXAIAutoDisableSchedule reports whether Register/Update/MarkResult
// must retain Disabled + NextRetryAfter for a quota/no-think auto-disable.
func shouldKeepXAIAutoDisableSchedule(auth *Auth) bool {
	if auth == nil || !isXAIProvider(auth.Provider) {
		return false
	}
	if !auth.Disabled && auth.Status != StatusDisabled {
		return false
	}
	if isXAIAutoDisabledStatusMessage(auth.StatusMessage) {
		return true
	}
	return shouldPreserveXAIAutoDisableSchedule(auth)
}

func metadataString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	raw, ok := meta[key]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func parseFlexibleTime(raw any) (time.Time, bool) {
	if raw == nil {
		return time.Time{}, false
	}
	switch v := raw.(type) {
	case time.Time:
		if v.IsZero() {
			return time.Time{}, false
		}
		return v.UTC(), true
	case *time.Time:
		if v == nil || v.IsZero() {
			return time.Time{}, false
		}
		return v.UTC(), true
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return time.Time{}, false
		}
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return ts.UTC(), true
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts.UTC(), true
		}
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return unixFlexible(f), true
		}
	case float64:
		return unixFlexible(v), true
	case float32:
		return unixFlexible(float64(v)), true
	case int:
		return unixFlexible(float64(v)), true
	case int64:
		return unixFlexible(float64(v)), true
	case int32:
		return unixFlexible(float64(v)), true
	}
	return time.Time{}, false
}

func unixFlexible(v float64) time.Time {
	if v > 1e12 {
		return time.UnixMilli(int64(v)).UTC()
	}
	return time.Unix(int64(v), 0).UTC()
}

// SyncXAIAutoDisableMetadataForPersist exports auto-disable metadata fields for auth file writes.
func SyncXAIAutoDisableMetadataForPersist(auth *Auth) {
	syncXAIAutoDisableMetadata(auth)
}

func syncXAIAutoDisableMetadata(auth *Auth) {
	if auth == nil {
		return
	}
	auth.mapsMu.Lock()
	defer auth.mapsMu.Unlock()
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["disabled"] = auth.Disabled
	if !auth.Disabled && auth.Status != StatusDisabled {
		delete(auth.Metadata, xaiAutoDisableStatusMessageKey)
		delete(auth.Metadata, xaiAutoDisableNextRetryAfterKey)
		delete(auth.Metadata, xaiAutoDisableUpdatedAtKey)
		delete(auth.Metadata, xaiAutoDisableKindKey)
		return
	}
	if msg := strings.TrimSpace(auth.StatusMessage); msg != "" {
		auth.Metadata[xaiAutoDisableStatusMessageKey] = msg
	}
	if !auth.NextRetryAfter.IsZero() {
		auth.Metadata[xaiAutoDisableNextRetryAfterKey] = auth.NextRetryAfter.UTC().Format(time.RFC3339Nano)
	}
	if !auth.UpdatedAt.IsZero() {
		auth.Metadata[xaiAutoDisableUpdatedAtKey] = auth.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	if isXAIAutoDisabledStatusMessage(auth.StatusMessage) || metadataString(auth.Metadata, xaiAutoDisableKindKey) != "" {
		auth.Metadata[xaiAutoDisableKindKey] = xaiAutoDisableKindValue
	}
}

// RestoreXAIAutoDisableStateFromMetadata rehydrates runtime auto-disable fields from auth file metadata.
// Callers must own auth (load/import/update path). Never call this on a live manager
// entry while other goroutines may Clone/List the same *Auth.
func RestoreXAIAutoDisableStateFromMetadata(auth *Auth) {
	if auth == nil || !isXAIProvider(auth.Provider) {
		return
	}
	if !auth.Disabled && auth.Status != StatusDisabled {
		return
	}
	auth.mapsMu.Lock()
	defer auth.mapsMu.Unlock()
	meta := auth.Metadata
	if meta == nil {
		meta = make(map[string]any)
		auth.Metadata = meta
	}
	if msg := metadataString(meta, xaiAutoDisableStatusMessageKey); msg != "" {
		auth.StatusMessage = msg
	}
	if next, ok := parseFlexibleTime(meta[xaiAutoDisableNextRetryAfterKey]); ok {
		auth.NextRetryAfter = next
	}
	if updated, ok := parseFlexibleTime(meta[xaiAutoDisableUpdatedAtKey]); ok {
		auth.UpdatedAt = updated
	}
	// Legacy files only have disabled=true. Treat them as quota-window disables so the
	// rolling 24h re-enable path can recover them from file mtime/UpdatedAt.
	if strings.TrimSpace(auth.StatusMessage) == "" {
		auth.StatusMessage = "quota_exhausted"
		meta[xaiAutoDisableKindKey] = xaiAutoDisableKindValue
		meta[xaiAutoDisableStatusMessageKey] = auth.StatusMessage
	}
	if auth.NextRetryAfter.IsZero() && !auth.UpdatedAt.IsZero() {
		auth.NextRetryAfter = auth.UpdatedAt.Add(xaiDisabledAutoReenableAfter)
		meta[xaiAutoDisableNextRetryAfterKey] = auth.NextRetryAfter.UTC().Format(time.RFC3339Nano)
	}
	if !auth.UpdatedAt.IsZero() {
		meta[xaiAutoDisableUpdatedAtKey] = auth.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	if isXAIAutoDisabledStatusMessage(auth.StatusMessage) {
		meta[xaiAutoDisableKindKey] = xaiAutoDisableKindValue
	}
}

// xaiDisabledReenableAt is read-only: it must not mutate auth.Metadata.
func xaiDisabledReenableAt(auth *Auth) (time.Time, bool) {
	if auth == nil || !isXAIProvider(auth.Provider) {
		return time.Time{}, false
	}
	if !isOutlookXAIAuth(auth) {
		return time.Time{}, false
	}
	if !auth.Disabled && auth.Status != StatusDisabled {
		return time.Time{}, false
	}
	auth.mapsMu.RLock()
	statusMessage := strings.TrimSpace(auth.StatusMessage)
	nextRetryAfter := auth.NextRetryAfter
	updatedAt := auth.UpdatedAt
	meta := auth.Metadata
	if msg := metadataString(meta, xaiAutoDisableStatusMessageKey); msg != "" {
		statusMessage = msg
	}
	if next, ok := parseFlexibleTime(metadataValue(meta, xaiAutoDisableNextRetryAfterKey)); ok {
		nextRetryAfter = next
	}
	if updated, ok := parseFlexibleTime(metadataValue(meta, xaiAutoDisableUpdatedAtKey)); ok {
		updatedAt = updated
	}
	auth.mapsMu.RUnlock()
	if statusMessage == "" {
		// Legacy disabled files are treated as quota-window disables.
		statusMessage = "quota_exhausted"
	}
	if !isXAIAutoDisabledStatusMessage(statusMessage) {
		return time.Time{}, false
	}
	if !nextRetryAfter.IsZero() {
		return nextRetryAfter, true
	}
	if !updatedAt.IsZero() {
		return updatedAt.Add(xaiDisabledAutoReenableAfter), true
	}
	return time.Time{}, false
}

func metadataValue(meta map[string]any, key string) any {
	if meta == nil {
		return nil
	}
	return meta[key]
}

func (m *Manager) reenableAuthAfterAutoDisable(ctx context.Context, auth *Auth, now time.Time) bool {
	if m == nil || auth == nil {
		return false
	}
	reenableAt, ok := xaiDisabledReenableAt(auth)
	if !ok || now.Before(reenableAt) {
		return false
	}
	clone := auth.Clone()
	if clone == nil {
		return false
	}
	clone.Disabled = false
	clone.Status = StatusActive
	clone.StatusMessage = ""
	clone.Unavailable = false
	clone.NextRetryAfter = time.Time{}
	clone.LastError = nil
	clone.Quota.Exceeded = false
	clone.Quota.Reason = ""
	clone.Quota.NextRecoverAt = time.Time{}
	clone.Quota.BackoffLevel = 0
	clone.UpdatedAt = now
	syncXAIAutoDisableMetadata(clone)
	if _, errUpdate := m.Update(ctx, clone); errUpdate != nil {
		log.WithError(errUpdate).Warnf("xai: failed to re-enable auth %s after auto-disable window", auth.ID)
		return false
	}
	log.Infof("xai: re-enabled auth %s after %s auto-disable window", auth.ID, xaiDisabledAutoReenableAfter)
	return true
}

func (m *Manager) reenableExpiredXAIDisabledAuths(ctx context.Context, now time.Time) int {
	if m == nil {
		return 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	m.mu.RLock()
	candidates := make([]*Auth, 0)
	for _, auth := range m.auths {
		// Clone first, then evaluate on the snapshot. Never hydrate/write the live
		// manager entry while holding RLock (plugin List/Clone races on Metadata).
		if auth == nil || (!auth.Disabled && auth.Status != StatusDisabled) {
			continue
		}
		if !isXAIProvider(auth.Provider) {
			continue
		}
		snapshot := auth.Clone()
		if _, ok := xaiDisabledReenableAt(snapshot); ok {
			candidates = append(candidates, snapshot)
		}
	}
	m.mu.RUnlock()

	reenabled := 0
	for _, auth := range candidates {
		if m.reenableAuthAfterAutoDisable(ctx, auth, now) {
			reenabled++
		}
	}
	return reenabled
}

func (m *Manager) runXAIDisabledReenableLoop(ctx context.Context) {
	if m == nil || ctx == nil {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	m.reenableExpiredXAIDisabledAuths(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.reenableExpiredXAIDisabledAuths(ctx, now)
		}
	}
}
