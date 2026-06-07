package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// DownloadAuthFile downloads a single auth file by name.
func (h *Handler) DownloadAuthFile(c *gin.Context) {
	data, name, status, message := h.readAuthFileByName(c.Query("name"))
	if status != http.StatusOK {
		c.JSON(status, gin.H{"error": message})
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", name))
	c.Data(http.StatusOK, "application/json", data)
}

// PreviewAuthFile returns an auth file JSON view with internal runtime metadata omitted.
func (h *Handler) PreviewAuthFile(c *gin.Context) {
	data, _, status, message := h.readAuthFileByName(c.Query("name"))
	if status != http.StatusOK {
		c.JSON(status, gin.H{"error": message})
		return
	}
	preview, err := authFilePreviewJSON(data)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid auth file JSON: %v", err)})
		return
	}
	c.Data(http.StatusOK, "application/json", preview)
}

// UploadAuthFile stores a multipart upload or raw JSON body with ?name=.
func (h *Handler) UploadAuthFile(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	ctx := c.Request.Context()

	fileHeaders, errMultipart := h.multipartAuthFileHeaders(c)
	if errMultipart != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid multipart form: %v", errMultipart)})
		return
	}
	if len(fileHeaders) == 1 {
		if _, errUpload := h.storeUploadedAuthFile(ctx, fileHeaders[0]); errUpload != nil {
			if errors.Is(errUpload, errAuthFileMustBeJSON) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "file must be .json"})
				return
			}
			if errors.Is(errUpload, util.ErrResponseBodyTooLarge) {
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": fmt.Sprintf("auth file exceeds maximum allowed size of %d bytes", maxAuthFileUploadBytes)})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": errUpload.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if len(fileHeaders) > 1 {
		uploaded := make([]string, 0, len(fileHeaders))
		failed := make([]gin.H, 0)
		for _, file := range fileHeaders {
			name, errUpload := h.storeUploadedAuthFile(ctx, file)
			if errUpload != nil {
				failureName := ""
				if file != nil {
					failureName = filepath.Base(file.Filename)
				}
				msg := errUpload.Error()
				if errors.Is(errUpload, errAuthFileMustBeJSON) {
					msg = "file must be .json"
				} else if errors.Is(errUpload, util.ErrResponseBodyTooLarge) {
					msg = fmt.Sprintf("auth file exceeds maximum allowed size of %d bytes", maxAuthFileUploadBytes)
				}
				failed = append(failed, gin.H{"name": failureName, "error": msg})
				continue
			}
			uploaded = append(uploaded, name)
		}
		if len(failed) > 0 {
			c.JSON(http.StatusMultiStatus, gin.H{
				"status":   "partial",
				"uploaded": len(uploaded),
				"files":    uploaded,
				"failed":   failed,
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "uploaded": len(uploaded), "files": uploaded})
		return
	}
	if c.ContentType() == "multipart/form-data" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no files uploaded"})
		return
	}
	name := strings.TrimSpace(c.Query("name"))
	if isUnsafeAuthFileName(name) {
		c.JSON(400, gin.H{"error": "invalid name"})
		return
	}
	if !util.HasJSONFileName(name) {
		c.JSON(400, gin.H{"error": "name must end with .json"})
		return
	}
	data, err := util.ReadResponseBodyLimited(c.Request.Body, maxAuthFileUploadBytes)
	if err != nil {
		if errors.Is(err, util.ErrResponseBodyTooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": fmt.Sprintf("auth file exceeds maximum allowed size of %d bytes", maxAuthFileUploadBytes)})
			return
		}
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	if err = h.writeAuthFile(ctx, filepath.Base(name), data); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

func (h *Handler) multipartAuthFileHeaders(c *gin.Context) ([]*multipart.FileHeader, error) {
	if h == nil || c == nil || c.ContentType() != "multipart/form-data" {
		return nil, nil
	}
	form, err := c.MultipartForm()
	if err != nil {
		return nil, err
	}
	if form == nil || len(form.File) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(form.File))
	for key := range form.File {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	headers := make([]*multipart.FileHeader, 0)
	for _, key := range keys {
		headers = append(headers, form.File[key]...)
	}
	return headers, nil
}

func (h *Handler) storeUploadedAuthFile(ctx context.Context, file *multipart.FileHeader) (string, error) {
	if file == nil {
		return "", fmt.Errorf("no file uploaded")
	}
	name := filepath.Base(strings.TrimSpace(file.Filename))
	if !util.HasJSONFileName(name) {
		return "", errAuthFileMustBeJSON
	}
	src, err := file.Open()
	if err != nil {
		return "", fmt.Errorf("failed to open uploaded file: %w", err)
	}
	defer src.Close()

	data, err := util.ReadResponseBodyLimited(src, maxAuthFileUploadBytes)
	if err != nil {
		if errors.Is(err, util.ErrResponseBodyTooLarge) {
			return "", fmt.Errorf("uploaded auth file exceeds maximum allowed size of %d bytes: %w", maxAuthFileUploadBytes, err)
		}
		return "", fmt.Errorf("failed to read uploaded file: %w", err)
	}
	if err := h.writeAuthFile(ctx, name, data); err != nil {
		return "", err
	}
	return name, nil
}

func (h *Handler) writeAuthFile(ctx context.Context, name string, data []byte) error {
	dst := filepath.Join(h.cfg.AuthDir, filepath.Base(name))
	if !filepath.IsAbs(dst) {
		if abs, errAbs := filepath.Abs(dst); errAbs == nil {
			dst = abs
		}
	}
	auth, err := h.buildAuthFromFileData(dst, data)
	if err != nil {
		return err
	}
	dataToWrite, _, errNormalize := normalizeImportedAuthJSON(data)
	if errNormalize != nil {
		return fmt.Errorf("invalid auth file: %w", errNormalize)
	}
	if errWrite := writeManagedAuthPathFile(dst, h.cfg.AuthDir, dataToWrite, 0o600); errWrite != nil {
		return fmt.Errorf("failed to write file: %w", errWrite)
	}
	if err := h.upsertAuthRecord(ctx, auth); err != nil {
		return err
	}
	return nil
}

func (h *Handler) authIDForPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		if abs, errAbs := filepath.Abs(path); errAbs == nil {
			path = abs
		}
	}
	authDir := ""
	if h != nil && h.cfg != nil {
		authDir = strings.TrimSpace(h.cfg.AuthDir)
		if resolvedAuthDir, errResolve := util.ResolveAuthDir(authDir); errResolve == nil && resolvedAuthDir != "" {
			authDir = resolvedAuthDir
		}
		if authDir != "" {
			authDir = filepath.Clean(authDir)
			if !filepath.IsAbs(authDir) {
				if abs, errAbs := filepath.Abs(authDir); errAbs == nil {
					authDir = abs
				}
			}
		}
	}
	return coreauth.AuthFileIDForPath(path, authDir)
}

func (h *Handler) registerAuthFromFile(ctx context.Context, path string, data []byte) error {
	if h.authManager == nil {
		return nil
	}
	auth, err := h.buildAuthFromFileData(path, data)
	if err != nil {
		return err
	}
	return h.upsertAuthRecord(ctx, auth)
}

func (h *Handler) buildAuthFromFileData(path string, data []byte) (*coreauth.Auth, error) {
	if path == "" {
		return nil, fmt.Errorf("auth path is empty")
	}
	if data == nil {
		var err error
		authDir := ""
		if h != nil && h.cfg != nil {
			authDir = h.cfg.AuthDir
		}
		data, err = readManagedAuthPathFile(path, authDir)
		if err != nil {
			return nil, fmt.Errorf("failed to read auth file: %w", err)
		}
	}
	normalizedData, _, errNormalize := normalizeImportedAuthJSON(data)
	if errNormalize != nil {
		return nil, fmt.Errorf("invalid auth file: %w", errNormalize)
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(normalizedData, &metadata); err != nil {
		return nil, fmt.Errorf("invalid auth file: %w", err)
	}
	lastRefresh, hasLastRefresh := extractLastRefreshTimestamp(metadata)

	authID := h.authIDForPath(path)
	if authID == "" {
		authID = path
	}
	now := time.Now()
	auth := coreauth.NewAuthFromAuthFileMetadata(metadata, coreauth.AuthFileProjectionOptions{
		ID:                     authID,
		Path:                   path,
		FileName:               filepath.Base(path),
		IncludeSourceAttribute: true,
		Now:                    now,
	})
	if auth.Label == "" {
		auth.Label = auth.Provider
	}
	if hasLastRefresh {
		auth.LastRefreshedAt = lastRefresh
	}
	if h != nil && h.authManager != nil {
		if existing, ok := h.authManager.GetByID(authID); ok {
			auth.CreatedAt = existing.CreatedAt
			if !hasLastRefresh {
				auth.LastRefreshedAt = existing.LastRefreshedAt
			}
			auth.NextRefreshAfter = existing.NextRefreshAfter
			auth.Runtime = existing.Runtime
		}
	}
	return auth, nil
}

func normalizeImportedAuthJSON(data []byte) ([]byte, bool, error) {
	metadata := make(map[string]any)
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, false, err
	}
	normalized, changed := coreauth.NormalizeImportedAuthMetadata(metadata)
	if !changed {
		return data, false, nil
	}
	normalizedData, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return nil, false, err
	}
	normalizedData = append(normalizedData, '\n')
	return normalizedData, true, nil
}

func (h *Handler) upsertAuthRecord(ctx context.Context, auth *coreauth.Auth) error {
	if h == nil || h.authManager == nil || auth == nil {
		return nil
	}
	if existing, ok := h.authManager.GetByID(auth.ID); ok {
		auth.CreatedAt = existing.CreatedAt
		_, err := h.authManager.Update(ctx, auth)
		return err
	}
	_, err := h.authManager.Register(ctx, auth)
	return err
}
