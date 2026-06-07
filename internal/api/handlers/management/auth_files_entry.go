package management

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func (h *Handler) buildAuthFileEntry(auth *coreauth.Auth) gin.H {
	return h.buildAuthFileEntryWithOptions(auth, authFileEntryBuildOptions{})
}

func (h *Handler) buildAuthFileEntryWithOptions(auth *coreauth.Auth, opts authFileEntryBuildOptions) gin.H {
	if auth == nil {
		return nil
	}
	auth.EnsureIndex()
	runtimeOnly := isRuntimeOnlyAuth(auth)
	if runtimeOnly && (auth.Disabled || auth.Status == coreauth.StatusDisabled) {
		return nil
	}
	path := strings.TrimSpace(authAttribute(auth, "path"))
	if path == "" && !runtimeOnly {
		return nil
	}
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		name = auth.ID
	}
	entry := gin.H{
		"id":             auth.ID,
		"auth_index":     auth.Index,
		"name":           name,
		"type":           strings.TrimSpace(auth.Provider),
		"provider":       strings.TrimSpace(auth.Provider),
		"label":          auth.Label,
		"status":         auth.Status,
		"status_message": auth.StatusMessage,
		"disabled":       auth.Disabled,
		"unavailable":    auth.Unavailable,
		"runtime_only":   runtimeOnly,
		"source":         "memory",
		"size":           int64(0),
	}
	if !opts.Summary {
		if serialized := serializeAuthError(auth.LastError); serialized != nil {
			entry["last_error"] = serialized
		}
		if serialized := serializeModelStates(auth.ModelStates); len(serialized) > 0 {
			entry["model_states"] = serialized
		}
	}
	entry["success"] = auth.Success
	entry["failed"] = auth.Failed
	entry["recent_requests"] = auth.RecentRequestsSnapshot(time.Now())
	if email := authEmail(auth); email != "" {
		entry["email"] = email
	}
	if projectID := authProjectID(auth); projectID != "" {
		entry["project_id"] = projectID
	}
	if authFileHasRefreshToken(auth) {
		entry["has_refresh_token"] = true
	}
	if opts.Summary {
		applyCodexSubscriptionSnapshotSummary(entry, auth)
	} else {
		applyCodexSubscriptionSnapshot(entry, auth)
	}
	if accountType, account := auth.AccountInfo(); accountType != "" || account != "" {
		if accountType != "" {
			entry["account_type"] = accountType
		}
		if account != "" {
			entry["account"] = account
		}
	}
	if !auth.CreatedAt.IsZero() {
		entry["created_at"] = auth.CreatedAt
	}
	if !auth.UpdatedAt.IsZero() {
		entry["modtime"] = auth.UpdatedAt
		entry["updated_at"] = auth.UpdatedAt
	}
	if lastRefresh, ok := authFileLastRefresh(auth); ok {
		entry["last_refresh"] = lastRefresh
	}
	if runtimeUpdatedAt, updatedOK, runtimeSavedAt, savedOK := authFileRuntimeStateTimes(auth.Metadata); updatedOK || savedOK {
		if updatedOK {
			entry["runtime_updated_at"] = runtimeUpdatedAt
		}
		if savedOK {
			entry["runtime_saved_at"] = runtimeSavedAt
		}
	}
	if !auth.NextRetryAfter.IsZero() {
		entry["next_retry_after"] = auth.NextRetryAfter
	}
	if path != "" {
		entry["path"] = path
		entry["source"] = "file"
		if !opts.Summary {
			if info, err := statAuthFileEntryPath(path, opts); err == nil {
				entry["size"] = info.Size()
				entry["modtime"] = info.ModTime()
			} else if os.IsNotExist(err) {
				// Hide file-backed credentials removed from disk but still lingering in memory.
				removedByManagement := auth.Disabled || auth.Status == coreauth.StatusDisabled || strings.EqualFold(strings.TrimSpace(auth.StatusMessage), "removed via management api")
				if !runtimeOnly && (h.isManagedAuthFilePath(path) || removedByManagement) {
					return nil
				}
				entry["source"] = "memory"
			} else {
				log.WithError(err).Warnf("failed to stat auth file %s", path)
			}
		}
	}
	if !opts.Summary {
		if claims := extractCodexIDTokenClaims(auth); claims != nil {
			entry["id_token"] = claims
			applyCodexSubscriptionFromClaims(entry, claims)
		}
	}
	if prefix := strings.TrimSpace(auth.Prefix); prefix != "" {
		entry["prefix"] = prefix
	} else if auth.Metadata != nil {
		if rawPrefix, ok := auth.Metadata["prefix"].(string); ok {
			if trimmed := strings.TrimSpace(rawPrefix); trimmed != "" {
				entry["prefix"] = trimmed
			}
		}
	}
	fileProxyURL := authFileMetadataProxyURL(auth)
	runtimeProxyURL := strings.TrimSpace(auth.ProxyURL)
	if authFileProxyPoolAssigned(auth) {
		entry["proxy_pool_assigned"] = true
		if runtimeProxyURL != "" {
			entry["runtime_proxy_url"] = runtimeProxyURL
		}
		if fileProxyURL != "" {
			entry["proxy_url"] = fileProxyURL
		}
	} else if runtimeProxyURL != "" {
		entry["proxy_url"] = runtimeProxyURL
	} else if fileProxyURL != "" {
		entry["proxy_url"] = fileProxyURL
	}
	// Expose priority from Attributes (set by synthesizer from JSON "priority" field).
	// Fall back to Metadata for auths registered via UploadAuthFile (no synthesizer).
	if p := strings.TrimSpace(authAttribute(auth, "priority")); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			entry["priority"] = parsed
		}
	} else if auth.Metadata != nil {
		if rawPriority, ok := auth.Metadata["priority"]; ok {
			switch v := rawPriority.(type) {
			case float64:
				entry["priority"] = int(v)
			case int:
				entry["priority"] = v
			case string:
				if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					entry["priority"] = parsed
				}
			}
		}
	}
	// Expose note from Attributes (set by synthesizer from JSON "note" field).
	// Fall back to Metadata for auths registered via UploadAuthFile (no synthesizer).
	if note := strings.TrimSpace(authAttribute(auth, "note")); note != "" {
		entry["note"] = note
	} else if auth.Metadata != nil {
		if rawNote, ok := auth.Metadata["note"].(string); ok {
			if trimmed := strings.TrimSpace(rawNote); trimmed != "" {
				entry["note"] = trimmed
			}
		}
	}
	if userAgent := authFileUserAgent(auth); userAgent != "" {
		entry["user_agent"] = userAgent
	}
	if clientProfile := authFileClientProfile(auth); len(clientProfile) > 0 {
		entry["client_profile"] = clientProfile
	}
	if websockets, ok := authFileWebsockets(auth); ok {
		entry["websockets"] = websockets
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		entry[coreauth.AuthFileServiceTierPassthroughKey] = auth.ServiceTierPassthrough()
	}
	if disableCooling, ok := auth.DisableCoolingOverride(); ok {
		entry["disable_cooling"] = disableCooling
	}
	return entry
}

func statAuthFileEntryPath(path string, opts authFileEntryBuildOptions) (os.FileInfo, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, os.ErrNotExist
	}
	cacheKey := path
	if abs, errAbs := filepath.Abs(path); errAbs == nil {
		cacheKey = abs
	}
	if opts.StatCache != nil {
		if cached, ok := opts.StatCache[cacheKey]; ok {
			return cached.Info, cached.Err
		}
	}

	var info os.FileInfo
	var err error
	authDir := strings.TrimSpace(opts.AuthDir)
	if opts.AuthRoot != nil && authDir != "" && authFilePathWithinDir(path, authDir) {
		_, relPath, scopedErr := scopedManagedAuthPath(path, authDir)
		if scopedErr == nil {
			info, err = opts.AuthRoot.Stat(relPath)
		} else {
			err = scopedErr
		}
	} else {
		info, err = os.Stat(path)
	}

	if opts.StatCache != nil {
		opts.StatCache[cacheKey] = authFileStatResult{Info: info, Err: err}
	}
	return info, err
}
