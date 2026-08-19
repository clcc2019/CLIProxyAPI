package auth

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const AttributeConfigIndex = "config_index"

const (
	RequestScopedActionStop                = "stop"
	RequestScopedActionStopAndCooldown     = "stop-and-cooldown"
	RequestScopedActionContinue            = "continue"
	RequestScopedActionContinueAndCooldown = "continue-and-cooldown"
)

type requestStopError struct{ error }

func (e requestStopError) Unwrap() error       { return e.error }
func (e requestStopError) IsRequestStop() bool { return true }

type requestContinueError struct{ error }

func (e requestContinueError) Unwrap() error           { return e.error }
func (e requestContinueError) IsRequestContinue() bool { return true }

func isRequestStopError(err error) bool {
	if err == nil {
		return false
	}
	type stopChecker interface{ IsRequestStop() bool }
	var checker stopChecker
	return errors.As(err, &checker) && checker != nil && checker.IsRequestStop()
}

func isRequestContinueError(err error) bool {
	if err == nil {
		return false
	}
	type continueChecker interface{ IsRequestContinue() bool }
	var checker continueChecker
	return errors.As(err, &checker) && checker != nil && checker.IsRequestContinue()
}

func unwrapRequestStopError(err error) error {
	var stopErr requestStopError
	if errors.As(err, &stopErr) {
		return stopErr.error
	}
	return err
}

func unwrapRequestContinueError(err error) error {
	var continueErr requestContinueError
	if errors.As(err, &continueErr) {
		return continueErr.error
	}
	return err
}

func wrapRequestStopError(err error) error {
	if err == nil {
		return nil
	}
	return requestStopError{error: unwrapRequestStopError(err)}
}

func wrapRequestContinueError(err error) error {
	if err == nil {
		return nil
	}
	return requestContinueError{error: unwrapRequestContinueError(err)}
}

func (m *Manager) runtimeConfigSnapshot() *internalconfig.Config {
	if m == nil {
		return nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return cfg
}

func requestScopedRulesFromMetadata(auth *Auth) []internalconfig.RequestScopedErrorRule {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	raw, ok := auth.Metadata["request_scoped_errors"]
	if !ok {
		raw, ok = auth.Metadata["request-scoped-errors"]
	}
	if !ok || raw == nil {
		return nil
	}
	if rules, ok := raw.([]internalconfig.RequestScopedErrorRule); ok && len(rules) > 0 {
		return rules
	}
	data, errMarshal := json.Marshal(raw)
	if errMarshal != nil {
		return nil
	}
	var rules []internalconfig.RequestScopedErrorRule
	if errUnmarshal := json.Unmarshal(data, &rules); errUnmarshal != nil {
		return nil
	}
	return rules
}

func authConfigIndex(auth *Auth) int {
	if auth == nil || auth.Attributes == nil {
		return -1
	}
	index, err := strconv.Atoi(strings.TrimSpace(auth.Attributes[AttributeConfigIndex]))
	if err != nil || index < 0 {
		return -1
	}
	return index
}

func extractRequestScopedErrorRules(auth *Auth, cfg *internalconfig.Config) []internalconfig.RequestScopedErrorRule {
	if rules := requestScopedRulesFromMetadata(auth); len(rules) > 0 {
		return rules
	}
	if auth == nil || cfg == nil {
		return nil
	}

	index := authConfigIndex(auth)
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	providerKey, compatName, baseURL := "", "", ""
	if auth.Attributes != nil {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
	}
	if compatName == "" {
		switch {
		case strings.HasPrefix(provider, "openai-compatible-"):
			compatName = strings.TrimPrefix(provider, "openai-compatible-")
		case strings.HasPrefix(provider, "openai-compatibility:"):
			compatName = strings.TrimPrefix(provider, "openai-compatibility:")
		}
	}

	if strings.Contains(provider, "compat") || providerKey != "" || compatName != "" {
		if index >= 0 && index < len(cfg.OpenAICompatibility) {
			entry := &cfg.OpenAICompatibility[index]
			if !entry.Disabled {
				return entry.RequestScopedErrors
			}
		}
		if entry := internalconfig.ResolveOpenAICompatibility(cfg.OpenAICompatibility, providerKey, compatName, provider, baseURL); entry != nil {
			return entry.RequestScopedErrors
		}
		return nil
	}

	switch provider {
	case "claude":
		if index >= 0 && index < len(cfg.ClaudeKey) {
			return cfg.ClaudeKey[index].RequestScopedErrors
		}
	case "codex":
		if index >= 0 && index < len(cfg.CodexKey) {
			return cfg.CodexKey[index].RequestScopedErrors
		}
	}
	return nil
}

func extractErrorBody(err error) string {
	if err == nil {
		return ""
	}
	type responseBodyProvider interface{ ResponseBody() []byte }
	var provider responseBodyProvider
	if errors.As(err, &provider) && provider != nil {
		if body := provider.ResponseBody(); len(body) > 0 {
			return string(body)
		}
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil && authErr.Message != "" {
		return authErr.Message
	}
	return err.Error()
}

func matchRequestScopedErrorAction(auth *Auth, err error, cfg *internalconfig.Config) (string, bool) {
	if err == nil {
		return "", false
	}
	rules := extractRequestScopedErrorRules(auth, cfg)
	if len(rules) == 0 {
		return "", false
	}

	statusCode := statusCodeFromError(err)
	body := extractErrorBody(err)
	for _, rule := range rules {
		if rule.Status <= 0 || rule.Status != statusCode || (len(rule.Match) == 0 && len(rule.MatchRegexr) == 0) {
			continue
		}
		matched := false
		for _, value := range rule.Match {
			if value != "" && strings.Contains(body, value) {
				matched = true
				break
			}
		}
		if !matched {
			for _, pattern := range rule.MatchRegexr {
				if pattern == "" {
					continue
				}
				compiled, errCompile := regexp.Compile(pattern)
				if errCompile == nil && compiled.MatchString(body) {
					matched = true
					break
				}
			}
		}
		if !matched {
			continue
		}
		action := strings.ToLower(strings.TrimSpace(rule.Action))
		switch action {
		case RequestScopedActionStop, RequestScopedActionStopAndCooldown, RequestScopedActionContinue, RequestScopedActionContinueAndCooldown:
			return action, true
		}
	}
	return "", false
}

func applyRequestScopedActionToResult(action string, okAction bool, result *Result) {
	if !okAction || result == nil || result.Error == nil {
		return
	}
	switch action {
	case RequestScopedActionStop, RequestScopedActionContinue:
		result.Error.Code = ErrorCodeRequestScoped
	case RequestScopedActionStopAndCooldown, RequestScopedActionContinueAndCooldown:
		result.Error.Code = ErrorCodeForceCooldown
	}
}

func isRequestScopedStop(action string, okAction bool) bool {
	return okAction && (action == RequestScopedActionStop || action == RequestScopedActionStopAndCooldown)
}

func isRequestScopedResultError(err *Error) bool {
	return err != nil && err.Code == ErrorCodeRequestScoped
}
