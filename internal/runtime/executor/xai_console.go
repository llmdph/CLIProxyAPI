package executor

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	xaiConsoleBaseURL      = "https://console.x.ai"
	xaiConsoleUserAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	xaiConsoleCluster      = "https://us-east-1.api.x.ai"
	xaiConsoleDPoPMaxLife  = time.Hour
	xaiConsoleDPoPSkewWait = 20 * time.Second
)

type xaiConsoleDPoPSession struct {
	accessToken string
	privateKey  *ecdsa.PrivateKey
	publicJWK   map[string]string
	expiresAt   time.Time
	clockSkew   time.Duration
}

type xaiConsoleDPoPJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

var xaiConsoleDPoPCache sync.Map // key -> *xaiConsoleDPoPSession

func xaiConsoleEndpoint(path string) string {
	path = "/" + strings.TrimLeft(strings.TrimSpace(path), "/")
	return strings.TrimRight(xaiConsoleBaseURL, "/") + "/v1" + path
}

func (e *XAIExecutor) applyXAIConsoleRequestBody(auth *cliproxyauth.Auth, prepared *xaiPreparedRequest) {
	if prepared == nil || !cliproxyauth.XAIUsingConsoleChannel(auth) {
		return
	}
	// Official docs: encrypted thinking is returned only when requested via
	// include=["reasoning.encrypted_content"] or use_encrypted_content=true.
	// Do not opt into ciphertext. Do not force reasoning.summary=detailed:
	// Console still will not return readable think text, and detailed delays TTFT.
	prepared.body = stripXAIConsoleEncryptedThink(prepared.body)
}

func stripXAIConsoleEncryptedThink(body []byte) []byte {
	body, _ = sjson.DeleteBytes(body, "use_encrypted_content")
	include := gjson.GetBytes(body, "include")
	if !include.Exists() {
		return body
	}
	if !include.IsArray() {
		if include.String() == "reasoning.encrypted_content" {
			updated, err := sjson.DeleteBytes(body, "include")
			if err == nil {
				return updated
			}
		}
		return body
	}
	kept := make([]any, 0, len(include.Array()))
	for _, item := range include.Array() {
		if item.String() == "reasoning.encrypted_content" {
			continue
		}
		kept = append(kept, item.Value())
	}
	if len(kept) == 0 {
		updated, err := sjson.DeleteBytes(body, "include")
		if err != nil {
			return body
		}
		return updated
	}
	updated, err := sjson.SetBytes(body, "include", kept)
	if err != nil {
		return body
	}
	return updated
}

func ensureXAIConsoleReasoningSummary(body []byte) []byte {
	return body
}

func xaiConsoleCacheKey(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	sso := cliproxyauth.XAIConsoleSSO(auth)
	sum := sha256.Sum256([]byte(sso))
	return strings.TrimSpace(auth.ID) + "|" + base64.RawURLEncoding.EncodeToString(sum[:8])
}

func (e *XAIExecutor) doXAIChatHTTP(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request, client *http.Client) (*http.Response, error) {
	if client == nil {
		return nil, fmt.Errorf("xai console: missing http client")
	}
	if cliproxyauth.XAIUsingConsoleChannel(auth) {
		if err := e.prepareConsoleChatRequest(ctx, auth, req, client); err != nil {
			return nil, err
		}
	}
	return client.Do(req)
}

func (e *XAIExecutor) prepareConsoleChatRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request, client *http.Client) error {
	if req == nil || req.URL == nil {
		return fmt.Errorf("xai console: missing request")
	}
	sso := cliproxyauth.XAIConsoleSSO(auth)
	if sso == "" {
		return fmt.Errorf("xai console: missing sso")
	}
	req.URL.Scheme = "https"
	req.URL.Host = "console.x.ai"
	if !strings.HasPrefix(req.URL.Path, "/v1/") {
		req.URL.Path = "/v1/responses"
	}
	req.Host = "console.x.ai"

	session, err := e.getConsoleDPoPSession(ctx, auth, sso, client)
	if err != nil {
		return err
	}
	applyXAIConsoleBrowserHeaders(req, auth, sso)
	if strings.HasSuffix(req.URL.Path, "/responses") {
		req.Header.Set("x-cluster", xaiConsoleCluster)
	}
	return applyXAIConsoleDPoP(req, session)
}

func applyXAIConsoleBrowserHeaders(req *http.Request, auth *cliproxyauth.Auth, sso string) {
	ua := xaiConsoleUserAgent
	if auth != nil && auth.Metadata != nil {
		if raw, ok := auth.Metadata["headers"].(map[string]any); ok {
			if v, ok := raw["User-Agent"].(string); ok && strings.TrimSpace(v) != "" && !strings.Contains(strings.ToLower(v), "grok-shell") {
				ua = strings.TrimSpace(v)
			}
		}
	}
	cf := ""
	if auth != nil {
		cf = strings.TrimSpace(xaiMetadataString(auth.Metadata, "cf_clearance"))
	}
	cookie := "sso=" + sso
	if cf != "" && cf != "<nil>" {
		cookie += "; cf_clearance=" + cf
	}
	req.Header.Del("Authorization")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", "https://console.x.ai")
	req.Header.Set("Referer", "https://console.x.ai/")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("User-Agent", ua)
}

func (e *XAIExecutor) getConsoleDPoPSession(ctx context.Context, auth *cliproxyauth.Auth, sso string, client *http.Client) (*xaiConsoleDPoPSession, error) {
	key := xaiConsoleCacheKey(auth)
	if key != "" {
		if raw, ok := xaiConsoleDPoPCache.Load(key); ok {
			if session, _ := raw.(*xaiConsoleDPoPSession); session != nil && time.Now().Add(xaiConsoleDPoPSkewWait).Before(session.expiresAt) {
				return session, nil
			}
			xaiConsoleDPoPCache.Delete(key)
		}
	}
	session, err := mintXAIConsoleDPoP(ctx, sso, client)
	if err != nil {
		return nil, err
	}
	if key != "" {
		xaiConsoleDPoPCache.Store(key, session)
	}
	return session, nil
}

func mintXAIConsoleDPoP(ctx context.Context, sso string, client *http.Client) (*xaiConsoleDPoPSession, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("xai console dpop key: %w", err)
	}
	jwk := xaiConsoleDPoPJWK{
		Kty: "EC",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(privateKey.X.FillBytes(make([]byte, 32))),
		Y:   base64.RawURLEncoding.EncodeToString(privateKey.Y.FillBytes(make([]byte, 32))),
	}
	payload, err := json.Marshal(map[string]any{"jwk": jwk})
	if err != nil {
		return nil, err
	}
	endpoint := xaiConsoleEndpoint("/dpop/token")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	applyXAIConsoleBrowserHeaders(req, nil, sso)
	req.Header.Set("Content-Type", "application/json")
	localBefore := time.Now().UTC()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	localAfter := time.Now().UTC()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("xai console dpop token: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tokenResponse struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResponse); err != nil {
		return nil, fmt.Errorf("xai console dpop parse: %w", err)
	}
	if strings.TrimSpace(tokenResponse.AccessToken) == "" || !strings.EqualFold(strings.TrimSpace(tokenResponse.TokenType), "DPoP") {
		return nil, fmt.Errorf("xai console dpop token invalid")
	}
	if tokenResponse.ExpiresIn <= 0 || time.Duration(tokenResponse.ExpiresIn)*time.Second > xaiConsoleDPoPMaxLife {
		return nil, fmt.Errorf("xai console dpop ttl invalid")
	}
	skew := time.Duration(0)
	if dateHeader := strings.TrimSpace(resp.Header.Get("Date")); dateHeader != "" {
		if serverTime, errParse := http.ParseTime(dateHeader); errParse == nil {
			mid := localBefore.Add(localAfter.Sub(localBefore) / 2)
			skew = serverTime.UTC().Sub(mid.UTC()).Round(time.Second)
		}
	}
	now := time.Now().UTC()
	return &xaiConsoleDPoPSession{
		accessToken: tokenResponse.AccessToken,
		privateKey:  privateKey,
		publicJWK:   map[string]string{"kty": jwk.Kty, "crv": jwk.Crv, "x": jwk.X, "y": jwk.Y},
		expiresAt:   now.Add(time.Duration(tokenResponse.ExpiresIn) * time.Second),
		clockSkew:   skew,
	}, nil
}

func applyXAIConsoleDPoP(req *http.Request, session *xaiConsoleDPoPSession) error {
	if req == nil || req.URL == nil || session == nil || session.privateKey == nil {
		return fmt.Errorf("xai console dpop: missing session")
	}
	digest := sha256.Sum256([]byte(session.accessToken))
	htu := req.URL.Scheme + "://" + req.URL.Host + req.URL.EscapedPath()
	if req.URL.EscapedPath() == "" {
		htu = req.URL.Scheme + "://" + req.URL.Host + "/"
	}
	header := map[string]any{
		"typ": "dpop+jwt",
		"alg": "ES256",
		"jwk": session.publicJWK,
	}
	claims := map[string]any{
		"jti": uuid.NewString(),
		"htm": strings.ToUpper(req.Method),
		"htu": htu,
		"iat": time.Now().UTC().Add(session.clockSkew).Unix(),
		"ath": base64.RawURLEncoding.EncodeToString(digest[:]),
	}
	proof, err := signXAIConsoleES256(session.privateKey, header, claims)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "DPoP "+session.accessToken)
	req.Header.Set("DPoP", proof)
	return nil
}

func signXAIConsoleES256(key *ecdsa.PrivateKey, header, claims map[string]any) (string, error) {
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", err
	}
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

