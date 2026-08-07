package auth

import (
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type modelAliasEntry interface {
	GetName() string
	GetAlias() string
}

type oauthModelAliasTable struct {
	// reverse maps channel -> alias (lower) -> upstream routing rule.
	reverse map[string]map[string]oauthModelAliasRule
}

type oauthModelAliasRule struct {
	upstreamModel   string
	reasoningEffort map[string]string
}

func compileOAuthModelAliasTable(aliases map[string][]internalconfig.OAuthModelAlias) *oauthModelAliasTable {
	if len(aliases) == 0 {
		return &oauthModelAliasTable{}
	}
	out := &oauthModelAliasTable{
		reverse: make(map[string]map[string]oauthModelAliasRule, len(aliases)),
	}
	for rawChannel, entries := range aliases {
		channel := strings.ToLower(strings.TrimSpace(rawChannel))
		if channel == "" || len(entries) == 0 {
			continue
		}
		rev := make(map[string]oauthModelAliasRule, len(entries))
		for _, entry := range entries {
			name := strings.TrimSpace(entry.Name)
			alias := strings.TrimSpace(entry.Alias)
			if name == "" || alias == "" {
				continue
			}
			reasoningEffort := cloneOAuthModelAliasReasoningEffort(entry.ReasoningEffort)
			if strings.EqualFold(name, alias) && (channel != "codex" || len(reasoningEffort) == 0) {
				continue
			}
			aliasKey := strings.ToLower(alias)
			if _, exists := rev[aliasKey]; exists {
				continue
			}
			rev[aliasKey] = oauthModelAliasRule{
				upstreamModel:   name,
				reasoningEffort: reasoningEffort,
			}
		}
		if len(rev) > 0 {
			out.reverse[channel] = rev
		}
	}
	if len(out.reverse) == 0 {
		out.reverse = nil
	}
	return out
}

func cloneOAuthModelAliasReasoningEffort(efforts map[string]string) map[string]string {
	if len(efforts) == 0 {
		return nil
	}
	out := make(map[string]string, len(efforts))
	for rawSource, rawTarget := range efforts {
		source := strings.ToLower(strings.TrimSpace(rawSource))
		target := strings.ToLower(strings.TrimSpace(rawTarget))
		if source == "" || target == "" {
			continue
		}
		if _, exists := out[source]; exists {
			continue
		}
		out[source] = target
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (r oauthModelAliasRule) targetReasoningEffort(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	if source != "" {
		return strings.TrimSpace(r.reasoningEffort[source])
	}
	return strings.TrimSpace(r.reasoningEffort["default"])
}

// SetOAuthModelAlias updates the OAuth model name alias table used during execution.
// The alias is applied per-auth channel to resolve the upstream model name while keeping the
// client-visible model name unchanged for translation/response formatting.
func (m *Manager) SetOAuthModelAlias(aliases map[string][]internalconfig.OAuthModelAlias) {
	if m == nil {
		return
	}
	table := compileOAuthModelAliasTable(aliases)
	// atomic.Value requires non-nil store values.
	if table == nil {
		table = &oauthModelAliasTable{}
	}
	m.oauthModelAlias.Store(table)
	m.clearRouteAwareCaches()
}

// applyOAuthModelAlias resolves the upstream model from OAuth model alias.
// If an alias exists, the returned model is the upstream model.
func (m *Manager) applyOAuthModelAlias(auth *Auth, requestedModel string) string {
	upstreamModel := m.resolveOAuthUpstreamModel(auth, requestedModel)
	if upstreamModel == "" {
		upstreamModel = resolveBuiltinCodexOAuthModelAlias(auth, requestedModel)
	}
	if upstreamModel == "" {
		return requestedModel
	}
	return upstreamModel
}

func resolveBuiltinCodexOAuthModelAlias(auth *Auth, requestedModel string) string {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return ""
	}
	if modelAliasChannel(auth) == "" {
		return ""
	}
	requestResult, candidates := modelAliasLookupCandidates(requestedModel)
	for _, candidate := range candidates {
		if strings.EqualFold(strings.TrimSpace(candidate), "gpt-5.6") {
			return preserveResolvedModelSuffix("gpt-5.6-sol", requestResult)
		}
	}
	return ""
}

func modelAliasLookupCandidates(requestedModel string) (thinking.SuffixResult, []string) {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return thinking.SuffixResult{}, nil
	}
	requestResult := thinking.ParseSuffix(requestedModel)
	base := requestResult.ModelName
	if base == "" {
		base = requestedModel
	}
	// Prefer an exact suffix-qualified alias when one is configured. The base
	// model remains a fallback so a general alias still covers unlisted efforts.
	candidates := make([]string, 0, 2)
	if base != requestedModel {
		candidates = append(candidates, requestedModel)
	}
	candidates = append(candidates, base)
	return requestResult, candidates
}

func preserveResolvedModelSuffix(resolved string, requestResult thinking.SuffixResult) string {
	resolved = strings.TrimSpace(resolved)
	if resolved == "" {
		return ""
	}
	if thinking.ParseSuffix(resolved).HasSuffix {
		return resolved
	}
	if requestResult.HasSuffix && requestResult.RawSuffix != "" {
		return resolved + "(" + requestResult.RawSuffix + ")"
	}
	return resolved
}

func resolveModelAliasPoolFromConfigModels(requestedModel string, models []modelAliasEntry) []string {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return nil
	}
	if len(models) == 0 {
		return nil
	}

	requestResult, candidates := modelAliasLookupCandidates(requestedModel)
	if len(candidates) == 0 {
		return nil
	}

	out := make([]string, 0)
	seen := make(map[string]struct{})
	for i := range models {
		name := strings.TrimSpace(models[i].GetName())
		alias := strings.TrimSpace(models[i].GetAlias())
		for _, candidate := range candidates {
			if candidate == "" || alias == "" || !strings.EqualFold(alias, candidate) {
				continue
			}
			resolved := candidate
			if name != "" {
				resolved = name
			}
			resolved = preserveResolvedModelSuffix(resolved, requestResult)
			key := strings.ToLower(strings.TrimSpace(resolved))
			if key == "" {
				break
			}
			if _, exists := seen[key]; exists {
				break
			}
			seen[key] = struct{}{}
			out = append(out, resolved)
			break
		}
	}
	if len(out) > 0 {
		return out
	}

	for i := range models {
		name := strings.TrimSpace(models[i].GetName())
		for _, candidate := range candidates {
			if candidate == "" || name == "" || !strings.EqualFold(name, candidate) {
				continue
			}
			return []string{preserveResolvedModelSuffix(name, requestResult)}
		}
	}
	return nil
}

func resolveModelAliasFromConfigModels(requestedModel string, models []modelAliasEntry) string {
	resolved := resolveModelAliasPoolFromConfigModels(requestedModel, models)
	if len(resolved) > 0 {
		return resolved[0]
	}
	return ""
}

// resolveOAuthUpstreamModel resolves the upstream model name from OAuth model alias.
// If an alias exists, returns the original (upstream) model name that corresponds
// to the requested alias.
//
// If the requested model contains a thinking suffix (e.g., "gpt-5(high)"),
// the suffix is preserved in the returned model name. A configured
// reasoning-effort mapping for a Codex alias instead applies the target effort
// through request metadata and leaves the upstream model name clean.
func (m *Manager) resolveOAuthUpstreamModel(auth *Auth, requestedModel string) string {
	return resolveUpstreamModelFromAliasTable(m, auth, requestedModel, modelAliasChannel(auth))
}

func (m *Manager) lookupOAuthModelAliasRule(auth *Auth, requestedModel, channel string) (oauthModelAliasRule, thinking.SuffixResult, bool) {
	if m == nil || auth == nil {
		return oauthModelAliasRule{}, thinking.SuffixResult{}, false
	}
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		return oauthModelAliasRule{}, thinking.SuffixResult{}, false
	}

	requestResult, candidates := modelAliasLookupCandidates(requestedModel)

	raw := m.oauthModelAlias.Load()
	table, _ := raw.(*oauthModelAliasTable)
	if table == nil || table.reverse == nil {
		return oauthModelAliasRule{}, requestResult, false
	}
	rev := table.reverse[channel]
	if rev == nil {
		return oauthModelAliasRule{}, requestResult, false
	}

	for _, candidate := range candidates {
		key := strings.ToLower(strings.TrimSpace(candidate))
		if key == "" {
			continue
		}
		rule, exists := rev[key]
		if !exists || strings.TrimSpace(rule.upstreamModel) == "" {
			continue
		}
		return rule, requestResult, true
	}

	return oauthModelAliasRule{}, requestResult, false
}

func resolveUpstreamModelFromAliasTable(m *Manager, auth *Auth, requestedModel, channel string) string {
	rule, requestResult, ok := m.lookupOAuthModelAliasRule(auth, requestedModel, channel)
	if !ok {
		return ""
	}

	original := strings.TrimSpace(rule.upstreamModel)

	// If config already has suffix, it takes priority.
	if thinking.ParseSuffix(original).HasSuffix {
		return original
	}

	// A configured Codex effort map owns the translated effort. When a legacy
	// suffix selects a mapped source effort, do not propagate that suffix to the
	// upstream model name; the execution path will apply the configured target.
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") && rule.targetReasoningEffort(requestResult.RawSuffix) != "" {
		return original
	}

	// Preserve the caller's suffix for aliases without an effort override.
	if requestResult.HasSuffix && requestResult.RawSuffix != "" {
		return original + "(" + requestResult.RawSuffix + ")"
	}
	return original
}

func (m *Manager) oauthModelAliasReasoningEffort(auth *Auth, requestedModel, sourceEffort string) string {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return ""
	}
	rule, _, ok := m.lookupOAuthModelAliasRule(auth, requestedModel, modelAliasChannel(auth))
	if !ok {
		return ""
	}
	// A suffix configured on the upstream model is the most specific alias
	// setting and must not be replaced by the rule's effort map.
	if thinking.ParseSuffix(rule.upstreamModel).HasSuffix {
		return ""
	}
	return rule.targetReasoningEffort(sourceEffort)
}

func (m *Manager) withOAuthModelAliasReasoningEffort(req cliproxyexecutor.Request, auth *Auth, routeModel string, opts cliproxyexecutor.Options) cliproxyexecutor.Request {
	routeModel = rewriteModelForAuth(routeModel, auth)
	if routeModel == "" {
		routeModel = req.Model
	}
	sourceEffort := reasoningEffortFromOptions(opts)
	if sourceEffort == "" {
		sourceEffort = thinking.ExtractReasoningEffort(req.Payload, opts.SourceFormat.String(), routeModel)
	}
	targetEffort := m.oauthModelAliasReasoningEffort(auth, routeModel, sourceEffort)
	if targetEffort == "" {
		return req
	}

	metadata := make(map[string]any, len(req.Metadata)+1)
	for key, value := range req.Metadata {
		metadata[key] = value
	}
	metadata[cliproxyexecutor.UpstreamReasoningEffortOverrideMetadataKey] = targetEffort
	req.Metadata = metadata
	return req
}

// modelAliasChannel extracts the OAuth model alias channel from an Auth object.
// It determines the provider and auth kind from the Auth's attributes and delegates
// to OAuthModelAliasChannel for the actual channel resolution.
func modelAliasChannel(auth *Auth) string {
	if auth == nil {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	authKind := ""
	if auth.Attributes != nil {
		authKind = strings.ToLower(strings.TrimSpace(auth.Attributes["auth_kind"]))
	}
	if authKind == "" {
		if kind, _ := auth.AccountInfo(); strings.EqualFold(kind, "api_key") {
			authKind = "apikey"
		}
	}
	return OAuthModelAliasChannel(provider, authKind)
}

// OAuthModelAliasChannel returns the OAuth model alias channel name for a given provider
// and auth kind. Returns empty string if the provider/authKind combination doesn't support
// OAuth model alias (e.g., API key authentication).
//
// Supported channels: claude, codex, kimi, xai.
func OAuthModelAliasChannel(provider, authKind string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	authKind = strings.ToLower(strings.TrimSpace(authKind))
	if authKind == "api_key" {
		authKind = "apikey"
	}
	switch provider {
	case "claude":
		if authKind == "apikey" {
			return ""
		}
		return "claude"
	case "codex":
		if authKind == "apikey" {
			return ""
		}
		return "codex"
	case "kimi", "xai":
		return provider
	default:
		return ""
	}
}
