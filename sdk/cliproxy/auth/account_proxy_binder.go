package auth

import (
	"context"
	"strings"
)

// AccountProxyBinder assigns an upstream proxy to a fill-first account.
type AccountProxyBinder interface {
	BindAccount(ctx context.Context, accountID string) (proxyURL string)
	UnbindAccount(ctx context.Context, accountID, reason string)
	ListBoundAccounts(ctx context.Context) []string
}

func (m *Manager) SetAccountProxyBinder(binder AccountProxyBinder) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.accountBinder = binder
	m.mu.Unlock()
}

func authUsesDedicatedWarp(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if isXAIProvider(auth.Provider) {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(auth.ID)), "xai-")
}

func (m *Manager) applyBoundProxy(auth *Auth) *Auth {
	if m == nil || auth == nil {
		return auth
	}
	if !authUsesDedicatedWarp(auth) {
		return auth
	}
	m.mu.RLock()
	binder := m.accountBinder
	m.mu.RUnlock()
	if binder == nil {
		return auth
	}
	proxyURL := binder.BindAccount(context.Background(), auth.ID)
	if proxyURL == "" {
		return auth
	}
	auth.ProxyURL = proxyURL
	return auth
}

func (m *Manager) releaseOrphanAccountProxies() {
	if m == nil {
		return
	}
	keep := map[string]struct{}{}
	if m.fillFirst != nil {
		for _, id := range m.fillFirst.memberIDs() {
			keep[id] = struct{}{}
		}
	}
	if m.fillFirstDownrank != nil {
		for _, id := range m.fillFirstDownrank.memberIDs() {
			keep[id] = struct{}{}
		}
	}
	m.mu.RLock()
	binder := m.accountBinder
	filtered := map[string]struct{}{}
	for id := range keep {
		auth := m.auths[id]
		if auth != nil && !authUsesDedicatedWarp(auth) {
			continue
		}
		filtered[id] = struct{}{}
	}
	m.mu.RUnlock()
	if binder == nil {
		return
	}
	for _, id := range binder.ListBoundAccounts(context.Background()) {
		if _, ok := filtered[id]; ok {
			continue
		}
		binder.UnbindAccount(context.Background(), id, "orphan")
	}
}

func (m *Manager) unbindAccountProxy(id, reason string) {
	if m == nil || id == "" {
		return
	}
	m.mu.RLock()
	binder := m.accountBinder
	m.mu.RUnlock()
	if binder == nil {
		return
	}
	binder.UnbindAccount(context.Background(), id, reason)
}
