package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type KiroAuthenticator struct{}

func NewKiroAuthenticator() *KiroAuthenticator {
	return &KiroAuthenticator{}
}

func (a *KiroAuthenticator) Provider() string { return "kiro" }

func (a *KiroAuthenticator) RefreshLead() *time.Duration {
	d := 20 * time.Minute
	return &d
}

func (a *KiroAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}
	if opts.Metadata != nil && strings.EqualFold(opts.Metadata["mode"], "import") {
		return a.ImportFromKiroIDE(ctx, cfg)
	}
	if opts.Metadata != nil && strings.EqualFold(opts.Metadata["mode"], "builder-id") {
		return a.loginWithBuilderID(ctx, cfg, opts)
	}
	if tokenData, err := kiroauth.LoadKiroCLIToken(); err == nil {
		return a.createAuthRecord(tokenData, "kiro-cli"), nil
	} else if !errors.Is(err, kiroauth.ErrKiroCLITokenNotFound) {
		return nil, err
	}
	if record, err := a.loginWithSocialOAuth(ctx, cfg, opts); err == nil {
		return record, nil
	} else if opts.Metadata != nil && (strings.EqualFold(opts.Metadata["mode"], "oauth") || strings.EqualFold(opts.Metadata["mode"], "social")) {
		return nil, err
	} else {
		log.Warnf("kiro OAuth login failed, falling back to AWS Builder ID device flow: %v", err)
	}
	return a.loginWithBuilderID(ctx, cfg, opts)
}

func (a *KiroAuthenticator) ImportFromKiroIDE(ctx context.Context, cfg *config.Config) (*coreauth.Auth, error) {
	tokenData, err := kiroauth.LoadKiroIDEToken()
	if err != nil {
		if cliTokenData, cliErr := kiroauth.LoadKiroCLIToken(); cliErr == nil {
			return a.createAuthRecord(cliTokenData, "kiro-cli"), nil
		}
		return nil, err
	}
	a.populateProfileArn(ctx, cfg, tokenData)
	return a.createAuthRecord(tokenData, "kiro-ide-import"), nil
}

func (a *KiroAuthenticator) loginWithSocialOAuth(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if opts == nil {
		opts = &LoginOptions{}
	}
	provider := "google"
	prompt := ""
	forceReauth := false
	if opts.Metadata != nil {
		if p := strings.TrimSpace(opts.Metadata["provider"]); p != "" {
			provider = p
		}
		prompt = strings.TrimSpace(opts.Metadata["prompt"])
		forceReauth = parseBoolMetadata(opts.Metadata["force_reauth"])
	}
	callbackPort := opts.CallbackPort
	if callbackPort <= 0 {
		callbackPort = kiroauth.DefaultOAuthCallbackPort
	}
	srv, port, resultCh, errCh, errServer := startKiroOAuthCallbackServer(callbackPort)
	if errServer != nil {
		return nil, fmt.Errorf("kiro oauth: failed to start callback server: %w", errServer)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	redirectURI := kiroauth.OAuthRedirectURI(port)

	pkce, err := kiroauth.GeneratePKCECodes()
	if err != nil {
		return nil, err
	}
	state, err := kiroauth.GenerateOAuthState()
	if err != nil {
		return nil, err
	}
	client := kiroauth.NewSocialOAuthClient(cfg)
	loginURL, err := client.BuildLoginURLWithOptions(provider, redirectURI, state, pkce.CodeChallenge, kiroauth.SocialLoginURLOptions{
		Prompt:      prompt,
		ForceReauth: forceReauth,
	})
	if err != nil {
		return nil, err
	}

	fmt.Println("Starting Kiro OAuth login...")
	fmt.Printf("\nTo authenticate Kiro, visit:\n%s\n\n", loginURL)
	fmt.Printf("Waiting for Kiro OAuth callback on %s\n", redirectURI)
	if !opts.NoBrowser {
		if errOpen := browser.OpenURL(loginURL); errOpen != nil {
			log.Warnf("Failed to open browser automatically: %v", errOpen)
		} else {
			fmt.Println("Browser opened automatically.")
		}
	}

	cb, err := waitForKiroOAuthCallback(ctx, opts.Prompt, state, resultCh, errCh)
	if err != nil {
		return nil, err
	}
	token, err := client.ExchangeCode(ctx, cb.Code, redirectURI, pkce.CodeVerifier)
	if err != nil {
		return nil, err
	}
	tokenData := kiroauth.TokenDataFromSocialResponse(provider, token)
	if tokenData == nil {
		return nil, fmt.Errorf("kiro oauth: token response is empty")
	}
	if tokenData.Email == "" {
		tokenData.Email = kiroauth.ExtractEmailFromJWT(tokenData.AccessToken)
	}
	if tokenData.ProfileArn == "" {
		a.populateProfileArn(ctx, cfg, tokenData)
	}
	if tokenData.Email != "" {
		fmt.Printf("\nLogged in as: %s\n", tokenData.Email)
	}
	fmt.Println("Kiro OAuth login successful!")
	return a.createAuthRecord(tokenData, "kiro-oauth"), nil
}

func (a *KiroAuthenticator) loginWithBuilderID(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	ssoClient := kiroauth.NewSSOOIDCClient(cfg)

	fmt.Println("Starting Kiro authentication with AWS Builder ID...")
	fmt.Println("Registering Kiro client...")
	regResp, err := ssoClient.RegisterClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("kiro: failed to register client: %w", err)
	}

	fmt.Println("Starting device authorization...")
	authResp, err := ssoClient.StartDeviceAuthorization(ctx, regResp.ClientID, regResp.ClientSecret)
	if err != nil {
		return nil, fmt.Errorf("kiro: failed to start device authorization: %w", err)
	}

	verificationURL := strings.TrimSpace(authResp.VerificationURIComplete)
	if verificationURL == "" {
		verificationURL = strings.TrimSpace(authResp.VerificationURI)
	}
	if verificationURL != "" {
		fmt.Printf("\nTo authenticate Kiro, visit:\n%s\n\n", verificationURL)
	}
	if authResp.UserCode != "" {
		fmt.Printf("User code: %s\n\n", authResp.UserCode)
	}

	if !opts.NoBrowser && verificationURL != "" {
		if errOpen := browser.OpenURL(verificationURL); errOpen != nil {
			log.Warnf("Failed to open browser automatically: %v", errOpen)
		} else {
			fmt.Println("Browser opened automatically.")
		}
	}

	fmt.Println("Waiting for authorization...")
	if authResp.ExpiresIn > 0 {
		fmt.Printf("(This will timeout in %d seconds if not authorized)\n", authResp.ExpiresIn)
	}

	tokenResp, err := waitForKiroBuilderIDToken(ctx, ssoClient, regResp, authResp)
	if err != nil {
		return nil, err
	}

	expiresIn := tokenResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	expiresAt := time.Now().UTC().Add(time.Duration(expiresIn) * time.Second)
	email := ssoClient.FetchUserEmail(ctx, tokenResp.AccessToken)

	tokenData := &kiroauth.TokenData{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    expiresAt.Format(time.RFC3339),
		AuthMethod:   "builder-id",
		Provider:     "AWS",
		ClientID:     regResp.ClientID,
		ClientSecret: regResp.ClientSecret,
		Email:        email,
		StartURL:     kiroauth.BuilderIDStartURL,
		Region:       kiroauth.DefaultRegion,
	}
	a.populateProfileArn(ctx, cfg, tokenData)

	if email != "" {
		fmt.Printf("\nLogged in as: %s\n", email)
	}
	fmt.Println("Kiro authentication successful!")
	return a.createAuthRecord(tokenData, "aws-builder-id"), nil
}

func (a *KiroAuthenticator) populateProfileArn(ctx context.Context, cfg *config.Config, tokenData *kiroauth.TokenData) {
	if tokenData == nil || strings.TrimSpace(tokenData.ProfileArn) != "" || strings.TrimSpace(tokenData.AccessToken) == "" {
		return
	}
	profileArn, err := kiroauth.NewKiroAuth(cfg).ResolveProfileArn(ctx, tokenData)
	if err != nil {
		log.Debugf("kiro: failed to resolve profile arn: %v", err)
		return
	}
	tokenData.ProfileArn = profileArn
}

// CreateAuthRecord converts Kiro token data into a persisted auth record.
func (a *KiroAuthenticator) CreateAuthRecord(tokenData *kiroauth.TokenData, source string) *coreauth.Auth {
	return a.createAuthRecord(tokenData, source)
}

func waitForKiroBuilderIDToken(ctx context.Context, ssoClient *kiroauth.SSOOIDCClient, regResp *kiroauth.RegisterClientResponse, authResp *kiroauth.StartDeviceAuthResponse) (*kiroauth.CreateTokenResponse, error) {
	interval := time.Duration(authResp.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}

	deadline := time.Now().Add(15 * time.Minute)
	if authResp.ExpiresIn > 0 {
		deadline = time.Now().Add(time.Duration(authResp.ExpiresIn) * time.Second)
	}

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("kiro: authorization cancelled: %w", ctx.Err())
		case <-time.After(interval):
			tokenResp, err := ssoClient.CreateToken(ctx, regResp.ClientID, regResp.ClientSecret, authResp.DeviceCode)
			if err == nil {
				return tokenResp, nil
			}
			if errors.Is(err, kiroauth.ErrAuthorizationPending) {
				fmt.Print(".")
				continue
			}
			if errors.Is(err, kiroauth.ErrSlowDown) {
				interval += 5 * time.Second
				continue
			}
			return nil, fmt.Errorf("kiro: token creation failed: %w", err)
		}
	}
	return nil, fmt.Errorf("kiro: authorization timed out")
}

func (a *KiroAuthenticator) createAuthRecord(tokenData *kiroauth.TokenData, source string) *coreauth.Auth {
	now := time.Now()
	expiresAt, err := time.Parse(time.RFC3339, tokenData.ExpiresAt)
	if err != nil {
		expiresAt = now.Add(time.Hour)
		tokenData.ExpiresAt = expiresAt.UTC().Format(time.RFC3339)
	}
	if kiroauth.NormalizeKiroMachineID(tokenData.MachineID) == "" {
		tokenData.MachineID = kiroauth.GenerateKiroMachineID()
	}

	provider := kiroauth.SanitizeEmailForFilename(strings.ToLower(strings.TrimSpace(tokenData.Provider)))
	if provider == "" {
		provider = "imported"
	}
	idPart := kiroAuthIdentifier(tokenData)
	fileName := fmt.Sprintf("kiro-%s-%s.json", provider, idPart)

	metadata := map[string]any{
		"type":                     "kiro",
		"last_refresh":             now.UTC().Format(time.RFC3339),
		"refresh_interval_seconds": kiroauth.RandomRefreshIntervalSeconds(),
		"machine_id":               kiroauth.MachineIDFromTokenData(tokenData),
	}
	setNonEmptyKiroMetadata(metadata,
		"access_token", tokenData.AccessToken,
		"refresh_token", tokenData.RefreshToken,
		"profile_arn", tokenData.ProfileArn,
		"expires_at", tokenData.ExpiresAt,
		"auth_method", tokenData.AuthMethod,
		"provider", tokenData.Provider,
		"client_id", tokenData.ClientID,
		"client_secret", tokenData.ClientSecret,
		"client_id_hash", tokenData.ClientIDHash,
		"email", tokenData.Email,
		"region", tokenData.Region,
		"start_url", tokenData.StartURL,
		"machine_id", tokenData.MachineID,
	)

	attributes := map[string]string{"source": source}
	if profileArn := strings.TrimSpace(tokenData.ProfileArn); profileArn != "" {
		attributes["profile_arn"] = profileArn
	}
	if email := strings.TrimSpace(tokenData.Email); email != "" {
		attributes["email"] = email
	}
	if authMethod := strings.TrimSpace(tokenData.AuthMethod); authMethod != "" {
		attributes["auth_method"] = authMethod
	}
	if region := strings.TrimSpace(tokenData.Region); region != "" {
		attributes["region"] = region
	}
	if machineID := kiroauth.MachineIDFromTokenData(tokenData); machineID != "" {
		attributes["machine_id"] = machineID
	}

	// Resolve refresh interval defensively. While createAuthRecord constructs
	// metadata locally as int, JSON-decoded metadata (from disk, Redis, or
	// callers reusing this helper) would carry float64. A bare .(int) panics
	// in those paths.
	refreshInterval := kiroRefreshIntervalFromMetadata(metadata)
	return &coreauth.Auth{
		ID:               fileName,
		Provider:         a.Provider(),
		FileName:         fileName,
		Label:            "kiro-" + provider,
		Status:           coreauth.StatusActive,
		CreatedAt:        now,
		UpdatedAt:        now,
		Metadata:         metadata,
		Attributes:       attributes,
		LastRefreshedAt:  now.UTC(),
		NextRefreshAfter: now.UTC().Add(refreshInterval),
	}
}

func setNonEmptyKiroMetadata(metadata map[string]any, keyValues ...string) {
	if metadata == nil {
		return
	}
	for i := 0; i+1 < len(keyValues); i += 2 {
		key := strings.TrimSpace(keyValues[i])
		value := strings.TrimSpace(keyValues[i+1])
		if key == "" || value == "" {
			continue
		}
		metadata[key] = value
	}
}

func RemoveEmptyKiroMetadataFields(metadata map[string]any) {
	coreauth.RemoveEmptyKiroMetadataFields(metadata)
}

func kiroAuthIdentifier(tokenData *kiroauth.TokenData) string {
	if tokenData == nil {
		return fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	}
	if tokenData.Email != "" {
		return kiroauth.SanitizeEmailForFilename(tokenData.Email)
	}
	if tokenData.ProfileArn != "" {
		parts := strings.Split(tokenData.ProfileArn, "/")
		if len(parts) > 0 {
			return kiroauth.SanitizeEmailForFilename(parts[len(parts)-1])
		}
	}
	if tokenData.ClientID != "" {
		return kiroauth.SanitizeEmailForFilename(tokenData.ClientID)
	}
	return fmt.Sprintf("%d", time.Now().UnixNano()%100000)
}

type kiroOAuthCallbackResult struct {
	Code             string
	State            string
	Error            string
	ErrorDescription string
}

func startKiroOAuthCallbackServer(port int) (*http.Server, int, <-chan kiroOAuthCallbackResult, <-chan error, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, 0, nil, nil, err
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port
	resultCh := make(chan kiroOAuthCallbackResult, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		errCode := strings.TrimSpace(q.Get("error"))
		errDesc := strings.TrimSpace(q.Get("error_description"))
		if errCode == "" {
			errCode = errDesc
			errDesc = ""
		}
		res := kiroOAuthCallbackResult{
			Code:             strings.TrimSpace(q.Get("code")),
			State:            strings.TrimSpace(q.Get("state")),
			Error:            errCode,
			ErrorDescription: errDesc,
		}
		if res.Code == "" && res.Error == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<h1>Kiro login waiting</h1><p>Complete the browser login flow, then keep this window open.</p>"))
			return
		}
		select {
		case resultCh <- res:
		default:
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if res.Code != "" && res.Error == "" {
			_, _ = w.Write([]byte("<h1>Kiro login successful</h1><p>You can close this window.</p>"))
			return
		}
		_, _ = w.Write([]byte("<h1>Kiro login failed</h1><p>Please check the CLI output.</p>"))
	}
	registeredPaths := map[string]struct{}{}
	for _, path := range []string{kiroauth.OAuthCallbackPath, "/callback", "/"} {
		if path == "" {
			path = "/"
		}
		if _, ok := registeredPaths[path]; ok {
			continue
		}
		registeredPaths[path] = struct{}{}
		mux.HandleFunc(path, handler)
	}

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if errServe := srv.Serve(listener); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) {
			select {
			case errCh <- errServe:
			default:
			}
		}
	}()
	return srv, actualPort, resultCh, errCh, nil
}

func waitForKiroOAuthCallback(ctx context.Context, prompt func(string) (string, error), state string, resultCh <-chan kiroOAuthCallbackResult, errCh <-chan error) (kiroOAuthCallbackResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := time.NewTimer(10 * time.Minute)
	defer timeout.Stop()

	var manualInputCh <-chan string
	var manualInputErrCh <-chan error
	var manualPromptC <-chan time.Time
	var manualPromptTimer *time.Timer
	if prompt != nil {
		manualPromptTimer = time.NewTimer(15 * time.Second)
		defer manualPromptTimer.Stop()
		manualPromptC = manualPromptTimer.C
	}

	for {
		select {
		case <-ctx.Done():
			return kiroOAuthCallbackResult{}, fmt.Errorf("kiro oauth: login cancelled: %w", ctx.Err())
		case err := <-errCh:
			return kiroOAuthCallbackResult{}, fmt.Errorf("kiro oauth: callback server failed: %w", err)
		case res := <-resultCh:
			if err := validateKiroOAuthCallback(res, state); err != nil {
				return kiroOAuthCallbackResult{}, err
			}
			return res, nil
		case <-manualPromptC:
			manualPromptC = nil
			select {
			case res := <-resultCh:
				if err := validateKiroOAuthCallback(res, state); err != nil {
					return kiroOAuthCallbackResult{}, err
				}
				return res, nil
			default:
			}
			manualInputCh, manualInputErrCh = misc.AsyncPrompt(prompt, "Paste the Kiro callback URL (or press Enter to keep waiting): ")
		case input := <-manualInputCh:
			manualInputCh = nil
			manualInputErrCh = nil
			parsed, err := misc.ParseOAuthCallback(input)
			if err != nil {
				return kiroOAuthCallbackResult{}, err
			}
			if parsed == nil {
				if manualPromptTimer != nil {
					manualPromptTimer.Reset(15 * time.Second)
					manualPromptC = manualPromptTimer.C
				}
				continue
			}
			res := kiroOAuthCallbackResult{
				Code:             parsed.Code,
				State:            parsed.State,
				Error:            parsed.Error,
				ErrorDescription: parsed.ErrorDescription,
			}
			if err := validateKiroOAuthCallback(res, state); err != nil {
				return kiroOAuthCallbackResult{}, err
			}
			return res, nil
		case err := <-manualInputErrCh:
			return kiroOAuthCallbackResult{}, err
		case <-timeout.C:
			return kiroOAuthCallbackResult{}, fmt.Errorf("kiro oauth: timeout waiting for callback")
		}
	}
}

func validateKiroOAuthCallback(res kiroOAuthCallbackResult, state string) error {
	if strings.TrimSpace(res.Error) != "" {
		if res.ErrorDescription != "" {
			return fmt.Errorf("kiro oauth: callback error: %s (%s)", res.Error, res.ErrorDescription)
		}
		return fmt.Errorf("kiro oauth: callback error: %s", res.Error)
	}
	if strings.TrimSpace(res.Code) == "" {
		return fmt.Errorf("kiro oauth: callback missing code")
	}
	if strings.TrimSpace(res.State) == "" {
		return fmt.Errorf("kiro oauth: callback missing state")
	}
	if subtleConstantTimeCompare(res.State, state) {
		return nil
	}
	return fmt.Errorf("kiro oauth: callback state mismatch")
}

func subtleConstantTimeCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func parseBoolMetadata(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// kiroRefreshIntervalFromMetadata extracts the refresh interval from metadata in
// a type-tolerant way. JSON-decoded numeric values arrive as float64 while
// freshly-built metadata uses int; both must work without panicking. A zero
// return falls back to the executor's per-call computation, which is safe.
func kiroRefreshIntervalFromMetadata(metadata map[string]any) time.Duration {
	if metadata == nil {
		return 0
	}
	for _, key := range []string{"refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"} {
		switch v := metadata[key].(type) {
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
				return time.Duration(int64(v)) * time.Second
			}
		case json.Number:
			if n, err := v.Int64(); err == nil && n > 0 {
				return time.Duration(n) * time.Second
			}
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n > 0 {
				return time.Duration(n) * time.Second
			}
		}
	}
	return 0
}
