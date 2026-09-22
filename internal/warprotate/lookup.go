package warprotate

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	baseMu   sync.RWMutex
	agentURL string
)

// SetBaseURL stores the rotate-agent base URL used for session lookup.
func SetBaseURL(raw string) {
	baseMu.Lock()
	agentURL = strings.TrimRight(strings.TrimSpace(raw), "/")
	baseMu.Unlock()
}

// BaseURL returns the configured rotate-agent URL.
func BaseURL() string {
	baseMu.RLock()
	defer baseMu.RUnlock()
	return agentURL
}

// LookupResult is the rotate-agent mapping from a SOCKS client address to an LB node.
type LookupResult struct {
	OK       bool   `json:"ok"`
	Client   string `json:"client"`
	Server   string `json:"server"`
	Instance string `json:"instance"`
	Label    string `json:"label"`
	ExitIP   string `json:"exit_ip"`
}

// Lookup maps a HAProxy client address (containerIP:port) to warp-lb-N / lbN.
func Lookup(ctx context.Context, client string) *LookupResult {
	return LookupURL(ctx, BaseURL(), client)
}

// LookupURL queries a specific rotate-agent base URL.
func LookupURL(ctx context.Context, base, client string) *LookupResult {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	client = strings.TrimSpace(client)
	if base == "" || client == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	endpoint := base + "/v1/lookup?client=" + url.QueryEscape(client)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return nil
	}
	var out LookupResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil
	}
	out.Client = firstNonEmpty(out.Client, client)
	out.Label = firstNonEmpty(out.Label, NodeLabel(out.Instance, out.Server))
	if !LooksLikeIP(out.ExitIP) {
		out.ExitIP = ""
	}
	if !out.OK && out.Instance == "" && out.Server == "" {
		return nil
	}
	return &out
}

// NodeLabel turns warp-lb-3 / w3 into lb3.
func NodeLabel(instance, server string) string {
	name := strings.TrimSpace(instance)
	if strings.HasPrefix(name, "warp-lb-") {
		return "lb" + strings.TrimPrefix(name, "warp-lb-")
	}
	srv := strings.TrimSpace(server)
	if strings.HasPrefix(srv, "w") {
		n := strings.TrimPrefix(srv, "w")
		if n != "" && isDigits(n) {
			return "lb" + n
		}
	}
	return firstNonEmpty(name, srv)
}

// LooksLikeIP reports whether value is a bare IPv4/IPv6 address, not an error body.
func LooksLikeIP(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, " {}[]\"") {
		return false
	}
	return net.ParseIP(value) != nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}
