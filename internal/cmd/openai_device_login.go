package cmd

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// DoCodexDeviceLogin remains as a compatibility alias now that the default
// Codex login ceremony is device authentication.
func DoCodexDeviceLogin(cfg *config.Config, options *LoginOptions) {
	DoCodexLogin(cfg, options)
}
