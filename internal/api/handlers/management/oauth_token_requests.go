package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	xaiauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type codexDeviceLoginService interface {
	StartDeviceFlow(context.Context, *config.Config) (*sdkAuth.CodexDeviceAuthorization, error)
	CompleteDeviceFlow(context.Context, *config.Config, *sdkAuth.CodexDeviceAuthorization, *sdkAuth.LoginOptions) (*coreauth.Auth, error)
}

var newCodexDeviceLoginService = func() codexDeviceLoginService {
	return sdkAuth.NewCodexAuthenticator()
}

func (h *Handler) tokenStoreWithBaseDir() coreauth.Store {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	store := h.tokenStore
	if store == nil {
		store = sdkAuth.GetTokenStore()
		h.tokenStore = store
	}
	authDir := ""
	if h.cfg != nil {
		authDir = h.cfg.AuthDir
	}
	h.mu.Unlock()

	if dirSetter, ok := store.(interface{ SetBaseDir(string) }); ok {
		dirSetter.SetBaseDir(authDir)
	}
	return store
}

func (h *Handler) saveTokenRecord(ctx context.Context, record *coreauth.Auth) (string, error) {
	if record == nil {
		return "", fmt.Errorf("token record is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	store := h.tokenStoreWithBaseDir()
	if store == nil {
		return "", fmt.Errorf("token store unavailable")
	}
	if err := coreauth.ReuseCodexInstallationID(ctx, store, record); err != nil {
		return "", err
	}
	if hook := h.postAuthHookSnapshot(); hook != nil {
		if err := hook(ctx, record); err != nil {
			return "", fmt.Errorf("post-auth hook failed: %w", err)
		}
	}
	savedPath, err := store.Save(ctx, record)
	if err != nil {
		return "", err
	}

	// The management list is backed by the core manager, while the OAuth flow
	// persists through the token store. Normally the file watcher bridges those
	// two components, but it is asynchronous and is not present for every
	// embedded/server setup. Publish the saved record immediately so a successful
	// OAuth flow is visible to management callers without waiting for a watcher
	// round trip. Skip the second persistence pass; the token store write above
	// is already complete and the watcher can later replace this snapshot with
	// its file-projected version.
	if manager := h.authManagerSnapshot(); manager != nil {
		if strings.TrimSpace(savedPath) != "" {
			if record.Attributes == nil {
				record.Attributes = make(map[string]string)
			}
			if strings.TrimSpace(record.Attributes["path"]) == "" {
				record.Attributes["path"] = savedPath
			}
		}
		publishedRecord := h.projectSavedTokenRecord(record, savedPath)
		publishCtx := coreauth.WithSkipPersist(ctx)
		var publishErr error
		if _, exists := manager.GetByID(publishedRecord.ID); exists {
			_, publishErr = manager.Update(publishCtx, publishedRecord)
		} else {
			_, publishErr = manager.Register(publishCtx, publishedRecord)
		}
		if publishErr != nil {
			return savedPath, fmt.Errorf("register saved authentication: %w", publishErr)
		}
	}

	return savedPath, nil
}

// projectSavedTokenRecord reads the just-persisted JSON back into the same
// shape used by the file watcher. OAuth storage implementations may keep the
// actual token fields on Auth.Storage rather than Auth.Metadata, so publishing
// the pre-save record directly would make a newly authenticated account visible
// but temporarily unusable until the watcher projected the file.
func (h *Handler) projectSavedTokenRecord(record *coreauth.Auth, savedPath string) *coreauth.Auth {
	if h == nil || record == nil || strings.TrimSpace(savedPath) == "" {
		return record
	}
	authDir := h.authDirSnapshot()
	data, errRead := readManagedAuthPathFile(savedPath, authDir)
	if errRead != nil || len(data) == 0 {
		return record
	}

	projected, errProject := coreauth.NewAuthFromAuthFileData(data, coreauth.AuthFileProjectionOptions{
		ID:                     record.ID,
		Path:                   savedPath,
		BaseDir:                authDir,
		FileName:               record.FileName,
		UseBaseNameAsFileName:  strings.TrimSpace(record.FileName) == "",
		IncludeSourceAttribute: true,
		Now:                    time.Now(),
	})
	if errProject != nil || projected == nil {
		return record
	}
	return projected
}

func (h *Handler) waitForOAuthCallbackFile(provider, state, fileName string, timeout time.Duration) (map[string]string, error) {
	deadline := time.Now().Add(timeout)
	for {
		if !IsOAuthSessionPending(state, provider) {
			return nil, errOAuthSessionNotPending
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout waiting for OAuth callback")
		}
		authDir := h.authDirSnapshot()
		data, errRead := readAuthDirFileAt(authDir, fileName)
		if errRead == nil {
			var payload oauthCallbackFilePayload
			if errDecode := json.Unmarshal(data, &payload); errDecode != nil {
				return nil, fmt.Errorf("decode OAuth callback: %w", errDecode)
			}
			payload.Code = strings.TrimSpace(payload.Code)
			payload.State = strings.TrimSpace(payload.State)
			payload.Error = strings.TrimSpace(payload.Error)
			if payload.State == "" {
				return nil, fmt.Errorf("OAuth callback is missing state")
			}
			if payload.Code == "" && payload.Error == "" {
				return nil, fmt.Errorf("OAuth callback is missing code or error")
			}
			_ = os.Remove(filepath.Join(authDir, fileName))
			return map[string]string{
				"code":  payload.Code,
				"state": payload.State,
				"error": payload.Error,
			}, nil
		}
		if !errors.Is(errRead, os.ErrNotExist) {
			return nil, fmt.Errorf("read OAuth callback: %w", errRead)
		}
		time.Sleep(500 * time.Millisecond)
	}
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
	anthropicAuth := claude.NewClaudeAuthWithProxyURL(h.cfg, util.OAuthProxyURL(h.cfg))

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

		waitFileName := fmt.Sprintf(".oauth-anthropic-%s.oauth", state)

		fmt.Println("Waiting for authentication callback...")
		// Wait up to 5 minutes
		resultMap, errWait := h.waitForOAuthCallbackFile("anthropic", state, waitFileName, 5*time.Minute)
		if errWait != nil {
			if errors.Is(errWait, errOAuthSessionNotPending) {
				return
			}
			SetOAuthSessionError(state, "Timeout waiting for OAuth callback")
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
		fileName := claude.CredentialFileName(tokenStorage.Email)
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "claude",
			FileName: fileName,
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
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestCodexToken(c *gin.Context) {
	mode, errMode := codexLoginModeFromRequest(c)
	if errMode != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMode.Error()})
		return
	}
	if mode == sdkAuth.CodexLoginModeBrowser {
		h.requestCodexBrowserToken(c)
		return
	}
	h.requestCodexDeviceToken(c)
}

func codexLoginModeFromRequest(c *gin.Context) (string, error) {
	mode := ""
	if c != nil {
		mode = strings.TrimSpace(c.Query("mode"))
		if mode == "" {
			mode = strings.TrimSpace(c.Query("login_mode"))
		}
	}
	switch {
	case mode == "", strings.EqualFold(mode, sdkAuth.CodexLoginModeDevice):
		return sdkAuth.CodexLoginModeDevice, nil
	case strings.EqualFold(mode, sdkAuth.CodexLoginModeBrowser):
		return sdkAuth.CodexLoginModeBrowser, nil
	default:
		return "", fmt.Errorf("unsupported Codex login mode %q", mode)
	}
}

func (h *Handler) requestCodexDeviceToken(c *gin.Context) {
	ctx := PopulateAuthContext(context.Background(), c)
	state, errState := misc.GenerateRandomState()
	if errState != nil {
		log.Errorf("Failed to generate state parameter: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}

	h.mu.RLock()
	cfg := h.cfg
	h.mu.RUnlock()
	service := newCodexDeviceLoginService()
	if service == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "codex device login service unavailable"})
		return
	}
	authorization, errStart := service.StartDeviceFlow(ctx, cfg)
	if errStart != nil {
		log.Errorf("Failed to start Codex device authentication: %v", errStart)
		c.JSON(http.StatusBadGateway, gin.H{"error": oauthSessionErrorWithCause("failed to start Codex device authentication", errStart)})
		return
	}
	if authorization == nil || strings.TrimSpace(authorization.UserCode) == "" || strings.TrimSpace(authorization.VerificationURL) == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Codex device authentication returned incomplete instructions"})
		return
	}

	expiresAt := authorization.ExpiresAt
	if expiresAt.IsZero() || !expiresAt.After(time.Now()) {
		expiresAt = time.Now().Add(15 * time.Minute)
		authorization.ExpiresAt = expiresAt
	}
	sessionTTL := time.Until(expiresAt)
	RegisterOAuthSessionWithTTL(state, "codex", sessionTTL)

	metadata := map[string]string{sdkAuth.CodexLoginModeMetadataKey: sdkAuth.CodexLoginModeDevice}
	if userAgent := codexLoginRequestUserAgent(c); userAgent != "" {
		metadata["user_agent"] = userAgent
	}
	loginOptions := &sdkAuth.LoginOptions{
		NoBrowser: true,
		Metadata:  metadata,
	}

	go func() {
		deviceCtx, cancel := context.WithDeadline(ctx, expiresAt)
		defer cancel()
		record, errComplete := service.CompleteDeviceFlow(deviceCtx, cfg, authorization, loginOptions)
		if errComplete != nil {
			log.Errorf("Codex device authentication failed: %v", errComplete)
			SetOAuthSessionError(state, oauthSessionErrorWithCause("Codex device authentication failed", errComplete))
			return
		}
		if record == nil {
			SetOAuthSessionError(state, "Codex device authentication returned no credential")
			return
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save Codex device authentication tokens: %v", errSave)
			SetOAuthSessionError(state, "Failed to save authentication tokens")
			return
		}
		fmt.Printf("Codex device authentication successful! Token saved to %s\n", savedPath)
		CompleteOAuthSession(state)
	}()

	expiresIn := int(time.Until(expiresAt) / time.Second)
	if expiresIn < 1 {
		expiresIn = 1
	}
	intervalSeconds := int(authorization.PollInterval / time.Second)
	if intervalSeconds < 1 {
		intervalSeconds = 1
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"status":           "ok",
		"mode":             sdkAuth.CodexLoginModeDevice,
		"url":              authorization.VerificationURL,
		"verification_url": authorization.VerificationURL,
		"user_code":        authorization.UserCode,
		"state":            state,
		"expires_in":       expiresIn,
		"interval":         intervalSeconds,
	})
}

func (h *Handler) requestCodexBrowserToken(c *gin.Context) {
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
	openaiAuth := codex.NewCodexAuthWithProxyURL(h.cfg, util.OAuthProxyURL(h.cfg))
	clientFeatures := sdkAuth.NewCodexClientFeatures(nil)

	// Generate authorization URL
	authURL, err := openaiAuth.GenerateAuthURLWithOptions(state, pkceCodes, clientFeatures.Originator, "")
	if err != nil {
		log.Errorf("Failed to generate authorization URL: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	RegisterOAuthSession(state, "codex")

	isWebUI := isWebUIRequest(c)
	requestUserAgent := codexLoginRequestUserAgent(c)
	if requestUserAgent != "" {
		clientFeatures.UserAgent = requestUserAgent
		clientFeatures.UserAgentExplicit = true
	}
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
		waitFileName := fmt.Sprintf(".oauth-codex-%s.oauth", state)
		resultMap, errWait := h.waitForOAuthCallbackFile("codex", state, waitFileName, 5*time.Minute)
		if errWait != nil {
			if errors.Is(errWait, errOAuthSessionNotPending) {
				return
			}
			authErr := codex.NewAuthenticationError(codex.ErrCallbackTimeout, errWait)
			log.Error(codex.GetUserFriendlyMessage(authErr))
			SetOAuthSessionError(state, "Timeout waiting for OAuth callback")
			return
		}
		if errStr := resultMap["error"]; errStr != "" {
			oauthErr := codex.NewOAuthError(errStr, "", http.StatusBadRequest)
			log.Error(codex.GetUserFriendlyMessage(oauthErr))
			SetOAuthSessionError(state, "Bad Request")
			return
		}
		if resultMap["state"] != state {
			authErr := codex.NewAuthenticationError(codex.ErrInvalidState, fmt.Errorf("expected %s, got %s", state, resultMap["state"]))
			SetOAuthSessionError(state, "State code error")
			log.Error(codex.GetUserFriendlyMessage(authErr))
			return
		}
		code := resultMap["code"]

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
		clientFeatures.AddToMetadata(record.Metadata)
		record.Attributes = map[string]string{}
		clientFeatures.AddToAttributes(record.Attributes)
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
	}()

	c.JSON(200, gin.H{"status": "ok", "mode": sdkAuth.CodexLoginModeBrowser, "url": authURL, "state": state})
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

	authSvc := xaiauth.NewXAIAuthWithProxyURL(h.cfg, util.OAuthProxyURL(h.cfg))
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

		waitFileName := fmt.Sprintf(".oauth-xai-%s.oauth", state)
		payload, errWait := h.waitForOAuthCallbackFile("xai", state, waitFileName, 5*time.Minute)
		if errWait != nil {
			if errors.Is(errWait, errOAuthSessionNotPending) {
				return
			}
			log.Error("xai oauth flow timed out")
			SetOAuthSessionError(state, "OAuth flow timed out")
			return
		}
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
		authCode := strings.TrimSpace(payload["code"])
		if authCode == "" {
			log.Error("xAI authentication failed: code not found")
			SetOAuthSessionError(state, "Authentication failed: code not found")
			return
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
		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		fmt.Println("You can now use xAI services through this CLI")
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
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

	_, status, completed, ok := GetOAuthSessionDetails(state)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"status": "error", "error": "unknown or expired state"})
		return
	}
	if completed {
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
