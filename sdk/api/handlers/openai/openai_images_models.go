package openai

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func imagesModelParts(model string) (prefix string, baseModel string) {
	model = strings.TrimSpace(model)
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx < len(model)-1 {
		return strings.TrimSpace(model[:idx]), strings.TrimSpace(model[idx+1:])
	}
	return "", model
}

func imagesModelBase(model string) string {
	_, baseModel := imagesModelParts(model)
	return strings.ToLower(strings.TrimSpace(baseModel))
}

func isXAIImagesModel(model string) bool {
	prefix, baseModel := imagesModelParts(model)
	baseModel = strings.TrimSpace(baseModel)
	if !strings.EqualFold(baseModel, defaultXAIImagesModel) && !strings.EqualFold(baseModel, xaiImagesQualityModel) {
		return false
	}

	prefix = strings.TrimSpace(prefix)
	return prefix == "" ||
		strings.EqualFold(prefix, "xai") ||
		strings.EqualFold(prefix, "x-ai") ||
		strings.EqualFold(prefix, "grok")
}

func isSupportedImagesModel(model string) bool {
	baseModel := imagesModelBase(model)
	if baseModel == defaultImagesToolModel {
		return true
	}
	return isXAIImagesModel(model) || isOpenAICompatImagesModel(model)
}

func isDefaultImagesToolModel(model string) bool {
	return imagesModelBase(model) == defaultImagesToolModel
}

func isOpenAICompatImagesModel(model string) bool {
	return openAICompatibleImageModel(model)
}

func openAICompatibleImageModel(modelName string) bool {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return false
	}
	if info := registry.LookupModelInfo(modelName); info != nil && strings.EqualFold(strings.TrimSpace(info.Type), registry.OpenAIImageModelType) {
		return true
	}
	if !strings.Contains(modelName, "/") {
		return false
	}
	registryRef := registry.GetGlobalRegistry()
	for _, provider := range registryRef.GetModelProviders(modelName) {
		info := registryRef.GetModelInfo(modelName, provider)
		if info == nil {
			continue
		}
		typ := strings.TrimSpace(info.Type)
		if strings.EqualFold(typ, "openai-compatibility") || strings.EqualFold(typ, registry.OpenAIImageModelType) {
			return true
		}
	}
	return false
}
