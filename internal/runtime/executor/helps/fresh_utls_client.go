package helps

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

const stickyDialTimeout = 12 * time.Second

// freshUtlsHosts are upstream hosts that should use Chrome-like uTLS.
var freshUtlsHosts = map[string]struct{}{
	"api.x.ai":                {},
	"cli-chat-proxy.grok.com": {},
	"console.x.ai":            {},
}

var chromeHelloIDs = []tls.ClientHelloID{
	tls.HelloChrome_Auto,
	tls.HelloChrome_120,
}

type stickyXAIClientCache struct {
	mu      sync.Mutex
	clients map[string]*http.Client
}

var sharedStickyXAIClients = &stickyXAIClientCache{clients: map[string]*http.Client{}}

// stickyUtlsRoundTripper reuses one Chrome-like h2 session per host.
type stickyH2Conn struct {
	h2        *http2.ClientConn
	localAddr net.Addr
}

type stickyUtlsRoundTripper struct {
	dialer proxy.Dialer
	hello  tls.ClientHelloID
	mu     sync.Mutex
	conns  map[string]*stickyH2Conn
}

func newStickyUtlsRoundTripper(proxyURL string) *stickyUtlsRoundTripper {
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("fresh-utls: failed to configure proxy dialer for %q: %v", proxyutil.Redact(proxyURL), errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}
	return &stickyUtlsRoundTripper{
		dialer: dialer,
		hello:  pickChromeHelloID(),
		conns:  make(map[string]*stickyH2Conn),
	}
}

func pickChromeHelloID() tls.ClientHelloID {
	if len(chromeHelloIDs) == 0 {
		return tls.HelloChrome_Auto
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(chromeHelloIDs))))
	if err != nil {
		return tls.HelloChrome_Auto
	}
	return chromeHelloIDs[int(n.Int64())]
}

func (t *stickyUtlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil || req == nil || req.URL == nil {
		return nil, fmt.Errorf("fresh-utls: invalid request")
	}
	req = req.Clone(req.Context())
	req.Close = false
	req.Header.Del("Connection")
	req.Header.Del("Keep-Alive")
	hostname := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(hostname, port)

	h2Conn, err := t.getConn(req, hostname, addr)
	if err != nil {
		return nil, err
	}
	resp, err := h2Conn.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	t.dropConn(hostname, h2Conn)
	h2Conn, errRetry := t.getConn(req, hostname, addr)
	if errRetry != nil {
		return nil, err
	}
	return h2Conn.RoundTrip(req)
}

func (t *stickyUtlsRoundTripper) getConn(req *http.Request, hostname, addr string) (*http2.ClientConn, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cached := t.conns[hostname]; cached != nil && cached.h2 != nil && cached.h2.CanTakeNewRequest() {
		if req != nil {
			recordWarpDialAddr(req.Context(), cached.localAddr)
		}
		return cached.h2, nil
	}
	if cached := t.conns[hostname]; cached != nil && cached.h2 != nil {
		_ = cached.h2.Close()
		delete(t.conns, hostname)
	}

	dialCtx := context.Background()
	if req != nil && req.Context() != nil {
		dialCtx = req.Context()
	}
	conn, err := dialSticky(t.dialer, dialCtx, addr)
	if err != nil {
		return nil, err
	}
	if req != nil {
		recordWarpDial(req.Context(), conn)
	}

	_ = conn.SetDeadline(time.Now().Add(stickyDialTimeout))
	tlsConn := tls.UClient(conn, &tls.Config{ServerName: hostname}, t.hello)
	if err = tlsConn.Handshake(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})

	h2Conn, err := (&http2.Transport{}).NewClientConn(tlsConn)
	if err != nil {
		_ = tlsConn.Close()
		return nil, err
	}
	t.conns[hostname] = &stickyH2Conn{h2: h2Conn, localAddr: conn.LocalAddr()}
	return h2Conn, nil
}

func (t *stickyUtlsRoundTripper) dropConn(hostname string, conn *http2.ClientConn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if current := t.conns[hostname]; current != nil && current.h2 == conn {
		delete(t.conns, hostname)
	}
	if conn != nil {
		_ = conn.Close()
	}
}

func (t *stickyUtlsRoundTripper) CloseIdleConnections() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for host, conn := range t.conns {
		if conn != nil && conn.h2 != nil {
			_ = conn.h2.Close()
		}
		delete(t.conns, host)
	}
}

type stickyXAIRoundTripper struct {
	utls     http.RoundTripper
	fallback http.RoundTripper
}

func (f *stickyXAIRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && req.URL != nil && req.URL.Scheme == "https" {
		if _, ok := freshUtlsHosts[strings.ToLower(req.URL.Hostname())]; ok {
			resp, err := f.utls.RoundTrip(req)
			if err == nil {
				return resp, nil
			}
			log.Debugf("fresh-utls: falling back after error: %v", err)
		}
	}
	return f.fallback.RoundTrip(req)
}

func (f *stickyXAIRoundTripper) CloseIdleConnections() {
	if f == nil {
		return
	}
	if closer, ok := f.utls.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
	if closer, ok := f.fallback.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func stickyXAIClientKey(auth *cliproxyauth.Auth, proxyURL string) string {
	id := ""
	if auth != nil {
		id = strings.TrimSpace(auth.ID)
	}
	return id + "\x00" + strings.TrimSpace(proxyURL)
}

func (c *stickyXAIClientCache) getOrCreate(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	proxyURL := resolveUpstreamProxyURL(cfg, auth)
	key := stickyXAIClientKey(auth, proxyURL)
	c.mu.Lock()
	if client := c.clients[key]; client != nil {
		c.mu.Unlock()
		return client
	}
	c.mu.Unlock()

	client := newStickyXAIHTTPClient(ctx, cfg, auth, timeout, proxyURL)

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing := c.clients[key]; existing != nil {
		if closer, ok := client.Transport.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
		return existing
	}
	if c.clients == nil {
		c.clients = map[string]*http.Client{}
	}
	c.clients[key] = client
	return client
}

func (c *stickyXAIClientCache) invalidate(authID string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	prefix := authID + "\x00"
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, client := range c.clients {
		if key == authID || strings.HasPrefix(key, prefix) {
			if client != nil {
				if closer, ok := client.Transport.(interface{ CloseIdleConnections() }); ok {
					closer.CloseIdleConnections()
				}
			}
			delete(c.clients, key)
		}
	}
}

// InvalidateStickyXAIHTTPClient drops the cached TCP/h2 session for one auth.
func InvalidateStickyXAIHTTPClient(authID string) {
	sharedStickyXAIClients.invalidate(authID)
}

func newStickyXAIHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration, proxyURL string) *http.Client {
	if proxyURL == "" {
		proxyURL = resolveUpstreamProxyURL(cfg, auth)
	}
	fallback := NewProxyAwareHTTPClient(ctx, cfg, auth, timeout)
	client := &http.Client{
		Transport: &stickyXAIRoundTripper{
			utls:     newStickyUtlsRoundTripper(proxyURL),
			fallback: fallback.Transport,
		},
	}
	if timeout > 0 {
		client.Timeout = timeout
	} else if fallback != nil && fallback.Timeout > 0 {
		client.Timeout = fallback.Timeout
	}
	return client
}

// NewFreshProxyAwareHTTPClient returns a one-shot proxy-aware client with
// keep-alives disabled so each call opens a new TCP path.
func NewFreshProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	proxyURL := resolveUpstreamProxyURL(cfg, auth)
	var transport *http.Transport
	if proxyURL != "" {
		transport = buildProxyTransport(proxyURL)
	}
	if transport == nil {
		transport = &http.Transport{}
		if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
			return &http.Client{Transport: rt, Timeout: timeout}
		}
	} else {
		transport = transport.Clone()
	}
	transport.DisableKeepAlives = true
	transport.MaxIdleConns = 0
	transport.MaxIdleConnsPerHost = 0
	client := &http.Client{Transport: transport}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client
}

// NewFreshXAIHTTPClient returns a reusable client for one xAI auth:
// sticky Chrome uTLS + keep-alive h2 for the same account, new session on rotate.
func NewFreshXAIHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return newStickyXAIHTTPClient(ctx, cfg, auth, timeout, "")
	}
	return sharedStickyXAIClients.getOrCreate(ctx, cfg, auth, timeout)
}

func dialSticky(dialer proxy.Dialer, ctx context.Context, addr string) (net.Conn, error) {
	if dialer == nil {
		dialer = proxy.Direct
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dialCtx, cancel := context.WithTimeout(ctx, stickyDialTimeout)
	defer cancel()
	if cd, ok := dialer.(proxy.ContextDialer); ok {
		return cd.DialContext(dialCtx, "tcp", addr)
	}
	type dialResult struct {
		conn net.Conn
		err  error
	}
	ch := make(chan dialResult, 1)
	go func() {
		conn, err := dialer.Dial("tcp", addr)
		ch <- dialResult{conn: conn, err: err}
	}()
	select {
	case <-dialCtx.Done():
		return nil, dialCtx.Err()
	case res := <-ch:
		return res.conn, res.err
	}
}
