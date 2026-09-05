package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type clientModelAccess struct {
	allowed  []string
	excluded []string
}

func clientModelAccessFromContext(ctx context.Context) clientModelAccess {
	if ctx == nil {
		return clientModelAccess{}
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil {
		return clientModelAccess{}
	}
	return clientModelAccessFromGin(ginCtx)
}

func clientModelAccessFromGin(c *gin.Context) clientModelAccess {
	if c == nil {
		return clientModelAccess{}
	}
	raw, exists := c.Get("accessMetadata")
	if !exists || raw == nil {
		return clientModelAccess{}
	}
	switch typed := raw.(type) {
	case map[string]string:
		return clientModelAccess{
			allowed:  parseClientModelPatterns(typed["allowed_models"]),
			excluded: parseClientModelPatterns(typed["excluded_models"]),
		}
	case map[string]any:
		return clientModelAccess{
			allowed:  parseClientModelPatterns(fmt.Sprintf("%v", typed["allowed_models"])),
			excluded: parseClientModelPatterns(fmt.Sprintf("%v", typed["excluded_models"])),
		}
	default:
		return clientModelAccess{}
	}
}

func clientAuthFilesFromContext(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil {
		return nil
	}
	raw, exists := ginCtx.Get("accessMetadata")
	if !exists || raw == nil {
		return nil
	}
	var encoded any
	switch typed := raw.(type) {
	case map[string]string:
		encoded = typed[coreexecutor.ClientAuthFilesMetadataKey]
	case map[string]any:
		encoded = typed[coreexecutor.ClientAuthFilesMetadataKey]
	}
	var files []string
	switch typed := encoded.(type) {
	case string:
		_ = json.Unmarshal([]byte(typed), &files)
	case []string:
		files = typed
	case []any:
		for _, item := range typed {
			if value, ok := item.(string); ok {
				files = append(files, value)
			}
		}
	}
	return internalconfig.NormalizeClientAPIKeyAuthFiles(files)
}

func clientAuthFilesFromGin(c *gin.Context) []string {
	if c == nil {
		return nil
	}
	return clientAuthFilesFromContext(context.WithValue(context.Background(), "gin", c))
}

func resolveClientAuthFileIDs(manager *coreauth.Manager, files []string) ([]string, []string) {
	files = internalconfig.NormalizeClientAPIKeyAuthFiles(files)
	if len(files) == 0 || manager == nil {
		return nil, files
	}
	auths := manager.List()
	ids := make([]string, 0, len(files))
	missing := make([]string, 0)
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		matched := ""
		for _, auth := range auths {
			if auth == nil {
				continue
			}
			for _, candidate := range []string{auth.FileName, auth.ID} {
				if normalized := internalconfig.NormalizeClientAPIKeyAuthFiles([]string{candidate}); len(normalized) > 0 && normalized[0] == file {
					matched = strings.TrimSpace(auth.ID)
					break
				}
			}
			if matched != "" {
				break
			}
		}
		if matched == "" {
			missing = append(missing, file)
			continue
		}
		if _, exists := seen[matched]; !exists {
			seen[matched] = struct{}{}
			ids = append(ids, matched)
		}
	}
	return ids, missing
}

func parseClientModelPatterns(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return internalconfig.NormalizeModelPatternList(strings.Split(raw, ","))
}

func clientModelAllowed(access clientModelAccess, model string) bool {
	return internalconfig.IsModelAllowed(model, access.allowed, access.excluded)
}

func clientModelAllowedForContext(ctx context.Context, model string) bool {
	return clientModelAllowed(clientModelAccessFromContext(ctx), model)
}

func clientModelAccessError(model string) *interfaces.ErrorMessage {
	model = strings.TrimSpace(model)
	if model == "" {
		model = "requested model"
	}
	return &interfaces.ErrorMessage{
		StatusCode: http.StatusForbidden,
		Error:      fmt.Errorf("model %s is not permitted for this api key", model),
	}
}

func FilterModelMapsForClient(c *gin.Context, models []map[string]any) []map[string]any {
	access := clientModelAccessFromGin(c)
	if len(access.allowed) == 0 && len(access.excluded) == 0 {
		return models
	}
	filtered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		if !clientModelAllowed(access, modelIdentifierFromMap(model)) {
			continue
		}
		filtered = append(filtered, model)
	}
	return filtered
}

func FilterModelMapsForClientWithAuthManager(c *gin.Context, manager *coreauth.Manager, models []map[string]any) []map[string]any {
	models = FilterModelMapsForClient(c, models)
	files := clientAuthFilesFromGin(c)
	if len(files) == 0 {
		return models
	}
	ids, missing := resolveClientAuthFileIDs(manager, files)
	if len(missing) > 0 || len(ids) == 0 {
		return nil
	}
	filtered := make([]map[string]any, 0, len(models))
	registryRef := registry.GetGlobalRegistry()
	for _, model := range models {
		id := modelIdentifierFromMap(model)
		for _, authID := range ids {
			if registryRef.ClientSupportsModel(authID, id) {
				filtered = append(filtered, model)
				break
			}
		}
	}
	return filtered
}

func FilterOpenAIModelSummariesForClient(c *gin.Context, models []registry.OpenAIModelSummary) []registry.OpenAIModelSummary {
	access := clientModelAccessFromGin(c)
	if len(access.allowed) == 0 && len(access.excluded) == 0 {
		return models
	}
	filtered := make([]registry.OpenAIModelSummary, 0, len(models))
	for _, model := range models {
		if !clientModelAllowed(access, model.ID) {
			continue
		}
		filtered = append(filtered, model)
	}
	return filtered
}

func FilterOpenAIModelSummariesForClientWithAuthManager(c *gin.Context, manager *coreauth.Manager, models []registry.OpenAIModelSummary) []registry.OpenAIModelSummary {
	models = FilterOpenAIModelSummariesForClient(c, models)
	files := clientAuthFilesFromGin(c)
	if len(files) == 0 {
		return models
	}
	ids, missing := resolveClientAuthFileIDs(manager, files)
	if len(missing) > 0 || len(ids) == 0 {
		return nil
	}
	filtered := make([]registry.OpenAIModelSummary, 0, len(models))
	registryRef := registry.GetGlobalRegistry()
	for _, model := range models {
		for _, authID := range ids {
			if registryRef.ClientSupportsModel(authID, model.ID) {
				filtered = append(filtered, model)
				break
			}
		}
	}
	return filtered
}

func modelIdentifierFromMap(model map[string]any) string {
	if len(model) == 0 {
		return ""
	}
	if raw, ok := model["id"].(string); ok && strings.TrimSpace(raw) != "" {
		return raw
	}
	if raw, ok := model["name"].(string); ok && strings.TrimSpace(raw) != "" {
		return raw
	}
	return ""
}
