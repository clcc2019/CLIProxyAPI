package auth

import (
	_ "embed"
	"sort"
	"strings"
	"time"
	_ "time/tzdata"
)

const (
	DefaultOpenAIRequestTimezone     = "Asia/Singapore"
	AuthFileOpenAIRequestTimezoneKey = "openai_request_timezone"
)

//go:embed openai_request_timezones.txt
var openAIRequestTimezoneNames string

var openAIRequestTimezoneOptions = strings.Fields(openAIRequestTimezoneNames)
var openAIRequestTimezoneAllowed = func() map[string]struct{} {
	allowed := make(map[string]struct{}, len(openAIRequestTimezoneOptions))
	for _, name := range openAIRequestTimezoneOptions {
		allowed[name] = struct{}{}
	}
	return allowed
}()

func OpenAIRequestTimezoneOptions() []string {
	options := append([]string(nil), openAIRequestTimezoneOptions...)
	sort.Strings(options)
	return options
}

func IsValidOpenAIRequestTimezone(name string) bool {
	name = strings.TrimSpace(name)
	if _, ok := openAIRequestTimezoneAllowed[name]; !ok {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

func (a *Auth) OpenAIRequestTimezone() string {
	if a != nil && strings.EqualFold(strings.TrimSpace(a.Provider), "codex") && a.Metadata != nil {
		if name, ok := a.Metadata[AuthFileOpenAIRequestTimezoneKey].(string); ok {
			name = strings.TrimSpace(name)
			if IsValidOpenAIRequestTimezone(name) {
				return name
			}
		}
	}
	return DefaultOpenAIRequestTimezone
}
