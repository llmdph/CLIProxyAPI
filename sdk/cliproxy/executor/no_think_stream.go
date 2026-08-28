package executor

import (
	"errors"
	"fmt"
	"net/http"
)

// NoThinkStreamError is returned by xAI executors when a request that
// expected a Think stream got none (or 有(0字)). This includes HTTP 200
// response.completed and fail-fast cases where visible answer tokens start
// without Think. Quota, incomplete, disconnects, and other upstream errors
// must not use this type. Auth managers should disable the credential,
// retry with another account (up to a small cap), and after the cap return
	// FallbackResponse / FallbackStreamChunks to the client. Conversations are
	// not marked; the no-think credential is moved to the downrank pool.
type NoThinkStreamError struct {
	AuthID       string
	Detail       string
	HasThink     bool
	ThinkingLen  int
	Fallback     Response
	StreamHeader http.Header
	StreamChunks [][]byte
}

func (e *NoThinkStreamError) Error() string {
	if e == nil {
		return "no_think_stream"
	}
	if e.Detail != "" {
		return "no_think_stream: " + e.Detail
	}
	return fmt.Sprintf("no_think_stream: has=%v len=%d", e.HasThink, e.ThinkingLen)
}

// AsNoThinkStream extracts a NoThinkStreamError from err.
func AsNoThinkStream(err error) (*NoThinkStreamError, bool) {
	var noThink *NoThinkStreamError
	if errors.As(err, &noThink) && noThink != nil {
		return noThink, true
	}
	return nil, false
}
