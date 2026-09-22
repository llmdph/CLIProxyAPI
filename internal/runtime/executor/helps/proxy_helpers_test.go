package helps

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
		0,
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestNewProxyAwareHTTPClientReusesPooledClientsAndTransport(t *testing.T) {
	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://pool-proxy.example.com:8080"}}
	auth := &cliproxyauth.Auth{}

	first := NewProxyAwareHTTPClient(context.Background(), cfg, auth, 0)
	seen := map[*http.Client]struct{}{first: {}}
	var sharedTransport http.RoundTripper = first.Transport

	for i := 0; i < proxyHTTPClientPoolSize*3; i++ {
		client := NewProxyAwareHTTPClient(context.Background(), cfg, auth, 0)
		seen[client] = struct{}{}
		if client.Transport != sharedTransport {
			t.Fatalf("iteration %d: transport was not shared across pooled clients", i)
		}
	}

	if len(seen) == 0 {
		t.Fatal("expected pooled clients")
	}
	if len(seen) > proxyHTTPClientPoolSize {
		t.Fatalf("unique clients = %d, want <= pool size %d", len(seen), proxyHTTPClientPoolSize)
	}

	transport, ok := sharedTransport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", sharedTransport)
	}
	if transport.IdleConnTimeout != 30*time.Second {
		t.Fatalf("IdleConnTimeout = %v, want 30s", transport.IdleConnTimeout)
	}
	if transport.MaxIdleConnsPerHost != proxyHTTPClientPoolIdleKeep {
		t.Fatalf("MaxIdleConnsPerHost = %d, want %d", transport.MaxIdleConnsPerHost, proxyHTTPClientPoolIdleKeep)
	}
	if transport.MaxConnsPerHost != 0 {
		t.Fatalf("MaxConnsPerHost = %d, want 0 (unlimited)", transport.MaxConnsPerHost)
	}
}

func TestNewProxyAwareHTTPClientPoolKeyIncludesTimeout(t *testing.T) {
	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://timeout-proxy.example.com:8080"}}
	auth := &cliproxyauth.Auth{}

	noTimeout := NewProxyAwareHTTPClient(context.Background(), cfg, auth, 0)
	withTimeout := NewProxyAwareHTTPClient(context.Background(), cfg, auth, 15*time.Second)

	if noTimeout == withTimeout {
		t.Fatal("clients with different timeouts should not be identical pool entries")
	}
	if withTimeout.Timeout != 15*time.Second {
		t.Fatalf("Timeout = %v, want 15s", withTimeout.Timeout)
	}
	if noTimeout.Timeout != 0 {
		t.Fatalf("Timeout = %v, want 0", noTimeout.Timeout)
	}
}

func TestNewProxyAwareHTTPClientAccountProxyIsIsolated(t *testing.T) {
	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}}
	authA := &cliproxyauth.Auth{ID: "account-a", ProxyURL: "http://account-a-proxy.example.com:8080"}
	authB := &cliproxyauth.Auth{ID: "account-b", ProxyURL: "http://account-a-proxy.example.com:8080"}

	clientA := NewProxyAwareHTTPClient(context.Background(), cfg, authA, 0)
	clientB := NewProxyAwareHTTPClient(context.Background(), cfg, authB, 0)
	if clientA.Transport == clientB.Transport {
		t.Fatal("per-account proxy overrides with different account IDs should not share transport")
	}

	clientA2 := NewProxyAwareHTTPClient(context.Background(), cfg, authA, 0)
	if clientA.Transport != clientA2.Transport {
		t.Fatal("same account proxy scope should reuse transport")
	}
}

func TestHTTPUpstreamDoNilRequest(t *testing.T) {
	_, err := HTTPUpstreamDo(context.Background(), nil, nil, nil, 0)
	if err == nil {
		t.Fatal("expected error for nil request")
	}
}

func TestNewDevinHTTPClient_ReusesTransportFromContext(t *testing.T) {
	baseTransport := &http.Transport{}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", baseTransport)

	c1 := NewDevinHTTPClient(ctx, nil, nil, 0)
	c2 := NewDevinHTTPClient(ctx, nil, nil, 0)

	if c1.Transport != c2.Transport {
		t.Errorf("expected c1.Transport == c2.Transport across requests, got different pointers %p vs %p", c1.Transport, c2.Transport)
	}

	tr, ok := c1.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c1.Transport)
	}
	if !tr.DisableCompression {
		t.Error("expected DisableCompression = true")
	}
}

func TestNewDevinHTTPClient_NonStandardRoundTripperDisablesGzip(t *testing.T) {
	var seenEncoding string
	customRT := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		seenEncoding = req.Header.Get("Accept-Encoding")
		return &http.Response{StatusCode: 200}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", customRT)

	c := NewDevinHTTPClient(ctx, nil, nil, 0)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid", nil)
	_, _ = c.Transport.RoundTrip(req)

	if seenEncoding != "identity" {
		t.Errorf("expected Accept-Encoding: identity, got %q", seenEncoding)
	}
}

type roundTripperFunc func(req *http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
