package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type patchAuthFileFieldsRequest struct {
	Name                         string            `json:"name"`
	Prefix                       *string           `json:"prefix"`
	ProxyURL                     *string           `json:"proxy_url"`
	ProxyURLLegacy               *string           `json:"proxy-url"`
	ProxyURLCamel                *string           `json:"proxyUrl"`
	Headers                      map[string]string `json:"headers"`
	Priority                     json.RawMessage   `json:"priority"`
	Note                         *string           `json:"note"`
	UserAgent                    *string           `json:"user_agent"`
	UserAgentCamel               *string           `json:"userAgent"`
	ExcludedModels               *[]string         `json:"excluded_models"`
	ExcludedModelsLegacy         *[]string         `json:"excluded-models"`
	ExcludedModelsCamel          *[]string         `json:"excludedModels"`
	DisableCooling               json.RawMessage   `json:"disable_cooling"`
	DisableCoolingLegacy         json.RawMessage   `json:"disable-cooling"`
	DisableCoolingCamel          json.RawMessage   `json:"disableCooling"`
	Websockets                   *bool             `json:"websockets"`
	ServiceTierPassthrough       *bool             `json:"service_tier_passthrough"`
	ServiceTierPassthroughLegacy *bool             `json:"service-tier-passthrough"`
	ServiceTierPassthroughCamel  *bool             `json:"serviceTierPassthrough"`
	Fast                         *bool             `json:"fast"`
}

func (req patchAuthFileFieldsRequest) resolvedProxyURL() *string {
	if req.ProxyURL != nil {
		return req.ProxyURL
	}
	if req.ProxyURLLegacy != nil {
		return req.ProxyURLLegacy
	}
	return req.ProxyURLCamel
}

func (req patchAuthFileFieldsRequest) resolvedUserAgent() *string {
	if req.UserAgent != nil {
		return req.UserAgent
	}
	return req.UserAgentCamel
}

func (req patchAuthFileFieldsRequest) resolvedExcludedModels() *[]string {
	if req.ExcludedModels != nil {
		return req.ExcludedModels
	}
	if req.ExcludedModelsLegacy != nil {
		return req.ExcludedModelsLegacy
	}
	return req.ExcludedModelsCamel
}

func (req patchAuthFileFieldsRequest) resolvedServiceTierPassthrough() *bool {
	if req.ServiceTierPassthrough != nil {
		return req.ServiceTierPassthrough
	}
	if req.ServiceTierPassthroughLegacy != nil {
		return req.ServiceTierPassthroughLegacy
	}
	if req.ServiceTierPassthroughCamel != nil {
		return req.ServiceTierPassthroughCamel
	}
	return req.Fast
}

func resolvePatchAuthFilePath(targetAuth *coreauth.Auth, authDir, fallbackName string) string {
	candidates := make([]string, 0, 2)
	if path := strings.TrimSpace(authAttribute(targetAuth, "path")); path != "" {
		if strings.TrimSpace(authDir) == "" || authFilePathWithinDir(path, authDir) {
			candidates = append(candidates, path)
		}
	}
	if strings.TrimSpace(authDir) != "" && !isUnsafeAuthFileName(fallbackName) {
		candidates = append(candidates, filepath.Join(authDir, filepath.Base(fallbackName)))
	}

	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if !filepath.IsAbs(candidate) {
			if abs, err := filepath.Abs(candidate); err == nil {
				candidate = abs
			}
		}
		if strings.TrimSpace(authDir) != "" && !authFilePathWithinDir(candidate, authDir) {
			continue
		}
		if _, err := statManagedAuthPath(candidate, authDir); err == nil {
			return candidate
		}
	}

	return ""
}

func applyPatchAuthFileDocument(
	doc map[string]any,
	req patchAuthFileFieldsRequest,
	priorityPresent bool,
	prioritySet bool,
	priorityValue int,
	disableCoolingPresent bool,
	disableCoolingSet bool,
	disableCoolingValue bool,
) {
	if doc == nil {
		return
	}

	if req.Prefix != nil {
		prefix := strings.TrimSpace(*req.Prefix)
		if prefix == "" {
			delete(doc, "prefix")
		} else {
			doc["prefix"] = prefix
		}
	}
	if reqProxyURL := req.resolvedProxyURL(); reqProxyURL != nil {
		proxyURL := strings.TrimSpace(*reqProxyURL)
		delete(doc, "proxy-url")
		delete(doc, "proxyUrl")
		if proxyURL == "" {
			delete(doc, "proxy_url")
		} else {
			doc["proxy_url"] = proxyURL
		}
	}
	if len(req.Headers) > 0 {
		nextHeaders := coreauth.ExtractCustomHeadersFromMetadata(doc)
		if nextHeaders == nil {
			nextHeaders = make(map[string]string)
		}
		for key, value := range req.Headers {
			name := strings.TrimSpace(key)
			if name == "" {
				continue
			}
			val := strings.TrimSpace(value)
			if val == "" {
				delete(nextHeaders, name)
				continue
			}
			nextHeaders[name] = val
		}
		if len(nextHeaders) == 0 {
			delete(doc, "headers")
		} else {
			doc["headers"] = nextHeaders
		}
	}
	if priorityPresent {
		if !prioritySet {
			delete(doc, "priority")
		} else {
			doc["priority"] = priorityValue
		}
	}
	if req.Note != nil {
		note := strings.TrimSpace(*req.Note)
		if note == "" {
			delete(doc, "note")
		} else {
			doc["note"] = note
		}
	}
	if reqUserAgent := req.resolvedUserAgent(); reqUserAgent != nil {
		userAgent := strings.TrimSpace(*reqUserAgent)
		delete(doc, "user-agent")
		delete(doc, "userAgent")
		if userAgent == "" {
			delete(doc, "user_agent")
		} else {
			doc["user_agent"] = userAgent
		}
	}
	if reqExcludedModels := req.resolvedExcludedModels(); reqExcludedModels != nil {
		models := normalizeExcludedModelsInput(*reqExcludedModels)
		delete(doc, "excluded-models")
		delete(doc, "excludedModels")
		if len(models) == 0 {
			delete(doc, "excluded_models")
		} else {
			doc["excluded_models"] = models
		}
	}
	if disableCoolingPresent {
		delete(doc, "disable-cooling")
		delete(doc, "disableCooling")
		if !disableCoolingSet {
			delete(doc, "disable_cooling")
		} else {
			doc["disable_cooling"] = disableCoolingValue
		}
	}
	if req.Websockets != nil {
		delete(doc, "websocket")
		doc["websockets"] = *req.Websockets
	}
	if reqServiceTierPassthrough := req.resolvedServiceTierPassthrough(); reqServiceTierPassthrough != nil {
		delete(doc, "service-tier-passthrough")
		delete(doc, "serviceTierPassthrough")
		delete(doc, "fast")
		doc[coreauth.AuthFileServiceTierPassthroughKey] = *reqServiceTierPassthrough
	}
}

func (h *Handler) persistPatchedAuthFile(
	targetAuth *coreauth.Auth,
	req patchAuthFileFieldsRequest,
	priorityPresent bool,
	prioritySet bool,
	priorityValue int,
	disableCoolingPresent bool,
	disableCoolingSet bool,
	disableCoolingValue bool,
) error {
	if h == nil || targetAuth == nil {
		return nil
	}

	authDir := ""
	if h.cfg != nil {
		authDir = h.cfg.AuthDir
	}
	path := resolvePatchAuthFilePath(targetAuth, authDir, req.Name)
	if path == "" {
		return nil
	}

	data, err := readManagedAuthPathFile(path, authDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read auth file: %w", err)
	}

	doc := make(map[string]any)
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("invalid auth file: %w", err)
	}

	applyPatchAuthFileDocument(
		doc,
		req,
		priorityPresent,
		prioritySet,
		priorityValue,
		disableCoolingPresent,
		disableCoolingSet,
		disableCoolingValue,
	)

	updated, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode auth file: %w", err)
	}
	updated = append(updated, '\n')

	if err := writeManagedAuthPathFile(path, authDir, updated, 0o600); err != nil {
		return fmt.Errorf("failed to write auth file: %w", err)
	}
	return nil
}

// PatchAuthFileStatus toggles the disabled state of an auth file
func (h *Handler) PatchAuthFileStatus(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Name     string `json:"name"`
		Disabled *bool  `json:"disabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if req.Disabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "disabled is required"})
		return
	}

	ctx := c.Request.Context()

	// Find auth by name or ID
	var targetAuth *coreauth.Auth
	if auth, ok := h.authManager.GetByID(name); ok {
		targetAuth = auth
	} else {
		auths := h.authManager.List()
		for _, auth := range auths {
			if auth.FileName == name {
				targetAuth = auth
				break
			}
		}
	}

	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}

	// Update disabled state
	targetAuth.Disabled = *req.Disabled
	if *req.Disabled {
		targetAuth.Status = coreauth.StatusDisabled
		targetAuth.StatusMessage = "disabled via management API"
	} else {
		targetAuth.Status = coreauth.StatusActive
		targetAuth.StatusMessage = ""
	}
	targetAuth.UpdatedAt = time.Now()

	if _, err := h.authManager.Update(ctx, targetAuth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "disabled": *req.Disabled})
}

// PatchAuthFileFields updates editable fields on an auth file without re-uploading the whole JSON file.
func (h *Handler) PatchAuthFileFields(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req patchAuthFileFieldsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	ctx := c.Request.Context()

	// Find auth by name or ID
	var targetAuth *coreauth.Auth
	if auth, ok := h.authManager.GetByID(name); ok {
		targetAuth = auth
	} else {
		auths := h.authManager.List()
		for _, auth := range auths {
			if auth.FileName == name {
				targetAuth = auth
				break
			}
		}
	}

	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}

	priorityPresent, prioritySet, priorityValue, err := parseOptionalJSONIntField(req.Priority)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	reqProxyURL := req.resolvedProxyURL()
	reqUserAgent := req.resolvedUserAgent()
	reqExcludedModels := req.resolvedExcludedModels()
	reqServiceTierPassthrough := req.resolvedServiceTierPassthrough()
	disableCoolingRaw := req.DisableCooling
	if len(disableCoolingRaw) == 0 {
		disableCoolingRaw = req.DisableCoolingLegacy
	}
	if len(disableCoolingRaw) == 0 {
		disableCoolingRaw = req.DisableCoolingCamel
	}
	disableCoolingPresent, disableCoolingSet, disableCoolingValue, err := parseOptionalJSONBoolField(disableCoolingRaw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	changed := false
	if req.Prefix != nil {
		prefix := strings.TrimSpace(*req.Prefix)
		targetAuth.Prefix = prefix
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		if prefix == "" {
			delete(targetAuth.Metadata, "prefix")
		} else {
			targetAuth.Metadata["prefix"] = prefix
		}
		changed = true
	}
	if reqProxyURL != nil {
		proxyURL := strings.TrimSpace(*reqProxyURL)
		targetAuth.ProxyURL = proxyURL
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		delete(targetAuth.Metadata, "proxy-url")
		delete(targetAuth.Metadata, "proxyUrl")
		if proxyURL == "" {
			delete(targetAuth.Metadata, "proxy_url")
		} else {
			targetAuth.Metadata["proxy_url"] = proxyURL
		}
		changed = true
	}
	if len(req.Headers) > 0 {
		existingHeaders := coreauth.ExtractCustomHeadersFromMetadata(targetAuth.Metadata)
		nextHeaders := make(map[string]string, len(existingHeaders))
		for k, v := range existingHeaders {
			nextHeaders[k] = v
		}
		headerChanged := false

		for key, value := range req.Headers {
			name := strings.TrimSpace(key)
			if name == "" {
				continue
			}
			val := strings.TrimSpace(value)
			attrKey := "header:" + name
			if val == "" {
				if _, ok := nextHeaders[name]; ok {
					delete(nextHeaders, name)
					headerChanged = true
				}
				if targetAuth.Attributes != nil {
					if _, ok := targetAuth.Attributes[attrKey]; ok {
						headerChanged = true
					}
				}
				continue
			}
			if prev, ok := nextHeaders[name]; !ok || prev != val {
				headerChanged = true
			}
			nextHeaders[name] = val
			if targetAuth.Attributes != nil {
				if prev, ok := targetAuth.Attributes[attrKey]; !ok || prev != val {
					headerChanged = true
				}
			} else {
				headerChanged = true
			}
		}

		if headerChanged {
			if targetAuth.Metadata == nil {
				targetAuth.Metadata = make(map[string]any)
			}
			if targetAuth.Attributes == nil {
				targetAuth.Attributes = make(map[string]string)
			}

			for key, value := range req.Headers {
				name := strings.TrimSpace(key)
				if name == "" {
					continue
				}
				val := strings.TrimSpace(value)
				attrKey := "header:" + name
				if val == "" {
					delete(nextHeaders, name)
					delete(targetAuth.Attributes, attrKey)
					continue
				}
				nextHeaders[name] = val
				targetAuth.Attributes[attrKey] = val
			}

			if len(nextHeaders) == 0 {
				delete(targetAuth.Metadata, "headers")
			} else {
				metaHeaders := make(map[string]any, len(nextHeaders))
				for k, v := range nextHeaders {
					metaHeaders[k] = v
				}
				targetAuth.Metadata["headers"] = metaHeaders
			}
			changed = true
		}
	}
	if priorityPresent || req.Note != nil || reqUserAgent != nil || disableCoolingPresent || req.Websockets != nil || reqServiceTierPassthrough != nil {
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		if targetAuth.Attributes == nil {
			targetAuth.Attributes = make(map[string]string)
		}

		if priorityPresent {
			if !prioritySet {
				delete(targetAuth.Metadata, "priority")
				delete(targetAuth.Attributes, "priority")
			} else {
				targetAuth.Metadata["priority"] = priorityValue
				targetAuth.Attributes["priority"] = strconv.Itoa(priorityValue)
			}
		}
		if req.Note != nil {
			trimmedNote := strings.TrimSpace(*req.Note)
			if trimmedNote == "" {
				delete(targetAuth.Metadata, "note")
				delete(targetAuth.Attributes, "note")
			} else {
				targetAuth.Metadata["note"] = trimmedNote
				targetAuth.Attributes["note"] = trimmedNote
			}
		}
		if reqUserAgent != nil {
			trimmedUserAgent := strings.TrimSpace(*reqUserAgent)
			if trimmedUserAgent == "" {
				delete(targetAuth.Metadata, "user_agent")
				delete(targetAuth.Metadata, "user-agent")
				delete(targetAuth.Metadata, "userAgent")
				delete(targetAuth.Attributes, "header:User-Agent")
				delete(targetAuth.Attributes, "user_agent")
				delete(targetAuth.Attributes, "user-agent")
				delete(targetAuth.Attributes, "userAgent")
			} else {
				targetAuth.Metadata["user_agent"] = trimmedUserAgent
				delete(targetAuth.Metadata, "user-agent")
				delete(targetAuth.Metadata, "userAgent")
				targetAuth.Attributes["header:User-Agent"] = trimmedUserAgent
				delete(targetAuth.Attributes, "user_agent")
				delete(targetAuth.Attributes, "user-agent")
				delete(targetAuth.Attributes, "userAgent")
			}
		}
		if disableCoolingPresent {
			delete(targetAuth.Metadata, "disable-cooling")
			delete(targetAuth.Metadata, "disableCooling")
			if disableCoolingSet {
				targetAuth.Metadata["disable_cooling"] = disableCoolingValue
			} else {
				delete(targetAuth.Metadata, "disable_cooling")
			}
		}
		if req.Websockets != nil {
			delete(targetAuth.Metadata, "websocket")
			targetAuth.Metadata["websockets"] = *req.Websockets
			targetAuth.Attributes["websockets"] = strconv.FormatBool(*req.Websockets)
		}
		if reqServiceTierPassthrough != nil {
			delete(targetAuth.Metadata, "service-tier-passthrough")
			delete(targetAuth.Metadata, "serviceTierPassthrough")
			delete(targetAuth.Metadata, "fast")
			targetAuth.Metadata[coreauth.AuthFileServiceTierPassthroughKey] = *reqServiceTierPassthrough
			targetAuth.Attributes[coreauth.AuthFileServiceTierPassthroughKey] = strconv.FormatBool(*reqServiceTierPassthrough)
		}
		changed = true
	}
	if reqExcludedModels != nil {
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		normalizedExcludedModels := normalizeExcludedModelsInput(*reqExcludedModels)
		delete(targetAuth.Metadata, "excluded-models")
		delete(targetAuth.Metadata, "excludedModels")
		if len(normalizedExcludedModels) == 0 {
			delete(targetAuth.Metadata, "excluded_models")
		} else {
			targetAuth.Metadata["excluded_models"] = normalizedExcludedModels
		}
		resetExcludedModelsAttributes(targetAuth)
		synthesizer.ApplyAuthExcludedModelsMeta(targetAuth, h.cfg, extractExcludedModelsFromMetadata(targetAuth.Metadata), "oauth")
		changed = true
	}

	if !changed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}
	if err := h.persistPatchedAuthFile(
		targetAuth,
		req,
		priorityPresent,
		prioritySet,
		priorityValue,
		disableCoolingPresent,
		disableCoolingSet,
		disableCoolingValue,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	targetAuth.UpdatedAt = time.Now()

	updatedAuth, err := h.authManager.Update(ctx, targetAuth)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "file": h.buildAuthFileEntry(updatedAuth)})
}

func parseOptionalJSONIntField(raw json.RawMessage) (present bool, set bool, value int, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false, false, 0, nil
	}
	present = true
	if bytes.Equal(trimmed, []byte("null")) {
		return true, false, 0, nil
	}

	var parsedInt int
	if err = json.Unmarshal(trimmed, &parsedInt); err == nil {
		return true, true, parsedInt, nil
	}

	var parsedText string
	if err = json.Unmarshal(trimmed, &parsedText); err != nil {
		return true, false, 0, fmt.Errorf("priority must be an integer")
	}
	parsedText = strings.TrimSpace(parsedText)
	if parsedText == "" {
		return true, false, 0, nil
	}
	parsedInt, err = strconv.Atoi(parsedText)
	if err != nil {
		return true, false, 0, fmt.Errorf("priority must be an integer")
	}
	return true, true, parsedInt, nil
}

func parseOptionalJSONBoolField(raw json.RawMessage) (present bool, set bool, value bool, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false, false, false, nil
	}
	present = true
	if bytes.Equal(trimmed, []byte("null")) {
		return true, false, false, nil
	}

	if err = json.Unmarshal(trimmed, &value); err == nil {
		return true, true, value, nil
	}

	var parsedText string
	if err = json.Unmarshal(trimmed, &parsedText); err != nil {
		return true, false, false, fmt.Errorf("disable_cooling must be true, false, or empty")
	}
	parsedText = strings.TrimSpace(parsedText)
	if parsedText == "" {
		return true, false, false, nil
	}
	value, err = strconv.ParseBool(parsedText)
	if err != nil {
		return true, false, false, fmt.Errorf("disable_cooling must be true, false, or empty")
	}
	return true, true, value, nil
}

func normalizeExcludedModelsInput(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	normalized := make([]string, 0, len(models))
	for _, model := range models {
		key := strings.ToLower(strings.TrimSpace(model))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, key)
	}
	sort.Strings(normalized)
	return normalized
}

func extractExcludedModelsFromMetadata(metadata map[string]any) []string {
	if metadata == nil {
		return nil
	}

	var raw any
	switch {
	case metadata["excluded_models"] != nil:
		raw = metadata["excluded_models"]
	case metadata["excluded-models"] != nil:
		raw = metadata["excluded-models"]
	default:
		return nil
	}

	switch values := raw.(type) {
	case []string:
		return normalizeExcludedModelsInput(values)
	case []any:
		items := make([]string, 0, len(values))
		for _, value := range values {
			items = append(items, fmt.Sprintf("%v", value))
		}
		return normalizeExcludedModelsInput(items)
	case string:
		return normalizeExcludedModelsInput(strings.Split(values, ","))
	default:
		return nil
	}
}

func resetExcludedModelsAttributes(auth *coreauth.Auth) {
	if auth == nil || auth.Attributes == nil {
		return
	}
	delete(auth.Attributes, "excluded_models")
	delete(auth.Attributes, "excluded_models_hash")
}
