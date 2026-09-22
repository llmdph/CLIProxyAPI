package warprotate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var dedicatedBound sync.Map // accountID -> instance
var bindCache sync.Map      // accountID -> *BindResult

// BindResult is a dedicated-or-backup assignment for one account.
type BindResult struct {
	OK       bool   `json:"ok"`
	Account  string `json:"account"`
	Pool     string `json:"pool"`
	Instance string `json:"instance"`
	Label    string `json:"label"`
	ProxyURL string `json:"proxy_url"`
	ExitIP   string `json:"exit_ip"`
	Error    string `json:"error,omitempty"`
}

// AccountBinder assigns one dedicated Warp node per account, or the backup pool.
type AccountBinder struct{}

// BindAccount implements auth.AccountProxyBinder.
func (AccountBinder) BindAccount(ctx context.Context, accountID string) string {
	result := Bind(ctx, accountID)
	if result == nil {
		return ""
	}
	return strings.TrimSpace(result.ProxyURL)
}

// UnbindAccount implements auth.AccountProxyBinder.
func (AccountBinder) UnbindAccount(ctx context.Context, accountID, reason string) {
	Unbind(ctx, accountID, reason)
}

// ListBoundAccounts implements auth.AccountProxyBinder.
func (AccountBinder) ListBoundAccounts(ctx context.Context) []string {
	bindings := ListBindings(ctx)
	out := make([]string, 0, len(bindings))
	for _, row := range bindings {
		if id := strings.TrimSpace(row.Account); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// Bind claims a dedicated LB for the account, or returns the backup proxy.
func Bind(ctx context.Context, accountID string) *BindResult {
	accountID = strings.TrimSpace(accountID)
	base := BaseURL()
	if base == "" || accountID == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]string{"account": accountID})
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, base+"/v1/bind", bytes.NewReader(payload))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var out BindResult
	if err := json.Unmarshal(body, &out); err != nil || !out.OK {
		return nil
	}
	if strings.TrimSpace(out.Label) == "" {
		out.Label = NodeLabel(out.Instance, "")
		if out.Label == "" && strings.EqualFold(out.Pool, "backup") {
			out.Label = "backup"
		}
	}
	copied := out
	if strings.EqualFold(out.Pool, "dedicated") && out.Instance != "" {
		dedicatedBound.Store(accountID, out.Instance)
		bindCache.Store(accountID, &copied)
	} else {
		dedicatedBound.Delete(accountID)
		bindCache.Delete(accountID)
	}
	return &copied
}

type bindingsResponse struct {
	OK       bool         `json:"ok"`
	Bindings []BindResult `json:"bindings"`
}

// ListBindings returns dedicated account binds currently held by the agent.
func ListBindings(ctx context.Context) []BindResult {
	base := BaseURL()
	if base == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/v1/bindings", nil)
	if err != nil {
		return nil
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil
	}
	var out bindingsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil
	}
	return out.Bindings
}

// Unbind releases a dedicated LB and restarts it.
func Unbind(ctx context.Context, accountID, reason string) {
	accountID = strings.TrimSpace(accountID)
	base := BaseURL()
	if base == "" || accountID == "" {
		return
	}
	dedicatedBound.Delete(accountID)
	bindCache.Delete(accountID)
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]string{"account": accountID, "reason": strings.TrimSpace(reason)})
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, base+"/v1/unbind", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
}

// HasDedicatedBind reports whether this account currently owns a dedicated node.
func HasDedicatedBind(accountID string) bool {
	_, ok := dedicatedBound.Load(strings.TrimSpace(accountID))
	return ok
}

// LookupAccount returns the dedicated bind for an account if present.
func LookupAccount(ctx context.Context, accountID string) *LookupResult {
	accountID = strings.TrimSpace(accountID)
	base := BaseURL()
	if base == "" || accountID == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	endpoint := base + "/v1/lookup?account=" + url.QueryEscape(accountID)
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
	if strings.TrimSpace(out.Label) == "" {
		out.Label = NodeLabel(out.Instance, "")
	}
	if !LooksLikeIP(out.ExitIP) {
		out.ExitIP = ""
	}
	if out.Instance == "" && out.Label == "" {
		return nil
	}
	return &out
}

// NodeLabelFromHost maps warp-lb-3:1080 / warp-lb:1080 to lb3 / backup.
func NodeLabelFromHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	switch host {
	case "", "direct":
		return ""
	case "warp-lb", "warp-lb-backup":
		return "backup"
	}
	if strings.HasPrefix(host, "warp-lb-") {
		return NodeLabel(host, "")
	}
	return ""
}
