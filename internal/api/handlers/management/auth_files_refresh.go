package management

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// PostAuthFilesRefresh refreshes OAuth access tokens for one or more auth files.
// Accepts batch {"names":["..."]} or upstream {"all":true,"name":"..."} / query params.
func (h *Handler) PostAuthFilesRefresh(c *gin.Context) {
	h.RefreshAuthFiles(c)
}

// RefreshAuthFiles triggers active refresh for a single auth file, batch names, or all auth files.
// Accepts query parameters (?all=true, ?name=file.json) or JSON body ({"all": true, "name": "file.json", "names": [...]})
func (h *Handler) RefreshAuthFiles(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Name      string   `json:"name"`
		Names     []string `json:"names"`
		AuthIndex string   `json:"auth_index"`
		All       bool     `json:"all"`
	}
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if errBind := c.ShouldBindJSON(&req); errBind != nil && !errors.Is(errBind, io.EOF) {
			c.JSON(clienterror.HTTPStatusFromErrorOr(errBind, http.StatusBadRequest), gin.H{"error": "invalid request body: " + errBind.Error()})
			return
		}
	}
	if c.Query("all") == "true" {
		req.All = true
	}
	if queryName := strings.TrimSpace(c.Query("name")); queryName != "" && req.Name == "" {
		req.Name = queryName
	}
	if queryAuthIndex := strings.TrimSpace(c.Query("auth_index")); queryAuthIndex != "" && req.AuthIndex == "" {
		req.AuthIndex = queryAuthIndex
	}

	ctx := c.Request.Context()

	if len(req.Names) > 0 {
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
		return
	}

	if req.All {
		results := h.authManager.ForceRefreshAll(ctx)
		c.JSON(http.StatusOK, gin.H{
			"ok":      true,
			"results": results,
		})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name or all=true is required"})
		return
	}

	targetAuth, ok := h.lookupAuthFile(name, req.AuthIndex)
	if !ok || targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}

	refreshed, err := h.authManager.ForceRefreshAuth(ctx, targetAuth.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	if c.GetBool(ConfigV8ContextKey) {
		h.mu.Lock()
		host := h.pluginHost
		h.mu.Unlock()
		quota := authFileQuotaContext{v8: true}
		if host != nil {
			quota.pluginIDs = host.QuotaProviderPluginIDs(ctx)
		}
		c.JSON(http.StatusOK, gin.H{
			"ok":   true,
			"auth": h.buildAuthFileEntry(refreshed, quota),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":   true,
		"auth": refreshed,
	})
}
