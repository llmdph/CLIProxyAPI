package management

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	thinkProbeModel       = "grok-4.6"
	thinkProbeURL         = "https://cli-chat-proxy.grok.com/v1/responses"
	thinkProbeClientVer   = "0.2.93"
	thinkProbeHTTPTimeout = 25 * time.Second
	thinkProbeDefaultN    = 4
	thinkProbeMaxWorkers  = 8
	thinkProbeMaxNames    = 500
)

type thinkProbeResult struct {
	Name      string `json:"name"`
	Email     string `json:"email,omitempty"`
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
	HasThink  bool   `json:"has_think"`
	Status    int    `json:"http_status,omitempty"`
	Downrank  bool   `json:"downrank"`
}

type thinkProbeState struct {
	mu         sync.Mutex
	running    bool
	cancel     context.CancelFunc
	done       int
	total      int
	marked     int
	restored   int
	failed     int
	skipped    int
	current    string
	results    []thinkProbeResult
	startedAt  time.Time
	finishedAt time.Time
	err        string
}

func (h *Handler) thinkProbeJob() *thinkProbeState {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.thinkProbe == nil {
		h.thinkProbe = &thinkProbeState{}
	}
	return h.thinkProbe
}

func (s *thinkProbeState) snapshot() gin.H {
	s.mu.Lock()
	defer s.mu.Unlock()
	results := append([]thinkProbeResult(nil), s.results...)
	out := gin.H{
		"running":  s.running,
		"done":     s.done,
		"total":    s.total,
		"marked":   s.marked,
		"restored": s.restored,
		"failed":   s.failed,
		"skipped":  s.skipped,
		"current":  s.current,
		"results":  results,
	}
	if !s.startedAt.IsZero() {
		out["started_at"] = s.startedAt
	}
	if !s.finishedAt.IsZero() {
		out["finished_at"] = s.finishedAt
	}
	if s.err != "" {
		out["error"] = s.err
	}
	return out
}

// GetAuthFilesThinkProbe returns the current think-probe job snapshot.
func (h *Handler) GetAuthFilesThinkProbe(c *gin.Context) {
	c.JSON(http.StatusOK, h.thinkProbeJob().snapshot())
}

// PostAuthFilesThinkProbe starts an inspection-style think probe on xAI auth files.
func (h *Handler) PostAuthFilesThinkProbe(c *gin.Context) {
	var req struct {
		Names   []string `json:"names"`
		Workers int      `json:"workers"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	names := uniqueNonEmptyNames(req.Names)
	if len(names) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "names is required"})
		return
	}
	if len(names) > thinkProbeMaxNames {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("too many names (max %d)", thinkProbeMaxNames)})
		return
	}
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	workers := req.Workers
	if workers <= 0 {
		workers = thinkProbeDefaultN
	}
	if workers > thinkProbeMaxWorkers {
		workers = thinkProbeMaxWorkers
	}

	job := h.thinkProbeJob()
	job.mu.Lock()
	if job.running {
		job.mu.Unlock()
		c.JSON(http.StatusConflict, gin.H{"error": "think probe already running"})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	job.running = true
	job.cancel = cancel
	job.done = 0
	job.total = len(names)
	job.marked = 0
	job.restored = 0
	job.failed = 0
	job.skipped = 0
	job.current = ""
	job.results = nil
	job.startedAt = time.Now()
	job.finishedAt = time.Time{}
	job.err = ""
	job.mu.Unlock()

	go h.runThinkProbe(ctx, names, workers)

	c.JSON(http.StatusOK, job.snapshot())
}

func uniqueNonEmptyNames(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (h *Handler) runThinkProbe(ctx context.Context, names []string, workers int) {
	job := h.thinkProbeJob()
	defer func() {
		job.mu.Lock()
		job.running = false
		job.current = ""
		job.finishedAt = time.Now()
		if job.cancel != nil {
			job.cancel()
			job.cancel = nil
		}
		job.mu.Unlock()
	}()

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, name := range names {
		if ctx.Err() != nil {
			break
		}
		name := name
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			h.probeOneAuthFile(ctx, name)
		}()
	}
	wg.Wait()
}

func (h *Handler) probeOneAuthFile(ctx context.Context, name string) {
	job := h.thinkProbeJob()
	job.mu.Lock()
	job.current = name
	job.mu.Unlock()

	result := thinkProbeResult{Name: name, Action: "unchanged"}
	defer func() {
		job.mu.Lock()
		job.done++
		job.results = append(job.results, result)
		switch result.Action {
		case "marked":
			job.marked++
		case "restored":
			job.restored++
		case "skipped":
			job.skipped++
		case "failed":
			job.failed++
		}
		job.mu.Unlock()
	}()

	auth, _ := h.lookupAuthFile(name, "")
	if auth == nil {
		result.Action = "failed"
		result.Reason = "auth file not found"
		return
	}
	result.Email = strings.TrimSpace(authEmail(auth))
	if !isXAIThinkProbeProvider(auth.Provider) {
		result.Action = "skipped"
		result.Reason = "not xai"
		return
	}

	status, body, errProbe := h.doThinkProbeRequest(ctx, auth)
	result.Status = status
	if errProbe != nil {
		result.Action = "failed"
		result.Reason = errProbe.Error()
		return
	}
	if status < 200 || status >= 300 {
		result.Action = "unchanged"
		result.Reason = fmt.Sprintf("HTTP %d", status)
		result.Downrank = coreauth.IsXAIDownrankAuth(auth)
		return
	}

	ev := parseThinkFromResponse(body)
	result.HasThink = ev.ok()
	action := h.authManager.ApplyXAIThinkProbeResult(ctx, auth, ev.ok())
	if latest, _ := h.lookupAuthFile(name, ""); latest != nil {
		result.Downrank = coreauth.IsXAIDownrankAuth(latest)
	} else {
		result.Downrank = !ev.ok()
	}
	result.Action = action
	if ev.ok() {
		result.Reason = "think ok"
	} else if ev.HasThink {
		result.Reason = "think empty"
	} else {
		result.Reason = "no think"
	}
}

func isXAIThinkProbeProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "xai", "x-ai", "grok":
		return true
	default:
		return false
	}
}

func (h *Handler) doThinkProbeRequest(ctx context.Context, auth *coreauth.Auth) (int, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	token, errToken := h.resolveTokenForAuth(ctx, auth, "")
	if errToken != nil {
		return 0, "", errToken
	}
	if strings.TrimSpace(token) == "" {
		return 0, "", fmt.Errorf("auth token not found")
	}
	body := fmt.Sprintf(
		`{"model":%q,"input":"Briefly explain why the sky appears blue in one short sentence.","stream":false,"reasoning":{"effort":"high"}}`,
		thinkProbeModel,
	)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, thinkProbeURL, strings.NewReader(body))
	if errReq != nil {
		return 0, "", errReq
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", thinkProbeClientVer)
	req.Header.Set("User-Agent", "xai-grok-workspace/"+thinkProbeClientVer)

	client := &http.Client{
		Timeout:   thinkProbeHTTPTimeout,
		Transport: h.apiCallTransport(auth, ""),
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return 0, "", errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("think probe body close error: %v", errClose)
		}
	}()
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return resp.StatusCode, "", errRead
	}
	return resp.StatusCode, string(raw), nil
}
