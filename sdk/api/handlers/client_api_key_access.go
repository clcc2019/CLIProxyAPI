package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
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
