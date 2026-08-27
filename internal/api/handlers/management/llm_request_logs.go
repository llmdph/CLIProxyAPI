package management

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/llmreqlog"
)

// GetLLMRequestLogs returns recent LLM request log rows for the standalone page.
func (h *Handler) GetLLMRequestLogs(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	class := c.Query("class")
	account := strings.TrimSpace(c.Query("account"))
	if account == "" {
		account = strings.TrimSpace(c.Query("email"))
	}
	items, total := llmreqlog.List(limit, offset, class, account)
	c.JSON(http.StatusOK, gin.H{
		"items":  items,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// ClearLLMRequestLogs removes all in-memory LLM request log rows.
func (h *Handler) ClearLLMRequestLogs(c *gin.Context) {
	cleared := llmreqlog.Clear()
	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"cleared": cleared,
	})
}
