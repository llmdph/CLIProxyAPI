package auth

import (
	"net/http"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	// ErrorCodeGrokSessionMarked is returned after a conversation hits consecutive no-thinks.
	ErrorCodeGrokSessionMarked = "session_marked"

	grokSessionNoThinkLimit      = 2
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

func (m *Manager) errIfGrokSessionMarked(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
	sessionID := grokConversationSessionID(req, opts)
	if sessionID == "" || m == nil || m.grokSessionNoThink == nil {
		return nil
	}
	if !m.grokSessionNoThink.marked(sessionID) {
		return nil
	}
	log.Warnf("xai: rejecting marked grok session %s", truncateSessionID(sessionID))
	return wrapRequestStopError(newGrokSessionMarkedError())
}

func (m *Manager) handleGrokSessionNoThink(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, consecutive *int, err error) error {
	// AsNoThinkStream is the existing HTTP 200 + response.completed missing-Think signal.
	// Quota, incomplete, disconnects, and other upstream errors must not count.
	if _, ok := cliproxyexecutor.AsNoThinkStream(err); !ok {
		return nil
	}
	if consecutive != nil {
		*consecutive++
	}
	sessionID := grokConversationSessionID(req, opts)
	reached := consecutive != nil && *consecutive >= grokSessionNoThinkLimit
	if sessionID != "" && m != nil && m.grokSessionNoThink != nil && m.grokSessionNoThink.note(sessionID) {
		reached = true
	}
	if !reached {
		return nil
	}
	count := 0
	if consecutive != nil {
		count = *consecutive
	}
	log.Warnf("xai: grok session marked after consecutive no-think session=%s count=%d", truncateSessionID(sessionID), count)
	return wrapRequestStopError(newGrokSessionMarkedError())
}

func (m *Manager) clearGrokSessionNoThink(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) {
	sessionID := grokConversationSessionID(req, opts)
	if sessionID == "" || m == nil || m.grokSessionNoThink == nil {
		return
	}
	m.grokSessionNoThink.clear(sessionID)
}

func (t *grokSessionNoThinkTracker) note(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if t == nil || sessionID == "" {
		return false
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.purgeExpiredLocked(now)
	entry := t.sessions[sessionID]
	if entry.marked && (entry.expiresAt.IsZero() || now.Before(entry.expiresAt)) {
		return true
	}
	if !entry.expiresAt.IsZero() && !now.Before(entry.expiresAt) {
		entry = grokSessionNoThinkEntry{}
	}
	entry.consecutive++
	entry.expiresAt = now.Add(grokSessionMarkedTTL)
	if entry.consecutive >= grokSessionNoThinkLimit {
		entry.marked = true
	}
	t.sessions[sessionID] = entry
	return entry.marked
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
