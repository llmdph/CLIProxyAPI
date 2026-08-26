package auth

import (
	"context"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const xaiDisabledAutoReenableAfter = 24 * time.Hour

// xaiCloudflareSameAuthMaxAttempts is the total number of upstream attempts on the
// same credential when Cloudflare returns a challenge / blocked HTML (often as 400).
const xaiCloudflareSameAuthMaxAttempts = 3

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
	if _, errUpdate := m.Update(ctx, clone); errUpdate != nil {
		log.WithError(errUpdate).Warnf("xai: failed to disable auth %s after %s", auth.ID, detail)
		return
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
	if _, errUpdate := m.Update(ctx, clone); errUpdate != nil {
		log.WithError(errUpdate).Warnf("xai: failed to disable auth %s after %s", auth.ID, detail)
		return
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

func xaiDisabledReenableAt(auth *Auth) (time.Time, bool) {
	if auth == nil || !isXAIProvider(auth.Provider) {
		return time.Time{}, false
	}
	if !auth.Disabled && auth.Status != StatusDisabled {
		return time.Time{}, false
	}
	if !isXAIAutoDisabledStatusMessage(auth.StatusMessage) {
		return time.Time{}, false
	}
	if !auth.NextRetryAfter.IsZero() {
		return auth.NextRetryAfter, true
	}
	if !auth.UpdatedAt.IsZero() {
		return auth.UpdatedAt.Add(xaiDisabledAutoReenableAfter), true
	}
	return time.Time{}, false
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
		if _, ok := xaiDisabledReenableAt(auth); ok {
			candidates = append(candidates, auth.Clone())
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
