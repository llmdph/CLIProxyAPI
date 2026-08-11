package executor

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// xaiClientIdentity is a synthetic Grok CLI machine profile. Official chat-proxy
// traffic is gated on CLI identity headers; agent-id is the closest stand-in for
// a persistent machine fingerprint.
//
// Device-level fields stick per auth account. Request-scoped ids (req-id /
// traceparent) still rotate every HTTP call. A new device profile is minted only
// when a different auth id is used (account rotation after quota / no-think).
type xaiClientIdentity struct {
	AgentID        string
	RequestID      string
	DeviceID       string
	HardwareUUID   string
	MACAddress     string
	ClientVersion  string
	UserAgent      string
	OS             string
	Arch           string
	AcceptLanguage string
	AcceptEncoding string
	TraceParent    string
	TraceState     string
}

type xaiUAPlatform struct {
	os   string
	arch string
}

var xaiUAPlatforms = []xaiUAPlatform{
	{os: "linux", arch: "x86_64"},
	{os: "linux", arch: "aarch64"},
	{os: "darwin", arch: "arm64"},
	{os: "darwin", arch: "x86_64"},
	{os: "windows", arch: "x86_64"},
	{os: "windows", arch: "arm64"},
}

// Common desktop CLI locales. Weighted toward en-* to look natural.
var xaiAcceptLanguages = []string{
	"en-US,en;q=0.9",
	"en-GB,en;q=0.9",
	"en-US,en;q=0.9,zh-CN;q=0.8",
	"zh-CN,zh;q=0.9,en;q=0.8",
	"zh-TW,zh;q=0.9,en;q=0.8",
	"ja-JP,ja;q=0.9,en-US;q=0.8,en;q=0.7",
	"ko-KR,ko;q=0.9,en-US;q=0.8,en;q=0.7",
	"de-DE,de;q=0.9,en;q=0.8",
	"fr-FR,fr;q=0.9,en;q=0.8",
	"es-ES,es;q=0.9,en;q=0.8",
	"pt-BR,pt;q=0.9,en;q=0.8",
}

var xaiAcceptEncodings = []string{
	"gzip",
	"gzip, deflate",
	"gzip, deflate, br",
}

var xaiIdentityByAuth sync.Map // map[string]xaiClientIdentity

func newXAIClientIdentity() xaiClientIdentity {
	platform := pickXAIUAPlatform()
	version := xaiClientVersionValue
	traceID := randomHex(16)
	spanID := randomHex(8)
	return xaiClientIdentity{
		AgentID:        uuid.NewString(),
		RequestID:      uuid.NewString(),
		DeviceID:       randomHex(32),
		HardwareUUID:   uuid.NewString(),
		MACAddress:     randomMACAddress(),
		ClientVersion:  version,
		OS:             platform.os,
		Arch:           platform.arch,
		UserAgent:      fmt.Sprintf("grok-shell/%s (%s; %s)", version, platform.os, platform.arch),
		AcceptLanguage: pickXAIString(xaiAcceptLanguages),
		AcceptEncoding: pickXAIString(xaiAcceptEncodings),
		// W3C traceparent: version-traceid-spanid-flags
		TraceParent: "00-" + traceID + "-" + spanID + "-01",
		TraceState:  "",
	}
}

// xaiIdentityForAuth returns the sticky device profile for an auth account.
// Empty authID falls back to a one-off identity (no cache).
func xaiIdentityForAuth(authID string) xaiClientIdentity {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return newXAIClientIdentity()
	}
	if cached, ok := xaiIdentityByAuth.Load(authID); ok {
		if identity, okIdentity := cached.(xaiClientIdentity); okIdentity {
			return refreshXAIRequestScopedIdentity(identity)
		}
	}
	identity := newXAIClientIdentity()
	actual, _ := xaiIdentityByAuth.LoadOrStore(authID, identity)
	if stored, ok := actual.(xaiClientIdentity); ok {
		return refreshXAIRequestScopedIdentity(stored)
	}
	return refreshXAIRequestScopedIdentity(identity)
}

// InvalidateXAIClientIdentity drops the cached device profile for an auth.
// Next use of that account mints a fresh fingerprint.
func InvalidateXAIClientIdentity(authID string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	xaiIdentityByAuth.Delete(authID)
}

func refreshXAIRequestScopedIdentity(identity xaiClientIdentity) xaiClientIdentity {
	identity.RequestID = uuid.NewString()
	traceID := randomHex(16)
	spanID := randomHex(8)
	identity.TraceParent = "00-" + traceID + "-" + spanID + "-01"
	identity.TraceState = ""
	return identity
}

func pickXAIUAPlatform() xaiUAPlatform {
	if len(xaiUAPlatforms) == 0 {
		return xaiUAPlatform{os: "linux", arch: "x86_64"}
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(xaiUAPlatforms))))
	if err != nil {
		return xaiUAPlatforms[0]
	}
	return xaiUAPlatforms[int(n.Int64())]
}

func pickXAIString(options []string) string {
	if len(options) == 0 {
		return ""
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(options))))
	if err != nil {
		return options[0]
	}
	return options[int(n.Int64())]
}

func randomHex(byteLen int) string {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	return hex.EncodeToString(buf)
}

func randomMACAddress() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "02:00:00:00:00:01"
	}
	// Locally administered unicast MAC (bit0=0, bit1=1 on first octet).
	buf[0] = (buf[0] | 0x02) & 0xFE
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", buf[0], buf[1], buf[2], buf[3], buf[4], buf[5])
}

// applyXAIFreshClientIdentity attaches device fingerprints for a CLI chat-proxy call.
// It never fabricates x-grok-conv-id / session-id; those stay session-stable.
func applyXAIFreshClientIdentity(r *http.Request, identity xaiClientIdentity, includeCLIHeaders bool) {
	if r == nil {
		return
	}
	r.Header.Set("Connection", "close")
	if identity.AgentID != "" {
		r.Header.Set("x-grok-agent-id", identity.AgentID)
	}
	if identity.RequestID != "" {
		r.Header.Set("x-grok-req-id", identity.RequestID)
	}
	if identity.DeviceID != "" {
		r.Header.Set("x-grok-device-id", identity.DeviceID)
		r.Header.Set("X-Device-Id", identity.DeviceID)
	}
	if identity.HardwareUUID != "" {
		r.Header.Set("x-hardware-uuid", identity.HardwareUUID)
	}
	if identity.MACAddress != "" {
		r.Header.Set("x-device-mac", identity.MACAddress)
		r.Header.Set("X-Client-Device-Mac", identity.MACAddress)
	}
	if identity.AcceptLanguage != "" {
		r.Header.Set("Accept-Language", identity.AcceptLanguage)
	}
	if identity.TraceParent != "" {
		r.Header.Set("traceparent", identity.TraceParent)
	}
	if identity.TraceState != "" {
		r.Header.Set("tracestate", identity.TraceState)
	}
	if includeCLIHeaders {
		r.Header.Set(xaiTokenAuthHeader, xaiTokenAuthValue)
		r.Header.Set(xaiClientVersionHeader, identity.ClientVersion)
		r.Header.Set("x-grok-client-identifier", "grok-shell")
		r.Header.Set("x-grok-client-mode", "headless")
		r.Header.Set("x-authenticateresponse", "authenticate-response")
		r.Header.Set("User-Agent", identity.UserAgent)
		if identity.AcceptEncoding != "" {
			r.Header.Set("Accept-Encoding", identity.AcceptEncoding)
		} else {
			r.Header.Set("Accept-Encoding", "gzip")
		}
	}
}
