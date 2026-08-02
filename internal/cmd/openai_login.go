package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	log "github.com/sirupsen/logrus"
)

// LoginOptions contains options for the login processes.
// It provides configuration for authentication flows including browser behavior
// and interactive prompting capabilities.
type LoginOptions struct {
	// NoBrowser indicates whether to skip opening the browser automatically.
	NoBrowser bool

	// CallbackPort overrides the local OAuth callback port when set (>0).
	CallbackPort int

	// Prompt allows the caller to provide interactive input when needed.
	Prompt func(prompt string) (string, error)
}

// DoCodexLogin triggers the default Codex login flow through the shared
// authentication manager. Codex device login is the default ceremony.
//
// Parameters:
//   - cfg: The application configuration
//   - options: Login options including browser behavior and prompts
func DoCodexLogin(cfg *config.Config, options *LoginOptions) {
	doCodexLogin(cfg, options, "")
}

// DoCodexBrowserLogin explicitly selects the legacy localhost callback flow.
func DoCodexBrowserLogin(cfg *config.Config, options *LoginOptions) {
	doCodexLogin(cfg, options, sdkAuth.CodexLoginModeBrowser)
}

func doCodexLogin(cfg *config.Config, options *LoginOptions, mode string) {
	if options == nil {
		options = &LoginOptions{}
	}

	promptFn := options.Prompt
	if promptFn == nil {
		promptFn = defaultProjectPrompt()
	}

	manager := newAuthManager()

	var metadata map[string]string
	if mode != "" {
		metadata = map[string]string{sdkAuth.CodexLoginModeMetadataKey: mode}
	}
	authOpts := &sdkAuth.LoginOptions{
		NoBrowser:    options.NoBrowser,
		CallbackPort: options.CallbackPort,
		Metadata:     metadata,
		Prompt:       promptFn,
	}

	_, savedPath, err := manager.Login(context.Background(), "codex", cfg, authOpts)
	if err != nil {
		if authErr, ok := errors.AsType[*codex.AuthenticationError](err); ok {
			log.Error(codex.GetUserFriendlyMessage(authErr))
			if authErr.Type == codex.ErrPortInUse.Type {
				os.Exit(codex.ErrPortInUse.Code)
			}
			return
		}
		fmt.Printf("Codex authentication failed: %v\n", err)
		return
	}

	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	fmt.Println("Codex authentication successful!")
}
