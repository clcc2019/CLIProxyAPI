package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// CodexLoginModeMetadataKey selects the Codex login ceremony through
	// LoginOptions.Metadata. Device login is the default; callers that still
	// need the localhost callback flow can explicitly select browser mode.
	CodexLoginModeMetadataKey = "codex_login_mode"
	CodexLoginModeDevice      = "device"
	CodexLoginModeBrowser     = "browser"

	codexDeviceUserCodeURL                = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	codexDeviceTokenURL                   = "https://auth.openai.com/api/accounts/deviceauth/token"
	codexDeviceVerificationURL            = "https://auth.openai.com/codex/device"
	codexDeviceTokenExchangeRedirectURI   = "https://auth.openai.com/deviceauth/callback"
	codexDeviceTimeout                    = 15 * time.Minute
	codexDeviceDefaultPollIntervalSeconds = 5
)

type codexDeviceUserCodeRequest struct {
	ClientID string `json:"client_id"`
}

type codexDeviceUserCodeResponse struct {
	DeviceAuthID string          `json:"device_auth_id"`
	UserCode     string          `json:"user_code"`
	UserCodeAlt  string          `json:"usercode"`
	Interval     json.RawMessage `json:"interval"`
	ExpiresIn    json.RawMessage `json:"expires_in"`
}

type codexDeviceTokenRequest struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
}

type codexDeviceTokenResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
	CodeChallenge     string `json:"code_challenge"`
}

// CodexDeviceAuthorization contains the public instructions and private
// handle for an in-progress Codex device login. DeviceAuthID must remain on
// the trusted side of an integration; only VerificationURL and UserCode
// should be rendered to an end user.
type CodexDeviceAuthorization struct {
	DeviceAuthID    string        `json:"-"`
	ProxyURL        string        `json:"-"`
	UserCode        string        `json:"user_code"`
	VerificationURL string        `json:"verification_url"`
	PollInterval    time.Duration `json:"-"`
	ExpiresAt       time.Time     `json:"expires_at"`
}

func shouldUseCodexDeviceFlow(opts *LoginOptions) bool {
	if opts == nil || opts.Metadata == nil {
		return true
	}
	mode := strings.TrimSpace(opts.Metadata[CodexLoginModeMetadataKey])
	return !strings.EqualFold(mode, CodexLoginModeBrowser)
}

func (a *CodexAuthenticator) loginWithDeviceFlow(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	authorization, err := a.StartDeviceFlow(ctx, cfg)
	if err != nil {
		return nil, err
	}

	fmt.Println("Starting Codex device authentication...")
	fmt.Printf("Codex device URL: %s\n", authorization.VerificationURL)
	fmt.Printf("Codex device code: %s\n", authorization.UserCode)

	if !opts.NoBrowser {
		if !browser.IsAvailable() {
			log.Warn("No browser available; please open the device URL manually")
		} else if errOpen := browser.OpenURL(authorization.VerificationURL); errOpen != nil {
			log.Warnf("Failed to open browser automatically: %v", errOpen)
		}
	}

	return a.CompleteDeviceFlow(ctx, cfg, authorization, opts)
}

// StartDeviceFlow requests a one-time Codex user code. It deliberately does
// not open a browser or start polling so API and TUI integrations can return
// the public instructions before completing the login in the background.
func (a *CodexAuthenticator) StartDeviceFlow(ctx context.Context, cfg *config.Config) (*CodexDeviceAuthorization, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	proxyURL := util.OAuthProxyURL(cfg)
	httpClient := codexDeviceHTTPClient(cfg, proxyURL)
	userCodeResp, err := requestCodexDeviceUserCode(ctx, httpClient, codexDeviceUserCodeURL)
	if err != nil {
		return nil, err
	}

	userCode := strings.TrimSpace(userCodeResp.UserCode)
	if userCode == "" {
		userCode = strings.TrimSpace(userCodeResp.UserCodeAlt)
	}
	deviceAuthID := strings.TrimSpace(userCodeResp.DeviceAuthID)
	if userCode == "" || deviceAuthID == "" {
		return nil, fmt.Errorf("codex device flow did not return required fields")
	}

	return &CodexDeviceAuthorization{
		DeviceAuthID:    deviceAuthID,
		ProxyURL:        proxyURL,
		UserCode:        userCode,
		VerificationURL: codexDeviceVerificationURL,
		PollInterval:    parseCodexDevicePollInterval(userCodeResp.Interval),
		ExpiresAt:       time.Now().Add(parseCodexDeviceExpiresIn(userCodeResp.ExpiresIn)),
	}, nil
}

// CompleteDeviceFlow waits for the user to approve a previously started
// device login, exchanges the resulting authorization code, and returns the
// same Auth record shape used by the browser callback flow.
func (a *CodexAuthenticator) CompleteDeviceFlow(ctx context.Context, cfg *config.Config, authorization *CodexDeviceAuthorization, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if authorization == nil {
		return nil, fmt.Errorf("codex device authorization is required")
	}
	deviceAuthID := strings.TrimSpace(authorization.DeviceAuthID)
	userCode := strings.TrimSpace(authorization.UserCode)
	if deviceAuthID == "" || userCode == "" {
		return nil, fmt.Errorf("codex device authorization is missing required fields")
	}

	interval := authorization.PollInterval
	if interval <= 0 {
		interval = time.Duration(codexDeviceDefaultPollIntervalSeconds) * time.Second
	}
	deadline := authorization.ExpiresAt
	if deadline.IsZero() {
		deadline = time.Now().Add(codexDeviceTimeout)
	}

	proxyURL := strings.TrimSpace(authorization.ProxyURL)
	if proxyURL == "" {
		proxyURL = util.OAuthProxyURL(cfg)
	}
	httpClient := codexDeviceHTTPClient(cfg, proxyURL)
	tokenResp, err := pollCodexDeviceToken(ctx, httpClient, codexDeviceTokenURL, deviceAuthID, userCode, interval, deadline)
	if err != nil {
		return nil, err
	}

	authCode := strings.TrimSpace(tokenResp.AuthorizationCode)
	codeVerifier := strings.TrimSpace(tokenResp.CodeVerifier)
	codeChallenge := strings.TrimSpace(tokenResp.CodeChallenge)
	if authCode == "" || codeVerifier == "" {
		return nil, fmt.Errorf("codex device flow token response missing required fields")
	}

	authSvc := codex.NewCodexAuthWithProxyURL(cfg, proxyURL)
	authBundle, err := authSvc.ExchangeCodeForTokensWithRedirect(
		ctx,
		authCode,
		codexDeviceTokenExchangeRedirectURI,
		&codex.PKCECodes{
			CodeVerifier:  codeVerifier,
			CodeChallenge: codeChallenge,
		},
	)
	if err != nil {
		return nil, codex.NewAuthenticationError(codex.ErrCodeExchangeFailed, err)
	}

	return a.buildAuthRecord(authSvc, authBundle, opts)
}

func codexDeviceHTTPClient(cfg *config.Config, proxyURL string) *http.Client {
	sdkCfg := cfg.SDKConfig
	sdkCfg.ProxyURL = strings.TrimSpace(proxyURL)
	return util.SetProxy(&sdkCfg, &http.Client{})
}

func requestCodexDeviceUserCode(ctx context.Context, client *http.Client, endpoint string) (*codexDeviceUserCodeResponse, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("codex device code endpoint is required")
	}
	body, err := json.Marshal(codexDeviceUserCodeRequest{ClientID: codex.ClientID})
	if err != nil {
		return nil, fmt.Errorf("failed to encode codex device request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create codex device request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to request codex device code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := util.ReadResponseBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read codex device code response: %w", err)
	}

	if !codexDeviceIsSuccessStatus(resp.StatusCode) {
		trimmed := strings.TrimSpace(string(respBody))
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("codex device login is unavailable (status %d); enable device code authentication in ChatGPT settings or use browser login", resp.StatusCode)
		}
		if trimmed == "" {
			trimmed = "empty response body"
		}
		return nil, fmt.Errorf("codex device code request failed with status %d: %s", resp.StatusCode, trimmed)
	}

	var parsed codexDeviceUserCodeResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("failed to decode codex device code response: %w", err)
	}

	return &parsed, nil
}

func pollCodexDeviceToken(ctx context.Context, client *http.Client, endpoint, deviceAuthID, userCode string, interval time.Duration, deadline time.Time) (*codexDeviceTokenResponse, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("codex device token endpoint is required")
	}
	if interval <= 0 {
		interval = time.Duration(codexDeviceDefaultPollIntervalSeconds) * time.Second
	}
	if deadline.IsZero() {
		deadline = time.Now().Add(codexDeviceTimeout)
	}

	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("codex device authentication timed out after 15 minutes")
		}

		body, err := json.Marshal(codexDeviceTokenRequest{
			DeviceAuthID: deviceAuthID,
			UserCode:     userCode,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to encode codex device poll request: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("failed to create codex device poll request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to poll codex device token: %w", err)
		}

		respBody, readErr := util.ReadResponseBody(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("failed to read codex device poll response: %w", readErr)
		}

		switch {
		case codexDeviceIsSuccessStatus(resp.StatusCode):
			var parsed codexDeviceTokenResponse
			if err := json.Unmarshal(respBody, &parsed); err != nil {
				return nil, fmt.Errorf("failed to decode codex device token response: %w", err)
			}
			return &parsed, nil
		case codexDevicePollShouldContinue(resp.StatusCode, respBody):
			if resp.StatusCode == http.StatusTooManyRequests || codexDevicePollErrorCode(respBody) == "slow_down" {
				interval += 5 * time.Second
			}
			if errWait := waitForCodexDevicePoll(ctx, interval, deadline); errWait != nil {
				return nil, errWait
			}
			continue
		default:
			trimmed := strings.TrimSpace(string(respBody))
			if trimmed == "" {
				trimmed = "empty response body"
			}
			return nil, fmt.Errorf("codex device token polling failed with status %d: %s", resp.StatusCode, trimmed)
		}
	}
}

func codexDevicePollShouldContinue(statusCode int, body []byte) bool {
	code := codexDevicePollErrorCode(body)
	if code == "authorization_pending" || code == "pending" || code == "slow_down" {
		return true
	}
	if codexDevicePollErrorIsTerminal(code) {
		return false
	}
	return statusCode == http.StatusForbidden ||
		statusCode == http.StatusNotFound ||
		statusCode == http.StatusTooManyRequests ||
		statusCode >= http.StatusInternalServerError
}

func codexDevicePollErrorCode(body []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(payload.Error))
}

func codexDevicePollErrorIsTerminal(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "access_denied", "authorization_declined", "expired_token", "invalid_device_code", "invalid_grant":
		return true
	default:
		return false
	}
}

func waitForCodexDevicePoll(ctx context.Context, interval time.Duration, deadline time.Time) error {
	if ctx == nil {
		ctx = context.Background()
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fmt.Errorf("codex device authentication timed out after 15 minutes")
	}
	if interval <= 0 {
		interval = time.Duration(codexDeviceDefaultPollIntervalSeconds) * time.Second
	}
	if interval > remaining {
		interval = remaining
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		if !time.Now().Before(deadline) {
			return fmt.Errorf("codex device authentication timed out after 15 minutes")
		}
		return nil
	}
}

func parseCodexDevicePollInterval(raw json.RawMessage) time.Duration {
	defaultInterval := time.Duration(codexDeviceDefaultPollIntervalSeconds) * time.Second
	return parseCodexDeviceSeconds(raw, defaultInterval)
}

func parseCodexDeviceExpiresIn(raw json.RawMessage) time.Duration {
	return parseCodexDeviceSeconds(raw, codexDeviceTimeout)
}

func parseCodexDeviceSeconds(raw json.RawMessage, fallback time.Duration) time.Duration {
	if len(raw) == 0 {
		return fallback
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if seconds, convErr := strconv.Atoi(strings.TrimSpace(asString)); convErr == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}

	var asInt int
	if err := json.Unmarshal(raw, &asInt); err == nil && asInt > 0 {
		return time.Duration(asInt) * time.Second
	}

	return fallback
}

func codexDeviceIsSuccessStatus(code int) bool {
	return code >= 200 && code < 300
}

func (a *CodexAuthenticator) buildAuthRecord(authSvc *codex.CodexAuth, authBundle *codex.CodexAuthBundle, opts *LoginOptions) (*coreauth.Auth, error) {
	var metadataInput map[string]string
	if opts != nil {
		metadataInput = opts.Metadata
	}
	return a.buildAuthRecordWithClientFeatures(authSvc, authBundle, opts, NewCodexClientFeatures(metadataInput))
}

func (a *CodexAuthenticator) buildAuthRecordWithClientFeatures(authSvc *codex.CodexAuth, authBundle *codex.CodexAuthBundle, opts *LoginOptions, clientFeatures CodexClientFeatures) (*coreauth.Auth, error) {
	tokenStorage := authSvc.CreateTokenStorage(authBundle)

	if tokenStorage == nil || tokenStorage.Email == "" {
		return nil, fmt.Errorf("codex token storage missing account information")
	}

	planType := strings.TrimSpace(tokenStorage.PlanType)
	hashAccountID := ""
	if accountID := strings.TrimSpace(tokenStorage.AccountID); accountID != "" {
		digest := sha256.Sum256([]byte(accountID))
		hashAccountID = hex.EncodeToString(digest[:])[:8]
	}
	if tokenStorage.IDToken != "" {
		if claims, errParse := codex.ParseJWTToken(tokenStorage.IDToken); errParse == nil && claims != nil {
			if planType == "" {
				planType = strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType)
			}
			if hashAccountID == "" {
				accountID := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID)
				if accountID == "" {
					accountID = strings.TrimSpace(tokenStorage.AccountID)
				}
				if accountID != "" {
					digest := sha256.Sum256([]byte(accountID))
					hashAccountID = hex.EncodeToString(digest[:])[:8]
				}
			}
		}
	}

	fileName := codex.CredentialFileName(tokenStorage.Email, planType, hashAccountID, true)
	metadata := map[string]any{
		"email":        tokenStorage.Email,
		"access_token": tokenStorage.AccessToken,
	}
	if tokenStorage.AccountID != "" {
		metadata["account_id"] = tokenStorage.AccountID
	}
	if planType != "" {
		metadata["plan_type"] = planType
	}
	if tokenStorage.RefreshToken != "" {
		metadata["refresh_token"] = tokenStorage.RefreshToken
	}
	if tokenStorage.IDToken != "" {
		metadata["id_token"] = tokenStorage.IDToken
	}
	clientFeatures.AddToMetadata(metadata)

	fmt.Println("Codex authentication successful")
	if authBundle.APIKey != "" {
		fmt.Println("Codex API key obtained and stored")
	}

	attrs := map[string]string{}
	if planType != "" {
		attrs["plan_type"] = planType
	}
	if tokenStorage.AccountID != "" {
		attrs["account_id"] = tokenStorage.AccountID
	}
	clientFeatures.AddToAttributes(attrs)

	record := &coreauth.Auth{
		ID:         fileName,
		Provider:   a.Provider(),
		FileName:   fileName,
		Storage:    tokenStorage,
		Metadata:   metadata,
		Attributes: attrs,
	}
	coreauth.MarkCodexInstallationIDExplicit(record, clientFeatures.InstallationExplicit)
	return record, nil
}
