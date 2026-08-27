package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	// ErrorCodeGrokSessionMarked is retained for log compatibility; sessions are no longer marked.
	ErrorCodeGrokSessionMarked = "session_marked"

	// grokSessionNoThinkLimit is how many consecutive HTTP 200 no-thinks the main
	// pool rotates through before returning the last response body to the client.
	grokSessionNoThinkLimit      = 3
	grokSessionMarkedTTL         = 24 * time.Hour
	grokSessionMarkedMessage     = "会话被标记，请更换会话"
	grokSessionNoThinkMaxEntries = 4096
)

type grokSessionNoThinkEntry struct {
	consecutive int
	marked      bool
	expiresAt   time.Time
}

type grokSessionNoThinkTracker struct {
	mu       sync.Mutex
	sessions map[string]grokSessionNoThinkEntry
}

func newGrokSessionNoThinkTracker() *grokSessionNoThinkTracker {
	return &grokSessionNoThinkTracker{sessions: make(map[string]grokSessionNoThinkEntry)}
}

func newGrokSessionMarkedError() *Error {
	return &Error{
		Code:       ErrorCodeGrokSessionMarked,
		Message:    grokSessionMarkedMessage,
		Retryable:  false,
		HTTPStatus: http.StatusConflict,
	}
}

func grokConversationSessionID(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	if sid := sessionHeaderValue(opts.Headers, "x-grok-conv-id"); sid != "" {
		return sid
	}
	if sid := sessionHeaderValue(opts.Headers, "x-grok-session-id"); sid != "" {
		return sid
	}
	if sid := metadataString(opts.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey); sid != "" {
		return sid
	}
	if sid := metadataString(req.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey); sid != "" {
		return sid
	}
	payload := req.Payload
	if len(payload) == 0 {
		payload = opts.OriginalRequest
	}
	if sid := strings.TrimSpace(gjson.GetBytes(payload, "prompt_cache_key").String()); sid != "" {
		return sid
	}
	if sid := ExtractSessionID(opts.Headers, payload, opts.Metadata); sid != "" {
		return sid
	}
	if sid := metadataString(opts.Metadata, cliproxyexecutor.DerivedSessionIDMetadataKey); sid != "" {
		return sid
	}
	return metadataString(req.Metadata, cliproxyexecutor.DerivedSessionIDMetadataKey)
}

func skipGrokSessionNoThinkMark(ctx context.Context, opts cliproxyexecutor.Options) bool {
	if cliproxyexecutor.RequestClassUsesAuxPool(cliproxyexecutor.RequestClassFromContext(ctx)) {
		return true
	}
	return cliproxyexecutor.RequestClassUsesAuxPool(cliproxyexecutor.RequestClassFromMetadata(opts.Metadata))
}

func (m *Manager) errIfGrokSessionMarked(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
	if m != nil && m.grokSessionNoThink != nil {
		if n := m.grokSessionNoThink.clearAll(); n > 0 {
			log.Infof("xai: cleared %d leftover grok session marks", n)
		}
	}
	return nil
}

// handleGrokSessionNoThink counts consecutive HTTP 200 no-thinks on the main
// pool. After grokSessionNoThinkLimit attempts the caller should return the
// last response body. Accounts are still moved to the downrank pool by the
// caller; conversations are not marked.
func (m *Manager) handleGrokSessionNoThink(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, consecutive *int, err error) bool {
	if skipGrokSessionNoThinkMark(ctx, opts) {
		return false
	}
	// AsNoThinkStream is the existing HTTP 200 + response.completed missing-Think signal.
	// Quota, incomplete, disconnects, and other upstream errors must not count.
	if _, ok := cliproxyexecutor.AsNoThinkStream(err); !ok {
		return false
	}
	if consecutive == nil {
		return false
	}
	*consecutive++
	if *consecutive < grokSessionNoThinkLimit {
		return false
	}
	sessionID := grokConversationSessionID(req, opts)
	log.Warnf("xai: returning no-think content after %d consecutive attempts session=%s", *consecutive, truncateSessionID(sessionID))
	return true
}

func (m *Manager) clearGrokSessionNoThink(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) {
	sessionID := grokConversationSessionID(req, opts)
	if sessionID == "" || m == nil || m.grokSessionNoThink == nil {
		return
	}
	m.grokSessionNoThink.clear(sessionID)
}

func (t *grokSessionNoThinkTracker) note(sessionID string) bool {
	return false
}

func (t *grokSessionNoThinkTracker) clearAll() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for id, entry := range t.sessions {
		if entry.marked {
			n++
		}
		delete(t.sessions, id)
	}
	return n
}

func (t *grokSessionNoThinkTracker) marked(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if t == nil || sessionID == "" {
		return false
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.sessions[sessionID]
	if !ok {
		return false
	}
	if !entry.expiresAt.IsZero() && !now.Before(entry.expiresAt) {
		delete(t.sessions, sessionID)
		return false
	}
	return entry.marked
}

func (t *grokSessionNoThinkTracker) clear(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if t == nil || sessionID == "" {
		return
	}
	t.mu.Lock()
	delete(t.sessions, sessionID)
	t.mu.Unlock()
}

func (t *grokSessionNoThinkTracker) purgeExpiredLocked(now time.Time) {
	if t == nil || len(t.sessions) == 0 {
		return
	}
	if len(t.sessions) < grokSessionNoThinkMaxEntries/2 {
		for id, entry := range t.sessions {
			if !entry.expiresAt.IsZero() && !now.Before(entry.expiresAt) {
				delete(t.sessions, id)
			}
		}
		return
	}
	for id, entry := range t.sessions {
		if entry.expiresAt.IsZero() || !now.Before(entry.expiresAt) {
			delete(t.sessions, id)
		}
	}
}
