package handlers

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type executionRequestMode struct {
	stream          bool
	allowImageModel bool
}

type preparedExecutionRequest struct {
	ctx                context.Context
	providers          []string
	normalizedModel    string
	request            coreexecutor.Request
	options            coreexecutor.Options
	passthroughHeaders bool
}

func (h *BaseAPIHandler) prepareExecutionRequest(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string, mode executionRequestMode) (preparedExecutionRequest, *interfaces.ErrorMessage) {
	if ctx == nil {
		ctx = context.Background()
	}
	if mode.stream && !mode.allowImageModel && strings.TrimSpace(alt) != "responses/compact" {
		ctx = coreexecutor.WithPreferUpstreamWebsocket(ctx)
	}

	providers, normalizedModel, errMsg := h.getRequestDetailsWithOptions(modelName, mode.allowImageModel)
	if errMsg != nil {
		return preparedExecutionRequest{}, errMsg
	}
	if !clientModelAllowedForContext(ctx, normalizedModel) {
		return preparedExecutionRequest{}, clientModelAccessError(normalizedModel)
	}

	metadata := requestExecutionMetadata(ctx)
	if metadata == nil {
		metadata = make(map[string]any, 4)
	}
	if boundFiles := clientAuthFilesFromContext(ctx); len(boundFiles) > 0 {
		boundIDs, missing := resolveClientAuthFileIDs(h.AuthManager, boundFiles)
		if len(missing) > 0 {
			return preparedExecutionRequest{}, &interfaces.ErrorMessage{
				StatusCode: http.StatusServiceUnavailable,
				Error: &coreauth.Error{
					Code:       "auth_binding_not_found",
					Message:    "configured auth file binding is not available",
					HTTPStatus: http.StatusServiceUnavailable,
				},
			}
		}
		if len(boundIDs) == 0 {
			return preparedExecutionRequest{}, &interfaces.ErrorMessage{
				StatusCode: http.StatusServiceUnavailable,
				Error: &coreauth.Error{
					Code:       "auth_binding_not_found",
					Message:    "configured auth file binding resolved to no credentials",
					HTTPStatus: http.StatusServiceUnavailable,
				},
			}
		}
		metadata[coreexecutor.AllowedAuthIDsMetadataKey] = boundIDs
	}
	metadata[coreexecutor.RequestedModelMetadataKey] = modelName
	setReasoningEffortMetadata(metadata, handlerType, normalizedModel, rawJSON)
	passthroughHeaders := PassthroughHeadersEnabled(h.Cfg)
	if passthroughHeaders {
		metadata[coreexecutor.NeedResponseHeadersMetadataKey] = true
	}
	setServiceTierMetadata(metadata, rawJSON)
	requestMetadata := coreexecutor.RequestMetadata{
		Parsed:          true,
		RequestedModel:  modelName,
		NormalizedModel: normalizedModel,
		ReasoningEffort: metadataString(metadata, coreexecutor.ReasoningEffortMetadataKey),
		ServiceTier:     metadataString(metadata, coreexecutor.ServiceTierMetadataKey),
		RequestPath:     metadataString(metadata, coreexecutor.RequestPathMetadataKey),
		ContentType:     metadataString(metadata, coreexecutor.RequestContentTypeMetadataKey),
		IdempotencyKey:  metadataString(metadata, idempotencyKeyMetadataKey),
		Stream:          mode.stream,
	}
	requestMetadata.ExecutionSessionID = metadataString(metadata, coreexecutor.ExecutionSessionMetadataKey)

	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	return preparedExecutionRequest{
		ctx:             ctx,
		providers:       providers,
		normalizedModel: normalizedModel,
		request: coreexecutor.Request{
			Model:   normalizedModel,
			Payload: payload,
		},
		options: coreexecutor.Options{
			Stream:          mode.stream,
			Alt:             alt,
			Headers:         requestHeadersFromContext(ctx),
			OriginalRequest: rawJSON,
			SourceFormat:    sdktranslator.FromString(handlerType),
			Metadata:        metadata,
			RequestMetadata: requestMetadata,
		},
		passthroughHeaders: passthroughHeaders,
	}, nil
}

func metadataString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	switch value := metadata[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}
