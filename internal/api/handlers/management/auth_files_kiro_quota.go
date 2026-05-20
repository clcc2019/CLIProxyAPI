package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"golang.org/x/sync/singleflight"
)

type kiroUsageClient interface {
	GetUsageLimits(context.Context, *kiroauth.TokenData) (*kiroauth.KiroUsageInfo, error)
}

var newKiroUsageClient = func(cfg *config.Config) kiroUsageClient {
	return kiroauth.NewKiroAuth(cfg)
}

// kiroUsageCacheDefaultTTL bounds how long a successful KiroUsageInfo response
// is reused between dashboards. Multiple monitoring tools (CPA Usage Keeper,
// CLIProxyAPI Quota Inspector, ProxyPilot, ZeroLimit, CLIProxy Pool Watch, ...)
// poll this endpoint on the order of seconds; without a cache each poll fans
// out to CodeWhisperer's getUsageLimits and burns rate-limit budget. 30s is a
// pragmatic default — long enough to collapse polling clusters, short enough
// that the displayed remaining quota tracks real consumption.
const (
	kiroUsageCacheDefaultTTL = 30 * time.Second
	kiroUsageCacheMaxTTL     = 5 * time.Minute
)

// kiroUsageCacheEntry retains a KiroUsageInfo until expiresAt. We snapshot the
// pointer rather than re-marshalling JSON because gin.Context.JSON copes with
// pointer values directly.
type kiroUsageCacheEntry struct {
	usage     *kiroauth.KiroUsageInfo
	expiresAt time.Time
}

// kiroUsageCache is a per-handler cache + singleflight collapse for kiro
// usage queries. Process-wide is not appropriate because tests construct
// fresh Handlers and the cache must not leak across them.
type kiroUsageCache struct {
	entries sync.Map // auth.ID -> *kiroUsageCacheEntry
	flights singleflight.Group
}

func (c *kiroUsageCache) load(authID string, now time.Time) (*kiroauth.KiroUsageInfo, bool) {
	if c == nil || authID == "" {
		return nil, false
	}
	value, ok := c.entries.Load(authID)
	if !ok {
		return nil, false
	}
	entry, ok := value.(*kiroUsageCacheEntry)
	if !ok || entry == nil || entry.usage == nil {
		return nil, false
	}
	if !now.Before(entry.expiresAt) {
		c.entries.Delete(authID)
		return nil, false
	}
	return entry.usage, true
}

func (c *kiroUsageCache) store(authID string, usage *kiroauth.KiroUsageInfo, ttl time.Duration) {
	if c == nil || authID == "" || usage == nil || ttl <= 0 {
		return
	}
	c.entries.Store(authID, &kiroUsageCacheEntry{usage: usage, expiresAt: time.Now().Add(ttl)})
}

func (c *kiroUsageCache) invalidate(authID string) {
	if c == nil || authID == "" {
		return
	}
	c.entries.Delete(authID)
}

// kiroUsageHandlerCache resolves the per-handler cache, lazily creating it.
// The cache is opt-out via `?force=true` and TTL-overridable via `?ttl=NN`.
func (h *Handler) kiroUsageHandlerCache() *kiroUsageCache {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.kiroUsageCache == nil {
		h.kiroUsageCache = &kiroUsageCache{}
	}
	return h.kiroUsageCache
}

// kiroUsageRequestOptions captures the per-request knobs supported by the
// quota endpoint. ttl is clamped at kiroUsageCacheMaxTTL to avoid pathological
// cache lifetimes; force bypasses cache fully (still records the response).
type kiroUsageRequestOptions struct {
	force bool
	ttl   time.Duration
}

func parseKiroUsageRequestOptions(c *gin.Context) kiroUsageRequestOptions {
	opts := kiroUsageRequestOptions{ttl: kiroUsageCacheDefaultTTL}
	if c == nil {
		return opts
	}
	switch strings.ToLower(strings.TrimSpace(c.Query("force"))) {
	case "1", "true", "yes", "on":
		opts.force = true
	}
	if raw := strings.TrimSpace(c.Query("ttl")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil {
			if seconds <= 0 {
				opts.force = true
			} else {
				ttl := time.Duration(seconds) * time.Second
				if ttl > kiroUsageCacheMaxTTL {
					ttl = kiroUsageCacheMaxTTL
				}
				opts.ttl = ttl
			}
		}
	}
	return opts
}

func (h *Handler) GetKiroUsage(c *gin.Context) {
	name := strings.TrimSpace(c.Query("name"))
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		authIndex = strings.TrimSpace(c.Query("authIndex"))
	}
	if name == "" && authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name or auth_index is required"})
		return
	}

	auth, status, err := h.resolveKiroUsageAuth(c.Request.Context(), name, authIndex)
	if err != nil {
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "kiro") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth file is not a Kiro credential"})
		return
	}

	if refreshed, status, err := h.refreshKiroUsageAuthIfNeeded(c.Request.Context(), auth); err != nil {
		if status == http.StatusBadGateway {
			c.JSON(http.StatusOK, kiroUsageUnavailableInfo(auth, err))
			return
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	} else if refreshed != nil {
		auth = refreshed
	}

	if _, err := tokenDataFromKiroAuth(auth); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	opts := parseKiroUsageRequestOptions(c)
	cache := h.kiroUsageHandlerCache()

	// Cache hit short-circuits the upstream call. Profile_arn discovered on a
	// previous call is already persisted, so we don't need to re-run the
	// resolve path here.
	if !opts.force && cache != nil {
		if cached, ok := cache.load(auth.ID, time.Now()); ok {
			c.JSON(http.StatusOK, cached)
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	usage, err := h.fetchKiroUsageWithCache(ctx, c.Request.Context(), auth, opts)
	if err != nil {
		if kiroauth.IsUnauthorizedStatusError(err) {
			refreshed, status, refreshErr := h.refreshKiroUsageAuth(c.Request.Context(), auth, true)
			if refreshErr != nil {
				if status == http.StatusBadGateway {
					c.JSON(http.StatusOK, kiroUsageUnavailableInfo(auth, refreshErr))
					return
				}
				c.JSON(status, gin.H{"error": refreshErr.Error()})
				return
			}
			if refreshed != nil {
				auth = refreshed
			}
			if cache != nil {
				cache.invalidate(auth.ID)
			}
			usage, err = h.fetchKiroUsageWithCache(ctx, c.Request.Context(), auth, opts)
		}
	}
	if err != nil {
		c.JSON(http.StatusOK, kiroUsageUnavailableInfo(auth, err))
		return
	}
	c.JSON(http.StatusOK, usage)
}

func kiroUsageUnavailableInfo(auth *coreauth.Auth, err error) *kiroauth.KiroUsageInfo {
	message := "Unable to fetch Kiro usage right now."
	if auth != nil {
		authMethod := strings.ToLower(kiroAuthString(auth, "auth_method", "authMethod"))
		provider := strings.ToLower(kiroAuthString(auth, "provider"))
		switch {
		case authMethod == "idc" || provider == "idc":
			message = "Kiro quota API is unavailable for the current AWS IAM Identity Center session. Chat may still work. If this persists after renewing your session, reconnect Kiro."
		case isKiroUsageSocialAuth(authMethod, provider):
			message = "Kiro quota API authentication expired or is unavailable for this social login. Chat may still work. If this persists, reconnect Kiro."
		case kiroauth.IsUnauthorizedStatusError(err):
			message = "Kiro quota API rejected the current token. Chat may still work. If this persists, reconnect Kiro."
		}
	}
	if err != nil {
		detail := strings.TrimSpace(err.Error())
		if detail != "" && !strings.Contains(message, detail) {
			message += " (" + detail + ")"
		}
	}
	return &kiroauth.KiroUsageInfo{Message: message, Quotas: map[string]any{}}
}

func isKiroUsageSocialAuth(authMethod, provider string) bool {
	switch strings.ToLower(strings.TrimSpace(authMethod)) {
	case "kiro-cli-social", "kiro-social", "social", "google", "github", "gitlab":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "google", "github", "gitlab", "kiro-cli", "kiro-social", "social":
		return true
	default:
		return false
	}
}

// fetchKiroUsageWithCache fans concurrent callers for the same auth into a
// single upstream request via singleflight, then memoises the result for the
// configured TTL. force=true bypasses the cache read and refreshes the entry.
func (h *Handler) fetchKiroUsageWithCache(ctx context.Context, requestCtx context.Context, auth *coreauth.Auth, opts kiroUsageRequestOptions) (*kiroauth.KiroUsageInfo, error) {
	if auth == nil {
		return nil, fmt.Errorf("kiro auth is missing")
	}
	cache := h.kiroUsageHandlerCache()

	// Singleflight key includes a stable suffix so that a force=true caller
	// does not piggyback on a non-force in-flight (the force-caller may
	// genuinely need a freshly-issued response, e.g., right after refresh).
	flightKey := auth.ID
	if opts.force {
		flightKey += "|force"
	}

	type result struct {
		usage *kiroauth.KiroUsageInfo
		err   error
	}

	value, err, _ := cache.flights.Do(flightKey, func() (any, error) {
		// Re-check cache inside the singleflight to coalesce stragglers.
		if !opts.force && cache != nil {
			if cached, ok := cache.load(auth.ID, time.Now()); ok {
				return result{usage: cached}, nil
			}
		}
		usage, err := h.executeKiroUsage(ctx, requestCtx, auth)
		if err != nil {
			return result{err: err}, nil
		}
		if cache != nil && usage != nil && opts.ttl > 0 {
			cache.store(auth.ID, usage, opts.ttl)
		}
		return result{usage: usage}, nil
	})
	if err != nil {
		return nil, err
	}
	res, ok := value.(result)
	if !ok {
		return nil, fmt.Errorf("kiro usage: invalid singleflight result")
	}
	return res.usage, res.err
}

// executeKiroUsage performs one upstream request through a proxy-aware HTTP
// client. The request context bounds the call; the outer requestCtx is used
// for any persistence-side updates so they survive a per-request timeout.
func (h *Handler) executeKiroUsage(ctx context.Context, requestCtx context.Context, auth *coreauth.Auth) (*kiroauth.KiroUsageInfo, error) {
	tokenData, err := tokenDataFromKiroAuth(auth)
	if err != nil {
		return nil, err
	}

	usageClient := newKiroUsageClient(h.cfg)
	// Inject a proxy-aware HTTP client that honours per-account auth.ProxyURL,
	// the global cfg.ProxyURL, the context RoundTripper, and the shared
	// transport pool — same chain used by the kiro request executor. This
	// only applies to the real *kiroauth.KiroAuth implementation; tests
	// inject their own kiroUsageClient stub which does not need this.
	if real, ok := usageClient.(*kiroauth.KiroAuth); ok {
		real.WithHTTPClient(helps.NewProxyAwareHTTPClient(ctx, h.cfg, auth, 30*time.Second))
	}

	usage, err := usageClient.GetUsageLimits(ctx, tokenData)
	if err != nil {
		return nil, err
	}
	h.persistResolvedKiroProfileArn(requestCtx, auth, tokenData.ProfileArn)
	return usage, nil
}

func (h *Handler) resolveKiroUsageAuth(ctx context.Context, name, authIndex string) (*coreauth.Auth, int, error) {
	if h == nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("handler not initialized")
	}

	if authIndex != "" {
		if auth := h.authByIndex(authIndex); auth != nil {
			return auth, http.StatusOK, nil
		}
	}

	if h.authManager != nil {
		for _, auth := range h.authManager.List() {
			if auth == nil {
				continue
			}
			auth.EnsureIndex()
			if authIndex != "" && auth.Index == authIndex {
				return auth, http.StatusOK, nil
			}
			if name != "" && (auth.FileName == name || auth.ID == name) {
				return auth, http.StatusOK, nil
			}
		}
	}

	if name == "" {
		return nil, http.StatusNotFound, fmt.Errorf("auth file not found")
	}
	return h.readKiroUsageAuthFromDisk(ctx, name)
}

func (h *Handler) readKiroUsageAuthFromDisk(_ context.Context, name string) (*coreauth.Auth, int, error) {
	if isUnsafeAuthFileName(name) {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid name")
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		return nil, http.StatusBadRequest, fmt.Errorf("name must end with .json")
	}
	if h == nil || h.cfg == nil || strings.TrimSpace(h.cfg.AuthDir) == "" {
		return nil, http.StatusNotFound, fmt.Errorf("auth file not found")
	}

	fullPath := filepath.Join(h.cfg.AuthDir, name)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, http.StatusNotFound, fmt.Errorf("auth file not found")
		}
		return nil, http.StatusInternalServerError, fmt.Errorf("failed to read auth file: %w", err)
	}

	metadata := make(map[string]any)
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid auth file json")
	}
	if normalized, changed := coreauth.NormalizeImportedAuthMetadata(metadata); changed {
		metadata = normalized
	}
	provider := strings.TrimSpace(valueAsString(metadata["type"]))
	if provider == "" {
		provider = "unknown"
	}
	auth := &coreauth.Auth{
		ID:         name,
		FileName:   name,
		Provider:   provider,
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"path": fullPath},
		Metadata:   metadata,
	}
	coreauth.ApplyAuthFileOptionsFromMetadata(auth)
	return auth, http.StatusOK, nil
}

func (h *Handler) refreshKiroUsageAuthIfNeeded(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, int, error) {
	return h.refreshKiroUsageAuth(ctx, auth, false)
}

func (h *Handler) refreshKiroUsageAuth(ctx context.Context, auth *coreauth.Auth, force bool) (*coreauth.Auth, int, error) {
	if h == nil || h.authManager == nil || auth == nil || auth.ID == "" {
		if force {
			return nil, http.StatusBadGateway, fmt.Errorf("kiro auth refresh is unavailable")
		}
		return auth, http.StatusOK, nil
	}
	refreshAuth := auth
	if latest, ok := h.authManager.GetByID(auth.ID); ok && latest != nil {
		refreshAuth = latest
	}
	if refreshAuth.Disabled || refreshAuth.Status == coreauth.StatusDisabled {
		return refreshAuth, http.StatusOK, nil
	}
	shouldRefresh, required := shouldRefreshKiroUsageAuth(refreshAuth, time.Now().UTC())
	if !shouldRefresh {
		if force {
			required = true
		} else {
			return refreshAuth, http.StatusOK, nil
		}
	}
	// Ensure the kiro executor is registered with the manager so that
	// Manager.RefreshAuth can dispatch to it. Returns silently if a
	// non-kiro provider has no executor and we are not forcing.
	if _, ok := h.kiroUsageRefreshExecutor(refreshAuth.Provider); !ok {
		if force {
			return nil, http.StatusBadGateway, fmt.Errorf("kiro auth refresh executor is unavailable")
		}
		return refreshAuth, http.StatusOK, nil
	}

	// Route the actual refresh through Manager.RefreshAuth so it shares the
	// singleflight group with the auto-refresh loop and any in-flight
	// request-time refresh. Calling exec.Refresh directly here would
	// double-spend the rotating refresh_token (AWS SSO-OIDC and the kiro
	// social endpoint both rotate on every successful call), bricking the
	// credential with invalid_grant on the second concurrent call.
	updated, err := h.authManager.RefreshAuth(ctx, refreshAuth)
	if err != nil {
		if required {
			return nil, http.StatusBadGateway, err
		}
		return refreshAuth, http.StatusOK, nil
	}
	if updated == nil {
		return refreshAuth, http.StatusOK, nil
	}
	// On a successful refresh, drop any cached usage entry — the new
	// access token may resolve to a different profile_arn or change the
	// upstream view of the account, so the safer behaviour is to refetch.
	if cache := h.kiroUsageHandlerCache(); cache != nil {
		cache.invalidate(updated.ID)
	}
	if latest, ok := h.authManager.GetByID(updated.ID); ok && latest != nil {
		return latest, http.StatusOK, nil
	}
	return updated, http.StatusOK, nil
}

func (h *Handler) kiroUsageRefreshExecutor(provider string) (coreauth.ProviderExecutor, bool) {
	if h == nil || h.authManager == nil {
		return nil, false
	}
	exec, ok := h.authManager.Executor(provider)
	if ok && exec != nil {
		return exec, true
	}
	if !strings.EqualFold(strings.TrimSpace(provider), "kiro") {
		return nil, false
	}
	exec = runtimeexecutor.NewKiroExecutor(h.cfg)
	h.authManager.RegisterExecutor(exec)
	return exec, true
}

func shouldRefreshKiroUsageAuth(auth *coreauth.Auth, now time.Time) (bool, bool) {
	if auth == nil {
		return false, false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if expiresAt, ok := auth.ExpirationTime(); ok && !expiresAt.IsZero() && expiresAt.Sub(now) <= 2*time.Minute {
		return true, true
	}
	if !auth.NextRefreshAfter.IsZero() {
		return !now.Before(auth.NextRefreshAfter), false
	}
	interval := kiroUsageRefreshInterval(auth)
	if interval <= 0 {
		interval = time.Duration(kiroauth.DefaultRefreshIntervalMaxSeconds) * time.Second
	}
	lastRefresh, ok := kiroUsageLastRefresh(auth)
	if !ok || lastRefresh.IsZero() {
		return true, false
	}
	return !lastRefresh.Add(interval).After(now), false
}

func kiroUsageRefreshInterval(auth *coreauth.Auth) time.Duration {
	if auth == nil {
		return 0
	}
	for _, key := range []string{"refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"} {
		if auth.Metadata != nil {
			if d := kiroUsageParseDuration(auth.Metadata[key]); d > 0 {
				return d
			}
		}
		if auth.Attributes != nil {
			if d := kiroUsageParseDuration(auth.Attributes[key]); d > 0 {
				return d
			}
		}
	}
	return 0
}

func kiroUsageLastRefresh(auth *coreauth.Auth) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	for _, key := range []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"} {
		if auth.Metadata != nil {
			if ts, ok := kiroUsageParseTime(auth.Metadata[key]); ok {
				return ts, true
			}
		}
		if auth.Attributes != nil {
			if ts, ok := kiroUsageParseTime(auth.Attributes[key]); ok {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func kiroUsageParseDuration(value any) time.Duration {
	switch v := value.(type) {
	case int:
		if v > 0 {
			return time.Duration(v) * time.Second
		}
	case int64:
		if v > 0 {
			return time.Duration(v) * time.Second
		}
	case float64:
		if v > 0 {
			return time.Duration(v) * time.Second
		}
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0
		}
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil && i > 0 {
			return time.Duration(i) * time.Second
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
			return time.Duration(f) * time.Second
		}
	}
	return 0
}

func kiroUsageParseTime(value any) (time.Time, bool) {
	switch v := value.(type) {
	case time.Time:
		return v, !v.IsZero()
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return time.Time{}, false
		}
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return ts, true
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts, true
		}
	case int64:
		if v > 0 {
			return time.Unix(v, 0), true
		}
	case float64:
		if v > 0 {
			return time.Unix(int64(v), 0), true
		}
	}
	return time.Time{}, false
}

func tokenDataFromKiroAuth(auth *coreauth.Auth) (*kiroauth.TokenData, error) {
	if auth == nil || len(auth.Metadata) == 0 {
		return nil, fmt.Errorf("kiro auth metadata is missing")
	}

	data, err := json.Marshal(auth.Metadata)
	if err != nil {
		return nil, fmt.Errorf("kiro auth metadata is invalid: %w", err)
	}
	tokenData, err := kiroauth.ParseTokenData(data)
	if err != nil {
		return nil, fmt.Errorf("kiro auth metadata is invalid: %w", err)
	}

	if tokenData.AccessToken == "" {
		tokenData.AccessToken = kiroAuthString(auth, "access_token", "accessToken")
	}
	if tokenData.RefreshToken == "" {
		tokenData.RefreshToken = kiroAuthString(auth, "refresh_token", "refreshToken")
	}
	if tokenData.ProfileArn == "" {
		tokenData.ProfileArn = kiroAuthString(auth, "profile_arn", "profileArn")
	}
	if tokenData.ClientID == "" {
		tokenData.ClientID = kiroAuthString(auth, "client_id", "clientId")
	}
	if tokenData.ClientSecret == "" {
		tokenData.ClientSecret = kiroAuthString(auth, "client_secret", "clientSecret")
	}
	if tokenData.ClientIDHash == "" {
		tokenData.ClientIDHash = kiroAuthString(auth, "client_id_hash", "clientIdHash")
	}
	if tokenData.Email == "" {
		tokenData.Email = kiroAuthString(auth, "email")
	}
	if tokenData.Region == "" {
		tokenData.Region = kiroAuthString(auth, "region")
	}
	if tokenData.AuthMethod == "" {
		tokenData.AuthMethod = kiroAuthString(auth, "auth_method", "authMethod")
	}
	if tokenData.Provider == "" {
		tokenData.Provider = kiroAuthString(auth, "provider")
	}
	if tokenData.MachineID == "" {
		tokenData.MachineID = kiroAuthString(auth, "machine_id", "machineId", "device_id", "deviceId")
	}
	if strings.TrimSpace(tokenData.AccessToken) == "" {
		return nil, fmt.Errorf("kiro access token is missing")
	}
	return tokenData, nil
}

func kiroAuthString(auth *coreauth.Auth, keys ...string) string {
	if auth == nil {
		return ""
	}
	for _, key := range keys {
		if auth.Metadata != nil {
			if value := strings.TrimSpace(valueAsString(auth.Metadata[key])); value != "" {
				return value
			}
		}
		if auth.Attributes != nil {
			if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
				return value
			}
		}
	}
	return ""
}

func (h *Handler) persistResolvedKiroProfileArn(ctx context.Context, auth *coreauth.Auth, profileArn string) {
	profileArn = strings.TrimSpace(profileArn)
	if profileArn == "" || h == nil || h.authManager == nil || auth == nil || auth.ID == "" {
		return
	}
	current := strings.TrimSpace(kiroAuthString(auth, "profile_arn", "profileArn"))
	if current == profileArn {
		return
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = map[string]any{}
	}
	updated.Metadata["profile_arn"] = profileArn
	if updated.Attributes == nil {
		updated.Attributes = map[string]string{}
	}
	updated.Attributes["profile_arn"] = profileArn
	updated.UpdatedAt = time.Now().UTC()
	updateCtx := context.Background()
	if ctx != nil {
		updateCtx = context.WithoutCancel(ctx)
	}
	if _, err := h.authManager.Update(updateCtx, updated); err != nil {
		return
	}
}
