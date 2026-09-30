package auth

import (
	"context"
	"math/rand/v2"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	log "github.com/sirupsen/logrus"
)

func (m *Manager) grok47AccountPool() string {
	if m == nil {
		return internalconfig.Grok47AccountsOutlookClean
	}
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil {
		return internalconfig.Grok47AccountsOutlookClean
	}
	return cfg.XAI.Grok47AccountPool()
}

func (m *Manager) grok47PoolActive() bool {
	return m.grok47AccountPool() != internalconfig.Grok47AccountsOutlookClean
}

func canonicalGrokModelName(model string) string {
	name := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if parsed := thinking.ParseSuffix(name); strings.TrimSpace(parsed.ModelName) != "" {
		name = strings.ToLower(strings.TrimSpace(parsed.ModelName))
	}
	return name
}

func isGrok47AccountPoolModel(model string) bool {
	switch canonicalGrokModelName(model) {
	case "grok-4.7", "grok-4.7-build-fast", "grok-4.7-fast":
		return true
	default:
		return false
	}
}

func (m *Manager) retireGrok47PoolIfInactive() {
	if m == nil || m.grok47PoolActive() || m.fillFirstGrok47 == nil {
		return
	}
	for _, id := range m.fillFirstGrok47.memberIDs() {
		m.fillFirstGrok47.drop(id)
	}
}

func (m *Manager) reservedBySharedFillFirst(id string) bool {
	if m == nil || id == "" {
		return false
	}
	if m.fillFirst != nil && m.fillFirst.hasMember(id) {
		return true
	}
	if m.fillFirstDownrank != nil && m.fillFirstDownrank.hasMember(id) {
		return true
	}
	return false
}

func xaiGrok47Eligible(auth *Auth, mode, model string, now time.Time) bool {
	if auth == nil || auth.ID == "" || !isXAIProvider(auth.Provider) {
		return false
	}
	if xaiAuthEmail(auth) == "" {
		return false
	}
	outlook := isOutlookXAIAuth(auth)
	switch mode {
	case internalconfig.Grok47AccountsOutlookAll:
		if !outlook {
			return false
		}
	case internalconfig.Grok47AccountsNonOutlook:
		if outlook {
			return false
		}
	case internalconfig.Grok47AccountsAll:
	default:
		if !outlook || isXAIDownrankAuth(auth) {
			return false
		}
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return false
	}
	blocked, _, _ := isAuthBlockedForModel(auth, model, now)
	return !blocked
}

func (m *Manager) releaseUnusableGrok47Member(ctx context.Context, auth *Auth) {
	if m == nil || auth == nil || m.fillFirstGrok47 == nil {
		return
	}
	hold := fillFirstHoldFrom(ctx)
	if hold == nil || hold.pool != m.fillFirstGrok47 {
		return
	}
	m.fillFirstGrok47.drop(strings.TrimSpace(auth.ID))
}

func providerSetFromList(providers []string) map[string]struct{} {
	if len(providers) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		key := strings.TrimSpace(strings.ToLower(provider))
		if key != "" {
			out[key] = struct{}{}
		}
	}
	return out
}

func (m *Manager) pickGrok47AvailableAuth(model string, providers map[string]struct{}, skip map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if m == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	mode := m.grok47AccountPool()
	now := time.Now()
	candidates := make([]*Auth, 0, 32)
	m.mu.RLock()
	for _, auth := range m.auths {
		if auth == nil || auth.ID == "" {
			continue
		}
		if skip != nil {
			if _, used := skip[auth.ID]; used {
				continue
			}
		}
		if m.fillFirstGrok47 != nil && m.fillFirstGrok47.hasMember(auth.ID) {
			continue
		}
		if m.reservedBySharedFillFirst(auth.ID) {
			continue
		}
		if len(providers) > 0 {
			if _, ok := providers[executorKeyFromAuth(auth)]; !ok {
				continue
			}
		}
		if !xaiGrok47Eligible(auth, mode, model, now) {
			continue
		}
		candidates = append(candidates, auth)
	}
	m.mu.RUnlock()
	if len(candidates) == 0 {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	picked := candidates[rand.IntN(len(candidates))]
	provider := executorKeyFromAuth(picked)
	exec, ok := m.Executor(provider)
	if !ok || exec == nil {
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	return picked.Clone(), exec, provider, nil
}

func (m *Manager) pruneGrok47PolicyMembers(model string) {
	if m == nil || m.fillFirstGrok47 == nil {
		return
	}
	mode := m.grok47AccountPool()
	now := time.Now()
	alive := make(map[string]struct{})
	m.mu.RLock()
	for _, auth := range m.auths {
		if auth == nil || auth.ID == "" || m.reservedBySharedFillFirst(auth.ID) {
			continue
		}
		if xaiGrok47Eligible(auth, mode, model, now) {
			alive[auth.ID] = struct{}{}
		}
	}
	m.mu.RUnlock()
	for _, id := range m.fillFirstGrok47.memberIDs() {
		if m.fillFirstGrok47.inFlightHas(id) {
			alive[id] = struct{}{}
		}
	}
	for _, id := range m.fillFirstGrok47.pruneMissing(alive) {
		m.unbindAccountProxy(id, "missing")
	}
}

func (m *Manager) occupyGrok47Member(pool *fillFirstPool, model string, providers map[string]struct{}, tried map[string]struct{}) (*Auth, ProviderExecutor, string) {
	if m == nil || pool == nil {
		return nil, nil, ""
	}
	mode := m.grok47AccountPool()
	now := time.Now()
	members := pool.memberIDs()
	available := make([]string, 0, len(members))
	lookups := make(map[string]*Auth, len(members))
	m.mu.RLock()
	for _, id := range members {
		auth := m.auths[id]
		if auth == nil || m.reservedBySharedFillFirst(id) {
			continue
		}
		if len(providers) > 0 {
			if _, ok := providers[executorKeyFromAuth(auth)]; !ok {
				continue
			}
		}
		if !xaiGrok47Eligible(auth, mode, model, now) {
			continue
		}
		available = append(available, id)
		lookups[id] = auth
	}
	m.mu.RUnlock()
	occupied := pool.occupyIdle(available, tried)
	if occupied == "" {
		return nil, nil, ""
	}
	auth := lookups[occupied]
	if auth == nil {
		pool.release(occupied)
		return nil, nil, ""
	}
	provider := executorKeyFromAuth(auth)
	exec, ok := m.Executor(provider)
	if !ok || exec == nil {
		pool.release(occupied)
		return nil, nil, ""
	}
	return m.applyBoundProxy(auth.Clone()), exec, provider
}

func (m *Manager) acquireGrok47PolicyAuth(
	ctx context.Context,
	model string,
	providerSet map[string]struct{},
	tried map[string]struct{},
	hold *fillFirstHold,
) (*Auth, ProviderExecutor, string, error) {
	if m == nil || hold == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if m.fillFirstGrok47 == nil {
		m.fillFirstGrok47 = newFillFirstPool()
	}
	pool := m.fillFirstGrok47
	if ctx == nil {
		ctx = context.Background()
	}
	if tried == nil {
		tried = make(map[string]struct{})
	}
	m.pruneGrok47PolicyMembers(model)
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, "", err
		}
		retrying := hold.acquired
		prevID := hold.id
		prevGone := false
		if prevID != "" {
			prevGone = !pool.hasMember(prevID)
			pool.release(prevID)
			hold.id = ""
			if prevGone {
				tried[prevID] = struct{}{}
			}
		}
		if auth, exec, provider := m.occupyGrok47Member(pool, model, providerSet, tried); auth != nil {
			hold.occupy(auth.ID)
			return auth, exec, provider, nil
		}
		memberCount, inFlightCount, waiterCount, gen := pool.snapshot()
		canExpand := memberCount < fillFirstPoolMax && (!retrying || memberCount == 0 || prevGone)
		busy := inFlightCount > 0
		if canExpand {
			skip := make(map[string]struct{}, len(tried)+8)
			for id := range tried {
				skip[id] = struct{}{}
			}
			for _, id := range pool.memberIDs() {
				skip[id] = struct{}{}
			}
			auth, exec, provider, errPick := m.pickGrok47AvailableAuth(model, providerSet, skip)
			if errPick == nil && auth != nil {
				if pool.addAndOccupy(auth.ID) {
					hold.occupy(auth.ID)
					return m.applyBoundProxy(auth), exec, provider, nil
				}
				tried[auth.ID] = struct{}{}
				continue
			}
			if errPick != nil && (!busy || prevGone) {
				return nil, nil, "", errPick
			}
		}
		if !busy {
			log.Debugf("xai: grok-4.7 account pool exhausted model=%s mode=%s", model, m.grok47AccountPool())
			return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		if waiterCount >= fillFirstQueueMax {
			log.Warnf("fill-first busy model=%s members=%d in_flight=%d waiters=%d", model, memberCount, inFlightCount, waiterCount)
			return nil, nil, "", fillFirstBusyError()
		}
		if errWait := pool.wait(ctx, gen); errWait != nil {
			return nil, nil, "", errWait
		}
	}
}
