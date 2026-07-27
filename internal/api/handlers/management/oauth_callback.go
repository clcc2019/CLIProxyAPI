package management

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	log "github.com/sirupsen/logrus"
)

type oauthCallbackRequest struct {
	Provider    string `json:"provider"`
	RedirectURL string `json:"redirect_url"`
	Code        string `json:"code"`
	State       string `json:"state"`
	Error       string `json:"error"`
}

func (h *Handler) PostOAuthCallback(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "handler not initialized"})
		return
	}
	authDir := h.authDirSnapshot()
	if authDir == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "auth directory is not configured"})
		return
	}

	var req oauthCallbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid body"})
		return
	}

	canonicalProvider, err := NormalizeOAuthProvider(req.Provider)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "unsupported provider"})
		return
	}

	state := strings.TrimSpace(req.State)
	code := strings.TrimSpace(req.Code)
	errMsg := strings.TrimSpace(req.Error)

	if rawRedirect := strings.TrimSpace(req.RedirectURL); rawRedirect != "" {
		callback, errParse := parseOAuthCallbackRedirect(rawRedirect)
		if errParse != nil {
			// Explicit callback fields are authoritative, so an optional
			// redirect_url without OAuth parameters must not discard them.
			if state == "" || (code == "" && errMsg == "") {
				c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid redirect_url"})
				return
			}
		} else {
			if state == "" {
				state = callback.State
			}
			if code == "" {
				code = callback.Code
			}
			if errMsg == "" {
				errMsg = callback.Error
				if errMsg == "" {
					errMsg = callback.ErrorDescription
				}
			}
		}
	}

	if state == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "state is required"})
		return
	}
	if err := ValidateOAuthState(state); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid state"})
		return
	}
	if code == "" && errMsg == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "code or error is required"})
		return
	}

	sessionProvider, sessionStatus, completed, ok := GetOAuthSessionDetails(state)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "unknown or expired state"})
		return
	}
	if completed {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "error": "oauth flow is already completed"})
		return
	}
	if sessionStatus != "" {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "error": sessionStatus})
		return
	}
	if !strings.EqualFold(sessionProvider, canonicalProvider) {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "provider does not match state"})
		return
	}

	if _, errWrite := WriteOAuthCallbackFileForPendingSession(authDir, canonicalProvider, state, code, errMsg); errWrite != nil {
		if errors.Is(errWrite, errOAuthSessionNotPending) {
			_, status, okSession := GetOAuthSession(state)
			if okSession && status != "" {
				c.JSON(http.StatusConflict, gin.H{"status": "error", "error": status})
				return
			}
			c.JSON(http.StatusConflict, gin.H{"status": "error", "error": "oauth flow is not pending"})
			return
		}
		log.WithError(errWrite).Error("failed to persist oauth callback")
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to persist oauth callback"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// parseOAuthCallbackRedirect accepts the same callback forms as the CLI
// (including a host:port URL pasted without a scheme). A state-only callback
// is also useful when code/error was supplied in a separate request field.
func parseOAuthCallbackRedirect(input string) (*misc.OAuthCallback, error) {
	callback, callbackErr := misc.ParseOAuthCallback(input)
	if callbackErr == nil {
		return callback, nil
	}

	candidate := strings.TrimSpace(input)
	if !strings.Contains(candidate, "://") {
		if strings.HasPrefix(candidate, "?") {
			candidate = "http://localhost" + candidate
		} else if strings.ContainsAny(candidate, "/?#") || strings.Contains(candidate, ":") {
			candidate = "http://" + candidate
		} else if strings.Contains(candidate, "=") {
			candidate = "http://localhost/?" + candidate
		}
	}
	parsedURL, errParse := url.Parse(candidate)
	if errParse != nil {
		return nil, callbackErr
	}

	state := strings.TrimSpace(parsedURL.Query().Get("state"))
	if state == "" && parsedURL.Fragment != "" {
		if fragment, errFragment := url.ParseQuery(parsedURL.Fragment); errFragment == nil {
			state = strings.TrimSpace(fragment.Get("state"))
		}
	}
	if state == "" {
		return nil, callbackErr
	}
	return &misc.OAuthCallback{State: state}, nil
}
