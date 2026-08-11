package auth

import (
	"context"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

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
// account. Only no-think (风控降智) and quota/spending exhaustion rotate;
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
	if isXAIProvider(provider) && err != nil && !shouldRotateXAICredential(err) {
		return
	}
	m.recordExecutionResult(ctx, result, auth, ephemeral)
}

func (m *Manager) disableAuthForQuotaExhausted(ctx context.Context, auth *Auth, err error) {
	if m == nil || auth == nil {
		return
	}
	clone := auth.Clone()
	if clone == nil {
		return
	}
	clone.Disabled = true
	clone.Status = StatusDisabled
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
