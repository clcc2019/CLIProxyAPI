package management

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/antigravity"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	geminiAuth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/gemini"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kimi"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	xaiauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

var lastRefreshKeys = []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"}

const (
	anthropicCallbackPort  = 54545
	geminiCallbackPort     = 8085
	codexCallbackPort      = 1455
	geminiCLIEndpoint      = "https://cloudcode-pa.googleapis.com"
	geminiCLIVersion       = "v1internal"
	maxAuthFileUploadBytes = 2 << 20
	codexAccountsCheckURL  = "https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27"
)

type codexSubscriptionCacheEntry struct {
	info      codexAccountSubscriptionInfo
	found     bool
	expiresAt time.Time
}

type codexAccountSubscriptionInfo struct {
	PlanType              string
	Email                 string
	SubscriptionExpiresAt string
}

var codexSubscriptionCache sync.Map

type callbackForwarder struct {
	provider string
	server   *http.Server
	done     chan struct{}
}

var (
	callbackForwardersMu  sync.Mutex
	callbackForwarders    = make(map[int]*callbackForwarder)
	errAuthFileMustBeJSON = errors.New("auth file must be .json")
	errAuthFileNotFound   = errors.New("auth file not found")
)

type codexSubscriptionListMode int

const (
	codexSubscriptionListCache codexSubscriptionListMode = iota
	codexSubscriptionListRefresh
	codexSubscriptionListSkip
)

func codexSubscriptionListModeFromRequest(c *gin.Context) codexSubscriptionListMode {
	mode := strings.TrimSpace(firstNonEmptyQueryValue(c, "codex_subscription", "codexSubscription"))
	if mode == "" {
		return codexSubscriptionListCache
	}
	switch strings.ToLower(mode) {
	case "refresh", "force", "fetch", "1", "true", "yes", "on":
		return codexSubscriptionListRefresh
	case "skip", "none", "off", "0", "false", "no":
		return codexSubscriptionListSkip
	default:
		return codexSubscriptionListCache
	}
}

func extractLastRefreshTimestamp(meta map[string]any) (time.Time, bool) {
	if len(meta) == 0 {
		return time.Time{}, false
	}
	for _, key := range lastRefreshKeys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseLastRefreshValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func authFileLastRefresh(auth *coreauth.Auth) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	if !auth.LastRefreshedAt.IsZero() {
		return auth.LastRefreshedAt.UTC(), true
	}
	if ts, ok := extractLastRefreshTimestamp(auth.Metadata); ok {
		return ts, true
	}
	for _, key := range lastRefreshKeys {
		if val := strings.TrimSpace(authAttribute(auth, key)); val != "" {
			if ts, ok := parseLastRefreshValue(val); ok {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func authFileRuntimeStateTimes(meta map[string]any) (time.Time, bool, time.Time, bool) {
	state, ok := authFileRuntimeState(meta)
	if !ok {
		return time.Time{}, false, time.Time{}, false
	}
	updatedOK := !state.UpdatedAt.IsZero()
	savedOK := !state.SavedAt.IsZero()
	return state.UpdatedAt.UTC(), updatedOK, state.SavedAt.UTC(), savedOK
}

func authFileRuntimeState(meta map[string]any) (coreauth.AuthRuntimeState, bool) {
	if len(meta) == 0 {
		return coreauth.AuthRuntimeState{}, false
	}
	raw, ok := meta["cliproxy_runtime_state"]
	if !ok || raw == nil {
		return coreauth.AuthRuntimeState{}, false
	}
	var state coreauth.AuthRuntimeState
	switch v := raw.(type) {
	case coreauth.AuthRuntimeState:
		state = v
	case *coreauth.AuthRuntimeState:
		if v == nil {
			return coreauth.AuthRuntimeState{}, false
		}
		state = *v
	default:
		data, err := json.Marshal(raw)
		if err != nil {
			return coreauth.AuthRuntimeState{}, false
		}
		if err := json.Unmarshal(data, &state); err != nil {
			return coreauth.AuthRuntimeState{}, false
		}
	}
	if state.Status == "" && state.StatusMessage == "" && !state.Unavailable && state.LastError == nil &&
		state.Success == 0 && state.Failed == 0 && len(state.RecentRequests) == 0 && len(state.ModelStates) == 0 &&
		state.Quota.Reason == "" && !state.Quota.Exceeded && state.Quota.BackoffLevel == 0 && state.Quota.NextRecoverAt.IsZero() &&
		state.NextRetryAfter.IsZero() && state.UpdatedAt.IsZero() && state.SavedAt.IsZero() {
		return coreauth.AuthRuntimeState{}, false
	}
	return state, true
}

func parseLastRefreshValue(v any) (time.Time, bool) {
	switch val := v.(type) {
	case string:
		s := strings.TrimSpace(val)
		if s == "" {
			return time.Time{}, false
		}
		layouts := []string{time.RFC3339, time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00"}
		for _, layout := range layouts {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts.UTC(), true
			}
		}
		if unix, err := strconv.ParseInt(s, 10, 64); err == nil && unix > 0 {
			return time.Unix(unix, 0).UTC(), true
		}
	case float64:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(val), 0).UTC(), true
	case int64:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(val, 0).UTC(), true
	case int:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(val), 0).UTC(), true
	case json.Number:
		if i, err := val.Int64(); err == nil && i > 0 {
			return time.Unix(i, 0).UTC(), true
		}
	}
	return time.Time{}, false
}

func isWebUIRequest(c *gin.Context) bool {
	return isTruthyQueryValue(c.Query("is_webui"))
}

func isTruthyQueryValue(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func firstNonEmptyQueryValue(c *gin.Context, keys ...string) string {
	if c == nil {
		return ""
	}
	for _, key := range keys {
		if value := strings.TrimSpace(c.Query(key)); value != "" {
			return value
		}
	}
	return ""
}

func oauthSessionErrorWithDetail(prefix string, err error) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "Authentication failed"
	}
	if err == nil {
		return prefix
	}
	detail := strings.TrimSpace(err.Error())
	if detail == "" {
		return prefix
	}
	if len(detail) > 600 {
		detail = detail[:600] + "..."
	}
	return prefix + ": " + detail
}

func codexLoginRequestUserAgent(c *gin.Context) string {
	if c == nil || isWebUIRequest(c) {
		return ""
	}
	return strings.TrimSpace(c.GetHeader("User-Agent"))
}

func startCallbackForwarder(port int, provider, targetBase string) (*callbackForwarder, error) {
	callbackForwardersMu.Lock()
	prev := callbackForwarders[port]
	if prev != nil {
		delete(callbackForwarders, port)
	}
	callbackForwardersMu.Unlock()

	if prev != nil {
		stopForwarderInstance(port, prev)
	}

	addr := fmt.Sprintf("0.0.0.0:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := targetBase
		if raw := r.URL.RawQuery; raw != "" {
			if strings.Contains(target, "?") {
				target = target + "&" + raw
			} else {
				target = target + "?" + raw
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, target, http.StatusFound)
	})

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
	}
	done := make(chan struct{})

	go func() {
		if errServe := srv.Serve(ln); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) {
			log.WithError(errServe).Warnf("callback forwarder for %s stopped unexpectedly", provider)
		}
		close(done)
	}()

	forwarder := &callbackForwarder{
		provider: provider,
		server:   srv,
		done:     done,
	}

	callbackForwardersMu.Lock()
	callbackForwarders[port] = forwarder
	callbackForwardersMu.Unlock()

	log.Infof("callback forwarder for %s listening on %s", provider, addr)

	return forwarder, nil
}

func stopCallbackForwarderInstance(port int, forwarder *callbackForwarder) {
	if forwarder == nil {
		return
	}
	callbackForwardersMu.Lock()
	if current := callbackForwarders[port]; current == forwarder {
		delete(callbackForwarders, port)
	}
	callbackForwardersMu.Unlock()

	stopForwarderInstance(port, forwarder)
}

func stopForwarderInstance(port int, forwarder *callbackForwarder) {
	if forwarder == nil || forwarder.server == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := forwarder.server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.WithError(err).Warnf("failed to shut down callback forwarder on port %d", port)
	}

	select {
	case <-forwarder.done:
	case <-time.After(2 * time.Second):
	}

	log.Infof("callback forwarder on port %d stopped", port)
}

func (h *Handler) managementCallbackURL(path string) (string, error) {
	if h == nil || h.cfg == nil || h.cfg.Port <= 0 {
		return "", fmt.Errorf("server port is not configured")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	scheme := "http"
	if h.cfg.TLS.Enable {
		scheme = "https"
	}
	return fmt.Sprintf("%s://127.0.0.1:%d%s", scheme, h.cfg.Port, path), nil
}

func (h *Handler) ListAuthFiles(c *gin.Context) {
	if h == nil {
		c.JSON(500, gin.H{"error": "handler not initialized"})
		return
	}
	codexSubscriptionMode := codexSubscriptionListModeFromRequest(c)
	if h.authManager == nil {
		h.listAuthFilesFromDisk(c, codexSubscriptionMode)
		return
	}
	auths := h.authManager.List()
	files := make([]gin.H, 0, len(auths))
	for _, auth := range auths {
		auth = h.enrichCodexSubscriptionInfo(c.Request.Context(), auth, codexSubscriptionMode)
		if entry := h.buildAuthFileEntry(auth); entry != nil {
			files = append(files, entry)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		nameI, _ := files[i]["name"].(string)
		nameJ, _ := files[j]["name"].(string)
		return strings.ToLower(nameI) < strings.ToLower(nameJ)
	})
	c.JSON(200, gin.H{"files": files, "total": len(files)})
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

// List auth files from disk when the auth manager is unavailable.
func (h *Handler) listAuthFilesFromDisk(c *gin.Context, codexSubscriptionMode codexSubscriptionListMode) {
	entries, err := os.ReadDir(h.cfg.AuthDir)
	if err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("failed to read auth dir: %v", err)})
		return
	}
	files := make([]gin.H, 0)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		if info, errInfo := e.Info(); errInfo == nil {
			fileData := gin.H{"name": name, "size": info.Size(), "modtime": info.ModTime()}

			// Read file to get type field
			full := filepath.Join(h.cfg.AuthDir, name)
			if data, errRead := os.ReadFile(full); errRead == nil {
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
					if state, ok := authFileRuntimeState(metadata); ok {
						applyAuthFileRuntimeStateEntry(fileData, state, true)
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
						}, codexSubscriptionMode)
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
			}

			files = append(files, fileData)
		}
	}
	c.JSON(200, gin.H{"files": files, "total": len(files)})
}

func (h *Handler) buildAuthFileEntry(auth *coreauth.Auth) gin.H {
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
	if serialized := serializeAuthError(auth.LastError); serialized != nil {
		entry["last_error"] = serialized
	}
	if serialized := serializeModelStates(auth.ModelStates); len(serialized) > 0 {
		entry["model_states"] = serialized
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
		if info, err := os.Stat(path); err == nil {
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
	if until, ok := codexSubscriptionUntilValue(auth.Metadata); ok {
		entry["subscription_expires_at"] = until
	}
	if claims := extractCodexIDTokenClaims(auth); claims != nil {
		entry["id_token"] = claims
		applyCodexSubscriptionFromClaims(entry, claims)
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
	if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
		entry["proxy_url"] = proxyURL
	} else if auth.Metadata != nil {
		if rawProxyURL, ok := auth.Metadata["proxy_url"].(string); ok {
			if trimmed := strings.TrimSpace(rawProxyURL); trimmed != "" {
				entry["proxy_url"] = trimmed
			}
		}
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
	if websockets, ok := authFileWebsockets(auth); ok {
		entry["websockets"] = websockets
	}
	if disableCooling, ok := auth.DisableCoolingOverride(); ok {
		entry["disable_cooling"] = disableCooling
	}
	return entry
}

func boolFromGJSON(data []byte, keys ...string) (bool, bool) {
	for _, key := range keys {
		value := gjson.GetBytes(data, key)
		if !value.Exists() {
			continue
		}
		switch value.Type {
		case gjson.True:
			return true, true
		case gjson.False:
			return false, true
		case gjson.String:
			parsed, err := strconv.ParseBool(strings.TrimSpace(value.String()))
			if err == nil {
				return parsed, true
			}
		}
	}
	return false, false
}

func codexSubscriptionUntilValue(metadata map[string]any) (any, bool) {
	if len(metadata) == 0 {
		return nil, false
	}
	for _, key := range []string{
		"subscription_expires_at",
		"subscriptionExpiresAt",
		"chatgpt_subscription_active_until",
		"chatgptSubscriptionActiveUntil",
	} {
		if value, ok := metadata[key]; ok {
			if normalized, okNormalize := normalizeCodexSubscriptionUntilValue(value); okNormalize {
				return normalized, true
			}
		}
	}
	return nil, false
}

func normalizeCodexSubscriptionUntilValue(value any) (any, bool) {
	if value == nil {
		return nil, false
	}
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return nil, false
		}
		return trimmed, true
	case json.Number:
		trimmed := strings.TrimSpace(v.String())
		if trimmed == "" {
			return nil, false
		}
		return trimmed, true
	case int:
		return v, true
	case int64:
		return v, true
	case float64:
		return v, true
	case time.Time:
		if v.IsZero() {
			return nil, false
		}
		return v.UTC().Format(time.RFC3339), true
	default:
		return nil, false
	}
}

func applyCodexSubscriptionFromClaims(entry gin.H, claims gin.H) {
	if entry == nil || len(claims) == 0 {
		return
	}
	if current, ok := entry["subscription_expires_at"]; ok {
		if _, okNormalize := normalizeCodexSubscriptionUntilValue(current); okNormalize {
			return
		}
	}
	for _, key := range []string{
		"subscription_expires_at",
		"subscriptionExpiresAt",
		"chatgpt_subscription_active_until",
		"chatgptSubscriptionActiveUntil",
	} {
		if value, ok := claims[key]; ok {
			if normalized, okNormalize := normalizeCodexSubscriptionUntilValue(value); okNormalize {
				entry["subscription_expires_at"] = normalized
				return
			}
		}
	}
}

func (h *Handler) enrichCodexSubscriptionInfo(ctx context.Context, auth *coreauth.Auth, mode codexSubscriptionListMode) *coreauth.Auth {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return auth
	}
	if _, ok := codexSubscriptionUntilValue(auth.Metadata); ok {
		return auth
	}
	if mode == codexSubscriptionListSkip {
		return auth
	}
	accessToken := codexAuthMetadataString(auth.Metadata, "access_token", "accessToken")
	if accessToken == "" {
		accessToken = strings.TrimSpace(authAttribute(auth, "access_token"))
	}
	if accessToken == "" {
		return auth
	}

	proxyURL := h.codexSubscriptionProxyURL(auth)
	cacheKey := codexSubscriptionCacheKey(accessToken, proxyURL)
	now := time.Now()
	if cachedRaw, ok := codexSubscriptionCache.Load(cacheKey); ok {
		cached, _ := cachedRaw.(codexSubscriptionCacheEntry)
		if cached.expiresAt.After(now) {
			if cached.found {
				return applyCodexAccountSubscriptionInfo(auth, cached.info)
			}
			return auth
		}
		codexSubscriptionCache.Delete(cacheKey)
	}
	if mode == codexSubscriptionListCache {
		return auth
	}

	orgID := resolveCodexAccountCheckOrgID(auth, accessToken)
	info, err := h.fetchCodexAccountSubscriptionInfo(ctx, accessToken, proxyURL, orgID)
	if err != nil {
		log.WithError(err).Debug("failed to fetch codex subscription info")
		codexSubscriptionCache.Store(cacheKey, codexSubscriptionCacheEntry{found: false, expiresAt: now.Add(10 * time.Minute)})
		return auth
	}
	if info == nil || !info.hasData() {
		codexSubscriptionCache.Store(cacheKey, codexSubscriptionCacheEntry{found: false, expiresAt: now.Add(30 * time.Minute)})
		return auth
	}
	codexSubscriptionCache.Store(cacheKey, codexSubscriptionCacheEntry{info: *info, found: true, expiresAt: now.Add(6 * time.Hour)})
	return applyCodexAccountSubscriptionInfo(auth, *info)
}

func codexAuthMetadataString(metadata map[string]any, keys ...string) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range keys {
		if value, ok := metadata[key]; ok {
			if str := strings.TrimSpace(valueAsString(value)); str != "" {
				return str
			}
		}
	}
	for _, containerKey := range []string{"token", "tokens", "token_data", "tokenData"} {
		container, ok := metadata[containerKey]
		if !ok || container == nil {
			continue
		}
		if nested, ok := container.(map[string]any); ok {
			if str := codexAuthMetadataString(nested, keys...); str != "" {
				return str
			}
		}
	}
	return ""
}

func codexSubscriptionCacheKey(accessToken, proxyURL string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(strings.TrimSpace(proxyURL)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strings.TrimSpace(accessToken)))
	return hex.EncodeToString(h.Sum(nil))
}

func (h *Handler) codexSubscriptionProxyURL(auth *coreauth.Auth) string {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
		if proxyURL := codexAuthMetadataString(auth.Metadata, "proxy_url", "proxy-url", "proxyUrl"); proxyURL != "" {
			return proxyURL
		}
	}
	if h != nil && h.cfg != nil {
		return strings.TrimSpace(h.cfg.ProxyURL)
	}
	return ""
}

func applyCodexAccountSubscriptionInfo(auth *coreauth.Auth, info codexAccountSubscriptionInfo) *coreauth.Auth {
	if auth == nil || !info.hasData() {
		return auth
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	if info.SubscriptionExpiresAt != "" {
		updated.Metadata["subscription_expires_at"] = info.SubscriptionExpiresAt
	}
	if info.PlanType != "" && strings.TrimSpace(valueAsString(updated.Metadata["plan_type"])) == "" {
		updated.Metadata["plan_type"] = info.PlanType
	}
	if info.Email != "" && strings.TrimSpace(valueAsString(updated.Metadata["email"])) == "" {
		updated.Metadata["email"] = info.Email
	}
	return updated
}

func (info codexAccountSubscriptionInfo) hasData() bool {
	return strings.TrimSpace(info.PlanType) != "" || strings.TrimSpace(info.Email) != "" || strings.TrimSpace(info.SubscriptionExpiresAt) != ""
}

func resolveCodexAccountCheckOrgID(auth *coreauth.Auth, accessToken string) string {
	if auth != nil {
		for _, key := range []string{"organization_id", "organizationId", "org_id", "orgId", "poid"} {
			if value := codexAuthMetadataString(auth.Metadata, key); value != "" {
				return value
			}
		}
	}
	if claims := parseCodexJWTClaims(accessToken); claims != nil {
		if poid := strings.TrimSpace(claims.CodexAuthInfo.POID); poid != "" {
			return poid
		}
		if orgID := defaultCodexOrganizationID(claims); orgID != "" {
			return orgID
		}
		if accountID := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID); accountID != "" {
			return accountID
		}
	}
	if auth != nil {
		if idToken := codexAuthMetadataString(auth.Metadata, "id_token", "idToken"); idToken != "" {
			if claims := parseCodexJWTClaims(idToken); claims != nil {
				if poid := strings.TrimSpace(claims.CodexAuthInfo.POID); poid != "" {
					return poid
				}
				if orgID := defaultCodexOrganizationID(claims); orgID != "" {
					return orgID
				}
				if accountID := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID); accountID != "" {
					return accountID
				}
			}
		}
		if accountID := codexAuthMetadataString(auth.Metadata, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"); accountID != "" {
			return accountID
		}
	}
	return ""
}

func parseCodexJWTClaims(token string) *codex.JWTClaims {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	claims, err := codex.ParseJWTToken(token)
	if err != nil {
		return nil
	}
	return claims
}

func defaultCodexOrganizationID(claims *codex.JWTClaims) string {
	if claims == nil {
		return ""
	}
	for _, org := range claims.CodexAuthInfo.Organizations {
		if org.IsDefault && strings.TrimSpace(org.ID) != "" {
			return strings.TrimSpace(org.ID)
		}
	}
	for _, org := range claims.CodexAuthInfo.Organizations {
		if strings.TrimSpace(org.ID) != "" {
			return strings.TrimSpace(org.ID)
		}
	}
	return ""
}

func (h *Handler) fetchCodexAccountSubscriptionInfo(ctx context.Context, accessToken, proxyURL, orgID string) (*codexAccountSubscriptionInfo, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, codexAccountsCheckURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Dest", "empty")

	client := &http.Client{Timeout: 15 * time.Second}
	if h != nil && h.cfg != nil {
		sdkCfg := h.cfg.SDKConfig
		sdkCfg.ProxyURL = strings.TrimSpace(proxyURL)
		client = util.SetProxy(&sdkCfg, client)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("codex account check failed with status %d: %s", resp.StatusCode, truncateForLog(string(body), 200))
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return parseCodexAccountSubscriptionInfo(result, orgID), nil
}

func parseCodexAccountSubscriptionInfo(result map[string]any, orgID string) *codexAccountSubscriptionInfo {
	accounts, ok := result["accounts"].(map[string]any)
	if !ok || len(accounts) == 0 {
		return nil
	}
	orgID = strings.TrimSpace(orgID)
	if orgID != "" {
		if acct, ok := accountObject(accounts[orgID]); ok {
			info := codexAccountSubscriptionInfoFromAccount(acct)
			if info.hasData() {
				return &info
			}
		}
	}

	var defaultInfo, paidInfo, anyInfo codexAccountSubscriptionInfo
	for _, raw := range accounts {
		acct, ok := accountObject(raw)
		if !ok {
			continue
		}
		info := codexAccountSubscriptionInfoFromAccount(acct)
		if !info.hasData() {
			continue
		}
		if !anyInfo.hasData() {
			anyInfo = info
		}
		if isDefaultCodexAccount(acct) && !defaultInfo.hasData() {
			defaultInfo = info
		}
		if !strings.EqualFold(strings.TrimSpace(info.PlanType), "free") && !paidInfo.hasData() {
			paidInfo = info
		}
	}
	switch {
	case defaultInfo.hasData():
		return &defaultInfo
	case paidInfo.hasData():
		return &paidInfo
	case anyInfo.hasData():
		return &anyInfo
	default:
		return nil
	}
}

func accountObject(raw any) (map[string]any, bool) {
	acct, ok := raw.(map[string]any)
	return acct, ok
}

func codexAccountSubscriptionInfoFromAccount(acct map[string]any) codexAccountSubscriptionInfo {
	return codexAccountSubscriptionInfo{
		PlanType:              extractCodexAccountPlanType(acct),
		Email:                 extractCodexAccountEmail(acct),
		SubscriptionExpiresAt: extractCodexEntitlementExpiresAt(acct),
	}
}

func extractCodexAccountPlanType(acct map[string]any) string {
	if account, ok := acct["account"].(map[string]any); ok {
		if planType := strings.TrimSpace(valueAsString(account["plan_type"])); planType != "" {
			return planType
		}
		if planType := strings.TrimSpace(valueAsString(account["planType"])); planType != "" {
			return planType
		}
	}
	if entitlement, ok := acct["entitlement"].(map[string]any); ok {
		if planType := strings.TrimSpace(valueAsString(entitlement["subscription_plan"])); planType != "" {
			return planType
		}
		if planType := strings.TrimSpace(valueAsString(entitlement["subscriptionPlan"])); planType != "" {
			return planType
		}
	}
	return ""
}

func extractCodexAccountEmail(acct map[string]any) string {
	if account, ok := acct["account"].(map[string]any); ok {
		if email := strings.TrimSpace(valueAsString(account["email"])); email != "" {
			return email
		}
	}
	if user, ok := acct["user"].(map[string]any); ok {
		if email := strings.TrimSpace(valueAsString(user["email"])); email != "" {
			return email
		}
	}
	return ""
}

func extractCodexEntitlementExpiresAt(acct map[string]any) string {
	entitlement, ok := acct["entitlement"].(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"expires_at", "expiresAt"} {
		if value, ok := normalizeCodexSubscriptionUntilValue(entitlement[key]); ok {
			return strings.TrimSpace(valueAsString(value))
		}
	}
	return ""
}

func isDefaultCodexAccount(acct map[string]any) bool {
	account, ok := acct["account"].(map[string]any)
	if !ok {
		return false
	}
	value, ok := account["is_default"].(bool)
	if !ok {
		if camelValue, okCamel := account["isDefault"].(bool); okCamel {
			return camelValue
		}
		return false
	}
	return value
}

func truncateForLog(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func serializeAuthError(err *coreauth.Error) gin.H {
	if err == nil {
		return nil
	}
	entry := gin.H{}
	if code := strings.TrimSpace(err.Code); code != "" {
		entry["code"] = code
	}
	if message := strings.TrimSpace(err.Message); message != "" {
		entry["message"] = message
	}
	entry["retryable"] = err.Retryable
	if err.HTTPStatus > 0 {
		entry["http_status"] = err.HTTPStatus
	}
	if len(entry) == 0 {
		return nil
	}
	return entry
}

func applyAuthFileRuntimeStateEntry(entry gin.H, state coreauth.AuthRuntimeState, overwrite bool) {
	if entry == nil {
		return
	}
	set := func(key string, value any) {
		if overwrite {
			entry[key] = value
			return
		}
		if _, exists := entry[key]; !exists {
			entry[key] = value
		}
	}
	if state.Status != "" {
		set("status", state.Status)
	}
	if state.StatusMessage != "" {
		set("status_message", state.StatusMessage)
	}
	if state.Unavailable {
		set("unavailable", true)
	}
	if serialized := serializeAuthError(state.LastError); serialized != nil {
		set("last_error", serialized)
	}
	if serialized := serializeModelStates(state.ModelStates); len(serialized) > 0 {
		set("model_states", serialized)
	}
	if state.Success != 0 {
		set("success", state.Success)
	}
	if state.Failed != 0 {
		set("failed", state.Failed)
	}
	if !state.NextRetryAfter.IsZero() {
		set("next_retry_after", state.NextRetryAfter.UTC())
	}
	if !state.UpdatedAt.IsZero() {
		set("runtime_updated_at", state.UpdatedAt.UTC())
	}
	if !state.SavedAt.IsZero() {
		set("runtime_saved_at", state.SavedAt.UTC())
	}
	if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 || state.Quota.AuthScope {
		set("quota", gin.H{
			"exceeded":        state.Quota.Exceeded,
			"reason":          state.Quota.Reason,
			"next_recover_at": state.Quota.NextRecoverAt,
			"backoff_level":   state.Quota.BackoffLevel,
			"auth_scope":      state.Quota.AuthScope,
		})
	}
}

func serializeModelStates(states map[string]*coreauth.ModelState) map[string]gin.H {
	if len(states) == 0 {
		return nil
	}
	result := make(map[string]gin.H, len(states))
	for model, state := range states {
		model = strings.TrimSpace(model)
		if model == "" || state == nil {
			continue
		}
		entry := gin.H{
			"status":         state.Status,
			"status_message": state.StatusMessage,
			"unavailable":    state.Unavailable,
		}
		if !state.NextRetryAfter.IsZero() {
			entry["next_retry_after"] = state.NextRetryAfter
		}
		if serialized := serializeAuthError(state.LastError); serialized != nil {
			entry["last_error"] = serialized
		}
		if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 {
			entry["quota"] = gin.H{
				"exceeded":        state.Quota.Exceeded,
				"reason":          state.Quota.Reason,
				"next_recover_at": state.Quota.NextRecoverAt,
				"backoff_level":   state.Quota.BackoffLevel,
			}
		}
		if !state.UpdatedAt.IsZero() {
			entry["updated_at"] = state.UpdatedAt
		}
		result[model] = entry
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func authFileUserAgent(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if ua := strings.TrimSpace(auth.Attributes["header:User-Agent"]); ua != "" {
			return ua
		}
		if ua := strings.TrimSpace(auth.Attributes["user_agent"]); ua != "" {
			return ua
		}
		if ua := strings.TrimSpace(auth.Attributes["user-agent"]); ua != "" {
			return ua
		}
	}
	if auth.Metadata == nil {
		return ""
	}
	if raw, ok := auth.Metadata["user_agent"].(string); ok {
		if ua := strings.TrimSpace(raw); ua != "" {
			return ua
		}
	}
	if raw, ok := auth.Metadata["user-agent"].(string); ok {
		if ua := strings.TrimSpace(raw); ua != "" {
			return ua
		}
	}
	return ""
}

func authFileWebsockets(auth *coreauth.Auth) (bool, bool) {
	if auth == nil {
		return false, false
	}
	if auth.Attributes != nil {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			if value, err := strconv.ParseBool(raw); err == nil {
				return value, true
			}
		}
	}
	if auth.Metadata == nil {
		return false, false
	}
	if raw, ok := auth.Metadata["websockets"]; ok {
		switch value := raw.(type) {
		case bool:
			return value, true
		case string:
			if parsed, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
				return parsed, true
			}
		}
	}
	if raw, ok := auth.Metadata["websocket"]; ok {
		switch value := raw.(type) {
		case bool:
			return value, true
		case string:
			if parsed, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
				return parsed, true
			}
		}
	}
	return false, false
}

func extractCodexIDTokenClaims(auth *coreauth.Auth) gin.H {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return nil
	}
	result := gin.H{}
	if v := strings.TrimSpace(valueAsString(auth.Metadata["account_id"])); v != "" {
		result["chatgpt_account_id"] = v
	}
	if v := strings.TrimSpace(valueAsString(auth.Metadata["plan_type"])); v != "" {
		result["plan_type"] = v
	}
	if v, ok := auth.Metadata["chatgpt_subscription_active_start"]; ok && v != nil {
		result["chatgpt_subscription_active_start"] = v
	}
	if v, ok := auth.Metadata["chatgpt_subscription_active_until"]; ok && v != nil {
		result["chatgpt_subscription_active_until"] = v
	}
	if v, ok := codexSubscriptionUntilValue(auth.Metadata); ok {
		result["subscription_expires_at"] = v
	}

	idTokenRaw, ok := auth.Metadata["id_token"].(string)
	if !ok {
		if len(result) == 0 {
			return nil
		}
		return result
	}
	idToken := strings.TrimSpace(idTokenRaw)
	if idToken == "" {
		if len(result) == 0 {
			return nil
		}
		return result
	}
	claims, err := codex.ParseJWTToken(idToken)
	if err != nil || claims == nil {
		if len(result) == 0 {
			return nil
		}
		return result
	}
	if _, ok := result["chatgpt_account_id"]; !ok {
		if v := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID); v != "" {
			result["chatgpt_account_id"] = v
		}
	}
	if _, ok := result["plan_type"]; !ok {
		if v := strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType); v != "" {
			result["plan_type"] = v
		}
	}
	if _, ok := result["chatgpt_subscription_active_start"]; !ok {
		if v := claims.CodexAuthInfo.ChatgptSubscriptionActiveStart; v != nil {
			result["chatgpt_subscription_active_start"] = v
		}
	}
	if _, ok := result["chatgpt_subscription_active_until"]; !ok {
		if v := claims.CodexAuthInfo.ChatgptSubscriptionActiveUntil; v != nil {
			result["chatgpt_subscription_active_until"] = v
		}
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

func authEmail(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["email"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["email"]); v != "" {
			return v
		}
		if v := strings.TrimSpace(auth.Attributes["account_email"]); v != "" {
			return v
		}
	}
	return ""
}

func authProjectID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		for _, key := range []string{"project_id", "projectId"} {
			if v := strings.TrimSpace(valueAsString(auth.Metadata[key])); v != "" {
				return v
			}
		}
	}
	if auth.Attributes != nil {
		for _, key := range []string{"project_id", "projectId"} {
			if v := strings.TrimSpace(auth.Attributes[key]); v != "" {
				return v
			}
		}
	}
	return ""
}

func authAttribute(auth *coreauth.Auth, key string) string {
	if auth == nil || len(auth.Attributes) == 0 {
		return ""
	}
	return auth.Attributes[key]
}

func isRuntimeOnlyAuth(auth *coreauth.Auth) bool {
	if auth == nil || len(auth.Attributes) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["runtime_only"]), "true")
}

func isUnsafeAuthFileName(name string) bool {
	if strings.TrimSpace(name) == "" {
		return true
	}
	if strings.ContainsAny(name, "/\\") {
		return true
	}
	if filepath.VolumeName(name) != "" {
		return true
	}
	return false
}

func normalizeOptionalAuthFileName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		name += ".json"
	}
	if isUnsafeAuthFileName(name) {
		return "", fmt.Errorf("invalid auth file name")
	}
	return name, nil
}

// Download single auth file by name
func (h *Handler) DownloadAuthFile(c *gin.Context) {
	name := strings.TrimSpace(c.Query("name"))
	if isUnsafeAuthFileName(name) {
		c.JSON(400, gin.H{"error": "invalid name"})
		return
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		c.JSON(400, gin.H{"error": "name must end with .json"})
		return
	}
	full := filepath.Join(h.cfg.AuthDir, name)
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(404, gin.H{"error": "file not found"})
		} else {
			c.JSON(500, gin.H{"error": fmt.Sprintf("failed to read file: %v", err)})
		}
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", name))
	c.Data(200, "application/json", data)
}

// Upload auth file: multipart or raw JSON with ?name=
func (h *Handler) UploadAuthFile(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	ctx := c.Request.Context()

	fileHeaders, errMultipart := h.multipartAuthFileHeaders(c)
	if errMultipart != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid multipart form: %v", errMultipart)})
		return
	}
	if len(fileHeaders) == 1 {
		if _, errUpload := h.storeUploadedAuthFile(ctx, fileHeaders[0]); errUpload != nil {
			if errors.Is(errUpload, errAuthFileMustBeJSON) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "file must be .json"})
				return
			}
			if errors.Is(errUpload, util.ErrResponseBodyTooLarge) {
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": fmt.Sprintf("auth file exceeds maximum allowed size of %d bytes", maxAuthFileUploadBytes)})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": errUpload.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if len(fileHeaders) > 1 {
		uploaded := make([]string, 0, len(fileHeaders))
		failed := make([]gin.H, 0)
		for _, file := range fileHeaders {
			name, errUpload := h.storeUploadedAuthFile(ctx, file)
			if errUpload != nil {
				failureName := ""
				if file != nil {
					failureName = filepath.Base(file.Filename)
				}
				msg := errUpload.Error()
				if errors.Is(errUpload, errAuthFileMustBeJSON) {
					msg = "file must be .json"
				} else if errors.Is(errUpload, util.ErrResponseBodyTooLarge) {
					msg = fmt.Sprintf("auth file exceeds maximum allowed size of %d bytes", maxAuthFileUploadBytes)
				}
				failed = append(failed, gin.H{"name": failureName, "error": msg})
				continue
			}
			uploaded = append(uploaded, name)
		}
		if len(failed) > 0 {
			c.JSON(http.StatusMultiStatus, gin.H{
				"status":   "partial",
				"uploaded": len(uploaded),
				"files":    uploaded,
				"failed":   failed,
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "uploaded": len(uploaded), "files": uploaded})
		return
	}
	if c.ContentType() == "multipart/form-data" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no files uploaded"})
		return
	}
	name := strings.TrimSpace(c.Query("name"))
	if isUnsafeAuthFileName(name) {
		c.JSON(400, gin.H{"error": "invalid name"})
		return
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		c.JSON(400, gin.H{"error": "name must end with .json"})
		return
	}
	data, err := util.ReadResponseBodyLimited(c.Request.Body, maxAuthFileUploadBytes)
	if err != nil {
		if errors.Is(err, util.ErrResponseBodyTooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": fmt.Sprintf("auth file exceeds maximum allowed size of %d bytes", maxAuthFileUploadBytes)})
			return
		}
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	if err = h.writeAuthFile(ctx, filepath.Base(name), data); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

// Delete auth files: single by name or all
func (h *Handler) DeleteAuthFile(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	ctx := c.Request.Context()
	if all := c.Query("all"); all == "true" || all == "1" || all == "*" {
		entries, err := os.ReadDir(h.cfg.AuthDir)
		if err != nil {
			c.JSON(500, gin.H{"error": fmt.Sprintf("failed to read auth dir: %v", err)})
			return
		}
		deleted := 0
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(strings.ToLower(name), ".json") {
				continue
			}
			full := filepath.Join(h.cfg.AuthDir, name)
			if !filepath.IsAbs(full) {
				if abs, errAbs := filepath.Abs(full); errAbs == nil {
					full = abs
				}
			}
			removeID, restoreAuth := h.disableAuth(ctx, full)
			if removeID == "" {
				removeID = full
			}
			if errDel := h.deleteTokenRecord(ctx, full); errDel != nil {
				h.restoreAuth(ctx, restoreAuth)
				c.JSON(500, gin.H{"error": errDel.Error()})
				return
			}
			if err = os.Remove(full); err != nil && !os.IsNotExist(err) {
				h.restoreAuth(ctx, restoreAuth)
				c.JSON(500, gin.H{"error": fmt.Sprintf("failed to remove file: %v", err)})
				return
			}
			h.removeAuth(ctx, removeID)
			deleted++
		}
		c.JSON(200, gin.H{"status": "ok", "deleted": deleted})
		return
	}

	names, errNames := requestedAuthFileNamesForDelete(c)
	if errNames != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errNames.Error()})
		return
	}
	if len(names) == 0 {
		c.JSON(400, gin.H{"error": "invalid name"})
		return
	}
	if len(names) == 1 {
		if _, status, errDelete := h.deleteAuthFileByName(ctx, names[0]); errDelete != nil {
			c.JSON(status, gin.H{"error": errDelete.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}

	deletedFiles := make([]string, 0, len(names))
	failed := make([]gin.H, 0)
	for _, name := range names {
		deletedName, _, errDelete := h.deleteAuthFileByName(ctx, name)
		if errDelete != nil {
			failed = append(failed, gin.H{"name": name, "error": errDelete.Error()})
			continue
		}
		deletedFiles = append(deletedFiles, deletedName)
	}
	if len(failed) > 0 {
		c.JSON(http.StatusMultiStatus, gin.H{
			"status":  "partial",
			"deleted": len(deletedFiles),
			"files":   deletedFiles,
			"failed":  failed,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "deleted": len(deletedFiles), "files": deletedFiles})
}

func (h *Handler) multipartAuthFileHeaders(c *gin.Context) ([]*multipart.FileHeader, error) {
	if h == nil || c == nil || c.ContentType() != "multipart/form-data" {
		return nil, nil
	}
	form, err := c.MultipartForm()
	if err != nil {
		return nil, err
	}
	if form == nil || len(form.File) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(form.File))
	for key := range form.File {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	headers := make([]*multipart.FileHeader, 0)
	for _, key := range keys {
		headers = append(headers, form.File[key]...)
	}
	return headers, nil
}

func (h *Handler) storeUploadedAuthFile(ctx context.Context, file *multipart.FileHeader) (string, error) {
	if file == nil {
		return "", fmt.Errorf("no file uploaded")
	}
	name := filepath.Base(strings.TrimSpace(file.Filename))
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		return "", errAuthFileMustBeJSON
	}
	src, err := file.Open()
	if err != nil {
		return "", fmt.Errorf("failed to open uploaded file: %w", err)
	}
	defer src.Close()

	data, err := util.ReadResponseBodyLimited(src, maxAuthFileUploadBytes)
	if err != nil {
		if errors.Is(err, util.ErrResponseBodyTooLarge) {
			return "", fmt.Errorf("uploaded auth file exceeds maximum allowed size of %d bytes: %w", maxAuthFileUploadBytes, err)
		}
		return "", fmt.Errorf("failed to read uploaded file: %w", err)
	}
	if err := h.writeAuthFile(ctx, name, data); err != nil {
		return "", err
	}
	return name, nil
}

func (h *Handler) writeAuthFile(ctx context.Context, name string, data []byte) error {
	dst := filepath.Join(h.cfg.AuthDir, filepath.Base(name))
	if !filepath.IsAbs(dst) {
		if abs, errAbs := filepath.Abs(dst); errAbs == nil {
			dst = abs
		}
	}
	auth, err := h.buildAuthFromFileData(dst, data)
	if err != nil {
		return err
	}
	dataToWrite, _, errNormalize := normalizeImportedAuthJSON(data)
	if errNormalize != nil {
		return fmt.Errorf("invalid auth file: %w", errNormalize)
	}
	if errWrite := os.WriteFile(dst, dataToWrite, 0o600); errWrite != nil {
		return fmt.Errorf("failed to write file: %w", errWrite)
	}
	if err := h.upsertAuthRecord(ctx, auth); err != nil {
		return err
	}
	return nil
}

func (h *Handler) authIDForPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		if abs, errAbs := filepath.Abs(path); errAbs == nil {
			path = abs
		}
	}
	id := path
	if h != nil && h.cfg != nil {
		authDir := strings.TrimSpace(h.cfg.AuthDir)
		if resolvedAuthDir, errResolve := util.ResolveAuthDir(authDir); errResolve == nil && resolvedAuthDir != "" {
			authDir = resolvedAuthDir
		}
		if authDir != "" {
			authDir = filepath.Clean(authDir)
			if !filepath.IsAbs(authDir) {
				if abs, errAbs := filepath.Abs(authDir); errAbs == nil {
					authDir = abs
				}
			}
			if rel, errRel := filepath.Rel(authDir, path); errRel == nil && rel != "" {
				id = rel
			}
		}
	}
	// On Windows, normalize ID casing to avoid duplicate auth entries caused by case-insensitive paths.
	if runtime.GOOS == "windows" {
		id = strings.ToLower(id)
	}
	return id
}

func (h *Handler) registerAuthFromFile(ctx context.Context, path string, data []byte) error {
	if h.authManager == nil {
		return nil
	}
	auth, err := h.buildAuthFromFileData(path, data)
	if err != nil {
		return err
	}
	return h.upsertAuthRecord(ctx, auth)
}

func (h *Handler) buildAuthFromFileData(path string, data []byte) (*coreauth.Auth, error) {
	if path == "" {
		return nil, fmt.Errorf("auth path is empty")
	}
	if data == nil {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read auth file: %w", err)
		}
	}
	normalizedData, _, errNormalize := normalizeImportedAuthJSON(data)
	if errNormalize != nil {
		return nil, fmt.Errorf("invalid auth file: %w", errNormalize)
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(normalizedData, &metadata); err != nil {
		return nil, fmt.Errorf("invalid auth file: %w", err)
	}
	provider, _ := metadata["type"].(string)
	if provider == "" {
		provider = "unknown"
	}
	label := provider
	if email, ok := metadata["email"].(string); ok && email != "" {
		label = email
	}
	lastRefresh, hasLastRefresh := extractLastRefreshTimestamp(metadata)

	authID := h.authIDForPath(path)
	if authID == "" {
		authID = path
	}
	attr := map[string]string{
		"path":   path,
		"source": path,
	}
	auth := &coreauth.Auth{
		ID:         authID,
		Provider:   provider,
		FileName:   filepath.Base(path),
		Label:      label,
		Status:     coreauth.StatusActive,
		Attributes: attr,
		Metadata:   metadata,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if hasLastRefresh {
		auth.LastRefreshedAt = lastRefresh
	}
	if h != nil && h.authManager != nil {
		if existing, ok := h.authManager.GetByID(authID); ok {
			auth.CreatedAt = existing.CreatedAt
			if !hasLastRefresh {
				auth.LastRefreshedAt = existing.LastRefreshedAt
			}
			auth.NextRefreshAfter = existing.NextRefreshAfter
			auth.Runtime = existing.Runtime
		}
	}
	coreauth.ApplyAuthFileOptionsFromMetadata(auth)
	coreauth.ApplyCodexMetadataFromMetadata(auth)
	coreauth.ApplyCustomHeadersFromMetadata(auth)
	return auth, nil
}

func normalizeImportedAuthJSON(data []byte) ([]byte, bool, error) {
	metadata := make(map[string]any)
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, false, err
	}
	normalized, changed := coreauth.NormalizeImportedAuthMetadata(metadata)
	if !changed {
		return data, false, nil
	}
	normalizedData, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return nil, false, err
	}
	normalizedData = append(normalizedData, '\n')
	return normalizedData, true, nil
}

func (h *Handler) upsertAuthRecord(ctx context.Context, auth *coreauth.Auth) error {
	if h == nil || h.authManager == nil || auth == nil {
		return nil
	}
	if existing, ok := h.authManager.GetByID(auth.ID); ok {
		auth.CreatedAt = existing.CreatedAt
		_, err := h.authManager.Update(ctx, auth)
		return err
	}
	_, err := h.authManager.Register(ctx, auth)
	return err
}

type patchAuthFileFieldsRequest struct {
	Name                 string            `json:"name"`
	Prefix               *string           `json:"prefix"`
	ProxyURL             *string           `json:"proxy_url"`
	ProxyURLLegacy       *string           `json:"proxy-url"`
	ProxyURLCamel        *string           `json:"proxyUrl"`
	Headers              map[string]string `json:"headers"`
	Priority             json.RawMessage   `json:"priority"`
	Note                 *string           `json:"note"`
	UserAgent            *string           `json:"user_agent"`
	UserAgentCamel       *string           `json:"userAgent"`
	ExcludedModels       *[]string         `json:"excluded_models"`
	ExcludedModelsLegacy *[]string         `json:"excluded-models"`
	ExcludedModelsCamel  *[]string         `json:"excludedModels"`
	DisableCooling       json.RawMessage   `json:"disable_cooling"`
	DisableCoolingLegacy json.RawMessage   `json:"disable-cooling"`
	DisableCoolingCamel  json.RawMessage   `json:"disableCooling"`
	Websockets           *bool             `json:"websockets"`
}

func (req patchAuthFileFieldsRequest) resolvedProxyURL() *string {
	if req.ProxyURL != nil {
		return req.ProxyURL
	}
	if req.ProxyURLLegacy != nil {
		return req.ProxyURLLegacy
	}
	return req.ProxyURLCamel
}

func (req patchAuthFileFieldsRequest) resolvedUserAgent() *string {
	if req.UserAgent != nil {
		return req.UserAgent
	}
	return req.UserAgentCamel
}

func (req patchAuthFileFieldsRequest) resolvedExcludedModels() *[]string {
	if req.ExcludedModels != nil {
		return req.ExcludedModels
	}
	if req.ExcludedModelsLegacy != nil {
		return req.ExcludedModelsLegacy
	}
	return req.ExcludedModelsCamel
}

func resolvePatchAuthFilePath(targetAuth *coreauth.Auth, authDir, fallbackName string) string {
	candidates := make([]string, 0, 2)
	if path := strings.TrimSpace(authAttribute(targetAuth, "path")); path != "" {
		candidates = append(candidates, path)
	}
	if strings.TrimSpace(authDir) != "" && !isUnsafeAuthFileName(fallbackName) {
		candidates = append(candidates, filepath.Join(authDir, filepath.Base(fallbackName)))
	}

	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if !filepath.IsAbs(candidate) {
			if abs, err := filepath.Abs(candidate); err == nil {
				candidate = abs
			}
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	return ""
}

func applyPatchAuthFileDocument(
	doc map[string]any,
	req patchAuthFileFieldsRequest,
	priorityPresent bool,
	prioritySet bool,
	priorityValue int,
	disableCoolingPresent bool,
	disableCoolingSet bool,
	disableCoolingValue bool,
) {
	if doc == nil {
		return
	}

	if req.Prefix != nil {
		prefix := strings.TrimSpace(*req.Prefix)
		if prefix == "" {
			delete(doc, "prefix")
		} else {
			doc["prefix"] = prefix
		}
	}
	if reqProxyURL := req.resolvedProxyURL(); reqProxyURL != nil {
		proxyURL := strings.TrimSpace(*reqProxyURL)
		delete(doc, "proxy-url")
		delete(doc, "proxyUrl")
		if proxyURL == "" {
			delete(doc, "proxy_url")
		} else {
			doc["proxy_url"] = proxyURL
		}
	}
	if len(req.Headers) > 0 {
		nextHeaders := coreauth.ExtractCustomHeadersFromMetadata(doc)
		if nextHeaders == nil {
			nextHeaders = make(map[string]string)
		}
		for key, value := range req.Headers {
			name := strings.TrimSpace(key)
			if name == "" {
				continue
			}
			val := strings.TrimSpace(value)
			if val == "" {
				delete(nextHeaders, name)
				continue
			}
			nextHeaders[name] = val
		}
		if len(nextHeaders) == 0 {
			delete(doc, "headers")
		} else {
			doc["headers"] = nextHeaders
		}
	}
	if priorityPresent {
		if !prioritySet {
			delete(doc, "priority")
		} else {
			doc["priority"] = priorityValue
		}
	}
	if req.Note != nil {
		note := strings.TrimSpace(*req.Note)
		if note == "" {
			delete(doc, "note")
		} else {
			doc["note"] = note
		}
	}
	if reqUserAgent := req.resolvedUserAgent(); reqUserAgent != nil {
		userAgent := strings.TrimSpace(*reqUserAgent)
		delete(doc, "user-agent")
		delete(doc, "userAgent")
		if userAgent == "" {
			delete(doc, "user_agent")
		} else {
			doc["user_agent"] = userAgent
		}
	}
	if reqExcludedModels := req.resolvedExcludedModels(); reqExcludedModels != nil {
		models := normalizeExcludedModelsInput(*reqExcludedModels)
		delete(doc, "excluded-models")
		delete(doc, "excludedModels")
		if len(models) == 0 {
			delete(doc, "excluded_models")
		} else {
			doc["excluded_models"] = models
		}
	}
	if disableCoolingPresent {
		delete(doc, "disable-cooling")
		delete(doc, "disableCooling")
		if !disableCoolingSet {
			delete(doc, "disable_cooling")
		} else {
			doc["disable_cooling"] = disableCoolingValue
		}
	}
	if req.Websockets != nil {
		delete(doc, "websocket")
		doc["websockets"] = *req.Websockets
	}
}

func (h *Handler) persistPatchedAuthFile(
	targetAuth *coreauth.Auth,
	req patchAuthFileFieldsRequest,
	priorityPresent bool,
	prioritySet bool,
	priorityValue int,
	disableCoolingPresent bool,
	disableCoolingSet bool,
	disableCoolingValue bool,
) error {
	if h == nil || targetAuth == nil {
		return nil
	}

	authDir := ""
	if h.cfg != nil {
		authDir = h.cfg.AuthDir
	}
	path := resolvePatchAuthFilePath(targetAuth, authDir, req.Name)
	if path == "" {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read auth file: %w", err)
	}

	doc := make(map[string]any)
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("invalid auth file: %w", err)
	}

	applyPatchAuthFileDocument(
		doc,
		req,
		priorityPresent,
		prioritySet,
		priorityValue,
		disableCoolingPresent,
		disableCoolingSet,
		disableCoolingValue,
	)

	updated, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode auth file: %w", err)
	}
	updated = append(updated, '\n')

	if err := os.WriteFile(path, updated, 0o600); err != nil {
		return fmt.Errorf("failed to write auth file: %w", err)
	}
	return nil
}

// PatchAuthFileStatus toggles the disabled state of an auth file
func (h *Handler) PatchAuthFileStatus(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Name     string `json:"name"`
		Disabled *bool  `json:"disabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if req.Disabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "disabled is required"})
		return
	}

	ctx := c.Request.Context()

	// Find auth by name or ID
	var targetAuth *coreauth.Auth
	if auth, ok := h.authManager.GetByID(name); ok {
		targetAuth = auth
	} else {
		auths := h.authManager.List()
		for _, auth := range auths {
			if auth.FileName == name {
				targetAuth = auth
				break
			}
		}
	}

	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}

	// Update disabled state
	targetAuth.Disabled = *req.Disabled
	if *req.Disabled {
		targetAuth.Status = coreauth.StatusDisabled
		targetAuth.StatusMessage = "disabled via management API"
	} else {
		targetAuth.Status = coreauth.StatusActive
		targetAuth.StatusMessage = ""
	}
	targetAuth.UpdatedAt = time.Now()

	if _, err := h.authManager.Update(ctx, targetAuth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "disabled": *req.Disabled})
}

// PatchAuthFileFields updates editable fields on an auth file without re-uploading the whole JSON file.
func (h *Handler) PatchAuthFileFields(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req patchAuthFileFieldsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	ctx := c.Request.Context()

	// Find auth by name or ID
	var targetAuth *coreauth.Auth
	if auth, ok := h.authManager.GetByID(name); ok {
		targetAuth = auth
	} else {
		auths := h.authManager.List()
		for _, auth := range auths {
			if auth.FileName == name {
				targetAuth = auth
				break
			}
		}
	}

	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}

	priorityPresent, prioritySet, priorityValue, err := parseOptionalJSONIntField(req.Priority)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	reqProxyURL := req.resolvedProxyURL()
	reqUserAgent := req.resolvedUserAgent()
	reqExcludedModels := req.resolvedExcludedModels()
	disableCoolingRaw := req.DisableCooling
	if len(disableCoolingRaw) == 0 {
		disableCoolingRaw = req.DisableCoolingLegacy
	}
	if len(disableCoolingRaw) == 0 {
		disableCoolingRaw = req.DisableCoolingCamel
	}
	disableCoolingPresent, disableCoolingSet, disableCoolingValue, err := parseOptionalJSONBoolField(disableCoolingRaw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	changed := false
	if req.Prefix != nil {
		prefix := strings.TrimSpace(*req.Prefix)
		targetAuth.Prefix = prefix
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		if prefix == "" {
			delete(targetAuth.Metadata, "prefix")
		} else {
			targetAuth.Metadata["prefix"] = prefix
		}
		changed = true
	}
	if reqProxyURL != nil {
		proxyURL := strings.TrimSpace(*reqProxyURL)
		targetAuth.ProxyURL = proxyURL
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		delete(targetAuth.Metadata, "proxy-url")
		delete(targetAuth.Metadata, "proxyUrl")
		if proxyURL == "" {
			delete(targetAuth.Metadata, "proxy_url")
		} else {
			targetAuth.Metadata["proxy_url"] = proxyURL
		}
		changed = true
	}
	if len(req.Headers) > 0 {
		existingHeaders := coreauth.ExtractCustomHeadersFromMetadata(targetAuth.Metadata)
		nextHeaders := make(map[string]string, len(existingHeaders))
		for k, v := range existingHeaders {
			nextHeaders[k] = v
		}
		headerChanged := false

		for key, value := range req.Headers {
			name := strings.TrimSpace(key)
			if name == "" {
				continue
			}
			val := strings.TrimSpace(value)
			attrKey := "header:" + name
			if val == "" {
				if _, ok := nextHeaders[name]; ok {
					delete(nextHeaders, name)
					headerChanged = true
				}
				if targetAuth.Attributes != nil {
					if _, ok := targetAuth.Attributes[attrKey]; ok {
						headerChanged = true
					}
				}
				continue
			}
			if prev, ok := nextHeaders[name]; !ok || prev != val {
				headerChanged = true
			}
			nextHeaders[name] = val
			if targetAuth.Attributes != nil {
				if prev, ok := targetAuth.Attributes[attrKey]; !ok || prev != val {
					headerChanged = true
				}
			} else {
				headerChanged = true
			}
		}

		if headerChanged {
			if targetAuth.Metadata == nil {
				targetAuth.Metadata = make(map[string]any)
			}
			if targetAuth.Attributes == nil {
				targetAuth.Attributes = make(map[string]string)
			}

			for key, value := range req.Headers {
				name := strings.TrimSpace(key)
				if name == "" {
					continue
				}
				val := strings.TrimSpace(value)
				attrKey := "header:" + name
				if val == "" {
					delete(nextHeaders, name)
					delete(targetAuth.Attributes, attrKey)
					continue
				}
				nextHeaders[name] = val
				targetAuth.Attributes[attrKey] = val
			}

			if len(nextHeaders) == 0 {
				delete(targetAuth.Metadata, "headers")
			} else {
				metaHeaders := make(map[string]any, len(nextHeaders))
				for k, v := range nextHeaders {
					metaHeaders[k] = v
				}
				targetAuth.Metadata["headers"] = metaHeaders
			}
			changed = true
		}
	}
	if priorityPresent || req.Note != nil || reqUserAgent != nil || disableCoolingPresent || req.Websockets != nil {
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		if targetAuth.Attributes == nil {
			targetAuth.Attributes = make(map[string]string)
		}

		if priorityPresent {
			if !prioritySet {
				delete(targetAuth.Metadata, "priority")
				delete(targetAuth.Attributes, "priority")
			} else {
				targetAuth.Metadata["priority"] = priorityValue
				targetAuth.Attributes["priority"] = strconv.Itoa(priorityValue)
			}
		}
		if req.Note != nil {
			trimmedNote := strings.TrimSpace(*req.Note)
			if trimmedNote == "" {
				delete(targetAuth.Metadata, "note")
				delete(targetAuth.Attributes, "note")
			} else {
				targetAuth.Metadata["note"] = trimmedNote
				targetAuth.Attributes["note"] = trimmedNote
			}
		}
		if reqUserAgent != nil {
			trimmedUserAgent := strings.TrimSpace(*reqUserAgent)
			if trimmedUserAgent == "" {
				delete(targetAuth.Metadata, "user_agent")
				delete(targetAuth.Metadata, "user-agent")
				delete(targetAuth.Metadata, "userAgent")
				delete(targetAuth.Attributes, "header:User-Agent")
				delete(targetAuth.Attributes, "user_agent")
				delete(targetAuth.Attributes, "user-agent")
				delete(targetAuth.Attributes, "userAgent")
			} else {
				targetAuth.Metadata["user_agent"] = trimmedUserAgent
				delete(targetAuth.Metadata, "user-agent")
				delete(targetAuth.Metadata, "userAgent")
				targetAuth.Attributes["header:User-Agent"] = trimmedUserAgent
				delete(targetAuth.Attributes, "user_agent")
				delete(targetAuth.Attributes, "user-agent")
				delete(targetAuth.Attributes, "userAgent")
			}
		}
		if disableCoolingPresent {
			delete(targetAuth.Metadata, "disable-cooling")
			delete(targetAuth.Metadata, "disableCooling")
			if disableCoolingSet {
				targetAuth.Metadata["disable_cooling"] = disableCoolingValue
			} else {
				delete(targetAuth.Metadata, "disable_cooling")
			}
		}
		if req.Websockets != nil {
			delete(targetAuth.Metadata, "websocket")
			targetAuth.Metadata["websockets"] = *req.Websockets
			targetAuth.Attributes["websockets"] = strconv.FormatBool(*req.Websockets)
		}
		changed = true
	}
	if reqExcludedModels != nil {
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		normalizedExcludedModels := normalizeExcludedModelsInput(*reqExcludedModels)
		delete(targetAuth.Metadata, "excluded-models")
		delete(targetAuth.Metadata, "excludedModels")
		if len(normalizedExcludedModels) == 0 {
			delete(targetAuth.Metadata, "excluded_models")
		} else {
			targetAuth.Metadata["excluded_models"] = normalizedExcludedModels
		}
		resetExcludedModelsAttributes(targetAuth)
		synthesizer.ApplyAuthExcludedModelsMeta(targetAuth, h.cfg, extractExcludedModelsFromMetadata(targetAuth.Metadata), "oauth")
		changed = true
	}

	if !changed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}
	if err := h.persistPatchedAuthFile(
		targetAuth,
		req,
		priorityPresent,
		prioritySet,
		priorityValue,
		disableCoolingPresent,
		disableCoolingSet,
		disableCoolingValue,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	targetAuth.UpdatedAt = time.Now()

	updatedAuth, err := h.authManager.Update(ctx, targetAuth)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", err)})
		return
	}
	h.syncVirtualAuthChildren(ctx, updatedAuth)

	c.JSON(http.StatusOK, gin.H{"status": "ok", "file": h.buildAuthFileEntry(updatedAuth)})
}

func parseOptionalJSONIntField(raw json.RawMessage) (present bool, set bool, value int, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false, false, 0, nil
	}
	present = true
	if bytes.Equal(trimmed, []byte("null")) {
		return true, false, 0, nil
	}

	var parsedInt int
	if err = json.Unmarshal(trimmed, &parsedInt); err == nil {
		return true, true, parsedInt, nil
	}

	var parsedText string
	if err = json.Unmarshal(trimmed, &parsedText); err != nil {
		return true, false, 0, fmt.Errorf("priority must be an integer")
	}
	parsedText = strings.TrimSpace(parsedText)
	if parsedText == "" {
		return true, false, 0, nil
	}
	parsedInt, err = strconv.Atoi(parsedText)
	if err != nil {
		return true, false, 0, fmt.Errorf("priority must be an integer")
	}
	return true, true, parsedInt, nil
}

func parseOptionalJSONBoolField(raw json.RawMessage) (present bool, set bool, value bool, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false, false, false, nil
	}
	present = true
	if bytes.Equal(trimmed, []byte("null")) {
		return true, false, false, nil
	}

	if err = json.Unmarshal(trimmed, &value); err == nil {
		return true, true, value, nil
	}

	var parsedText string
	if err = json.Unmarshal(trimmed, &parsedText); err != nil {
		return true, false, false, fmt.Errorf("disable_cooling must be true, false, or empty")
	}
	parsedText = strings.TrimSpace(parsedText)
	if parsedText == "" {
		return true, false, false, nil
	}
	value, err = strconv.ParseBool(parsedText)
	if err != nil {
		return true, false, false, fmt.Errorf("disable_cooling must be true, false, or empty")
	}
	return true, true, value, nil
}

func normalizeExcludedModelsInput(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	normalized := make([]string, 0, len(models))
	for _, model := range models {
		key := strings.ToLower(strings.TrimSpace(model))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, key)
	}
	sort.Strings(normalized)
	return normalized
}

func extractExcludedModelsFromMetadata(metadata map[string]any) []string {
	if metadata == nil {
		return nil
	}

	var raw any
	switch {
	case metadata["excluded_models"] != nil:
		raw = metadata["excluded_models"]
	case metadata["excluded-models"] != nil:
		raw = metadata["excluded-models"]
	default:
		return nil
	}

	switch values := raw.(type) {
	case []string:
		return normalizeExcludedModelsInput(values)
	case []any:
		items := make([]string, 0, len(values))
		for _, value := range values {
			items = append(items, fmt.Sprintf("%v", value))
		}
		return normalizeExcludedModelsInput(items)
	case string:
		return normalizeExcludedModelsInput(strings.Split(values, ","))
	default:
		return nil
	}
}

func resetExcludedModelsAttributes(auth *coreauth.Auth) {
	if auth == nil || auth.Attributes == nil {
		return
	}
	delete(auth.Attributes, "excluded_models")
	delete(auth.Attributes, "excluded_models_hash")
}

func (h *Handler) syncVirtualAuthChildren(ctx context.Context, primary *coreauth.Auth) {
	if h == nil || h.authManager == nil || primary == nil {
		return
	}
	if strings.TrimSpace(primary.Attributes["gemini_virtual_primary"]) == "" {
		return
	}

	perAccountExcluded := extractExcludedModelsFromMetadata(primary.Metadata)
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		if strings.TrimSpace(auth.Attributes["gemini_virtual_parent"]) != primary.ID {
			continue
		}
		auth.Prefix = primary.Prefix
		auth.ProxyURL = primary.ProxyURL
		auth.UpdatedAt = primary.UpdatedAt
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string)
		}
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}

		if priority := strings.TrimSpace(primary.Attributes["priority"]); priority != "" {
			auth.Attributes["priority"] = priority
		} else {
			delete(auth.Attributes, "priority")
		}
		if note := strings.TrimSpace(primary.Attributes["note"]); note != "" {
			auth.Attributes["note"] = note
		} else {
			delete(auth.Attributes, "note")
		}

		for key := range auth.Attributes {
			if strings.HasPrefix(key, "header:") {
				delete(auth.Attributes, key)
			}
		}
		for key, value := range primary.Attributes {
			if strings.HasPrefix(key, "header:") && strings.TrimSpace(value) != "" {
				auth.Attributes[key] = value
			}
		}

		delete(auth.Metadata, "disable-cooling")
		if value, ok := primary.Metadata["disable_cooling"]; ok {
			auth.Metadata["disable_cooling"] = value
		} else {
			delete(auth.Metadata, "disable_cooling")
		}

		resetExcludedModelsAttributes(auth)
		synthesizer.ApplyAuthExcludedModelsMeta(auth, h.cfg, perAccountExcluded, "oauth")
		_, _ = h.authManager.Update(ctx, auth)
	}
}

func (h *Handler) tokenStoreWithBaseDir() coreauth.Store {
	if h == nil {
		return nil
	}
	store := h.tokenStore
	if store == nil {
		store = sdkAuth.GetTokenStore()
		h.tokenStore = store
	}
	if h.cfg != nil {
		if dirSetter, ok := store.(interface{ SetBaseDir(string) }); ok {
			dirSetter.SetBaseDir(h.cfg.AuthDir)
		}
	}
	return store
}

func (h *Handler) saveTokenRecord(ctx context.Context, record *coreauth.Auth) (string, error) {
	if record == nil {
		return "", fmt.Errorf("token record is nil")
	}
	store := h.tokenStoreWithBaseDir()
	if store == nil {
		return "", fmt.Errorf("token store unavailable")
	}
	if h.postAuthHook != nil {
		if err := h.postAuthHook(ctx, record); err != nil {
			return "", fmt.Errorf("post-auth hook failed: %w", err)
		}
	}
	return store.Save(ctx, record)
}

func (h *Handler) RequestAnthropicToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing Claude authentication...")

	// Generate PKCE codes
	pkceCodes, err := claude.GeneratePKCECodes()
	if err != nil {
		log.Errorf("Failed to generate PKCE codes: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		return
	}

	// Generate random state parameter
	state, err := misc.GenerateRandomState()
	if err != nil {
		log.Errorf("Failed to generate state parameter: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}

	// Initialize Claude auth service
	anthropicAuth := claude.NewClaudeAuth(h.cfg)

	// Generate authorization URL (then override redirect_uri to reuse server port)
	authURL, state, err := anthropicAuth.GenerateAuthURL(state, pkceCodes)
	if err != nil {
		log.Errorf("Failed to generate authorization URL: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	RegisterOAuthSession(state, "anthropic")

	isWebUI := isWebUIRequest(c)
	var forwarder *callbackForwarder
	if isWebUI {
		targetURL, errTarget := h.managementCallbackURL("/anthropic/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute anthropic callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(anthropicCallbackPort, "anthropic", targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start anthropic callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(anthropicCallbackPort, forwarder)
		}

		// Helper: wait for callback file
		waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-anthropic-%s.oauth", state))
		waitForFile := func(path string, timeout time.Duration) (map[string]string, error) {
			deadline := time.Now().Add(timeout)
			for {
				if !IsOAuthSessionPending(state, "anthropic") {
					return nil, errOAuthSessionNotPending
				}
				if time.Now().After(deadline) {
					SetOAuthSessionError(state, "Timeout waiting for OAuth callback")
					return nil, fmt.Errorf("timeout waiting for OAuth callback")
				}
				data, errRead := os.ReadFile(path)
				if errRead == nil {
					var m map[string]string
					_ = json.Unmarshal(data, &m)
					_ = os.Remove(path)
					return m, nil
				}
				time.Sleep(500 * time.Millisecond)
			}
		}

		fmt.Println("Waiting for authentication callback...")
		// Wait up to 5 minutes
		resultMap, errWait := waitForFile(waitFile, 5*time.Minute)
		if errWait != nil {
			if errors.Is(errWait, errOAuthSessionNotPending) {
				return
			}
			authErr := claude.NewAuthenticationError(claude.ErrCallbackTimeout, errWait)
			log.Error(claude.GetUserFriendlyMessage(authErr))
			return
		}
		if errStr := resultMap["error"]; errStr != "" {
			oauthErr := claude.NewOAuthError(errStr, "", http.StatusBadRequest)
			log.Error(claude.GetUserFriendlyMessage(oauthErr))
			SetOAuthSessionError(state, "Bad request")
			return
		}
		if resultMap["state"] != state {
			authErr := claude.NewAuthenticationError(claude.ErrInvalidState, fmt.Errorf("expected %s, got %s", state, resultMap["state"]))
			log.Error(claude.GetUserFriendlyMessage(authErr))
			SetOAuthSessionError(state, "State code error")
			return
		}

		// Parse code (Claude may append state after '#')
		rawCode := resultMap["code"]
		code := strings.Split(rawCode, "#")[0]

		// Exchange code for tokens using internal auth service
		bundle, errExchange := anthropicAuth.ExchangeCodeForTokens(ctx, code, state, pkceCodes)
		if errExchange != nil {
			authErr := claude.NewAuthenticationError(claude.ErrCodeExchangeFailed, errExchange)
			log.Errorf("Failed to exchange authorization code for tokens: %v", authErr)
			SetOAuthSessionError(state, "Failed to exchange authorization code for tokens")
			return
		}

		// Create token storage
		tokenStorage := anthropicAuth.CreateTokenStorage(bundle)
		record := &coreauth.Auth{
			ID:       fmt.Sprintf("claude-%s.json", tokenStorage.Email),
			Provider: "claude",
			FileName: fmt.Sprintf("claude-%s.json", tokenStorage.Email),
			Storage:  tokenStorage,
			Metadata: map[string]any{"email": tokenStorage.Email},
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			SetOAuthSessionError(state, "Failed to save authentication tokens")
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		if bundle.APIKey != "" {
			fmt.Println("API key obtained and saved")
		}
		fmt.Println("You can now use Claude services through this CLI")
		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("anthropic")
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestGeminiCLIToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)
	proxyHTTPClient := util.SetProxy(&h.cfg.SDKConfig, &http.Client{})
	ctx = context.WithValue(ctx, oauth2.HTTPClient, proxyHTTPClient)

	// Optional project ID from query
	projectID := c.Query("project_id")

	fmt.Println("Initializing Google authentication...")

	// OAuth2 configuration using exported constants from internal/auth/gemini
	conf := &oauth2.Config{
		ClientID:     geminiAuth.ClientID,
		ClientSecret: geminiAuth.ClientSecret,
		RedirectURL:  fmt.Sprintf("http://localhost:%d/oauth2callback", geminiAuth.DefaultCallbackPort),
		Scopes:       geminiAuth.Scopes,
		Endpoint:     google.Endpoint,
	}

	// Build authorization URL and return it immediately
	state := fmt.Sprintf("gem-%d", time.Now().UnixNano())
	authURL := conf.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))

	RegisterOAuthSession(state, "gemini")

	isWebUI := isWebUIRequest(c)
	var forwarder *callbackForwarder
	if isWebUI {
		targetURL, errTarget := h.managementCallbackURL("/google/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute gemini callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(geminiCallbackPort, "gemini", targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start gemini callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(geminiCallbackPort, forwarder)
		}

		// Wait for callback file written by server route
		waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-gemini-%s.oauth", state))
		fmt.Println("Waiting for authentication callback...")
		deadline := time.Now().Add(5 * time.Minute)
		var authCode string
		for {
			if !IsOAuthSessionPending(state, "gemini") {
				return
			}
			if time.Now().After(deadline) {
				log.Error("oauth flow timed out")
				SetOAuthSessionError(state, "OAuth flow timed out")
				return
			}
			if data, errR := os.ReadFile(waitFile); errR == nil {
				var m map[string]string
				_ = json.Unmarshal(data, &m)
				_ = os.Remove(waitFile)
				if errStr := m["error"]; errStr != "" {
					log.Errorf("Authentication failed: %s", errStr)
					SetOAuthSessionError(state, "Authentication failed")
					return
				}
				authCode = m["code"]
				if authCode == "" {
					log.Errorf("Authentication failed: code not found")
					SetOAuthSessionError(state, "Authentication failed: code not found")
					return
				}
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		// Exchange authorization code for token
		token, err := conf.Exchange(ctx, authCode)
		if err != nil {
			log.Errorf("Failed to exchange token: %v", err)
			SetOAuthSessionError(state, "Failed to exchange token")
			return
		}

		requestedProjectID := strings.TrimSpace(projectID)

		// Create token storage (mirrors internal/auth/gemini createTokenStorage)
		authHTTPClient := conf.Client(ctx, token)
		req, errNewRequest := http.NewRequestWithContext(ctx, "GET", "https://www.googleapis.com/oauth2/v1/userinfo?alt=json", nil)
		if errNewRequest != nil {
			log.Errorf("Could not get user info: %v", errNewRequest)
			SetOAuthSessionError(state, "Could not get user info")
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token.AccessToken))

		resp, errDo := authHTTPClient.Do(req)
		if errDo != nil {
			log.Errorf("Failed to execute request: %v", errDo)
			SetOAuthSessionError(state, "Failed to execute request")
			return
		}
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Printf("warn: failed to close response body: %v", errClose)
			}
		}()

		bodyBytes, errRead := helps.ReadNonStreamResponseBody(resp.Body)
		if errRead != nil {
			log.Errorf("Failed to read user info response: %v", errRead)
			SetOAuthSessionError(state, "Failed to read user info response")
			return
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.Errorf("Get user info request failed with status %d: %s", resp.StatusCode, string(bodyBytes))
			SetOAuthSessionError(state, fmt.Sprintf("Get user info request failed with status %d", resp.StatusCode))
			return
		}

		email := gjson.GetBytes(bodyBytes, "email").String()
		if email != "" {
			fmt.Printf("Authenticated user email: %s\n", email)
		} else {
			fmt.Println("Failed to get user email from token")
		}

		// Marshal/unmarshal oauth2.Token to generic map and enrich fields
		var ifToken map[string]any
		jsonData, _ := json.Marshal(token)
		if errUnmarshal := json.Unmarshal(jsonData, &ifToken); errUnmarshal != nil {
			log.Errorf("Failed to unmarshal token: %v", errUnmarshal)
			SetOAuthSessionError(state, "Failed to unmarshal token")
			return
		}

		ifToken["token_uri"] = "https://oauth2.googleapis.com/token"
		ifToken["client_id"] = geminiAuth.ClientID
		ifToken["client_secret"] = geminiAuth.ClientSecret
		ifToken["scopes"] = geminiAuth.Scopes
		ifToken["universe_domain"] = "googleapis.com"

		ts := geminiAuth.GeminiTokenStorage{
			Token:     ifToken,
			ProjectID: requestedProjectID,
			Email:     email,
			Auto:      requestedProjectID == "",
		}

		// Initialize authenticated HTTP client via GeminiAuth to honor proxy settings
		gemAuth := geminiAuth.NewGeminiAuth()
		gemClient, errGetClient := gemAuth.GetAuthenticatedClient(ctx, &ts, h.cfg, &geminiAuth.WebLoginOptions{
			NoBrowser: true,
		})
		if errGetClient != nil {
			log.Errorf("failed to get authenticated client: %v", errGetClient)
			SetOAuthSessionError(state, "Failed to get authenticated client")
			return
		}
		fmt.Println("Authentication successful.")

		if strings.EqualFold(requestedProjectID, "ALL") {
			ts.Auto = false
			projects, errAll := onboardAllGeminiProjects(ctx, gemClient, &ts)
			if errAll != nil {
				log.Errorf("Failed to complete Gemini CLI onboarding: %v", errAll)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to complete Gemini CLI onboarding: %v", errAll))
				return
			}
			if errVerify := ensureGeminiProjectsEnabled(ctx, gemClient, projects); errVerify != nil {
				log.Errorf("Failed to verify Cloud AI API status: %v", errVerify)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to verify Cloud AI API status: %v", errVerify))
				return
			}
			ts.ProjectID = strings.Join(projects, ",")
			ts.Checked = true
		} else if strings.EqualFold(requestedProjectID, "GOOGLE_ONE") {
			ts.Auto = false
			if errSetup := performGeminiCLISetup(ctx, gemClient, &ts, ""); errSetup != nil {
				log.Errorf("Google One auto-discovery failed: %v", errSetup)
				SetOAuthSessionError(state, fmt.Sprintf("Google One auto-discovery failed: %v", errSetup))
				return
			}
			if strings.TrimSpace(ts.ProjectID) == "" {
				log.Error("Google One auto-discovery returned empty project ID")
				SetOAuthSessionError(state, "Google One auto-discovery returned empty project ID")
				return
			}
			isChecked, errCheck := checkCloudAPIIsEnabled(ctx, gemClient, ts.ProjectID)
			if errCheck != nil {
				log.Errorf("Failed to verify Cloud AI API status: %v", errCheck)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to verify Cloud AI API status: %v", errCheck))
				return
			}
			ts.Checked = isChecked
			if !isChecked {
				log.Error("Cloud AI API is not enabled for the auto-discovered project")
				SetOAuthSessionError(state, fmt.Sprintf("Cloud AI API not enabled for project %s", ts.ProjectID))
				return
			}
		} else {
			if errEnsure := ensureGeminiProjectAndOnboard(ctx, gemClient, &ts, requestedProjectID); errEnsure != nil {
				log.Errorf("Failed to complete Gemini CLI onboarding: %v", errEnsure)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to complete Gemini CLI onboarding: %v", errEnsure))
				return
			}

			if strings.TrimSpace(ts.ProjectID) == "" {
				log.Error("Onboarding did not return a project ID")
				SetOAuthSessionError(state, "Failed to resolve project ID")
				return
			}

			isChecked, errCheck := checkCloudAPIIsEnabled(ctx, gemClient, ts.ProjectID)
			if errCheck != nil {
				log.Errorf("Failed to verify Cloud AI API status: %v", errCheck)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to verify Cloud AI API status: %v", errCheck))
				return
			}
			ts.Checked = isChecked
			if !isChecked {
				log.Error("Cloud AI API is not enabled for the selected project")
				SetOAuthSessionError(state, fmt.Sprintf("Cloud AI API not enabled for project %s", ts.ProjectID))
				return
			}
		}

		recordMetadata := map[string]any{
			"email":      ts.Email,
			"project_id": ts.ProjectID,
			"auto":       ts.Auto,
			"checked":    ts.Checked,
		}

		fileName := geminiAuth.CredentialFileName(ts.Email, ts.ProjectID, true)
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "gemini",
			FileName: fileName,
			Storage:  &ts,
			Metadata: recordMetadata,
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save token to file: %v", errSave)
			SetOAuthSessionError(state, "Failed to save token to file")
			return
		}

		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("gemini")
		fmt.Printf("You can now use Gemini CLI services through this CLI; token saved to %s\n", savedPath)
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestCodexToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing Codex authentication...")

	// Generate PKCE codes
	pkceCodes, err := codex.GeneratePKCECodes()
	if err != nil {
		log.Errorf("Failed to generate PKCE codes: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		return
	}

	// Generate random state parameter
	state, err := misc.GenerateRandomState()
	if err != nil {
		log.Errorf("Failed to generate state parameter: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}

	// Initialize Codex auth service
	openaiAuth := codex.NewCodexAuth(h.cfg)

	// Generate authorization URL
	authURL, err := openaiAuth.GenerateAuthURL(state, pkceCodes)
	if err != nil {
		log.Errorf("Failed to generate authorization URL: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	RegisterOAuthSession(state, "codex")

	isWebUI := isWebUIRequest(c)
	requestUserAgent := codexLoginRequestUserAgent(c)
	var forwarder *callbackForwarder
	if isWebUI {
		targetURL, errTarget := h.managementCallbackURL("/codex/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute codex callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(codexCallbackPort, "codex", targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start codex callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(codexCallbackPort, forwarder)
		}

		// Wait for callback file
		waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-codex-%s.oauth", state))
		deadline := time.Now().Add(5 * time.Minute)
		var code string
		for {
			if !IsOAuthSessionPending(state, "codex") {
				return
			}
			if time.Now().After(deadline) {
				authErr := codex.NewAuthenticationError(codex.ErrCallbackTimeout, fmt.Errorf("timeout waiting for OAuth callback"))
				log.Error(codex.GetUserFriendlyMessage(authErr))
				SetOAuthSessionError(state, "Timeout waiting for OAuth callback")
				return
			}
			if data, errR := os.ReadFile(waitFile); errR == nil {
				var m map[string]string
				_ = json.Unmarshal(data, &m)
				_ = os.Remove(waitFile)
				if errStr := m["error"]; errStr != "" {
					oauthErr := codex.NewOAuthError(errStr, "", http.StatusBadRequest)
					log.Error(codex.GetUserFriendlyMessage(oauthErr))
					SetOAuthSessionError(state, "Bad Request")
					return
				}
				if m["state"] != state {
					authErr := codex.NewAuthenticationError(codex.ErrInvalidState, fmt.Errorf("expected %s, got %s", state, m["state"]))
					SetOAuthSessionError(state, "State code error")
					log.Error(codex.GetUserFriendlyMessage(authErr))
					return
				}
				code = m["code"]
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		log.Debug("Authorization code received, exchanging for tokens...")
		// Exchange code for tokens using internal auth service
		bundle, errExchange := openaiAuth.ExchangeCodeForTokens(ctx, code, pkceCodes)
		if errExchange != nil {
			authErr := codex.NewAuthenticationError(codex.ErrCodeExchangeFailed, errExchange)
			SetOAuthSessionError(state, oauthSessionErrorWithCause("Failed to exchange authorization code for tokens", errExchange))
			log.Errorf("Failed to exchange authorization code for tokens: %v", authErr)
			return
		}

		// Extract additional info for filename generation
		claims, _ := codex.ParseJWTToken(bundle.TokenData.IDToken)
		planType := ""
		hashAccountID := ""
		if claims != nil {
			planType = strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType)
			if accountID := claims.GetAccountID(); accountID != "" {
				digest := sha256.Sum256([]byte(accountID))
				hashAccountID = hex.EncodeToString(digest[:])[:8]
			}
		}

		// Create token storage and persist
		tokenStorage := openaiAuth.CreateTokenStorage(bundle)
		fileName := codex.CredentialFileName(tokenStorage.Email, planType, hashAccountID, true)
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "codex",
			FileName: fileName,
			Storage:  tokenStorage,
			Metadata: map[string]any{
				"email":        tokenStorage.Email,
				"account_id":   tokenStorage.AccountID,
				"access_token": tokenStorage.AccessToken,
			},
		}
		if planType != "" {
			record.Metadata["plan_type"] = planType
		}
		if requestUserAgent != "" {
			record.Metadata["user_agent"] = requestUserAgent
			record.Attributes = map[string]string{
				"header:User-Agent": requestUserAgent,
			}
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			SetOAuthSessionError(state, "Failed to save authentication tokens")
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			return
		}
		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		if bundle.APIKey != "" {
			fmt.Println("API key obtained and saved")
		}
		fmt.Println("You can now use Codex services through this CLI")
		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("codex")
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestAntigravityToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing Antigravity authentication...")

	authSvc := antigravity.NewAntigravityAuth(h.cfg, nil)

	state, errState := misc.GenerateRandomState()
	if errState != nil {
		log.Errorf("Failed to generate state parameter: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}

	redirectURI := fmt.Sprintf("http://localhost:%d/oauth-callback", antigravity.CallbackPort)
	authURL := authSvc.BuildAuthURL(state, redirectURI)

	RegisterOAuthSession(state, "antigravity")

	isWebUI := isWebUIRequest(c)
	var forwarder *callbackForwarder
	if isWebUI {
		targetURL, errTarget := h.managementCallbackURL("/antigravity/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute antigravity callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(antigravity.CallbackPort, "antigravity", targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start antigravity callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(antigravity.CallbackPort, forwarder)
		}

		waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-antigravity-%s.oauth", state))
		deadline := time.Now().Add(5 * time.Minute)
		var authCode string
		for {
			if !IsOAuthSessionPending(state, "antigravity") {
				return
			}
			if time.Now().After(deadline) {
				log.Error("oauth flow timed out")
				SetOAuthSessionError(state, "OAuth flow timed out")
				return
			}
			if data, errReadFile := os.ReadFile(waitFile); errReadFile == nil {
				var payload map[string]string
				_ = json.Unmarshal(data, &payload)
				_ = os.Remove(waitFile)
				if errStr := strings.TrimSpace(payload["error"]); errStr != "" {
					log.Errorf("Authentication failed: %s", errStr)
					SetOAuthSessionError(state, "Authentication failed")
					return
				}
				if payloadState := strings.TrimSpace(payload["state"]); payloadState != "" && payloadState != state {
					log.Errorf("Authentication failed: state mismatch")
					SetOAuthSessionError(state, "Authentication failed: state mismatch")
					return
				}
				authCode = strings.TrimSpace(payload["code"])
				if authCode == "" {
					log.Error("Authentication failed: code not found")
					SetOAuthSessionError(state, "Authentication failed: code not found")
					return
				}
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		tokenResp, errToken := authSvc.ExchangeCodeForTokens(ctx, authCode, redirectURI)
		if errToken != nil {
			log.Errorf("Failed to exchange token: %v", errToken)
			SetOAuthSessionError(state, "Failed to exchange token")
			return
		}

		accessToken := strings.TrimSpace(tokenResp.AccessToken)
		if accessToken == "" {
			log.Error("antigravity: token exchange returned empty access token")
			SetOAuthSessionError(state, "Failed to exchange token")
			return
		}

		email, errInfo := authSvc.FetchUserInfo(ctx, accessToken)
		if errInfo != nil {
			log.Errorf("Failed to fetch user info: %v", errInfo)
			SetOAuthSessionError(state, "Failed to fetch user info")
			return
		}
		email = strings.TrimSpace(email)
		if email == "" {
			log.Error("antigravity: user info returned empty email")
			SetOAuthSessionError(state, "Failed to fetch user info")
			return
		}

		projectID := ""
		if accessToken != "" {
			fetchedProjectID, errProject := authSvc.FetchProjectID(ctx, accessToken)
			if errProject != nil {
				log.Warnf("antigravity: failed to fetch project ID: %v", errProject)
			} else {
				projectID = fetchedProjectID
				log.Infof("antigravity: obtained project ID %s", projectID)
			}
		}

		now := time.Now()
		metadata := map[string]any{
			"type":          "antigravity",
			"access_token":  tokenResp.AccessToken,
			"refresh_token": tokenResp.RefreshToken,
			"expires_in":    tokenResp.ExpiresIn,
			"timestamp":     now.UnixMilli(),
			"expired":       now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
		}
		if email != "" {
			metadata["email"] = email
		}
		if projectID != "" {
			metadata["project_id"] = projectID
		}

		fileName := antigravity.CredentialFileName(email)
		label := strings.TrimSpace(email)
		if label == "" {
			label = "antigravity"
		}

		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "antigravity",
			FileName: fileName,
			Label:    label,
			Metadata: metadata,
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save token to file: %v", errSave)
			SetOAuthSessionError(state, "Failed to save token to file")
			return
		}

		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("antigravity")
		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		if projectID != "" {
			fmt.Printf("Using GCP project: %s\n", projectID)
		}
		fmt.Println("You can now use Antigravity services through this CLI")
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestKiroToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing Kiro authentication...")

	pkceCodes, errPKCE := kiroauth.GeneratePKCECodes()
	if errPKCE != nil {
		log.Errorf("Failed to generate Kiro PKCE codes: %v", errPKCE)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		return
	}

	state, errState := kiroauth.GenerateOAuthState()
	if errState != nil {
		log.Errorf("Failed to generate Kiro state parameter: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}

	socialProvider := strings.TrimSpace(c.Query("provider"))
	if socialProvider == "" {
		socialProvider = strings.TrimSpace(c.Query("idp"))
	}
	if socialProvider == "" {
		socialProvider = "google"
	}
	requestedFileName := strings.TrimSpace(c.Query("auth_file_name"))
	if requestedFileName == "" {
		requestedFileName = strings.TrimSpace(c.Query("file_name"))
	}
	if requestedFileName == "" {
		requestedFileName = strings.TrimSpace(c.Query("name"))
	}
	customFileName, errFileName := normalizeOptionalAuthFileName(requestedFileName)
	if errFileName != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid auth file name"})
		return
	}
	prompt := strings.TrimSpace(c.Query("prompt"))
	forceReauth := isTruthyQueryValue(firstNonEmptyQueryValue(c, "force_reauth", "force_login", "switch_account"))

	redirectURI := kiroauth.OAuthRedirectURI(kiroauth.DefaultOAuthCallbackPort)
	authSvc := kiroauth.NewSocialOAuthClient(h.cfg)
	authURL, errURL := authSvc.BuildLoginURLWithOptions(socialProvider, redirectURI, state, pkceCodes.CodeChallenge, kiroauth.SocialLoginURLOptions{
		Prompt:      prompt,
		ForceReauth: forceReauth,
	})
	if errURL != nil {
		log.Errorf("Failed to generate Kiro authorization URL: %v", errURL)
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to generate authorization url"})
		return
	}

	RegisterOAuthSession(state, "kiro")

	isWebUI := isWebUIRequest(c)
	var forwarder *callbackForwarder
	if isWebUI && strings.HasPrefix(strings.ToLower(redirectURI), "http://") {
		targetURL, errTarget := h.managementCallbackURL("/kiro/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute kiro callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(kiroauth.DefaultOAuthCallbackPort, "kiro", targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start kiro callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(kiroauth.DefaultOAuthCallbackPort, forwarder)
		}

		waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-kiro-%s.oauth", state))
		deadline := time.Now().Add(5 * time.Minute)
		var authCode string
		for {
			if !IsOAuthSessionPending(state, "kiro") {
				return
			}
			if time.Now().After(deadline) {
				log.Error("kiro oauth flow timed out")
				SetOAuthSessionError(state, "OAuth flow timed out")
				return
			}
			if data, errReadFile := os.ReadFile(waitFile); errReadFile == nil {
				var payload map[string]string
				_ = json.Unmarshal(data, &payload)
				_ = os.Remove(waitFile)
				if errStr := strings.TrimSpace(payload["error"]); errStr != "" {
					log.Errorf("Kiro authentication failed: %s", errStr)
					SetOAuthSessionError(state, "Authentication failed")
					return
				}
				if payloadState := strings.TrimSpace(payload["state"]); payloadState != "" && payloadState != state {
					log.Error("Kiro authentication failed: state mismatch")
					SetOAuthSessionError(state, "Authentication failed: state mismatch")
					return
				}
				authCode = strings.TrimSpace(payload["code"])
				if authCode == "" {
					log.Error("Kiro authentication failed: code not found")
					SetOAuthSessionError(state, "Authentication failed: code not found")
					return
				}
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		tokenResp, errToken := authSvc.ExchangeCode(ctx, authCode, redirectURI, pkceCodes.CodeVerifier)
		if errToken != nil {
			log.Errorf("Failed to exchange Kiro token: %v", errToken)
			SetOAuthSessionError(state, oauthSessionErrorWithDetail("Failed to exchange token", errToken))
			return
		}

		tokenData := kiroauth.TokenDataFromSocialResponse(socialProvider, tokenResp)
		if tokenData == nil || strings.TrimSpace(tokenData.AccessToken) == "" {
			log.Error("kiro oauth: token response is empty")
			SetOAuthSessionError(state, "Failed to exchange token")
			return
		}
		if strings.TrimSpace(tokenData.ProfileArn) == "" {
			profileArn, errProfile := kiroauth.NewKiroAuth(h.cfg).ResolveProfileArn(ctx, tokenData)
			if errProfile != nil {
				log.Warnf("kiro: failed to resolve profile arn: %v", errProfile)
			} else {
				tokenData.ProfileArn = profileArn
			}
		}

		record := sdkAuth.NewKiroAuthenticator().CreateAuthRecord(tokenData, "kiro-oauth")
		if customFileName != "" {
			record.ID = customFileName
			record.FileName = customFileName
			if record.Metadata != nil {
				record.Metadata["custom_file_name"] = customFileName
			}
			if strings.TrimSpace(record.Label) == "" || strings.HasPrefix(record.Label, "kiro-") {
				record.Label = strings.TrimSuffix(customFileName, ".json")
			}
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save Kiro token to file: %v", errSave)
			SetOAuthSessionError(state, "Failed to save token to file")
			return
		}

		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("kiro")
		fmt.Printf("Kiro authentication successful! Token saved to %s\n", savedPath)
		fmt.Println("You can now use Kiro services through this CLI")
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestXAIToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing xAI authentication...")

	pkceCodes, errPKCE := xaiauth.GeneratePKCECodes()
	if errPKCE != nil {
		log.Errorf("Failed to generate xAI PKCE codes: %v", errPKCE)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		return
	}

	state, errState := misc.GenerateRandomState()
	if errState != nil {
		log.Errorf("Failed to generate state parameter: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}

	nonce, errNonce := misc.GenerateRandomState()
	if errNonce != nil {
		log.Errorf("Failed to generate nonce parameter: %v", errNonce)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate nonce parameter"})
		return
	}

	authSvc := xaiauth.NewXAIAuth(h.cfg)
	discovery, errDiscover := authSvc.Discover(ctx)
	if errDiscover != nil {
		log.Errorf("Failed to discover xAI OAuth endpoints: %v", errDiscover)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to discover oauth endpoints"})
		return
	}

	redirectURI := fmt.Sprintf("http://%s:%d%s", xaiauth.RedirectHost, xaiauth.CallbackPort, xaiauth.RedirectPath)
	authURL, errAuthURL := xaiauth.BuildAuthorizeURL(xaiauth.AuthorizeURLParams{
		AuthorizationEndpoint: discovery.AuthorizationEndpoint,
		RedirectURI:           redirectURI,
		CodeChallenge:         pkceCodes.CodeChallenge,
		State:                 state,
		Nonce:                 nonce,
	})
	if errAuthURL != nil {
		log.Errorf("Failed to generate xAI authorization URL: %v", errAuthURL)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	RegisterOAuthSession(state, "xai")

	isWebUI := isWebUIRequest(c)
	var forwarder *callbackForwarder
	if isWebUI {
		targetURL, errTarget := h.managementCallbackURL("/xai/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute xai callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(xaiauth.CallbackPort, "xai", targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start xai callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(xaiauth.CallbackPort, forwarder)
		}

		waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-xai-%s.oauth", state))
		deadline := time.Now().Add(5 * time.Minute)
		var authCode string
		for {
			if !IsOAuthSessionPending(state, "xai") {
				return
			}
			if time.Now().After(deadline) {
				log.Error("xai oauth flow timed out")
				SetOAuthSessionError(state, "OAuth flow timed out")
				return
			}
			if data, errReadFile := os.ReadFile(waitFile); errReadFile == nil {
				var payload map[string]string
				_ = json.Unmarshal(data, &payload)
				_ = os.Remove(waitFile)
				if errStr := strings.TrimSpace(payload["error"]); errStr != "" {
					log.Errorf("xAI authentication failed: %s", errStr)
					SetOAuthSessionError(state, "Authentication failed: "+errStr)
					return
				}
				if payloadState := strings.TrimSpace(payload["state"]); payloadState != "" && payloadState != state {
					log.Errorf("xAI authentication failed: state mismatch")
					SetOAuthSessionError(state, "Authentication failed: state mismatch")
					return
				}
				authCode = strings.TrimSpace(payload["code"])
				if authCode == "" {
					log.Error("xAI authentication failed: code not found")
					SetOAuthSessionError(state, "Authentication failed: code not found")
					return
				}
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		bundle, errExchange := authSvc.ExchangeCodeForTokens(ctx, authCode, redirectURI, pkceCodes, discovery.TokenEndpoint)
		if errExchange != nil {
			log.Errorf("Failed to exchange xAI token: %v", errExchange)
			SetOAuthSessionError(state, oauthSessionErrorWithCause("Failed to exchange authorization code for tokens", errExchange))
			return
		}

		tokenStorage := authSvc.CreateTokenStorage(bundle)
		if tokenStorage == nil || strings.TrimSpace(tokenStorage.AccessToken) == "" {
			log.Error("xAI token exchange returned empty access token")
			SetOAuthSessionError(state, "Failed to exchange token")
			return
		}

		fileName := xaiauth.CredentialFileName(tokenStorage.Email, tokenStorage.Subject)
		label := strings.TrimSpace(tokenStorage.Email)
		if label == "" {
			label = "xAI"
		}

		metadata := map[string]any{
			"type":           "xai",
			"access_token":   tokenStorage.AccessToken,
			"refresh_token":  tokenStorage.RefreshToken,
			"id_token":       tokenStorage.IDToken,
			"token_type":     tokenStorage.TokenType,
			"expires_in":     tokenStorage.ExpiresIn,
			"expired":        tokenStorage.Expire,
			"last_refresh":   tokenStorage.LastRefresh,
			"base_url":       tokenStorage.BaseURL,
			"redirect_uri":   tokenStorage.RedirectURI,
			"token_endpoint": tokenStorage.TokenEndpoint,
			"auth_kind":      "oauth",
		}
		if tokenStorage.Email != "" {
			metadata["email"] = tokenStorage.Email
		}
		if tokenStorage.Subject != "" {
			metadata["sub"] = tokenStorage.Subject
		}

		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "xai",
			FileName: fileName,
			Label:    label,
			Storage:  tokenStorage,
			Metadata: metadata,
			Attributes: map[string]string{
				"auth_kind": "oauth",
				"base_url":  tokenStorage.BaseURL,
			},
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save xAI token to file: %v", errSave)
			SetOAuthSessionError(state, "Failed to save token to file")
			return
		}

		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("xai")
		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		fmt.Println("You can now use xAI services through this CLI")
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestKimiToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing Kimi authentication...")

	state := fmt.Sprintf("kmi-%d", time.Now().UnixNano())
	// Initialize Kimi auth service
	kimiAuth := kimi.NewKimiAuth(h.cfg)

	// Generate authorization URL
	deviceFlow, errStartDeviceFlow := kimiAuth.StartDeviceFlow(ctx)
	if errStartDeviceFlow != nil {
		log.Errorf("Failed to generate authorization URL: %v", errStartDeviceFlow)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}
	authURL := deviceFlow.VerificationURIComplete
	if authURL == "" {
		authURL = deviceFlow.VerificationURI
	}

	RegisterOAuthSession(state, "kimi")

	go func() {
		fmt.Println("Waiting for authentication...")
		authBundle, errWaitForAuthorization := kimiAuth.WaitForAuthorization(ctx, deviceFlow)
		if errWaitForAuthorization != nil {
			SetOAuthSessionError(state, "Authentication failed")
			fmt.Printf("Authentication failed: %v\n", errWaitForAuthorization)
			return
		}

		// Create token storage
		tokenStorage := kimiAuth.CreateTokenStorage(authBundle)

		metadata := map[string]any{
			"type":          "kimi",
			"access_token":  authBundle.TokenData.AccessToken,
			"refresh_token": authBundle.TokenData.RefreshToken,
			"token_type":    authBundle.TokenData.TokenType,
			"scope":         authBundle.TokenData.Scope,
			"timestamp":     time.Now().UnixMilli(),
		}
		if authBundle.TokenData.ExpiresAt > 0 {
			expired := time.Unix(authBundle.TokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
			metadata["expired"] = expired
		}
		if strings.TrimSpace(authBundle.DeviceID) != "" {
			metadata["device_id"] = strings.TrimSpace(authBundle.DeviceID)
		}

		fileName := fmt.Sprintf("kimi-%d.json", time.Now().UnixMilli())
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "kimi",
			FileName: fileName,
			Label:    "Kimi User",
			Storage:  tokenStorage,
			Metadata: metadata,
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			SetOAuthSessionError(state, "Failed to save authentication tokens")
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		fmt.Println("You can now use Kimi services through this CLI")
		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("kimi")
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

type projectSelectionRequiredError struct{}

func (e *projectSelectionRequiredError) Error() string {
	return "gemini cli: project selection required"
}

func ensureGeminiProjectAndOnboard(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage, requestedProject string) error {
	if storage == nil {
		return fmt.Errorf("gemini storage is nil")
	}

	trimmedRequest := strings.TrimSpace(requestedProject)
	if trimmedRequest == "" {
		projects, errProjects := fetchGCPProjects(ctx, httpClient)
		if errProjects != nil {
			return fmt.Errorf("fetch project list: %w", errProjects)
		}
		if len(projects) == 0 {
			return fmt.Errorf("no Google Cloud projects available for this account")
		}
		trimmedRequest = strings.TrimSpace(projects[0].ProjectID)
		if trimmedRequest == "" {
			return fmt.Errorf("resolved project id is empty")
		}
		storage.Auto = true
	} else {
		storage.Auto = false
	}

	if err := performGeminiCLISetup(ctx, httpClient, storage, trimmedRequest); err != nil {
		return err
	}

	if strings.TrimSpace(storage.ProjectID) == "" {
		storage.ProjectID = trimmedRequest
	}

	return nil
}

func onboardAllGeminiProjects(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage) ([]string, error) {
	projects, errProjects := fetchGCPProjects(ctx, httpClient)
	if errProjects != nil {
		return nil, fmt.Errorf("fetch project list: %w", errProjects)
	}
	if len(projects) == 0 {
		return nil, fmt.Errorf("no Google Cloud projects available for this account")
	}
	activated := make([]string, 0, len(projects))
	seen := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		candidate := strings.TrimSpace(project.ProjectID)
		if candidate == "" {
			continue
		}
		if _, dup := seen[candidate]; dup {
			continue
		}
		if err := performGeminiCLISetup(ctx, httpClient, storage, candidate); err != nil {
			return nil, fmt.Errorf("onboard project %s: %w", candidate, err)
		}
		finalID := strings.TrimSpace(storage.ProjectID)
		if finalID == "" {
			finalID = candidate
		}
		activated = append(activated, finalID)
		seen[candidate] = struct{}{}
	}
	if len(activated) == 0 {
		return nil, fmt.Errorf("no Google Cloud projects available for this account")
	}
	return activated, nil
}

func ensureGeminiProjectsEnabled(ctx context.Context, httpClient *http.Client, projectIDs []string) error {
	for _, pid := range projectIDs {
		trimmed := strings.TrimSpace(pid)
		if trimmed == "" {
			continue
		}
		isChecked, errCheck := checkCloudAPIIsEnabled(ctx, httpClient, trimmed)
		if errCheck != nil {
			return fmt.Errorf("project %s: %w", trimmed, errCheck)
		}
		if !isChecked {
			return fmt.Errorf("project %s: Cloud AI API not enabled", trimmed)
		}
	}
	return nil
}

func performGeminiCLISetup(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage, requestedProject string) error {
	metadata := map[string]string{
		"ideType":    "IDE_UNSPECIFIED",
		"platform":   "PLATFORM_UNSPECIFIED",
		"pluginType": "GEMINI",
	}

	trimmedRequest := strings.TrimSpace(requestedProject)
	explicitProject := trimmedRequest != ""

	loadReqBody := map[string]any{
		"metadata": metadata,
	}
	if explicitProject {
		loadReqBody["cloudaicompanionProject"] = trimmedRequest
	}

	var loadResp map[string]any
	if errLoad := callGeminiCLI(ctx, httpClient, "loadCodeAssist", loadReqBody, &loadResp); errLoad != nil {
		return fmt.Errorf("load code assist: %w", errLoad)
	}

	tierID := "legacy-tier"
	if tiers, okTiers := loadResp["allowedTiers"].([]any); okTiers {
		for _, rawTier := range tiers {
			tier, okTier := rawTier.(map[string]any)
			if !okTier {
				continue
			}
			if isDefault, okDefault := tier["isDefault"].(bool); okDefault && isDefault {
				if id, okID := tier["id"].(string); okID && strings.TrimSpace(id) != "" {
					tierID = strings.TrimSpace(id)
					break
				}
			}
		}
	}

	projectID := trimmedRequest
	if projectID == "" {
		if id, okProject := loadResp["cloudaicompanionProject"].(string); okProject {
			projectID = strings.TrimSpace(id)
		}
		if projectID == "" {
			if projectMap, okProject := loadResp["cloudaicompanionProject"].(map[string]any); okProject {
				if id, okID := projectMap["id"].(string); okID {
					projectID = strings.TrimSpace(id)
				}
			}
		}
	}
	if projectID == "" {
		// Auto-discovery: try onboardUser without specifying a project
		// to let Google auto-provision one (matches Gemini CLI headless behavior
		// and Antigravity's FetchProjectID pattern).
		autoOnboardReq := map[string]any{
			"tierId":   tierID,
			"metadata": metadata,
		}

		autoCtx, autoCancel := context.WithTimeout(ctx, 30*time.Second)
		defer autoCancel()
		for attempt := 1; ; attempt++ {
			var onboardResp map[string]any
			if errOnboard := callGeminiCLI(autoCtx, httpClient, "onboardUser", autoOnboardReq, &onboardResp); errOnboard != nil {
				return fmt.Errorf("auto-discovery onboardUser: %w", errOnboard)
			}

			if done, okDone := onboardResp["done"].(bool); okDone && done {
				if resp, okResp := onboardResp["response"].(map[string]any); okResp {
					switch v := resp["cloudaicompanionProject"].(type) {
					case string:
						projectID = strings.TrimSpace(v)
					case map[string]any:
						if id, okID := v["id"].(string); okID {
							projectID = strings.TrimSpace(id)
						}
					}
				}
				break
			}

			log.Debugf("Auto-discovery: onboarding in progress, attempt %d...", attempt)
			select {
			case <-autoCtx.Done():
				return &projectSelectionRequiredError{}
			case <-time.After(2 * time.Second):
			}
		}

		if projectID == "" {
			return &projectSelectionRequiredError{}
		}
		log.Infof("Auto-discovered project ID via onboarding: %s", projectID)
	}

	onboardReqBody := map[string]any{
		"tierId":                  tierID,
		"metadata":                metadata,
		"cloudaicompanionProject": projectID,
	}

	storage.ProjectID = projectID

	for {
		var onboardResp map[string]any
		if errOnboard := callGeminiCLI(ctx, httpClient, "onboardUser", onboardReqBody, &onboardResp); errOnboard != nil {
			return fmt.Errorf("onboard user: %w", errOnboard)
		}

		if done, okDone := onboardResp["done"].(bool); okDone && done {
			responseProjectID := ""
			if resp, okResp := onboardResp["response"].(map[string]any); okResp {
				switch projectValue := resp["cloudaicompanionProject"].(type) {
				case map[string]any:
					if id, okID := projectValue["id"].(string); okID {
						responseProjectID = strings.TrimSpace(id)
					}
				case string:
					responseProjectID = strings.TrimSpace(projectValue)
				}
			}

			finalProjectID := projectID
			if responseProjectID != "" {
				if explicitProject && !strings.EqualFold(responseProjectID, projectID) {
					log.Infof("Gemini onboarding: requested project %s maps to backend project %s", projectID, responseProjectID)
					log.Infof("Using backend project ID: %s", responseProjectID)
				}
				finalProjectID = responseProjectID
			}

			storage.ProjectID = strings.TrimSpace(finalProjectID)
			if storage.ProjectID == "" {
				storage.ProjectID = strings.TrimSpace(projectID)
			}
			if storage.ProjectID == "" {
				return fmt.Errorf("onboard user completed without project id")
			}
			log.Infof("Onboarding complete. Using Project ID: %s", storage.ProjectID)
			return nil
		}

		log.Println("Onboarding in progress, waiting 5 seconds...")
		time.Sleep(5 * time.Second)
	}
}

func callGeminiCLI(ctx context.Context, httpClient *http.Client, endpoint string, body any, result any) error {
	endPointURL := fmt.Sprintf("%s/%s:%s", geminiCLIEndpoint, geminiCLIVersion, endpoint)
	if strings.HasPrefix(endpoint, "operations/") {
		endPointURL = fmt.Sprintf("%s/%s", geminiCLIEndpoint, endpoint)
	}

	var reader io.Reader
	if body != nil {
		rawBody, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			return fmt.Errorf("marshal request body: %w", errMarshal)
		}
		reader = bytes.NewReader(rawBody)
	}

	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endPointURL, reader)
	if errRequest != nil {
		return fmt.Errorf("create request: %w", errRequest)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return fmt.Errorf("execute request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, errRead := helps.ReadNonStreamResponseBody(resp.Body)
		if errRead != nil {
			return fmt.Errorf("api request failed with status %d and unreadable body: %w", resp.StatusCode, errRead)
		}
		return fmt.Errorf("api request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	if result == nil {
		if _, errRead := helps.ReadNonStreamResponseBody(resp.Body); errRead != nil {
			return fmt.Errorf("read response body: %w", errRead)
		}
		return nil
	}

	bodyBytes, errRead := helps.ReadNonStreamResponseBody(resp.Body)
	if errRead != nil {
		return fmt.Errorf("read response body: %w", errRead)
	}
	if errDecode := json.Unmarshal(bodyBytes, result); errDecode != nil {
		return fmt.Errorf("decode response body: %w", errDecode)
	}

	return nil
}

func fetchGCPProjects(ctx context.Context, httpClient *http.Client) ([]interfaces.GCPProjectProjects, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://cloudresourcemanager.googleapis.com/v1/projects", nil)
	if errRequest != nil {
		return nil, fmt.Errorf("could not create project list request: %w", errRequest)
	}

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("failed to execute project list request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, errRead := helps.ReadNonStreamResponseBody(resp.Body)
		if errRead != nil {
			return nil, fmt.Errorf("project list request failed with status %d and unreadable body: %w", resp.StatusCode, errRead)
		}
		return nil, fmt.Errorf("project list request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	bodyBytes, errRead := helps.ReadNonStreamResponseBody(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("read project list response: %w", errRead)
	}
	var projects interfaces.GCPProject
	if errDecode := json.Unmarshal(bodyBytes, &projects); errDecode != nil {
		return nil, fmt.Errorf("failed to unmarshal project list: %w", errDecode)
	}

	return projects.Projects, nil
}

func checkCloudAPIIsEnabled(ctx context.Context, httpClient *http.Client, projectID string) (bool, error) {
	serviceUsageURL := "https://serviceusage.googleapis.com"
	requiredServices := []string{
		"cloudaicompanion.googleapis.com",
	}
	for _, service := range requiredServices {
		checkURL := fmt.Sprintf("%s/v1/projects/%s/services/%s", serviceUsageURL, projectID, service)
		req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
		if errRequest != nil {
			return false, fmt.Errorf("failed to create request: %w", errRequest)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))
		resp, errDo := httpClient.Do(req)
		if errDo != nil {
			return false, fmt.Errorf("failed to execute request: %w", errDo)
		}

		if resp.StatusCode == http.StatusOK {
			bodyBytes, errRead := helps.ReadNonStreamResponseBody(resp.Body)
			if errRead != nil {
				_ = resp.Body.Close()
				return false, fmt.Errorf("failed to read service status response: %w", errRead)
			}
			if gjson.GetBytes(bodyBytes, "state").String() == "ENABLED" {
				_ = resp.Body.Close()
				continue
			}
		}
		_ = resp.Body.Close()

		enableURL := fmt.Sprintf("%s/v1/projects/%s/services/%s:enable", serviceUsageURL, projectID, service)
		req, errRequest = http.NewRequestWithContext(ctx, http.MethodPost, enableURL, strings.NewReader("{}"))
		if errRequest != nil {
			return false, fmt.Errorf("failed to create request: %w", errRequest)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))
		resp, errDo = httpClient.Do(req)
		if errDo != nil {
			return false, fmt.Errorf("failed to execute request: %w", errDo)
		}

		bodyBytes, errRead := helps.ReadNonStreamResponseBody(resp.Body)
		if errRead != nil {
			_ = resp.Body.Close()
			return false, fmt.Errorf("failed to read service enable response: %w", errRead)
		}
		errMessage := string(bodyBytes)
		errMessageResult := gjson.GetBytes(bodyBytes, "error.message")
		if errMessageResult.Exists() {
			errMessage = errMessageResult.String()
		}
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
			_ = resp.Body.Close()
			continue
		} else if resp.StatusCode == http.StatusBadRequest {
			_ = resp.Body.Close()
			if strings.Contains(strings.ToLower(errMessage), "already enabled") {
				continue
			}
		}
		_ = resp.Body.Close()
		return false, fmt.Errorf("project activation required: %s", errMessage)
	}
	return true, nil
}

func (h *Handler) GetAuthStatus(c *gin.Context) {
	state := strings.TrimSpace(c.Query("state"))
	if state == "" {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if err := ValidateOAuthState(state); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid state"})
		return
	}

	_, status, ok := GetOAuthSession(state)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if status != "" {
		c.JSON(http.StatusOK, gin.H{"status": "error", "error": status})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "wait"})
}

// PopulateAuthContext extracts request info and adds it to the context
func PopulateAuthContext(ctx context.Context, c *gin.Context) context.Context {
	info := &coreauth.RequestInfo{
		Query:   c.Request.URL.Query(),
		Headers: c.Request.Header,
	}
	return coreauth.WithRequestInfo(ctx, info)
}
