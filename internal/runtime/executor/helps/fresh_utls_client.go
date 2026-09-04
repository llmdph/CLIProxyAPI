package helps

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// freshUtlsHosts are upstream hosts that should dial a brand-new Chrome-like
// TLS session on every request (no h2 connection reuse).
var freshUtlsHosts = map[string]struct{}{
	"api.x.ai":                {},
	"cli-chat-proxy.grok.com": {},
	"console.x.ai":             {},
}

var chromeHelloIDs = []tls.ClientHelloID{
	tls.HelloChrome_Auto,
	tls.HelloChrome_120,
}

// freshUtlsRoundTripper dials a new utls Chrome fingerprint connection per request.
type freshUtlsRoundTripper struct {
	dialer proxy.Dialer
}

func newFreshUtlsRoundTripper(proxyURL string) *freshUtlsRoundTripper {
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("fresh-utls: failed to configure proxy dialer for %q: %v", proxyutil.Redact(proxyURL), errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}
	return &freshUtlsRoundTripper{dialer: dialer}
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

func (t *freshUtlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil || req == nil || req.URL == nil {
		return nil, fmt.Errorf("fresh-utls: invalid request")
	}
	hostname := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(hostname, port)

	conn, err := t.dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	if req != nil {
		recordWarpDial(req.Context(), conn)
	}

	tlsConn := tls.UClient(conn, &tls.Config{ServerName: hostname}, pickChromeHelloID())
	if err = tlsConn.Handshake(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	h2Conn, err := (&http2.Transport{}).NewClientConn(tlsConn)
	if err != nil {
		_ = tlsConn.Close()
		return nil, err
	}

	resp, err := h2Conn.RoundTrip(req)
	if err != nil {
		_ = h2Conn.Close()
		return nil, err
	}
	resp.Body = &freshCloseBody{ReadCloser: resp.Body, closeFn: func() {
		_ = h2Conn.Close()
	}}
	return resp, nil
}

type freshCloseBody struct {
	io.ReadCloser
	closeFn func()
	closed  bool
}

func (b *freshCloseBody) Close() error {
	if b == nil {
		return nil
	}
	var err error
	if !b.closed {
		b.closed = true
		err = b.ReadCloser.Close()
		if b.closeFn != nil {
			b.closeFn()
		}
	}
	return err
}

type freshXAIRoundTripper struct {
	utls     http.RoundTripper
	fallback http.RoundTripper
}

func (f *freshXAIRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
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

// NewFreshXAIHTTPClient returns a one-shot client for xAI upstreams:
// fresh TCP (no keep-alive pool) + rotating Chrome uTLS for xAI hosts.
func NewFreshXAIHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	proxyURL := resolveUpstreamProxyURL(cfg, auth)
	fallback := NewFreshProxyAwareHTTPClient(ctx, cfg, auth, timeout)
	client := &http.Client{
		Transport: &freshXAIRoundTripper{
			utls:     newFreshUtlsRoundTripper(proxyURL),
			fallback: fallback.Transport,
		},
	}
	if timeout > 0 {
		client.Timeout = timeout
	} else if fallback.Timeout > 0 {
		client.Timeout = fallback.Timeout
	}
	return client
}
