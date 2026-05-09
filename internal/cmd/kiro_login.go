package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func DoKiroImport(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}

	manager := newAuthManager()
	authOpts := &sdkAuth.LoginOptions{
		NoBrowser: options.NoBrowser,
		Metadata: map[string]string{
			"mode": "import",
		},
		Prompt: options.Prompt,
	}

	record, savedPath, err := manager.Login(context.Background(), "kiro", cfg, authOpts)
	if err != nil {
		log.Errorf("Kiro token import failed: %v", err)
		fmt.Println("Make sure you have logged in to Kiro IDE first, then retry with -kiro-import.")
		return
	}

	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Imported as %s\n", record.Label)
	}
	fmt.Println("Kiro token import successful!")
}

func DoKiroLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}

	manager := newAuthManager()
	authOpts := &sdkAuth.LoginOptions{
		NoBrowser: options.NoBrowser,
		Metadata:  map[string]string{},
		Prompt:    options.Prompt,
	}

	record, savedPath, err := manager.Login(context.Background(), "kiro", cfg, authOpts)
	if err != nil {
		log.Errorf("Kiro login failed: %v", err)
		fmt.Println("Run `kiro-cli login` first, or complete the AWS Builder ID login flow when prompted.")
		fmt.Println("Use -kiro-import only when importing an existing Kiro IDE login.")
		return
	}

	if savedPath != "" {
		fmt.Printf("Local Kiro auth file generated: %s\n", savedPath)
		fmt.Println("Upload this JSON file to the server management center to use it there.")
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Logged in as %s\n", record.Label)
	}
	fmt.Println("Kiro login successful. No server was started.")
}

func DoKiroRefresh(cfg *config.Config, options *LoginOptions) {
	_ = options

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	store := sdkAuth.GetTokenStore()
	authDir := kiroRefreshAuthDir(cfg)
	if setter, ok := store.(interface{ SetBaseDir(string) }); ok && authDir != "" {
		setter.SetBaseDir(authDir)
	}

	items, err := store.List(ctx)
	if err != nil {
		log.Errorf("Kiro auth refresh failed: %v", err)
		if authDir == "" {
			fmt.Println("No auth directory is configured. Set auth-dir in config.yaml or run from the directory containing Kiro auth JSON files.")
		}
		return
	}

	kiroExec := executor.NewKiroExecutor(cfg)
	var total, refreshed, skipped, failed int
	for _, auth := range items {
		if !isKiroAuthRecord(auth) {
			continue
		}
		total++
		name := kiroAuthDisplayName(auth)
		if auth.Disabled {
			skipped++
			fmt.Printf("Skipped disabled Kiro auth: %s\n", name)
			continue
		}
		if strings.TrimSpace(metadataStringAny(auth.Metadata, "refresh_token", "refreshToken")) == "" {
			skipped++
			fmt.Printf("Skipped Kiro auth without refresh token: %s\n", name)
			continue
		}

		auth.ApplyRuntimeStateFromMetadata()
		updated, refreshErr := kiroExec.Refresh(ctx, auth)
		if refreshErr != nil {
			failed++
			if errors.Is(refreshErr, executor.ErrKiroRefreshInvalidGrant) {
				fmt.Printf("Refresh failed, re-login required: %s (%v)\n", name, refreshErr)
			} else {
				fmt.Printf("Refresh failed: %s (%v)\n", name, refreshErr)
			}
			continue
		}
		if updated == nil {
			skipped++
			fmt.Printf("Skipped Kiro auth with no refresh result: %s\n", name)
			continue
		}
		updated.SetRuntimeStateMetadata()
		savedPath, saveErr := store.Save(ctx, updated)
		if saveErr != nil {
			failed++
			fmt.Printf("Refresh succeeded but save failed: %s (%v)\n", name, saveErr)
			continue
		}
		refreshed++
		if savedPath != "" {
			fmt.Printf("Refreshed Kiro auth: %s\n", savedPath)
		} else {
			fmt.Printf("Refreshed Kiro auth: %s\n", name)
		}
	}

	if total == 0 {
		if authDir != "" {
			fmt.Printf("No Kiro auth files found in %s\n", authDir)
		} else {
			fmt.Println("No Kiro auth files found.")
		}
		return
	}
	fmt.Printf("Kiro refresh complete: refreshed=%d skipped=%d failed=%d total=%d\n", refreshed, skipped, failed, total)
	if failed > 0 {
		fmt.Println("Files marked re-login required have unusable refresh tokens. Other failures can usually be retried after checking network/proxy access.")
	}
}

func kiroRefreshAuthDir(cfg *config.Config) string {
	if cfg != nil {
		if dir := strings.TrimSpace(cfg.AuthDir); dir != "" {
			return dir
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

func isKiroAuthRecord(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "kiro") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(metadataStringAny(auth.Metadata, "type")), "kiro")
}

func kiroAuthDisplayName(auth *coreauth.Auth) string {
	if auth == nil {
		return "<unknown>"
	}
	if auth.Attributes != nil {
		if path := strings.TrimSpace(auth.Attributes["path"]); path != "" {
			return path
		}
	}
	if name := strings.TrimSpace(auth.FileName); name != "" {
		return name
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		return id
	}
	return "<unknown>"
}

func metadataStringAny(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
