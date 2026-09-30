package auth

import (
	"context"
	"testing"
)

type fakeBinder struct {
	bound   map[string]string
	unbound []string
}

func (f *fakeBinder) BindAccount(_ context.Context, accountID string) string {
	if f.bound == nil {
		f.bound = map[string]string{}
	}
	if f.bound[accountID] == "" {
		f.bound[accountID] = "socks5://warp-lb-1:1080"
	}
	return f.bound[accountID]
}

func (f *fakeBinder) UnbindAccount(_ context.Context, accountID, _ string) {
	f.unbound = append(f.unbound, accountID)
	delete(f.bound, accountID)
}

func (f *fakeBinder) ListBoundAccounts(_ context.Context) []string {
	out := make([]string, 0, len(f.bound))
	for id := range f.bound {
		out = append(out, id)
	}
	return out
}

func TestApplyBoundProxyAndUnbind(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	binder := &fakeBinder{}
	m.SetAccountProxyBinder(binder)
	auth := &Auth{ID: "xai-a@outlook.com.json", Provider: "xai"}
	got := m.applyBoundProxy(auth)
	if got.ProxyURL != "socks5://warp-lb-1:1080" {
		t.Fatalf("proxy = %q", got.ProxyURL)
	}
	m.dropFillFirstMember(auth.ID)
	if len(binder.unbound) != 1 || binder.unbound[0] != auth.ID {
		t.Fatalf("unbound = %#v", binder.unbound)
	}
}

func TestApplyBoundProxySkipsNonXAI(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	binder := &fakeBinder{}
	m.SetAccountProxyBinder(binder)
	auth := &Auth{ID: "antigravity-lindahao28@gmail.com.json", Provider: "antigravity"}
	got := m.applyBoundProxy(auth)
	if got.ProxyURL != "" {
		t.Fatalf("non-xai proxy = %q", got.ProxyURL)
	}
	if len(binder.bound) != 0 {
		t.Fatalf("non-xai should not bind dedicated lb: %#v", binder.bound)
	}
}

func TestReleaseOrphanAccountProxies(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	binder := &fakeBinder{bound: map[string]string{
		"xai-stale.json": "socks5://warp-lb-1:1080",
		"xai-live.json":  "socks5://warp-lb-2:1080",
	}}
	m.SetAccountProxyBinder(binder)
	if m.fillFirst == nil {
		t.Fatal("fill-first pool missing")
	}
	if !m.fillFirst.addIdle("xai-live.json") {
		t.Fatal("add live member")
	}
	m.releaseOrphanAccountProxies()
	if _, ok := binder.bound["xai-stale.json"]; ok {
		t.Fatal("stale bind should be released")
	}
	if binder.bound["xai-live.json"] == "" {
		t.Fatal("in-bucket bind should be kept")
	}
}

func TestReleaseOrphanAccountProxiesKeepsGrok47(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	binder := &fakeBinder{bound: map[string]string{
		"xai-stale.json":  "socks5://warp-lb-1:1080",
		"xai-grok47.json": "socks5://warp-lb-3:1080",
	}}
	m.SetAccountProxyBinder(binder)
	if m.fillFirstGrok47 == nil {
		t.Fatal("grok47 pool missing")
	}
	if !m.fillFirstGrok47.addIdle("xai-grok47.json") {
		t.Fatal("add grok47 member")
	}
	m.releaseOrphanAccountProxies()
	if _, ok := binder.bound["xai-stale.json"]; ok {
		t.Fatal("stale bind should be released")
	}
	if binder.bound["xai-grok47.json"] == "" {
		t.Fatal("grok47 bind should be kept")
	}
}
