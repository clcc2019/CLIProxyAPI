package management

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

func isUnsafeAuthFileName(name string) bool {
	if strings.TrimSpace(name) == "" {
		return true
	}
	if strings.ContainsAny(name, "/\\") {
		return true
	}
	if filepath.VolumeName(name) != "" {
		return true
	}
	return false
}

func (h *Handler) readAuthDirFile(name string) ([]byte, error) {
	if h == nil || h.cfg == nil {
		return nil, fmt.Errorf("auth directory is unavailable")
	}
	name = strings.TrimSpace(name)
	if isUnsafeAuthFileName(name) {
		return nil, fmt.Errorf("invalid auth file name")
	}
	authDir := strings.TrimSpace(h.cfg.AuthDir)
	if authDir == "" {
		return nil, fmt.Errorf("auth directory is empty")
	}
	root, err := os.OpenRoot(authDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return readAuthRootFile(root, name)
}

func readAuthRootFile(root *os.Root, name string) ([]byte, error) {
	if root == nil {
		return nil, fmt.Errorf("auth directory is unavailable")
	}
	name = strings.TrimSpace(name)
	if isUnsafeAuthFileName(name) {
		return nil, fmt.Errorf("invalid auth file name")
	}
	return root.ReadFile(name)
}

func scopedManagedAuthPath(path, authDir string) (string, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", fmt.Errorf("auth path is empty")
	}
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return "", "", fmt.Errorf("resolve auth path: %w", err)
		}
		return filepath.Dir(absPath), filepath.Base(absPath), nil
	}
	absDir, err := filepath.Abs(authDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve auth directory: %w", err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve auth path: %w", err)
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return "", "", fmt.Errorf("relate auth path: %w", err)
	}
	if rel == "." || rel == "" || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("auth path is outside auth directory")
	}
	return absDir, rel, nil
}

func readManagedAuthPathFile(path, authDir string) ([]byte, error) {
	rootDir, relPath, err := scopedManagedAuthPath(path, authDir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.ReadFile(relPath)
}

func writeManagedAuthPathFile(path, authDir string, data []byte, perm os.FileMode) error {
	rootDir, relPath, err := scopedManagedAuthPath(path, authDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.WriteFile(relPath, data, perm)
}

func statManagedAuthPath(path, authDir string) (os.FileInfo, error) {
	rootDir, relPath, err := scopedManagedAuthPath(path, authDir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.Stat(relPath)
}

func authFilePathWithinDir(path string, authDir string) bool {
	path = strings.TrimSpace(path)
	authDir = strings.TrimSpace(authDir)
	if path == "" || authDir == "" {
		return false
	}
	cleanPath := filepath.Clean(path)
	if !filepath.IsAbs(cleanPath) {
		if abs, errAbs := filepath.Abs(cleanPath); errAbs == nil {
			cleanPath = abs
		}
	}
	cleanDir := filepath.Clean(authDir)
	if !filepath.IsAbs(cleanDir) {
		if abs, errAbs := filepath.Abs(cleanDir); errAbs == nil {
			cleanDir = abs
		}
	}
	rel, err := filepath.Rel(cleanDir, cleanPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return false
	}
	return !isUnsafeAuthFileName(filepath.Base(cleanPath))
}

func normalizeOptionalAuthFileName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if !util.HasJSONFileName(name) {
		name += ".json"
	}
	if isUnsafeAuthFileName(name) {
		return "", fmt.Errorf("invalid auth file name")
	}
	return name, nil
}

func (h *Handler) readAuthFileByName(name string) ([]byte, string, int, string) {
	name = strings.TrimSpace(name)
	if isUnsafeAuthFileName(name) {
		return nil, "", http.StatusBadRequest, "invalid name"
	}
	if !util.HasJSONFileName(name) {
		return nil, "", http.StatusBadRequest, "name must end with .json"
	}
	data, err := h.readAuthDirFile(name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", http.StatusNotFound, "file not found"
		}
		return nil, "", http.StatusInternalServerError, fmt.Sprintf("failed to read file: %v", err)
	}
	return data, name, http.StatusOK, ""
}
