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
	"time"

	"github.com/gin-gonic/gin"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type kiroUsageClient interface {
	GetUsageLimits(context.Context, *kiroauth.TokenData) (*kiroauth.KiroUsageInfo, error)
}

var newKiroUsageClient = func(cfg *config.Config) kiroUsageClient {
	return kiroauth.NewKiroAuth(cfg)
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
		c.JSON(status, gin.H{"error": err.Error()})
		return
	} else if refreshed != nil {
		auth = refreshed
	}

	tokenData, err := tokenDataFromKiroAuth(auth)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	usageClient := newKiroUsageClient(h.cfg)
	usage, err := usageClient.GetUsageLimits(ctx, tokenData)
	if err != nil {
		if kiroauth.IsUnauthorizedStatusError(err) {
			refreshed, status, refreshErr := h.refreshKiroUsageAuth(c.Request.Context(), auth, true)
			if refreshErr != nil {
				c.JSON(status, gin.H{"error": refreshErr.Error()})
				return
			}
			if refreshed != nil {
				auth = refreshed
			}
			tokenData, refreshErr = tokenDataFromKiroAuth(auth)
			if refreshErr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": refreshErr.Error()})
				return
			}
			usage, err = usageClient.GetUsageLimits(ctx, tokenData)
		}
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	h.persistResolvedKiroProfileArn(c.Request.Context(), auth, tokenData.ProfileArn)
	c.JSON(http.StatusOK, usage)
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
	return &coreauth.Auth{
		ID:         name,
		FileName:   name,
		Provider:   provider,
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"path": fullPath},
		Metadata:   metadata,
	}, http.StatusOK, nil
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
	shouldRefresh, required := shouldRefreshKiroUsageAuth(refreshAuth, time.Now().UTC())
	if !shouldRefresh {
		if force {
			required = true
		} else {
			return refreshAuth, http.StatusOK, nil
		}
	}
	exec, ok := h.authManager.Executor(refreshAuth.Provider)
	if !ok || exec == nil {
		if force {
			return nil, http.StatusBadGateway, fmt.Errorf("kiro auth refresh executor is unavailable")
		}
		return refreshAuth, http.StatusOK, nil
	}
	updated, err := exec.Refresh(ctx, refreshAuth.Clone())
	if err != nil {
		if required {
			return nil, http.StatusBadGateway, err
		}
		return refreshAuth, http.StatusOK, nil
	}
	if updated == nil {
		return refreshAuth, http.StatusOK, nil
	}
	if updated.ID == "" {
		updated.ID = refreshAuth.ID
	}
	if updated.FileName == "" {
		updated.FileName = refreshAuth.FileName
	}
	if updated.Provider == "" {
		updated.Provider = refreshAuth.Provider
	}
	if _, err := h.authManager.Update(ctx, updated); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if latest, ok := h.authManager.GetByID(updated.ID); ok && latest != nil {
		return latest, http.StatusOK, nil
	}
	return updated, http.StatusOK, nil
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
	if _, err := h.authManager.Update(ctx, updated); err != nil {
		return
	}
}
