package executor

import (
	"context"
	"net/http"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const (
	codexHeaderModelsETag        = "X-Models-Etag"
	codexHeaderOpenAIModel       = "OpenAI-Model"
	codexHeaderReasoningIncluded = "X-Reasoning-Included"
)

// CodexResponseMetadata contains response characteristics that the official
// Codex client consumes before processing stream events. The server-selected
// model can differ from the requested model when safety routing applies.
// ReasoningIncluded is true when the header is present, including when its
// value is empty, matching Codex's header-presence semantics.
type CodexResponseMetadata struct {
	ModelsETag        string
	ServerModel       string
	ReasoningIncluded bool
}

// CodexResponseObserver receives response metadata from either Codex transport.
// Implementations must not retain auth without cloning it and should return
// quickly; it runs on the request path before the response body is consumed.
type CodexResponseObserver func(context.Context, *cliproxyauth.Auth, CodexResponseMetadata)

func codexResponseMetadataFromHeaders(headers http.Header) CodexResponseMetadata {
	return CodexResponseMetadata{
		ModelsETag:        codexResponseHeaderValue(headers, codexHeaderModelsETag),
		ServerModel:       codexResponseHeaderValue(headers, codexHeaderOpenAIModel),
		ReasoningIncluded: codexResponseHeaderPresent(headers, codexHeaderReasoningIncluded),
	}
}

func (metadata CodexResponseMetadata) empty() bool {
	return metadata.ModelsETag == "" && metadata.ServerModel == "" && !metadata.ReasoningIncluded
}

func codexResponseHeaderValue(headers http.Header, target string) string {
	if headers == nil {
		return ""
	}
	if value := strings.TrimSpace(headers.Get(target)); value != "" {
		return value
	}
	for key, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), target) {
			continue
		}
		for _, value := range values {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func codexResponseHeaderPresent(headers http.Header, target string) bool {
	for key := range headers {
		if strings.EqualFold(strings.TrimSpace(key), target) {
			return true
		}
	}
	return false
}

// codexServerModelFromResponseData mirrors Codex's ResponseEvent::ServerModel
// updates. In addition to the model in a completed response, codex-rs reads
// OpenAI-Model from response.headers on ordinary stream events and from the
// top-level headers on response.metadata WebSocket events. The latter two
// shapes are important when the completed event does not repeat a model after
// server-side safety routing.
func codexServerModelFromResponseData(data []byte) string {
	for _, path := range []string{"response.headers", "headers"} {
		if model := codexServerModelFromEventHeaders(gjson.GetBytes(data, path)); model != "" {
			return model
		}
	}
	for _, path := range []string{"response.model", "model"} {
		if model := strings.TrimSpace(gjson.GetBytes(data, path).String()); model != "" {
			return model
		}
	}
	return ""
}

func codexServerModelFromEventHeaders(headers gjson.Result) string {
	if !headers.IsObject() {
		return ""
	}
	model := ""
	headers.ForEach(func(key, value gjson.Result) bool {
		name := strings.TrimSpace(key.String())
		if !strings.EqualFold(name, codexHeaderOpenAIModel) && !strings.EqualFold(name, "X-OpenAI-Model") {
			return true
		}
		switch {
		case value.Type == gjson.String:
			model = strings.TrimSpace(value.String())
		case value.IsArray():
			for _, item := range value.Array() {
				if item.Type == gjson.String && strings.TrimSpace(item.String()) != "" {
					model = strings.TrimSpace(item.String())
					break
				}
			}
		}
		return model == ""
	})
	return model
}

func (e *CodexExecutor) observeCodexResponseHeaders(ctx context.Context, auth *cliproxyauth.Auth, headers http.Header) {
	if e == nil || e.responseObserver == nil {
		return
	}
	metadata := codexResponseMetadataFromHeaders(headers)
	if metadata.empty() {
		return
	}
	e.responseObserver(ctx, auth, metadata)
}
