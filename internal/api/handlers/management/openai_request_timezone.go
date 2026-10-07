package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// GetOpenAIRequestTimezones returns the allow-listed per-Codex request timezones.
func (h *Handler) GetOpenAIRequestTimezones(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"default":   coreauth.DefaultOpenAIRequestTimezone,
		"timezones": coreauth.OpenAIRequestTimezoneOptions(),
	})
}
