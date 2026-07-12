package management

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

const usageExportSourceIDFilename = ".usage-export-source-id"

func (h *Handler) usageExportSourceID() string {
	if h == nil {
		return ""
	}

	baseDir, configPath := h.usageExportSourcePaths()
	if baseDir == "" {
		return ""
	}

	if root, err := os.OpenRoot(baseDir); err == nil {
		if raw, errRead := root.ReadFile(usageExportSourceIDFilename); errRead == nil {
			if sourceID := strings.TrimSpace(string(raw)); sourceID != "" {
				_ = root.Close()
				return sourceID
			}
		}
		_ = root.Close()
	}

	sourceID := uuid.NewString()
	if err := os.MkdirAll(baseDir, 0o750); err == nil {
		if root, errRoot := os.OpenRoot(baseDir); errRoot == nil {
			defer func() { _ = root.Close() }()
			tmpName := usageExportSourceIDFilename + ".tmp"
			if errWrite := root.WriteFile(tmpName, []byte(sourceID+"\n"), 0o600); errWrite == nil {
				if errRename := root.Rename(tmpName, usageExportSourceIDFilename); errRename == nil {
					return sourceID
				}
				_ = root.Remove(tmpName)
			}
		}
	}

	return fallbackUsageExportSourceID(baseDir, configPath)
}

func (h *Handler) usageExportSourcePaths() (baseDir, configPath string) {
	if h == nil {
		return "", ""
	}
	h.mu.RLock()
	authDir := ""
	if h.cfg != nil {
		authDir = strings.TrimSpace(h.cfg.AuthDir)
	}
	configPath = strings.TrimSpace(h.configFilePath)
	h.mu.RUnlock()

	if resolvedAuthDir, err := util.ResolveAuthDir(authDir); err == nil && resolvedAuthDir != "" {
		return resolvedAuthDir, configPath
	}
	if configPath != "" {
		return filepath.Dir(configPath), configPath
	}
	return "", configPath
}

func fallbackUsageExportSourceID(baseDir, configPath string) string {
	hostname, _ := os.Hostname()
	sum := sha256.Sum256([]byte(strings.TrimSpace(hostname) + "|" + filepath.Clean(baseDir) + "|" + filepath.Clean(strings.TrimSpace(configPath))))
	return "fallback-" + hex.EncodeToString(sum[:16])
}
