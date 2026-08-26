package auth

import (
	"context"
	"math/rand/v2"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
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

func (m *Manager) beginFillFirstHold(ctx context.Context) (context.Context, func()) {
	if m == nil || !m.fillFirstEnabled() {
		return ctx, func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if hold, _ := ctx.Value(fillFirstHoldKey{}).(*fillFirstHold); hold != nil {
		return ctx, func() {}
	}
	hold := &fillFirstHold{}
	ctx = context.WithValue(ctx, fillFirstHoldKey{}, hold)
	return ctx, func() {
		if hold.id != "" {
			m.fillFirst.release(hold.id)
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

func (m *Manager) acquireFillFirstAuth(
	ctx context.Context,
	providers []string,
	model string,
	tried map[string]struct{},
	hold *fillFirstHold,
) (*Auth, ProviderExecutor, string, error) {
	if m == nil || m.fillFirst == nil || hold == nil {
		return m.pickRandomAvailableAuth(model, nil, tried)
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

	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, "", err
		}
		if hold.id != "" {
			m.fillFirst.release(hold.id)
			hold.id = ""
		}
		m.pruneFillFirstMembers()

		if auth, exec, provider := m.occupyFillFirstMember(model, providerSet, tried); auth != nil {
			hold.id = auth.ID
			return auth, exec, provider, nil
		}

		skip := make(map[string]struct{}, len(tried)+len(m.fillFirst.memberIDs())+1)
		for id := range tried {
			skip[id] = struct{}{}
		}
		for _, id := range m.fillFirst.memberIDs() {
			skip[id] = struct{}{}
		}

		canExpand := false
		m.fillFirst.mu.Lock()
		canExpand = len(m.fillFirst.members) < fillFirstPoolMax
		busy := len(m.fillFirst.inFlight) > 0
		queueFull := len(m.fillFirst.waiters) >= fillFirstQueueMax
		m.fillFirst.mu.Unlock()

		if canExpand {
			auth, exec, provider, errPick := m.pickRandomAvailableAuth(model, providerSet, skip)
			if errPick == nil && auth != nil && m.fillFirst.addAndOccupy(auth.ID) {
				hold.id = auth.ID
				return auth, exec, provider, nil
			}
			if errPick != nil && !busy {
				return nil, nil, "", errPick
			}
		}

		if !busy {
			auth, exec, provider, errPick := m.pickRandomAvailableAuth(model, providerSet, tried)
			if errPick != nil {
				return nil, nil, "", errPick
			}
			if auth != nil && m.fillFirst.addAndOccupy(auth.ID) {
				hold.id = auth.ID
				return auth, exec, provider, nil
			}
			return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		if queueFull {
			return nil, nil, "", fillFirstBusyError()
		}
		if errWait := m.fillFirst.wait(ctx); errWait != nil {
			return nil, nil, "", errWait
		}
	}
}

func (m *Manager) pruneFillFirstMembers() {
	if m == nil || m.fillFirst == nil {
		return
	}
	alive := make(map[string]struct{})
	m.mu.RLock()
	for _, auth := range m.auths {
		if auth == nil || auth.ID == "" {
			continue
		}
		if auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		alive[auth.ID] = struct{}{}
	}
	m.mu.RUnlock()
	m.fillFirst.pruneMissing(alive)
}

func (m *Manager) occupyFillFirstMember(model string, providers map[string]struct{}, tried map[string]struct{}) (*Auth, ProviderExecutor, string) {
	if m == nil || m.fillFirst == nil {
		return nil, nil, ""
	}
	now := time.Now()
	members := m.fillFirst.memberIDs()
	availableIDs := make([]string, 0, len(members))
	lookups := make(map[string]*Auth, len(members))
	m.mu.RLock()
	for _, id := range members {
		auth := m.auths[id]
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
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
	occupied := m.fillFirst.occupyIdle(availableIDs, tried)
	if occupied == "" {
		return nil, nil, ""
	}
	auth := lookups[occupied]
	if auth == nil {
		m.fillFirst.release(occupied)
		return nil, nil, ""
	}
	provider := executorKeyFromAuth(auth)
	exec, ok := m.Executor(provider)
	if !ok || exec == nil {
		m.fillFirst.release(occupied)
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


func (m *Manager) pickRandomAvailableAuth(model string, providers map[string]struct{}, skip map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if m == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	now := time.Now()
	candidates := make([]*Auth, 0, 32)
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
