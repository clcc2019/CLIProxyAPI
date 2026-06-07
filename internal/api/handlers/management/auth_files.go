package management

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func (h *Handler) ListAuthFiles(c *gin.Context) {
	if h == nil {
		c.JSON(500, gin.H{"error": "handler not initialized"})
		return
	}
	codexSubscriptionMode := codexSubscriptionListModeFromRequest(c)
	listQuery := authFilesListQueryFromRequest(c)
	if h.authManager == nil {
		h.listAuthFilesFromDisk(c, codexSubscriptionMode, listQuery)
		return
	}
	if listQuery.active() {
		h.listAuthFilesFromManager(c, codexSubscriptionMode, listQuery)
		return
	}
	auths := h.authManager.List()
	files := make([]gin.H, 0, len(auths))
	for _, auth := range auths {
		auth = h.enrichCodexSubscriptionInfo(c.Request.Context(), auth, codexSubscriptionMode)
		if entry := h.buildAuthFileEntryWithOptions(auth, authFileEntryBuildOptions{}); entry != nil {
			files = append(files, entry)
		}
	}
	sortAuthFileEntriesByName(files)
	c.JSON(200, gin.H{"files": files, "total": len(files)})
}

func (h *Handler) listAuthsForManagement(summary bool) []*coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}
	if summary {
		return h.authManager.ListManagementSummary()
	}
	return h.authManager.List()
}

func (h *Handler) listAuthFilesFromManager(c *gin.Context, codexSubscriptionMode codexSubscriptionListMode, q authFilesListQuery) {
	auths := h.listAuthsForManagement(q.Summary)
	entrySubscriptionMode := codexSubscriptionMode
	deferRefreshToPage := q.Paginated && codexSubscriptionMode == codexSubscriptionListRefresh
	if deferRefreshToPage {
		entrySubscriptionMode = codexSubscriptionListCache
	}
	entryOpts := authFileEntryBuildOptions{Summary: q.Summary}
	if !q.Summary && h != nil && h.cfg != nil {
		entryOpts.AuthDir = strings.TrimSpace(h.cfg.AuthDir)
		if entryOpts.AuthDir != "" {
			if root, err := os.OpenRoot(entryOpts.AuthDir); err == nil {
				entryOpts.AuthRoot = root
				defer func() { _ = root.Close() }()
			}
		}
	}
	if !q.Summary {
		entryOpts.StatCache = make(map[string]authFileStatResult, len(auths))
	}
	countEntries := make([]gin.H, 0, len(auths))
	displayEntries := make([]gin.H, 0, len(auths))
	for _, auth := range auths {
		if authFileMatchesListDisplayQuery(auth, q) {
			if entry := authFileTypeCountEntry(auth); entry != nil {
				countEntries = append(countEntries, entry)
			}
		}
		if !authFileMatchesListPreQuery(auth, q) {
			continue
		}
		auth = h.enrichCodexSubscriptionInfo(c.Request.Context(), auth, entrySubscriptionMode)
		if entry := h.buildAuthFileEntryWithOptions(auth, entryOpts); entry != nil {
			displayEntries = append(displayEntries, entry)
		}
	}
	displayEntries = dedupeAuthFileEntries(displayEntries)
	typeCounts := authFileEntryTypeCounts(dedupeAuthFileEntries(countEntries), authFilesListQuery{})

	filtered := make([]gin.H, 0, len(displayEntries))
	for _, entry := range displayEntries {
		if authFileEntryMatchesListQuery(entry, q) {
			filtered = append(filtered, entry)
		}
	}
	sortAuthFileEntriesForList(filtered, q.Sort)
	total := len(filtered)
	q = clampAuthFilesListPage(q, total)
	pageFiles := authFileEntryPageSlice(filtered, q)
	if deferRefreshToPage {
		pageFiles = h.refreshAuthFileEntryPageFromManager(c.Request.Context(), pageFiles, auths, authFileEntryBuildOptions{Summary: q.Summary})
	}
	c.JSON(200, authFilesListPayload(pageFiles, total, q, typeCounts))
}

// GetAuthFileModels returns the models supported by a specific auth file
func (h *Handler) GetAuthFileModels(c *gin.Context) {
	name := c.Query("name")
	if name == "" {
		c.JSON(400, gin.H{"error": "name is required"})
		return
	}

	// Try to find auth ID via authManager
	var authID string
	if h.authManager != nil {
		auths := h.authManager.List()
		for _, auth := range auths {
			if auth.FileName == name || auth.ID == name {
				authID = auth.ID
				break
			}
		}
	}

	if authID == "" {
		authID = name // fallback to filename as ID
	}

	// Get models from registry
	reg := registry.GetGlobalRegistry()
	models := reg.GetModelsForClient(authID)

	result := make([]gin.H, 0, len(models))
	for _, m := range models {
		entry := gin.H{
			"id": m.ID,
		}
		if m.DisplayName != "" {
			entry["display_name"] = m.DisplayName
		}
		if m.Type != "" {
			entry["type"] = m.Type
		}
		if m.OwnedBy != "" {
			entry["owned_by"] = m.OwnedBy
		}
		result = append(result, entry)
	}

	c.JSON(200, gin.H{"models": result})
}

// GetCodexUsage fetches Codex quota with the same ChatGPT backend flow used by
// the official Codex client. Subscription expiry is not part of /wham/usage, so
// locally known/JWT-derived subscription fields are merged into the response.
func (h *Handler) GetCodexUsage(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	auth, status, message := h.resolveCodexUsageAuth(c)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"error": message})
		return
	}
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth file is not a Codex credential"})
		return
	}

	ctx := c.Request.Context()
	auth = h.refreshCodexUsageAuthIfNeeded(ctx, auth)
	var refreshedSubscription <-chan *coreauth.Auth
	if mode := codexSubscriptionListModeFromRequest(c); mode == codexSubscriptionListRefresh {
		ch := make(chan *coreauth.Auth, 1)
		refreshedSubscription = ch
		go func(authSnapshot *coreauth.Auth) {
			ch <- h.enrichCodexSubscriptionInfo(ctx, authSnapshot, codexSubscriptionListRefresh)
		}(auth)
	}

	usageOpts := parseCodexUsageRequestOptions(c)
	payload, upstreamStatus, err := h.fetchCodexUsageWithCache(ctx, auth, usageOpts)
	if err != nil {
		if codexUsageTransientFailure(upstreamStatus, err) {
			payload = codexUsageUnavailablePayload(err, upstreamStatus)
		} else {
			if upstreamStatus > 0 {
				c.JSON(upstreamStatus, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
	}
	if refreshedSubscription != nil {
		if updated := <-refreshedSubscription; updated != nil {
			auth = updated
		}
	}
	if h.authManager != nil {
		if latest, ok := h.authManager.GetByID(auth.ID); ok && latest != nil {
			auth = latest
		}
	}
	mergeCodexUsageLocalFields(payload, auth)
	if entry := h.buildAuthFileEntry(auth); entry != nil {
		payload["auth_file"] = entry
		payload["authFile"] = entry
	}
	c.JSON(http.StatusOK, payload)
}

// List auth files from disk when the auth manager is unavailable.
func (h *Handler) listAuthFilesFromDisk(c *gin.Context, codexSubscriptionMode codexSubscriptionListMode, q authFilesListQuery) {
	authDir := strings.TrimSpace(h.cfg.AuthDir)
	root, err := os.OpenRoot(authDir)
	if err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("failed to open auth dir: %v", err)})
		return
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("failed to read auth dir: %v", err)})
		return
	}

	entrySubscriptionMode := codexSubscriptionMode
	deferRefreshToPage := q.Paginated && codexSubscriptionMode == codexSubscriptionListRefresh
	if deferRefreshToPage {
		entrySubscriptionMode = codexSubscriptionListCache
	}
	files := make([]gin.H, 0)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !util.HasJSONFileName(name) {
			continue
		}
		if info, errInfo := e.Info(); errInfo == nil {
			full := filepath.Join(authDir, name)
			fileData := gin.H{"name": name, "size": info.Size(), "modtime": info.ModTime()}

			// Read file to get type field.
			if data, errRead := readAuthRootFile(root, name); errRead == nil {
				typeValue := gjson.GetBytes(data, "type").String()
				emailValue := gjson.GetBytes(data, "email").String()
				fileData["type"] = typeValue
				fileData["email"] = emailValue
				if projectID := strings.TrimSpace(gjson.GetBytes(data, "project_id").String()); projectID != "" {
					fileData["project_id"] = projectID
				} else if projectID := strings.TrimSpace(gjson.GetBytes(data, "projectId").String()); projectID != "" {
					fileData["project_id"] = projectID
				}
				var metadata map[string]any
				if err := json.Unmarshal(data, &metadata); err == nil {
					if authFileMetadataHasRefreshToken(metadata) {
						fileData["has_refresh_token"] = true
					}
					if state, ok := authFileRuntimeState(metadata); ok {
						if q.Summary {
							applyAuthFileRuntimeStateSummaryEntry(fileData, state, true)
						} else {
							applyAuthFileRuntimeStateEntry(fileData, state, true)
						}
					}
					if strings.EqualFold(strings.TrimSpace(typeValue), "codex") {
						auth := h.enrichCodexSubscriptionInfo(c.Request.Context(), &coreauth.Auth{
							ID:       name,
							Provider: typeValue,
							FileName: name,
							Metadata: metadata,
							Attributes: map[string]string{
								"path": full,
							},
						}, entrySubscriptionMode)
						metadata = auth.Metadata
						if until, ok := codexSubscriptionUntilValue(metadata); ok {
							fileData["subscription_expires_at"] = until
						}
						if claims := extractCodexIDTokenClaims(auth); claims != nil {
							fileData["id_token"] = claims
							applyCodexSubscriptionFromClaims(fileData, claims)
						}
					}
				}
				if prefix := strings.TrimSpace(gjson.GetBytes(data, "prefix").String()); prefix != "" {
					fileData["prefix"] = prefix
				}
				if proxyURL := strings.TrimSpace(gjson.GetBytes(data, "proxy_url").String()); proxyURL != "" {
					fileData["proxy_url"] = proxyURL
				}
				if pv := gjson.GetBytes(data, "priority"); pv.Exists() {
					switch pv.Type {
					case gjson.Number:
						fileData["priority"] = int(pv.Int())
					case gjson.String:
						if parsed, errAtoi := strconv.Atoi(strings.TrimSpace(pv.String())); errAtoi == nil {
							fileData["priority"] = parsed
						}
					}
				}
				if disabled, ok := boolFromGJSON(data, "disabled"); ok {
					fileData["disabled"] = disabled
				}
				if disableCooling, ok := boolFromGJSON(data, "disable_cooling", "disable-cooling"); ok {
					fileData["disable_cooling"] = disableCooling
				}
				if serviceTierPassthrough, ok := boolFromGJSON(data, coreauth.AuthFileServiceTierPassthroughKey, "service-tier-passthrough", "serviceTierPassthrough", "fast"); ok {
					fileData[coreauth.AuthFileServiceTierPassthroughKey] = serviceTierPassthrough
				} else if strings.EqualFold(strings.TrimSpace(typeValue), "codex") {
					fileData[coreauth.AuthFileServiceTierPassthroughKey] = false
				}
				if nv := gjson.GetBytes(data, "note"); nv.Exists() && nv.Type == gjson.String {
					if trimmed := strings.TrimSpace(nv.String()); trimmed != "" {
						fileData["note"] = trimmed
					}
				}
				if uav := gjson.GetBytes(data, "user_agent"); uav.Exists() && uav.Type == gjson.String {
					if trimmed := strings.TrimSpace(uav.String()); trimmed != "" {
						fileData["user_agent"] = trimmed
					}
				}
				if _, ok := fileData["user_agent"]; !ok {
					if uav := gjson.GetBytes(data, "user-agent"); uav.Exists() && uav.Type == gjson.String {
						if trimmed := strings.TrimSpace(uav.String()); trimmed != "" {
							fileData["user_agent"] = trimmed
						}
					}
				}
				if originator := firstTrimmedGJSONString(data, coreauth.AuthFileCodexOriginatorKey, coreauth.AuthFileCodexOriginatorHeader); originator != "" {
					fileData[coreauth.AuthFileCodexOriginatorKey] = originator
				}
				if betaFeatures := firstTrimmedGJSONString(data, coreauth.AuthFileCodexBetaFeaturesKey, "beta-features", "betaFeatures"); betaFeatures != "" {
					fileData[coreauth.AuthFileCodexBetaFeaturesKey] = betaFeatures
				}
				if installationID := firstTrimmedGJSONString(data, coreauth.AuthFileCodexInstallationIDKey, "installation-id", "installationId"); installationID != "" {
					fileData[coreauth.AuthFileCodexInstallationIDKey] = installationID
				}
				if includeTimingMetrics, ok := boolFromGJSON(data, coreauth.AuthFileCodexIncludeTimingMetricsKey, "include-timing-metrics", "includeTimingMetrics"); ok {
					fileData[coreauth.AuthFileCodexIncludeTimingMetricsKey] = includeTimingMetrics
				}
			}

			files = append(files, fileData)
		}
	}
	if !q.active() {
		c.JSON(200, gin.H{"files": files, "total": len(files)})
		return
	}
	typeCounts := authFileEntryTypeCounts(files, q)
	filtered := make([]gin.H, 0, len(files))
	for _, file := range files {
		if authFileEntryMatchesListQuery(file, q) {
			filtered = append(filtered, file)
		}
	}
	sortAuthFileEntriesForList(filtered, q.Sort)
	total := len(filtered)
	q = clampAuthFilesListPage(q, total)
	pageFiles := authFileEntryPageSlice(filtered, q)
	if deferRefreshToPage {
		pageFiles = h.refreshAuthFileEntryPageFromDisk(c.Request.Context(), pageFiles)
	}
	c.JSON(200, authFilesListPayload(pageFiles, total, q, typeCounts))
}
