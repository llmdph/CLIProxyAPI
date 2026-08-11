package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// PostAuthFilesRefresh refreshes OAuth access tokens for one or more auth files.
// Body: {"name":"..."} or {"names":["..."]} and optional "auth_index".
func (h *Handler) PostAuthFilesRefresh(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Name      string   `json:"name"`
		Names     []string `json:"names"`
		AuthIndex string   `json:"auth_index"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	names := make([]string, 0, len(req.Names)+1)
	for _, n := range req.Names {
		if trimmed := strings.TrimSpace(n); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	if single := strings.TrimSpace(req.Name); single != "" {
		names = append(names, single)
	}
	if len(names) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name or names is required"})
		return
	}

	authIndex := strings.TrimSpace(req.AuthIndex)
	ctx := c.Request.Context()
	type itemResult struct {
		Name   string `json:"name"`
		AuthID string `json:"auth_id,omitempty"`
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	results := make([]itemResult, 0, len(names))
	okCount := 0

	for _, name := range names {
		targetAuth, found := h.lookupAuthFile(name, authIndex)
		if !found || targetAuth == nil {
			results = append(results, itemResult{Name: name, Status: "error", Error: "auth file not found"})
			continue
		}
		if coreauth.IsConfigAPIKeyAuth(targetAuth) {
			results = append(results, itemResult{
				Name: name, AuthID: targetAuth.ID, Status: "error",
				Error: "config api key does not support token refresh",
			})
			continue
		}
		if coreauth.IsPluginVirtualAuth(targetAuth) && !isPluginVirtualSourceDelete(name, targetAuth) {
			results = append(results, itemResult{
				Name: name, AuthID: targetAuth.ID, Status: "error",
				Error: "plugin virtual auth cannot be refreshed independently",
			})
			continue
		}

		refreshed, errRefresh := h.authManager.RefreshAuth(ctx, targetAuth.ID)
		if errRefresh != nil {
			results = append(results, itemResult{
				Name: name, AuthID: targetAuth.ID, Status: "error", Error: errRefresh.Error(),
			})
			continue
		}
		authID := targetAuth.ID
		if refreshed != nil && strings.TrimSpace(refreshed.ID) != "" {
			authID = refreshed.ID
		}
		results = append(results, itemResult{Name: name, AuthID: authID, Status: "ok"})
		okCount++
	}

	status := http.StatusOK
	if okCount == 0 {
		status = http.StatusBadRequest
		if len(names) == 1 {
			status = http.StatusNotFound
			if results[0].Error != "auth file not found" {
				status = http.StatusBadRequest
			}
		}
	} else if okCount < len(names) {
		status = http.StatusMultiStatus
	}

	c.JSON(status, gin.H{
		"status":  "ok",
		"ok":      okCount,
		"failed":  len(names) - okCount,
		"total":   len(names),
		"results": results,
	})
}
