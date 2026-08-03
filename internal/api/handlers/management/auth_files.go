package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
	manager := h.authManagerSnapshot()
	if manager == nil {
		h.listAuthFilesFromDisk(c, codexSubscriptionMode, listQuery)
		return
	}
	if listQuery.active() {
		h.listAuthFilesFromManager(c, codexSubscriptionMode, listQuery)
		return
	}
	h.maybeClearExpiredQuotaCooldowns(manager, c.Request.Context())
	auths := manager.List()
	files := make([]gin.H, 0, len(auths))
	entryOpts := authFileEntryBuildOptions{RecentRequestSnapshotter: coreauth.NewRecentRequestSnapshotter(time.Now())}
	for _, auth := range auths {
		auth = h.enrichCodexSubscriptionInfo(c.Request.Context(), auth, codexSubscriptionMode)
		if entry := h.buildAuthFileEntryWithOptions(auth, entryOpts); entry != nil {
			files = append(files, entry)
		}
	}
	sortAuthFileEntriesByName(files)
	c.JSON(200, gin.H{"files": files, "total": len(files)})
}

func (h *Handler) listAuthsForManagement(c *gin.Context, q authFilesListQuery) ([]*coreauth.Auth, []*coreauth.Auth) {
	if h == nil {
		return nil, nil
	}
	manager := h.authManagerSnapshot()
	if manager == nil {
		return nil, nil
	}
	if c != nil && c.Request != nil {
		h.maybeClearExpiredQuotaCooldowns(manager, c.Request.Context())
	}
	if q.Summary {
		return manager.ListManagementSummary(), nil
	}
	if !q.hasManagerPreFilter() {
		return manager.List(), nil
	}

	// A filtered management page used to clone every credential, including
	// token-bearing metadata, before applying the cheap provider/name filters.
	// Lightweight summaries retain exactly the fields needed for those filters;
	// only matching IDs need the full defensive clone used to build a response.
	summaries := manager.ListManagementSummaryWithoutRecentRequests()
	ids := make([]string, 0, len(summaries))
	for _, auth := range summaries {
		if authFileMatchesListPreQuery(auth, q) {
			ids = append(ids, auth.ID)
		}
	}
	return manager.ListByIDs(ids), summaries
}

func (h *Handler) listAuthFilesFromManager(c *gin.Context, codexSubscriptionMode codexSubscriptionListMode, q authFilesListQuery) {
	if q.Paginated && q.PageSize > 0 {
		if h.listAuthFilesFromManagerPage(c, codexSubscriptionMode, q) {
			return
		}
	}

	auths, summaries := h.listAuthsForManagement(c, q)
	entrySubscriptionMode := codexSubscriptionMode
	deferRefreshToPage := q.Paginated && codexSubscriptionMode == codexSubscriptionListRefresh
	if deferRefreshToPage {
		entrySubscriptionMode = codexSubscriptionListCache
	}
	entryOpts := authFileEntryBuildOptions{
		Summary:                  q.Summary,
		RecentRequestSnapshotter: coreauth.NewRecentRequestSnapshotter(time.Now()),
	}
	if !q.Summary {
		entryOpts.AuthDir = h.authDirSnapshot()
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
	countSource := auths
	if summaries != nil {
		countSource = summaries
	}
	countEntries := make([]gin.H, 0, len(countSource))
	displayEntries := make([]gin.H, 0, len(auths))
	for _, auth := range countSource {
		if authFileMatchesListDisplayQuery(auth, q) {
			if entry := authFileTypeCountEntry(auth); entry != nil {
				countEntries = append(countEntries, entry)
			}
		}
	}
	for _, auth := range auths {
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
		pageFiles = h.refreshAuthFileEntryPageFromManager(c.Request.Context(), pageFiles, auths, authFileEntryBuildOptions{
			Summary:                  q.Summary,
			RecentRequestSnapshotter: entryOpts.RecentRequestSnapshotter,
		})
	}
	c.JSON(200, authFilesListPayload(pageFiles, total, q, typeCounts))
}

// listAuthFilesFromManagerPage uses lightweight management snapshots to find
// the requested page before cloning full credentials. The old path built every
// response entry first, which made page_size reduce the JSON payload but not
// the expensive work behind it.
func (h *Handler) listAuthFilesFromManagerPage(c *gin.Context, codexSubscriptionMode codexSubscriptionListMode, q authFilesListQuery) bool {
	if h == nil || c == nil || c.Request == nil {
		return false
	}
	manager := h.authManagerSnapshot()
	if manager == nil {
		return false
	}
	h.maybeClearExpiredQuotaCooldowns(manager, c.Request.Context())

	summaries := manager.ListManagementSummaryWithoutRecentRequests()
	entrySubscriptionMode := codexSubscriptionMode
	deferRefreshToPage := codexSubscriptionMode == codexSubscriptionListRefresh
	if deferRefreshToPage {
		entrySubscriptionMode = codexSubscriptionListCache
	}
	recentSnapshotter := coreauth.NewRecentRequestSnapshotter(time.Now())
	summaryOpts := authFileEntryBuildOptions{
		Summary:                  true,
		SkipRecentRequests:       true,
		RecentRequestSnapshotter: recentSnapshotter,
	}

	countEntries := make([]gin.H, 0, len(summaries))
	displayEntries := make([]gin.H, 0, len(summaries))
	var summaryAuthIDsByName map[string][]string
	if q.Summary {
		summaryAuthIDsByName = make(map[string][]string, len(summaries))
	}
	for _, auth := range summaries {
		if authFileMatchesListDisplayQuery(auth, q) {
			if entry := authFileTypeCountEntry(auth); entry != nil {
				countEntries = append(countEntries, entry)
			}
		}
		if !authFileMatchesListPreQuery(auth, q) {
			continue
		}
		auth = h.enrichCodexSubscriptionInfo(c.Request.Context(), auth, entrySubscriptionMode)
		if entry := h.buildAuthFileEntryWithOptions(auth, summaryOpts); entry != nil {
			displayEntries = append(displayEntries, entry)
			if q.Summary {
				name := authFileListKey(valueAsString(entry["name"]))
				id := strings.TrimSpace(auth.ID)
				if name != "" && id != "" {
					summaryAuthIDsByName[name] = append(summaryAuthIDsByName[name], id)
				}
			}
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

	entryOpts := authFileEntryBuildOptions{RecentRequestSnapshotter: recentSnapshotter}
	if !q.Summary {
		entryOpts.AuthDir = h.authDirSnapshot()
		if entryOpts.AuthDir != "" {
			if root, err := os.OpenRoot(entryOpts.AuthDir); err == nil {
				entryOpts.AuthRoot = root
				defer func() { _ = root.Close() }()
			}
		}
		entryOpts.StatCache = make(map[string]authFileStatResult, len(filtered))
		available := filtered[:0]
		for _, entry := range filtered {
			if h.authFileListEntryAvailable(entry, entryOpts) {
				available = append(available, entry)
			}
		}
		filtered = available
	}

	sortAuthFileEntriesForList(filtered, q.Sort)
	total := len(filtered)
	q = clampAuthFilesListPage(q, total)
	pageEntries := authFileEntryPageSlice(filtered, q)
	if q.Summary {
		pageRecentIDs := make([]string, 0, len(pageEntries))
		for _, entry := range pageEntries {
			name := authFileListKey(valueAsString(entry["name"]))
			pageRecentIDs = append(pageRecentIDs, summaryAuthIDsByName[name]...)
		}
		pageRecentAuths := manager.ListManagementSummaryByIDs(pageRecentIDs)
		recentAuthsByName := make(map[string][]*coreauth.Auth, len(pageRecentAuths))
		for _, auth := range pageRecentAuths {
			name := strings.TrimSpace(auth.FileName)
			if name == "" {
				name = auth.ID
			}
			name = authFileListKey(name)
			if name != "" {
				recentAuthsByName[name] = append(recentAuthsByName[name], auth)
			}
		}
		populateSummaryPageRecentRequests(pageEntries, recentAuthsByName, recentSnapshotter)
		if deferRefreshToPage {
			pageEntries = h.refreshAuthFileEntryPageFromManager(c.Request.Context(), pageEntries, pageRecentAuths, authFileEntryBuildOptions{
				Summary:                  true,
				RecentRequestSnapshotter: recentSnapshotter,
			})
			populateSummaryPageRecentRequests(pageEntries, recentAuthsByName, recentSnapshotter)
		}
		c.JSON(http.StatusOK, authFilesListPayload(pageEntries, total, q, typeCounts))
		return true
	}

	pageIDs := make([]string, 0, len(pageEntries))
	for _, entry := range pageEntries {
		if id := authFileEntryManagerID(entry); id != "" {
			pageIDs = append(pageIDs, id)
		}
	}
	pageAuths := manager.ListByIDs(pageIDs)
	authsByKey := make(map[string]*coreauth.Auth, len(pageAuths)*2)
	for _, auth := range pageAuths {
		for _, key := range authFileListAuthKeys(auth) {
			if _, exists := authsByKey[key]; !exists {
				authsByKey[key] = auth
			}
		}
	}

	fullEntries := make([]gin.H, 0, len(pageEntries))
	for _, summaryEntry := range pageEntries {
		auth := authsByKey[authFileEntryLookupKey(summaryEntry)]
		if auth == nil {
			continue
		}
		auth = h.enrichCodexSubscriptionInfo(c.Request.Context(), auth, entrySubscriptionMode)
		if entry := h.buildAuthFileEntryWithOptions(auth, entryOpts); entry != nil && authFileEntryMatchesListQuery(entry, q) {
			fullEntries = append(fullEntries, entry)
		}
	}
	if deferRefreshToPage {
		fullEntries = h.refreshAuthFileEntryPageFromManager(c.Request.Context(), fullEntries, pageAuths, authFileEntryBuildOptions{
			RecentRequestSnapshotter: recentSnapshotter,
		})
	}
	c.JSON(http.StatusOK, authFilesListPayload(fullEntries, total, q, typeCounts))
	return true
}

// populateSummaryPageRecentRequests delays the fixed-size request-history
// allocation until after pagination. Duplicate memory/file entries are kept
// compatible with mergeAuthFileEntryGroup by retaining the candidate with the
// highest recent-request total, while preferring the entry selected as the
// merged representative when totals tie.
func populateSummaryPageRecentRequests(entries []gin.H, authsByName map[string][]*coreauth.Auth, snapshotter *coreauth.RecentRequestSnapshotter) {
	if len(entries) == 0 || len(authsByName) == 0 || snapshotter == nil {
		return
	}
	for _, entry := range entries {
		name := authFileListKey(valueAsString(entry["name"]))
		candidates := authsByName[name]
		if len(candidates) == 0 {
			continue
		}
		preferredKey := authFileEntryLookupKey(entry)
		var (
			best          []coreauth.RecentRequestBucket
			bestTotal     int64 = -1
			bestPreferred bool
		)
		for _, auth := range candidates {
			recent := snapshotter.Snapshot(auth)
			total := authFileRecentRequestsTotal(recent)
			preferred := authFileListKey(auth.ID) == preferredKey
			if total > bestTotal || (total == bestTotal && preferred && !bestPreferred) {
				best = recent
				bestTotal = total
				bestPreferred = preferred
			}
		}
		if best != nil {
			entry["recent_requests"] = best
		}
	}
}

func (h *Handler) authFileListEntryAvailable(entry gin.H, opts authFileEntryBuildOptions) bool {
	if h == nil || opts.Summary {
		return true
	}
	path := strings.TrimSpace(valueAsString(entry["path"]))
	if path == "" {
		return true
	}
	if _, errStat := statAuthFileEntryPath(path, opts); errStat == nil {
		return true
	} else if !os.IsNotExist(errStat) {
		return true
	}
	if authFileEntryRuntimeOnly(entry) {
		return true
	}
	removedByManagement := authFileEntryDisabled(entry) || strings.EqualFold(strings.TrimSpace(authFileEntryString(entry, "status_message", "statusMessage")), "removed via management api")
	return !h.isManagedAuthFilePath(path) && !removedByManagement
}

const authListQuotaMaintenanceInterval = time.Second

func (h *Handler) maybeClearExpiredQuotaCooldowns(manager *coreauth.Manager, ctx context.Context) {
	if h == nil || manager == nil {
		return
	}
	now := time.Now().UnixNano()
	last := h.authListCooldownMaintenanceAt.Load()
	if last != 0 && now-last < authListQuotaMaintenanceInterval.Nanoseconds() {
		return
	}
	if !h.authListCooldownMaintenanceAt.CompareAndSwap(last, now) {
		return
	}
	manager.ClearExpiredQuotaCooldowns(ctx)
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
	if manager := h.authManagerSnapshot(); manager != nil {
		if auth, ok := manager.GetByID(name); ok {
			authID = auth.ID
		} else if auth, ok := manager.GetByFileName(name); ok {
			authID = auth.ID
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
	if manager := h.authManagerSnapshot(); manager != nil {
		if latest, ok := manager.GetByID(auth.ID); ok && latest != nil {
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
	authDir := h.authDirSnapshot()
	if authDir == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "auth directory is not configured"})
		return
	}
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
					if strings.TrimSpace(typeValue) == "" {
						typeValue = coreauth.AuthFileProviderFromMetadata(metadata)
						if typeValue != "" {
							fileData["type"] = typeValue
						}
					}
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
						applyCodexAuthModeEntry(fileData, auth)
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
