// Package kimi provides token refresh support for existing Kimi credentials.
package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	internalauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	// kimiClientID is Kimi Code's OAuth client ID.
	kimiClientID = "17e5f671-d194-4dfb-9706-5516cb48c098"
	// kimiOAuthHost is the OAuth server endpoint.
	kimiOAuthHost = "https://auth.kimi.com"
	// kimiTokenURL is the endpoint for refreshing access tokens.
	kimiTokenURL = kimiOAuthHost + "/api/oauth/token"
	// KimiAPIBaseURL is the base URL for Kimi API requests.
	KimiAPIBaseURL = "https://api.kimi.com/coding"
	// refreshThresholdSeconds is when to refresh token before expiry (5 minutes).
	refreshThresholdSeconds = 300
)

// TokenRefreshClient retains the HTTP client and device identity needed to
// refresh existing Kimi credentials.
type TokenRefreshClient struct {
	httpClient *http.Client
	deviceID   string
}

// kimiRefreshGroup prevents concurrent callers from exchanging the same Kimi
// refresh token more than once, including callers using separate refresh
// clients with different device metadata.
var kimiRefreshGroup internalauth.RefreshGroup[*KimiTokenData]

// NewTokenRefreshClient creates a refresh client with a device ID and proxy override.
// proxyURL takes precedence over cfg.ProxyURL when non-empty.
func NewTokenRefreshClient(cfg *config.Config, deviceID string, proxyURL string) *TokenRefreshClient {
	client := &http.Client{Timeout: 30 * time.Second}
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL
	client = util.SetProxy(&sdkCfg, client)

	resolvedDeviceID := strings.TrimSpace(deviceID)
	if resolvedDeviceID == "" {
		resolvedDeviceID = getOrCreateDeviceID()
	}
	return &TokenRefreshClient{
		httpClient: client,
		deviceID:   resolvedDeviceID,
	}
}

// getOrCreateDeviceID returns an in-memory device ID for token refresh requests.
func getOrCreateDeviceID() string {
	return uuid.New().String()
}

// getDeviceModel returns a device model string.
func getDeviceModel() string {
	osName := runtime.GOOS
	arch := runtime.GOARCH

	switch osName {
	case "darwin":
		return fmt.Sprintf("macOS %s", arch)
	case "windows":
		return fmt.Sprintf("Windows %s", arch)
	case "linux":
		return fmt.Sprintf("Linux %s", arch)
	default:
		return fmt.Sprintf("%s %s", osName, arch)
	}
}

// getHostname returns the machine hostname.
func getHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

// commonHeaders returns headers required for Kimi API requests.
func (c *TokenRefreshClient) commonHeaders() map[string]string {
	return map[string]string{
		"X-Msh-Platform":     "cli-proxy-api",
		"X-Msh-Version":      "1.0.0",
		"X-Msh-Device-Name":  getHostname(),
		"X-Msh-Device-Model": getDeviceModel(),
		"X-Msh-Device-Id":    c.deviceID,
	}
}

// RefreshError describes a non-OK response from the Kimi token endpoint.
//
// It exists so callers can act on the status rather than parse the message.
// Previously these were bare fmt.Errorf values, which left the conductor
// matching on the literal text "status 401": a 401 was classified as permanent
// only by accident, and a 403 — an equally final refusal — was not classified
// at all and would be retried on the background cadence indefinitely.
type RefreshError struct {
	statusCode int
	body       string
	permanent  bool
}

func (e *RefreshError) Error() string {
	if e == nil {
		return ""
	}
	if e.permanent {
		return fmt.Sprintf("kimi: refresh token rejected (status %d)", e.statusCode)
	}
	if e.body != "" {
		return fmt.Sprintf("kimi: refresh failed with status %d: %s", e.statusCode, e.body)
	}
	return fmt.Sprintf("kimi: refresh failed with status %d", e.statusCode)
}

// StatusCode reports the HTTP status returned by the token endpoint.
func (e *RefreshError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.statusCode
}

// IsPermanentAuthError reports whether the credential is dead rather than
// temporarily unavailable. 401 and 403 both mean the refresh token will not
// start working again on its own, so the auth should be parked for an operator
// instead of retried.
func (e *RefreshError) IsPermanentAuthError() bool {
	if e == nil {
		return false
	}
	return e.permanent
}

// RefreshTokenWithRetry refreshes a Kimi access token, retrying transient
// failures. A single network blip or upstream 5xx would otherwise surface as a
// refresh failure and cost the credential a failover it did not need.
//
// Permanent rejections (401/403) return immediately: the refresh token has been
// revoked or rotated away, and replaying it only burns upstream quota while
// keeping a dead credential looking busy.
func (c *TokenRefreshClient) RefreshTokenWithRetry(ctx context.Context, refreshToken string, maxRetries int) (*KimiTokenData, error) {
	if maxRetries < 1 {
		maxRetries = 1
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Linear backoff, matching the Claude and xAI refresh paths.
			// Waiting on ctx.Done() keeps a cancelled request from sitting
			// through the remaining schedule.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}

		tokenData, err := c.RefreshToken(ctx, refreshToken)
		if err == nil {
			return tokenData, nil
		}
		lastErr = err

		if ctx.Err() != nil {
			return nil, err
		}
		var refreshErr *RefreshError
		if errors.As(err, &refreshErr) && refreshErr.IsPermanentAuthError() {
			return nil, err
		}
		log.Warnf("kimi token refresh attempt %d/%d failed: %v", attempt+1, maxRetries, err)
	}

	return nil, lastErr
}

func (c *TokenRefreshClient) RefreshToken(ctx context.Context, refreshToken string) (*KimiTokenData, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, fmt.Errorf("kimi: refresh token is required")
	}
	tokenData, err := kimiRefreshGroup.Do(ctx, refreshToken, func(refreshCtx context.Context) (*KimiTokenData, error) {
		return c.refreshTokenSingleFlight(refreshCtx, refreshToken)
	})
	if err != nil {
		return nil, err
	}
	if tokenData == nil {
		return nil, fmt.Errorf("kimi: refresh returned invalid single-flight result")
	}
	cloned := *tokenData
	return &cloned, nil
}

func (c *TokenRefreshClient) refreshTokenSingleFlight(ctx context.Context, refreshToken string) (*KimiTokenData, error) {
	data := url.Values{}
	data.Set("client_id", kimiClientID)
	data.Set("grant_type", "refresh_token")
	data.Set("refresh_token", refreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kimiTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("kimi: failed to create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for k, v := range c.commonHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kimi: refresh request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("kimi refresh token: close body error: %v", errClose)
		}
	}()

	bodyBytes, err := util.ReadResponseBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("kimi: failed to read refresh response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, &RefreshError{statusCode: resp.StatusCode, permanent: true}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &RefreshError{statusCode: resp.StatusCode, body: strings.TrimSpace(string(bodyBytes))}
	}

	var tokenResp struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		TokenType    string  `json:"token_type"`
		ExpiresIn    float64 `json:"expires_in"`
		Scope        string  `json:"scope"`
	}

	if err = json.Unmarshal(bodyBytes, &tokenResp); err != nil {
		return nil, fmt.Errorf("kimi: failed to parse refresh response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("kimi: empty access token in refresh response")
	}

	var expiresAt int64
	if tokenResp.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + int64(tokenResp.ExpiresIn)
	}

	return &KimiTokenData{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    expiresAt,
		Scope:        tokenResp.Scope,
	}, nil
}
