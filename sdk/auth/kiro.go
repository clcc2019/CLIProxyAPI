package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
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
	if tokenData, err := kiroauth.LoadKiroCLIToken(); err == nil {
		return a.createAuthRecord(tokenData, "kiro-cli"), nil
	} else if !errors.Is(err, kiroauth.ErrKiroCLITokenNotFound) {
		return nil, err
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

	provider := kiroauth.SanitizeEmailForFilename(strings.ToLower(strings.TrimSpace(tokenData.Provider)))
	if provider == "" {
		provider = "imported"
	}
	idPart := kiroAuthIdentifier(tokenData)
	fileName := fmt.Sprintf("kiro-%s-%s.json", provider, idPart)

	metadata := map[string]any{
		"type":                     "kiro",
		"access_token":             tokenData.AccessToken,
		"refresh_token":            tokenData.RefreshToken,
		"profile_arn":              tokenData.ProfileArn,
		"expires_at":               tokenData.ExpiresAt,
		"auth_method":              tokenData.AuthMethod,
		"provider":                 tokenData.Provider,
		"client_id":                tokenData.ClientID,
		"client_secret":            tokenData.ClientSecret,
		"client_id_hash":           tokenData.ClientIDHash,
		"email":                    tokenData.Email,
		"region":                   tokenData.Region,
		"start_url":                tokenData.StartURL,
		"last_refresh":             now.UTC().Format(time.RFC3339),
		"refresh_interval_seconds": kiroauth.RandomRefreshIntervalSeconds(),
	}

	attributes := map[string]string{
		"profile_arn": tokenData.ProfileArn,
		"source":      source,
		"email":       tokenData.Email,
	}
	if tokenData.AuthMethod != "" {
		attributes["auth_method"] = tokenData.AuthMethod
	}
	if tokenData.Region != "" {
		attributes["region"] = tokenData.Region
	}

	refreshInterval := time.Duration(metadata["refresh_interval_seconds"].(int)) * time.Second
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
		NextRefreshAfter: now.UTC().Add(refreshInterval),
	}
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
