package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func (h *Handler) isManagedAuthFilePath(path string) bool {
	path = strings.TrimSpace(path)
	if h == nil || h.cfg == nil || path == "" {
		return false
	}
	authDir := strings.TrimSpace(h.cfg.AuthDir)
	if authDir == "" {
		return false
	}
	if resolved, errResolve := util.ResolveAuthDir(authDir); errResolve == nil && resolved != "" {
		authDir = resolved
	}
	cleanPath := cleanAbsPathForCompare(path)
	cleanDir := cleanAbsPathForCompare(authDir)
	rel, errRel := filepath.Rel(cleanDir, cleanPath)
	if errRel != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func requestedAuthFileNamesForDelete(c *gin.Context) ([]string, error) {
	if c == nil {
		return nil, nil
	}
	queryNames := append([]string{}, c.QueryArray("name")...)
	queryNames = append(queryNames, c.QueryArray("id")...)
	queryNames = append(queryNames, c.QueryArray("auth_index")...)
	queryNames = append(queryNames, c.QueryArray("authIndex")...)
	queryNames = append(queryNames, c.QueryArray("auth_file_name")...)
	queryNames = append(queryNames, c.QueryArray("authFileName")...)
	queryNames = append(queryNames, c.QueryArray("file_name")...)
	queryNames = append(queryNames, c.QueryArray("fileName")...)
	queryNames = append(queryNames, c.QueryArray("filename")...)
	queryNames = append(queryNames, c.QueryArray("path")...)
	names := uniqueAuthFileNames(queryNames)
	if len(names) > 0 {
		return names, nil
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read body")
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, nil
	}

	var objectBody struct {
		Name           string   `json:"name"`
		ID             string   `json:"id"`
		Names          []string `json:"names"`
		AuthIndex      string   `json:"auth_index"`
		AuthIndexCamel string   `json:"authIndex"`
		AuthFileName   string   `json:"auth_file_name"`
		AuthFileCamel  string   `json:"authFileName"`
		FileName       string   `json:"file_name"`
		FileNameCamel  string   `json:"fileName"`
		Filename       string   `json:"filename"`
		Path           string   `json:"path"`
	}
	if body[0] == '[' {
		var arrayBody []string
		if err := json.Unmarshal(body, &arrayBody); err != nil {
			return nil, fmt.Errorf("invalid request body")
		}
		return uniqueAuthFileNames(arrayBody), nil
	}
	if err := json.Unmarshal(body, &objectBody); err != nil {
		return nil, fmt.Errorf("invalid request body")
	}

	out := make([]string, 0, len(objectBody.Names)+10)
	out = appendNonEmptyAuthNames(out,
		objectBody.Name,
		objectBody.ID,
		objectBody.AuthIndex,
		objectBody.AuthIndexCamel,
		objectBody.AuthFileName,
		objectBody.AuthFileCamel,
		objectBody.FileName,
		objectBody.FileNameCamel,
		objectBody.Filename,
		objectBody.Path,
	)
	out = append(out, objectBody.Names...)
	return uniqueAuthFileNames(out), nil
}

func appendNonEmptyAuthNames(out []string, names ...string) []string {
	for _, name := range names {
		if strings.TrimSpace(name) != "" {
			out = append(out, name)
		}
	}
	return out
}

func uniqueAuthFileNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func authDeleteNameVariants(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	variants := []string{name}
	if !filepath.IsAbs(name) && !strings.ContainsAny(name, "/\\") {
		if strings.HasSuffix(strings.ToLower(name), ".json") {
			variants = append(variants, name[:len(name)-len(filepath.Ext(name))])
		} else {
			variants = append(variants, name+".json")
		}
	}
	return variants
}

type authDeleteMatcher struct {
	name           string
	variants       []string
	variantSet     map[string]struct{}
	cleanedPathSet map[string]struct{}
}

func newAuthDeleteMatcher(name string) authDeleteMatcher {
	name = strings.TrimSpace(name)
	variants := authDeleteNameVariants(name)
	m := authDeleteMatcher{
		name:           name,
		variants:       variants,
		variantSet:     make(map[string]struct{}, len(variants)),
		cleanedPathSet: make(map[string]struct{}, len(variants)),
	}
	for _, variant := range variants {
		m.variantSet[variant] = struct{}{}
		if cleaned := cleanAbsPathForCompare(variant); cleaned != "" {
			m.cleanedPathSet[cleaned] = struct{}{}
		}
	}
	return m
}

func (m authDeleteMatcher) matchesVariant(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	_, ok := m.variantSet[value]
	return ok
}

func (m authDeleteMatcher) matchesBase(path string) bool {
	return m.matchesVariant(filepath.Base(strings.TrimSpace(path)))
}

func (m authDeleteMatcher) matchesPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	cleaned := cleanAbsPathForCompare(path)
	_, ok := m.cleanedPathSet[cleaned]
	return ok
}

func cleanAbsPathForCompare(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		if abs, errAbs := filepath.Abs(cleaned); errAbs == nil {
			cleaned = abs
		}
	}
	return cleaned
}

func (h *Handler) deleteAuthFileByName(ctx context.Context, name string) (string, int, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", http.StatusBadRequest, fmt.Errorf("invalid name")
	}

	targetAuth := h.findAuthForDelete(name)
	resolvedName := name
	if targetAuth != nil {
		if fn := strings.TrimSpace(targetAuth.FileName); fn != "" {
			resolvedName = fn
		} else if id := strings.TrimSpace(targetAuth.ID); id != "" {
			resolvedName = filepath.Base(id)
		} else if path := strings.TrimSpace(authAttribute(targetAuth, "path")); path != "" {
			resolvedName = filepath.Base(path)
		}
	}
	if isUnsafeAuthFileName(resolvedName) {
		return "", http.StatusBadRequest, fmt.Errorf("invalid name")
	}

	targetPath := filepath.Join(h.cfg.AuthDir, filepath.Base(resolvedName))
	if !strings.HasSuffix(strings.ToLower(resolvedName), ".json") && !isUnsafeAuthFileName(resolvedName) {
		jsonPath := filepath.Join(h.cfg.AuthDir, filepath.Base(resolvedName)+".json")
		if _, errStat := os.Stat(jsonPath); errStat == nil {
			targetPath = jsonPath
		}
	}
	targetID := ""
	if targetAuth != nil {
		targetID = strings.TrimSpace(targetAuth.ID)
		if path := strings.TrimSpace(authAttribute(targetAuth, "path")); path != "" {
			targetPath = path
		}
	}
	if !filepath.IsAbs(targetPath) {
		if abs, errAbs := filepath.Abs(targetPath); errAbs == nil {
			targetPath = abs
		}
	}
	deletedName := filepath.Base(targetPath)
	if deletedName == "" || deletedName == "." {
		deletedName = filepath.Base(resolvedName)
	}
	removeID := targetID
	if removeID == "" {
		removeID = targetPath
	}
	suppressedID, restoreAuth := h.disableAuth(ctx, removeID)
	if suppressedID != "" {
		removeID = suppressedID
	}

	if targetAuth == nil {
		if _, errStat := os.Stat(targetPath); errStat != nil {
			h.restoreAuth(ctx, restoreAuth)
			if os.IsNotExist(errStat) {
				return deletedName, http.StatusNotFound, errAuthFileNotFound
			}
			return deletedName, http.StatusInternalServerError, fmt.Errorf("failed to stat file: %w", errStat)
		}
	}
	if errDeleteRecord := h.deleteTokenRecord(ctx, targetPath); errDeleteRecord != nil {
		h.restoreAuth(ctx, restoreAuth)
		return deletedName, http.StatusInternalServerError, errDeleteRecord
	}
	if errRemove := os.Remove(targetPath); errRemove != nil {
		if os.IsNotExist(errRemove) {
			if targetAuth == nil {
				h.restoreAuth(ctx, restoreAuth)
				return deletedName, http.StatusNotFound, errAuthFileNotFound
			}
		} else {
			h.restoreAuth(ctx, restoreAuth)
			return deletedName, http.StatusInternalServerError, fmt.Errorf("failed to remove file: %w", errRemove)
		}
	}
	h.removeAuth(ctx, removeID)
	return deletedName, http.StatusOK, nil
}

func (h *Handler) findAuthForDelete(name string) *coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}
	matcher := newAuthDeleteMatcher(name)
	if matcher.name == "" {
		return nil
	}
	for _, variant := range matcher.variants {
		if auth, ok := h.authManager.GetByID(variant); ok {
			return auth
		}
	}
	auths := h.authManager.List()
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		if strings.TrimSpace(auth.Index) == matcher.name {
			return auth
		}
		if matcher.matchesVariant(auth.FileName) || matcher.matchesVariant(auth.ID) {
			return auth
		}
		path := strings.TrimSpace(authAttribute(auth, "path"))
		if matcher.matchesBase(path) || matcher.matchesPath(path) {
			return auth
		}
		if matcher.matchesBase(authAttribute(auth, "source")) {
			return auth
		}
		// Match by stable auth index (16-char sha256 hex sent from the UI).
		if auth.EnsureIndex() == matcher.name {
			return auth
		}
	}
	return nil
}

func (h *Handler) disableAuth(ctx context.Context, id string) (string, *coreauth.Auth) {
	if h == nil || h.authManager == nil {
		return "", nil
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", nil
	}
	for _, variant := range authDeleteNameVariants(id) {
		if auth, ok := h.authManager.GetByID(variant); ok {
			original := auth.Clone()
			h.markAuthRemoved(ctx, auth)
			return auth.ID, original
		}
	}
	authID := h.authIDForPath(id)
	if authID == "" {
		return "", nil
	}
	for _, variant := range authDeleteNameVariants(authID) {
		if auth, ok := h.authManager.GetByID(variant); ok {
			original := auth.Clone()
			h.markAuthRemoved(ctx, auth)
			return auth.ID, original
		}
	}
	return "", nil
}

func (h *Handler) markAuthRemoved(ctx context.Context, auth *coreauth.Auth) {
	if h == nil || h.authManager == nil || auth == nil {
		return
	}
	auth.Disabled = true
	auth.Status = coreauth.StatusDisabled
	auth.StatusMessage = "removed via management API"
	auth.UpdatedAt = time.Now()
	_, _ = h.authManager.Update(coreauth.WithSkipPersist(ctx), auth)
}

func (h *Handler) restoreAuth(ctx context.Context, auth *coreauth.Auth) {
	if h == nil || h.authManager == nil || auth == nil {
		return
	}
	_, _ = h.authManager.Update(coreauth.WithSkipPersist(ctx), auth)
}

func (h *Handler) removeAuth(ctx context.Context, id string) {
	if h == nil || h.authManager == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	for _, variant := range authDeleteNameVariants(id) {
		if auth, ok := h.authManager.GetByID(variant); ok {
			_, _ = h.authManager.Remove(ctx, auth.ID)
			return
		}
	}
	authID := h.authIDForPath(id)
	if authID == "" {
		return
	}
	for _, variant := range authDeleteNameVariants(authID) {
		if auth, ok := h.authManager.GetByID(variant); ok {
			_, _ = h.authManager.Remove(ctx, auth.ID)
			return
		}
	}
}

func (h *Handler) deleteTokenRecord(ctx context.Context, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("auth path is empty")
	}
	store := h.tokenStoreWithBaseDir()
	if store == nil {
		return fmt.Errorf("token store unavailable")
	}
	return store.Delete(ctx, path)
}
