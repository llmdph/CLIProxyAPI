package auth

import (
	"context"
	"math/rand/v2"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

func (m *Manager) fillFirstEnabled() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	_, ok := selector.(*FillFirstSelector)
	return ok
}

func (m *Manager) beginFillFirstHold(ctx context.Context, req cliproxyexecutor.Request, opts *cliproxyexecutor.Options) (context.Context, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	var optVal cliproxyexecutor.Options
	if opts != nil {
		optVal = *opts
	}
	class := classifyLLMRequest(req, optVal)
	ctx = cliproxyexecutor.WithRequestClass(ctx, class)
	if m != nil {
		ctx = cliproxyexecutor.WithXAIThinkOK(ctx, func(authID string) {
			m.restoreAuthFromDownrankPoolByID(authID)
		})
	}
	if opts != nil {
		opts.EnsureMetadata()[cliproxyexecutor.RequestClassMetadataKey] = class
	}
	if class != cliproxyexecutor.RequestClassNormal {
		poolName := "main"
		if cliproxyexecutor.RequestClassUsesAuxPool(class) {
			poolName = "downrank"
		}
		log.Infof("llm request class=%s pool=%s", class, poolName)
	}
	if m == nil || !m.fillFirstEnabled() {
		return ctx, func() {}
	}
	if hold, _ := ctx.Value(fillFirstHoldKey{}).(*fillFirstHold); hold != nil {
		return ctx, func() {}
	}
	pool := m.fillFirst
	noRetry := false
	if cliproxyexecutor.RequestClassUsesAuxPool(class) {
		if m.fillFirstDownrank != nil {
			pool = m.fillFirstDownrank
		}
		noRetry = true
	}
	hold := &fillFirstHold{pool: pool, noRetry: noRetry}
	ctx = context.WithValue(ctx, fillFirstHoldKey{}, hold)
	return ctx, func() {
		if hold.id != "" {
			holdPool := hold.pool
			if holdPool == nil {
				holdPool = m.fillFirst
			}
			if holdPool != nil {
				holdPool.release(hold.id)
			}
			hold.id = ""
		}
	}
}

func fillFirstHoldFrom(ctx context.Context) *fillFirstHold {
	if ctx == nil {
		return nil
	}
	hold, _ := ctx.Value(fillFirstHoldKey{}).(*fillFirstHold)
	return hold
}

func skipCredentialRetry(ctx context.Context) bool {
	if cliproxyexecutor.RequestClassUsesAuxPool(cliproxyexecutor.RequestClassFromContext(ctx)) {
		return true
	}
	hold := fillFirstHoldFrom(ctx)
	return hold != nil && hold.noRetry
}

func (h *fillFirstHold) poolOr(m *Manager) *fillFirstPool {
	if h != nil && h.pool != nil {
		return h.pool
	}
	if m != nil {
		return m.fillFirst
	}
	return nil
}

func (m *Manager) otherFillFirstPool(pool *fillFirstPool) *fillFirstPool {
	if m == nil || pool == nil {
		return nil
	}
	if pool == m.fillFirstDownrank {
		return m.fillFirst
	}
	if pool == m.fillFirst {
		return m.fillFirstDownrank
	}
	return nil
}

func (m *Manager) syncFillFirstMembership(auth *Auth) {
	if m == nil || auth == nil {
		return
	}
	id := strings.TrimSpace(auth.ID)
	if id == "" {
		return
	}
	if isXAIDownrankAuth(auth) {
		if m.fillFirst != nil {
			m.fillFirst.drop(id)
		}
		if m.fillFirstDownrank != nil {
			_ = m.fillFirstDownrank.addIdle(id)
		}
		return
	}
	if m.fillFirstDownrank == nil || !m.fillFirstDownrank.hasMember(id) {
		return
	}
	if !m.fillFirstDownrank.dropIfIdle(id) {
		return
	}
	if m.fillFirst != nil {
		_ = m.fillFirst.addIdle(id)
	}
}

func (m *Manager) dropFillFirstMember(id string) {
	if m == nil || id == "" {
		return
	}
	if m.fillFirst != nil {
		m.fillFirst.drop(id)
	}
	if m.fillFirstDownrank != nil {
		m.fillFirstDownrank.drop(id)
	}
}

func (m *Manager) acquireFillFirstAuth(
	ctx context.Context,
	providers []string,
	model string,
	tried map[string]struct{},
	hold *fillFirstHold,
) (*Auth, ProviderExecutor, string, error) {
	if m == nil || hold == nil {
		return m.pickRandomAvailableAuth(model, nil, tried, false)
	}
	pool := hold.poolOr(m)
	if pool == nil {
		return m.pickRandomAvailableAuth(model, nil, tried, false)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		key := strings.TrimSpace(strings.ToLower(provider))
		if key != "" {
			providerSet[key] = struct{}{}
		}
	}
	if tried == nil {
		tried = make(map[string]struct{})
	}
	other := m.otherFillFirstPool(pool)

	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, "", err
		}
		if hold.id != "" {
			pool.release(hold.id)
			hold.id = ""
		}
		m.pruneFillFirstMembers()

		if auth, exec, provider := m.occupyFillFirstMember(pool, model, providerSet, tried); auth != nil {
			hold.id = auth.ID
			return auth, exec, provider, nil
		}

		skip := make(map[string]struct{}, len(tried)+len(pool.memberIDs())+8)
		for id := range tried {
			skip[id] = struct{}{}
		}
		for _, id := range pool.memberIDs() {
			skip[id] = struct{}{}
		}
		if other != nil {
			for _, id := range other.memberIDs() {
				skip[id] = struct{}{}
			}
		}

		canExpand := false
		pool.mu.Lock()
		canExpand = len(pool.members) < fillFirstPoolMax
		busy := len(pool.inFlight) > 0
		queueFull := len(pool.waiters) >= fillFirstQueueMax
		pool.mu.Unlock()

		downrank := pool == m.fillFirstDownrank
		if canExpand {
			auth, exec, provider, errPick := m.pickRandomAvailableAuth(model, providerSet, skip, downrank)
			if errPick == nil && auth != nil {
				if other != nil && other.hasMember(auth.ID) {
					tried[auth.ID] = struct{}{}
					continue
				}
				if downrank && !isXAIDownrankAuth(auth) {
					hold.id = auth.ID
					return auth, exec, provider, nil
				}
				if pool.addAndOccupy(auth.ID) {
					hold.id = auth.ID
					return auth, exec, provider, nil
				}
			}
			if downrank && (auth == nil || errPick != nil) {
				fallback, fallbackExec, fallbackProvider, errFallback := m.pickRandomAvailableAuth(model, providerSet, skip, false)
				if errFallback == nil && fallback != nil {
					if other == nil || !other.hasMember(fallback.ID) {
						hold.id = fallback.ID
						return fallback, fallbackExec, fallbackProvider, nil
					}
				}
			}
			if errPick != nil && !busy {
				return nil, nil, "", errPick
			}
		}

		if !busy {
			auth, exec, provider, errPick := m.pickRandomAvailableAuth(model, providerSet, tried, downrank)
			if errPick != nil && downrank {
				auth, exec, provider, errPick = m.pickRandomAvailableAuth(model, providerSet, tried, false)
				if errPick == nil && auth != nil && (other == nil || !other.hasMember(auth.ID)) {
					hold.id = auth.ID
					return auth, exec, provider, nil
				}
			}
			if errPick != nil {
				return nil, nil, "", errPick
			}
			if auth != nil {
				if other != nil && other.hasMember(auth.ID) {
					return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
				}
				if downrank && !isXAIDownrankAuth(auth) {
					hold.id = auth.ID
					return auth, exec, provider, nil
				}
				if pool.addAndOccupy(auth.ID) {
					hold.id = auth.ID
					return auth, exec, provider, nil
				}
			}
			return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		if queueFull {
			return nil, nil, "", fillFirstBusyError()
		}
		if errWait := pool.wait(ctx); errWait != nil {
			return nil, nil, "", errWait
		}
	}
}

func (m *Manager) pruneFillFirstMembers() {
	if m == nil {
		return
	}
	mainAlive := make(map[string]struct{})
	downrankAlive := make(map[string]struct{})
	m.mu.RLock()
	for _, auth := range m.auths {
		if auth == nil || auth.ID == "" {
			continue
		}
		if auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if isXAIDownrankAuth(auth) {
			downrankAlive[auth.ID] = struct{}{}
			continue
		}
		mainAlive[auth.ID] = struct{}{}
	}
	m.mu.RUnlock()
	if m.fillFirst != nil {
		m.fillFirst.pruneMissing(mainAlive)
	}
	if m.fillFirstDownrank != nil {
		m.fillFirstDownrank.pruneMissing(downrankAlive)
	}
}

func (m *Manager) occupyFillFirstMember(pool *fillFirstPool, model string, providers map[string]struct{}, tried map[string]struct{}) (*Auth, ProviderExecutor, string) {
	if m == nil {
		return nil, nil, ""
	}
	if pool == nil {
		pool = m.fillFirst
	}
	if pool == nil {
		return nil, nil, ""
	}
	now := time.Now()
	members := pool.memberIDs()
	availableIDs := make([]string, 0, len(members))
	lookups := make(map[string]*Auth, len(members))
	m.mu.RLock()
	for _, id := range members {
		auth := m.auths[id]
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if pool == m.fillFirst && isXAIDownrankAuth(auth) {
			continue
		}
		if pool == m.fillFirstDownrank && !isXAIDownrankAuth(auth) {
			continue
		}
		if len(providers) > 0 {
			if _, ok := providers[executorKeyFromAuth(auth)]; !ok {
				continue
			}
		}
		blocked, _, _ := isAuthBlockedForModel(auth, model, now)
		if blocked {
			continue
		}
		availableIDs = append(availableIDs, id)
		lookups[id] = auth
	}
	m.mu.RUnlock()
	occupied := pool.occupyIdle(availableIDs, tried)
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
	return auth.Clone(), exec, provider
}

func attachFillFirstStreamHold(result *cliproxyexecutor.StreamResult, finish func()) *cliproxyexecutor.StreamResult {
	if finish == nil {
		return result
	}
	if result == nil || result.Chunks == nil {
		finish()
		return result
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer finish()
		defer close(out)
		for chunk := range result.Chunks {
			out <- chunk
		}
	}()
	wrapped := *result
	wrapped.Chunks = out
	return &wrapped
}

func (m *Manager) pickRandomAvailableAuth(model string, providers map[string]struct{}, skip map[string]struct{}, downrank bool) (*Auth, ProviderExecutor, string, error) {
	if m == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	now := time.Now()
	marked := make([]*Auth, 0, 16)
	unmarked := make([]*Auth, 0, 32)
	m.mu.RLock()
	for _, auth := range m.auths {
		if auth == nil || auth.ID == "" || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if skip != nil {
			if _, used := skip[auth.ID]; used {
				continue
			}
		}
		if len(providers) > 0 {
			if _, ok := providers[executorKeyFromAuth(auth)]; !ok {
				continue
			}
		}
		blocked, _, _ := isAuthBlockedForModel(auth, model, now)
		if blocked {
			continue
		}
		if isXAIDownrankAuth(auth) {
			marked = append(marked, auth)
			continue
		}
		unmarked = append(unmarked, auth)
	}
	m.mu.RUnlock()
	candidates := unmarked
	if downrank {
		candidates = marked
	}
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
