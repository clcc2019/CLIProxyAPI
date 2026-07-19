package auth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

func sanitizeDownstreamWebsocketFallbackRequest(ctx context.Context, auth *Auth, req cliproxyexecutor.Request) cliproxyexecutor.Request {
	if !cliproxyexecutor.DownstreamWebsocket(ctx) || authWebsocketsEnabled(auth) || len(req.Payload) == 0 {
		return req
	}
	updated, errDelete := sjson.DeleteBytes(req.Payload, "generate")
	if errDelete != nil {
		return req
	}
	req.Payload = updated
	return req
}

func ensureRequestedModelMetadata(opts cliproxyexecutor.Options, requestedModel string) cliproxyexecutor.Options {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return opts
	}
	if hasRequestedModelMetadata(opts.Metadata) {
		return opts
	}
	if len(opts.Metadata) == 0 {
		opts.Metadata = map[string]any{cliproxyexecutor.RequestedModelMetadataKey: requestedModel}
		return opts
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	for k, v := range opts.Metadata {
		meta[k] = v
	}
	meta[cliproxyexecutor.RequestedModelMetadataKey] = requestedModel
	opts.Metadata = meta
	return opts
}

func withHomeAuthCount(opts cliproxyexecutor.Options, count int) cliproxyexecutor.Options {
	if count <= 0 {
		count = 1
	}
	if opts.Metadata == nil {
		opts.Metadata = make(map[string]any, 1)
	}
	opts.Metadata[homeAuthCountMetadataKey] = count
	return opts
}

func homeAuthCountFromMetadata(meta map[string]any) int {
	if len(meta) == 0 {
		return 1
	}
	switch value := meta[homeAuthCountMetadataKey].(type) {
	case int:
		if value > 0 {
			return value
		}
	case int64:
		if value > 0 {
			return int(value)
		}
	case float64:
		if value > 0 {
			return int(value)
		}
	}
	return 1
}

func hasRequestedModelMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case []byte:
		return strings.TrimSpace(string(v)) != ""
	default:
		return false
	}
}

type requestAuthPrepareLock struct {
	mu sync.Mutex
}

func (m *Manager) prepareRequestAuth(ctx context.Context, executor ProviderExecutor, auth *Auth) (*Auth, error) {
	if m == nil || executor == nil || auth == nil {
		return auth, nil
	}
	preparer, ok := executor.(RequestAuthPreparer)
	if !ok || preparer == nil || !preparer.ShouldPrepareRequestAuth(auth) {
		return auth, nil
	}

	id := strings.TrimSpace(auth.ID)
	if id == "" {
		return preparer.PrepareRequestAuth(ctx, auth.Clone())
	}

	lockValue, _ := m.requestPrepareLocks.LoadOrStore(id, &requestAuthPrepareLock{})
	lock, ok := lockValue.(*requestAuthPrepareLock)
	if !ok || lock == nil {
		return preparer.PrepareRequestAuth(ctx, auth.Clone())
	}

	lock.mu.Lock()
	defer lock.mu.Unlock()

	target := auth.Clone()
	m.mu.RLock()
	if current := m.auths[id]; current != nil {
		target = current.Clone()
	}
	m.mu.RUnlock()

	if !preparer.ShouldPrepareRequestAuth(target) {
		return target, nil
	}

	updated, errPrepare := preparer.PrepareRequestAuth(ctx, target)
	if errPrepare != nil {
		return auth, errPrepare
	}
	if updated == nil {
		return target, nil
	}

	saved, errUpdate := m.Update(ctx, updated)
	if errUpdate != nil {
		return updated, errUpdate
	}
	if saved != nil {
		return saved, nil
	}
	return updated, nil
}

func contextWithRequestedModelAlias(ctx context.Context, opts cliproxyexecutor.Options, fallback string) context.Context {
	alias := requestedModelAliasFromOptions(opts, fallback)
	ctx = coreusage.WithRequestedModelAlias(ctx, alias)
	effort := reasoningEffortFromOptions(opts)
	if effort != "" {
		ctx = coreusage.WithReasoningEffort(ctx, effort)
	}
	serviceTier := serviceTierFromOptions(opts)
	if serviceTier != "" {
		ctx = coreusage.WithServiceTier(ctx, serviceTier)
	}
	return ctx
}

func requestedModelAliasFromOptions(opts cliproxyexecutor.Options, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if len(opts.Metadata) == 0 {
		return fallback
	}
	raw, ok := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return fallback
	}
	switch value := raw.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return fallback
		}
		return strings.TrimSpace(value)
	case []byte:
		if len(value) == 0 {
			return fallback
		}
		return strings.TrimSpace(string(value))
	default:
		return fallback
	}
}

func reasoningEffortFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ReasoningEffortMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func serviceTierFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ServiceTierMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func pinnedAuthIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.PinnedAuthMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch val := raw.(type) {
	case string:
		return strings.TrimSpace(val)
	case []byte:
		return strings.TrimSpace(string(val))
	default:
		return ""
	}
}

func withPinnedAuthMetadata(opts cliproxyexecutor.Options, authID string) cliproxyexecutor.Options {
	authID = strings.TrimSpace(authID)
	if authID == "" || pinnedAuthIDFromMetadata(opts.Metadata) == authID {
		return opts
	}
	metadata := make(map[string]any, len(opts.Metadata)+1)
	for key, value := range opts.Metadata {
		metadata[key] = value
	}
	metadata[cliproxyexecutor.PinnedAuthMetadataKey] = authID
	opts.Metadata = metadata
	return opts
}

func ensureOptionsMetadata(opts *cliproxyexecutor.Options) {
	if opts == nil || opts.Metadata != nil {
		return
	}
	opts.Metadata = make(map[string]any, 1)
}

func forceNewUpstreamSessionForNextCredential(opts *cliproxyexecutor.Options) {
	if opts == nil {
		return
	}
	ensureOptionsMetadata(opts)
	opts.Metadata[cliproxyexecutor.ForcedUpstreamSessionMetadataKey] = uuid.NewString()
}

func isRecoverableAffinityPickError(err error) bool {
	if err == nil {
		return false
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) {
		return true
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	switch authErr.Code {
	case "auth_not_found", "auth_unavailable", "model_cooldown":
		return true
	default:
		return false
	}
}

func recoverableAffinityPickErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) {
		return "model_cooldown"
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		return strings.TrimSpace(authErr.Code)
	}
	return fmt.Sprintf("%T", err)
}

func logRecoverableAffinityPick(ctx context.Context, mode, providers, model, authID string, opts cliproxyexecutor.Options, err error) {
	if !log.IsLevelEnabled(log.InfoLevel) {
		return
	}
	primaryID, fallbackID := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	fields := log.Fields{
		"auth_id": authID,
		"mode":    mode,
	}
	if providers != "" {
		fields["provider"] = providers
	}
	if model != "" {
		fields["model"] = model
	}
	if reason := recoverableAffinityPickErrorCode(err); reason != "" {
		fields["reason"] = reason
	}
	if primaryID != "" {
		fields["session_id"] = truncateSessionID(primaryID)
	}
	if fallbackID != "" && fallbackID != primaryID {
		fields["fallback_session_id"] = truncateSessionID(fallbackID)
	}
	selectorLogEntry(ctx).WithFields(fields).Info("session-affinity: cached auth unavailable, reselecting")
}

func selectedAuthIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.SelectedAuthMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch val := raw.(type) {
	case string:
		return strings.TrimSpace(val)
	case []byte:
		return strings.TrimSpace(string(val))
	default:
		return ""
	}
}

func withProviderScopeMetadata(opts cliproxyexecutor.Options, providers []string) cliproxyexecutor.Options {
	providers = normalizeProviderKeys(providers)
	if len(providers) == 0 {
		return opts
	}
	scope := "providers:" + strings.Join(providers, ",")
	if metadataStringValue(opts.Metadata, cliproxyexecutor.ProviderScopeMetadataKey) == scope {
		return opts
	}
	if opts.Metadata == nil {
		opts.Metadata = make(map[string]any, 1)
	}
	opts.Metadata[cliproxyexecutor.ProviderScopeMetadataKey] = scope
	return opts
}

func disallowFreeAuthFromMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.DisallowFreeAuthMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch val := raw.(type) {
	case bool:
		return val
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(val))
		return err == nil && parsed
	case []byte:
		parsed, err := strconv.ParseBool(strings.TrimSpace(string(val)))
		return err == nil && parsed
	default:
		return false
	}
}

func isFreeCodexAuth(auth *Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

func publishSelectedAuthMetadata(meta map[string]any, authID string) {
	if len(meta) == 0 {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	meta[cliproxyexecutor.SelectedAuthMetadataKey] = authID
	if callback, ok := meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(func(string)); ok && callback != nil {
		callback(authID)
	}
}

func clearSelectedAuthMetadataForCredentialFailover(provider string, meta map[string]any, authID string, err error) {
	if len(meta) == 0 || !isCredentialFailoverFailure(err) {
		return
	}
	if selectedAuthIDFromMetadata(meta) != strings.TrimSpace(authID) {
		return
	}
	delete(meta, cliproxyexecutor.SelectedAuthMetadataKey)
}
