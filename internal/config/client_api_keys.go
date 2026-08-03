package config

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ClientAPIKeyEntry describes one client-facing API key that can authenticate
// requests sent to CLIProxyAPI. The key may optionally be disabled or restricted
// to a subset of client-visible model IDs and usage quotas.
type ClientAPIKeyEntry struct {
	APIKey         string            `yaml:"api-key" json:"api-key"`
	Note           string            `yaml:"note,omitempty" json:"note,omitempty"`
	Disabled       bool              `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	AllowedModels  []string          `yaml:"allowed-models,omitempty" json:"allowed-models,omitempty"`
	ExcludedModels []string          `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`
	Quota          ClientAPIKeyQuota `yaml:"quota,omitempty" json:"quota,omitempty"`
}

// ClientAPIKeyQuota limits request count, token usage, and spend for one
// client-facing API key. Zero values mean unlimited. Daily and monthly windows
// are evaluated in UTC; cost limits are measured in USD.
type ClientAPIKeyQuota struct {
	DailyCost   float64 `yaml:"daily-cost,omitempty" json:"daily-cost,omitempty"`
	MonthlyCost float64 `yaml:"monthly-cost,omitempty" json:"monthly-cost,omitempty"`
	TotalCost   float64 `yaml:"total-cost,omitempty" json:"total-cost,omitempty"`

	DailyTokens     int64 `yaml:"daily-tokens,omitempty" json:"daily-tokens,omitempty"`
	MonthlyTokens   int64 `yaml:"monthly-tokens,omitempty" json:"monthly-tokens,omitempty"`
	TotalTokens     int64 `yaml:"total-tokens,omitempty" json:"total-tokens,omitempty"`
	DailyRequests   int64 `yaml:"daily-requests,omitempty" json:"daily-requests,omitempty"`
	MonthlyRequests int64 `yaml:"monthly-requests,omitempty" json:"monthly-requests,omitempty"`
	TotalRequests   int64 `yaml:"total-requests,omitempty" json:"total-requests,omitempty"`
}

// ClientAPIKeys keeps backward compatibility with the historical
// `api-keys: ["k1", "k2"]` format while allowing richer object entries.
type ClientAPIKeys []ClientAPIKeyEntry

const (
	clientAPIKeyQuotaDailyCostMetadataKey       = "quota_daily_cost"
	clientAPIKeyQuotaMonthlyCostMetadataKey     = "quota_monthly_cost"
	clientAPIKeyQuotaTotalCostMetadataKey       = "quota_total_cost"
	clientAPIKeyQuotaDailyTokensMetadataKey     = "quota_daily_tokens"
	clientAPIKeyQuotaMonthlyTokensMetadataKey   = "quota_monthly_tokens"
	clientAPIKeyQuotaTotalTokensMetadataKey     = "quota_total_tokens"
	clientAPIKeyQuotaDailyRequestsMetadataKey   = "quota_daily_requests"
	clientAPIKeyQuotaMonthlyRequestsMetadataKey = "quota_monthly_requests"
	clientAPIKeyQuotaTotalRequestsMetadataKey   = "quota_total_requests"
)

// HasLimits reports whether any quota field is configured.
func (quota ClientAPIKeyQuota) HasLimits() bool {
	quota = NormalizeClientAPIKeyQuota(quota)
	return quota.DailyCost > 0 ||
		quota.MonthlyCost > 0 ||
		quota.TotalCost > 0 ||
		quota.DailyTokens > 0 ||
		quota.MonthlyTokens > 0 ||
		quota.TotalTokens > 0 ||
		quota.DailyRequests > 0 ||
		quota.MonthlyRequests > 0 ||
		quota.TotalRequests > 0
}

// IsZero lets encoders that honor IsZero omit empty quotas on direct entry marshaling.
func (quota ClientAPIKeyQuota) IsZero() bool {
	return !quota.HasLimits()
}

// NormalizeClientAPIKeyQuota removes invalid negative limits.
func NormalizeClientAPIKeyQuota(quota ClientAPIKeyQuota) ClientAPIKeyQuota {
	quota.DailyCost = normalizeClientAPIKeyQuotaLimit(quota.DailyCost)
	quota.MonthlyCost = normalizeClientAPIKeyQuotaLimit(quota.MonthlyCost)
	quota.TotalCost = normalizeClientAPIKeyQuotaLimit(quota.TotalCost)
	quota.DailyTokens = normalizeClientAPIKeyQuotaCount(quota.DailyTokens)
	quota.MonthlyTokens = normalizeClientAPIKeyQuotaCount(quota.MonthlyTokens)
	quota.TotalTokens = normalizeClientAPIKeyQuotaCount(quota.TotalTokens)
	quota.DailyRequests = normalizeClientAPIKeyQuotaCount(quota.DailyRequests)
	quota.MonthlyRequests = normalizeClientAPIKeyQuotaCount(quota.MonthlyRequests)
	quota.TotalRequests = normalizeClientAPIKeyQuotaCount(quota.TotalRequests)
	return quota
}

// AddClientAPIKeyQuotaMetadata serializes quota limits into access metadata.
func AddClientAPIKeyQuotaMetadata(metadata map[string]string, quota ClientAPIKeyQuota) {
	if metadata == nil {
		return
	}
	quota = NormalizeClientAPIKeyQuota(quota)
	if quota.DailyCost > 0 {
		metadata[clientAPIKeyQuotaDailyCostMetadataKey] = strconv.FormatFloat(quota.DailyCost, 'f', -1, 64)
	}
	if quota.MonthlyCost > 0 {
		metadata[clientAPIKeyQuotaMonthlyCostMetadataKey] = strconv.FormatFloat(quota.MonthlyCost, 'f', -1, 64)
	}
	if quota.TotalCost > 0 {
		metadata[clientAPIKeyQuotaTotalCostMetadataKey] = strconv.FormatFloat(quota.TotalCost, 'f', -1, 64)
	}
	if quota.DailyTokens > 0 {
		metadata[clientAPIKeyQuotaDailyTokensMetadataKey] = strconv.FormatInt(quota.DailyTokens, 10)
	}
	if quota.MonthlyTokens > 0 {
		metadata[clientAPIKeyQuotaMonthlyTokensMetadataKey] = strconv.FormatInt(quota.MonthlyTokens, 10)
	}
	if quota.TotalTokens > 0 {
		metadata[clientAPIKeyQuotaTotalTokensMetadataKey] = strconv.FormatInt(quota.TotalTokens, 10)
	}
	if quota.DailyRequests > 0 {
		metadata[clientAPIKeyQuotaDailyRequestsMetadataKey] = strconv.FormatInt(quota.DailyRequests, 10)
	}
	if quota.MonthlyRequests > 0 {
		metadata[clientAPIKeyQuotaMonthlyRequestsMetadataKey] = strconv.FormatInt(quota.MonthlyRequests, 10)
	}
	if quota.TotalRequests > 0 {
		metadata[clientAPIKeyQuotaTotalRequestsMetadataKey] = strconv.FormatInt(quota.TotalRequests, 10)
	}
}

// ClientAPIKeyQuotaFromMetadata parses quota limits emitted by the config API key provider.
func ClientAPIKeyQuotaFromMetadata(metadata map[string]string) ClientAPIKeyQuota {
	if len(metadata) == 0 {
		return ClientAPIKeyQuota{}
	}
	return NormalizeClientAPIKeyQuota(ClientAPIKeyQuota{
		DailyCost:       parseClientAPIKeyQuotaMetadataValue(metadata[clientAPIKeyQuotaDailyCostMetadataKey]),
		MonthlyCost:     parseClientAPIKeyQuotaMetadataValue(metadata[clientAPIKeyQuotaMonthlyCostMetadataKey]),
		TotalCost:       parseClientAPIKeyQuotaMetadataValue(metadata[clientAPIKeyQuotaTotalCostMetadataKey]),
		DailyTokens:     parseClientAPIKeyQuotaMetadataCount(metadata[clientAPIKeyQuotaDailyTokensMetadataKey]),
		MonthlyTokens:   parseClientAPIKeyQuotaMetadataCount(metadata[clientAPIKeyQuotaMonthlyTokensMetadataKey]),
		TotalTokens:     parseClientAPIKeyQuotaMetadataCount(metadata[clientAPIKeyQuotaTotalTokensMetadataKey]),
		DailyRequests:   parseClientAPIKeyQuotaMetadataCount(metadata[clientAPIKeyQuotaDailyRequestsMetadataKey]),
		MonthlyRequests: parseClientAPIKeyQuotaMetadataCount(metadata[clientAPIKeyQuotaMonthlyRequestsMetadataKey]),
		TotalRequests:   parseClientAPIKeyQuotaMetadataCount(metadata[clientAPIKeyQuotaTotalRequestsMetadataKey]),
	})
}

func parseClientAPIKeyQuotaMetadataValue(value string) float64 {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || parsed <= 0 {
		return 0
	}
	return normalizeClientAPIKeyQuotaLimit(parsed)
}

func parseClientAPIKeyQuotaMetadataCount(value string) int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
}

// SanitizeClientAPIKeys normalizes the configured client-facing API keys.
func (cfg *SDKConfig) SanitizeClientAPIKeys() {
	if cfg == nil {
		return
	}
	cfg.APIKeys = NormalizeClientAPIKeys(cfg.APIKeys)
}

// Values returns the plain API key values in order.
func (keys ClientAPIKeys) Values() []string {
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, raw := range keys {
		entry := normalizeClientAPIKeyEntry(raw)
		if entry.APIKey != "" {
			out = append(out, entry.APIKey)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// MarshalYAML emits plain strings when an entry has no extra settings, and an
// object entry otherwise.
func (keys ClientAPIKeys) MarshalYAML() (any, error) {
	if len(keys) == 0 {
		return []any{}, nil
	}
	out := make([]any, 0, len(keys))
	for _, raw := range keys {
		entry := normalizeClientAPIKeyEntry(raw)
		if entry.APIKey == "" {
			continue
		}
		if entry.Note == "" && !entry.Disabled && len(entry.AllowedModels) == 0 && len(entry.ExcludedModels) == 0 && !entry.Quota.HasLimits() {
			out = append(out, entry.APIKey)
			continue
		}
		item := map[string]any{
			"api-key": entry.APIKey,
		}
		if entry.Note != "" {
			item["note"] = entry.Note
		}
		if entry.Disabled {
			item["disabled"] = true
		}
		if len(entry.AllowedModels) > 0 {
			item["allowed-models"] = entry.AllowedModels
		}
		if len(entry.ExcludedModels) > 0 {
			item["excluded-models"] = entry.ExcludedModels
		}
		if entry.Quota.HasLimits() {
			item["quota"] = entry.Quota
		}
		out = append(out, item)
	}
	return out, nil
}

// UnmarshalYAML accepts both string and object entries.
func (keys *ClientAPIKeys) UnmarshalYAML(value *yaml.Node) error {
	if keys == nil {
		return nil
	}
	if value == nil || value.Kind == 0 || value.Tag == "!!null" {
		*keys = nil
		return nil
	}
	if value.Kind != yaml.SequenceNode {
		return fmt.Errorf("api-keys must be a sequence")
	}
	parsed := make(ClientAPIKeys, 0, len(value.Content))
	for _, item := range value.Content {
		if item == nil {
			continue
		}
		switch item.Kind {
		case yaml.ScalarNode:
			parsed = append(parsed, ClientAPIKeyEntry{APIKey: strings.TrimSpace(item.Value)})
		case yaml.MappingNode:
			var entry ClientAPIKeyEntry
			if err := item.Decode(&entry); err != nil {
				return err
			}
			parsed = append(parsed, entry)
		default:
			return fmt.Errorf("api-keys entries must be strings or objects")
		}
	}
	*keys = NormalizeClientAPIKeys(parsed)
	return nil
}

// MarshalJSON mirrors MarshalYAML for management API responses.
func (keys ClientAPIKeys) MarshalJSON() ([]byte, error) {
	serializable, err := keys.MarshalYAML()
	if err != nil {
		return nil, err
	}
	return json.Marshal(serializable)
}

// UnmarshalJSON accepts both string and object entries.
func (keys *ClientAPIKeys) UnmarshalJSON(data []byte) error {
	if keys == nil {
		return nil
	}
	var raw []any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	parsed := make(ClientAPIKeys, 0, len(raw))
	for _, item := range raw {
		switch typed := item.(type) {
		case string:
			parsed = append(parsed, ClientAPIKeyEntry{APIKey: strings.TrimSpace(typed)})
		case map[string]any:
			entry := ClientAPIKeyEntry{}
			if value, ok := typed["api-key"]; ok {
				entry.APIKey = strings.TrimSpace(fmt.Sprintf("%v", value))
			} else if value, ok := typed["apiKey"]; ok {
				entry.APIKey = strings.TrimSpace(fmt.Sprintf("%v", value))
			} else if value, ok := typed["key"]; ok {
				entry.APIKey = strings.TrimSpace(fmt.Sprintf("%v", value))
			}
			entry.Note = extractClientAPIKeyNote(typed)
			entry.Disabled = extractClientAPIKeyDisabled(typed)
			entry.AllowedModels = extractClientAPIKeyModels(typed, "allowed-models", "allowedModels")
			entry.ExcludedModels = extractClientAPIKeyModels(typed, "excluded-models", "excludedModels")
			entry.Quota = extractClientAPIKeyQuota(typed)
			parsed = append(parsed, entry)
		default:
			return fmt.Errorf("api-keys entries must be strings or objects")
		}
	}
	*keys = NormalizeClientAPIKeys(parsed)
	return nil
}

func extractClientAPIKeyDisabled(record map[string]any) bool {
	for _, name := range []string{"disabled", "disable", "isDisabled"} {
		raw, ok := record[name]
		if !ok {
			continue
		}
		value, parsed := parseClientAPIKeyBool(raw)
		return parsed && value
	}
	for _, name := range []string{"enabled", "enable", "isEnabled"} {
		raw, ok := record[name]
		if !ok {
			continue
		}
		value, parsed := parseClientAPIKeyBool(raw)
		return parsed && !value
	}
	return false
}

func parseClientAPIKeyBool(raw any) (bool, bool) {
	switch typed := raw.(type) {
	case bool:
		return typed, true
	case string:
		normalized := strings.ToLower(strings.TrimSpace(typed))
		switch normalized {
		case "yes", "y", "on":
			return true, true
		case "no", "n", "off":
			return false, true
		}
		parsed, err := strconv.ParseBool(normalized)
		return parsed, err == nil
	case int:
		return typed != 0, true
	case int8:
		return typed != 0, true
	case int16:
		return typed != 0, true
	case int32:
		return typed != 0, true
	case int64:
		return typed != 0, true
	case uint:
		return typed != 0, true
	case uint8:
		return typed != 0, true
	case uint16:
		return typed != 0, true
	case uint32:
		return typed != 0, true
	case uint64:
		return typed != 0, true
	case float32:
		return typed != 0, true
	case float64:
		return typed != 0, true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed != 0, err == nil
	default:
		return false, false
	}
}

func extractClientAPIKeyModels(record map[string]any, names ...string) []string {
	for _, name := range names {
		raw, ok := record[name]
		if !ok {
			continue
		}
		switch typed := raw.(type) {
		case []any:
			items := make([]string, 0, len(typed))
			for _, item := range typed {
				items = append(items, fmt.Sprintf("%v", item))
			}
			return NormalizeModelPatternList(items)
		case []string:
			return NormalizeModelPatternList(typed)
		case string:
			return NormalizeModelPatternList(strings.FieldsFunc(typed, func(r rune) bool {
				return r == ',' || r == '\n'
			}))
		}
	}
	return nil
}

func extractClientAPIKeyNote(record map[string]any) string {
	for _, name := range []string{"note", "remark", "description"} {
		raw, ok := record[name]
		if !ok {
			continue
		}
		return strings.TrimSpace(fmt.Sprintf("%v", raw))
	}
	return ""
}

func extractClientAPIKeyQuota(record map[string]any) ClientAPIKeyQuota {
	if len(record) == 0 {
		return ClientAPIKeyQuota{}
	}
	quota := ClientAPIKeyQuota{}
	if raw, ok := record["quota"]; ok {
		quota = mergeClientAPIKeyQuota(quota, parseClientAPIKeyQuota(raw))
	}
	quota = mergeClientAPIKeyQuota(quota, parseClientAPIKeyQuota(record))
	return NormalizeClientAPIKeyQuota(quota)
}

func parseClientAPIKeyQuota(raw any) ClientAPIKeyQuota {
	record, ok := raw.(map[string]any)
	if !ok || len(record) == 0 {
		return ClientAPIKeyQuota{}
	}
	return NormalizeClientAPIKeyQuota(ClientAPIKeyQuota{
		DailyCost: extractClientAPIKeyQuotaLimit(
			record,
			"daily-cost",
			"dailyCost",
			"daily-usd",
			"dailyUSD",
			"daily-spend",
			"dailySpend",
		),
		MonthlyCost: extractClientAPIKeyQuotaLimit(
			record,
			"monthly-cost",
			"monthlyCost",
			"monthly-usd",
			"monthlyUSD",
			"monthly-spend",
			"monthlySpend",
		),
		TotalCost: extractClientAPIKeyQuotaLimit(
			record,
			"total-cost",
			"totalCost",
			"total-usd",
			"totalUSD",
			"total-spend",
			"totalSpend",
		),
		DailyTokens: extractClientAPIKeyQuotaCount(
			record,
			"daily-tokens",
			"dailyTokens",
			"daily-token-limit",
			"dailyTokenLimit",
		),
		MonthlyTokens: extractClientAPIKeyQuotaCount(
			record,
			"monthly-tokens",
			"monthlyTokens",
			"monthly-token-limit",
			"monthlyTokenLimit",
		),
		TotalTokens: extractClientAPIKeyQuotaCount(
			record,
			"total-tokens",
			"totalTokens",
			"total-token-limit",
			"totalTokenLimit",
		),
		DailyRequests: extractClientAPIKeyQuotaCount(
			record,
			"daily-requests",
			"dailyRequests",
			"daily-request-limit",
			"dailyRequestLimit",
		),
		MonthlyRequests: extractClientAPIKeyQuotaCount(
			record,
			"monthly-requests",
			"monthlyRequests",
			"monthly-request-limit",
			"monthlyRequestLimit",
		),
		TotalRequests: extractClientAPIKeyQuotaCount(
			record,
			"total-requests",
			"totalRequests",
			"total-request-limit",
			"totalRequestLimit",
		),
	})
}

func extractClientAPIKeyQuotaLimit(record map[string]any, names ...string) float64 {
	for _, name := range names {
		raw, ok := record[name]
		if !ok {
			continue
		}
		return parseClientAPIKeyQuotaLimit(raw)
	}
	return 0
}

func extractClientAPIKeyQuotaCount(record map[string]any, names ...string) int64 {
	for _, name := range names {
		raw, ok := record[name]
		if !ok {
			continue
		}
		return parseClientAPIKeyQuotaCount(raw)
	}
	return 0
}

func parseClientAPIKeyQuotaLimit(raw any) float64 {
	switch typed := raw.(type) {
	case int:
		return normalizeClientAPIKeyQuotaLimit(float64(typed))
	case int8:
		return normalizeClientAPIKeyQuotaLimit(float64(typed))
	case int16:
		return normalizeClientAPIKeyQuotaLimit(float64(typed))
	case int32:
		return normalizeClientAPIKeyQuotaLimit(float64(typed))
	case int64:
		return normalizeClientAPIKeyQuotaLimit(float64(typed))
	case uint:
		return normalizeClientAPIKeyQuotaLimit(float64(typed))
	case uint8:
		return normalizeClientAPIKeyQuotaLimit(float64(typed))
	case uint16:
		return normalizeClientAPIKeyQuotaLimit(float64(typed))
	case uint32:
		return normalizeClientAPIKeyQuotaLimit(float64(typed))
	case uint64:
		return normalizeClientAPIKeyQuotaLimit(float64(typed))
	case float32:
		return normalizeClientAPIKeyQuotaLimit(float64(typed))
	case float64:
		return normalizeClientAPIKeyQuotaLimit(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0
		}
		return normalizeClientAPIKeyQuotaLimit(parsed)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0
		}
		return normalizeClientAPIKeyQuotaLimit(parsed)
	default:
		return 0
	}
}

func parseClientAPIKeyQuotaCount(raw any) int64 {
	var parsed int64
	switch typed := raw.(type) {
	case int:
		parsed = int64(typed)
	case int8:
		parsed = int64(typed)
	case int16:
		parsed = int64(typed)
	case int32:
		parsed = int64(typed)
	case int64:
		parsed = typed
	case uint:
		if uint64(typed) > uint64(math.MaxInt64) {
			return 0
		}
		parsed = int64(typed)
	case uint8:
		parsed = int64(typed)
	case uint16:
		parsed = int64(typed)
	case uint32:
		parsed = int64(typed)
	case uint64:
		if typed > uint64(math.MaxInt64) {
			return 0
		}
		parsed = int64(typed)
	case float32:
		return parseClientAPIKeyQuotaFloatCount(float64(typed))
	case float64:
		return parseClientAPIKeyQuotaFloatCount(typed)
	case json.Number:
		value, err := typed.Int64()
		if err != nil {
			return 0
		}
		parsed = value
	case string:
		value, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return 0
		}
		parsed = value
	default:
		return 0
	}
	return normalizeClientAPIKeyQuotaCount(parsed)
}

func parseClientAPIKeyQuotaFloatCount(value float64) int64 {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value || value >= float64(math.MaxInt64) {
		return 0
	}
	return int64(value)
}

func normalizeClientAPIKeyQuotaLimit(limit float64) float64 {
	if limit <= 0 || math.IsNaN(limit) || math.IsInf(limit, 0) {
		return 0
	}
	return limit
}

func normalizeClientAPIKeyQuotaCount(limit int64) int64 {
	if limit <= 0 {
		return 0
	}
	return limit
}

func normalizeClientAPIKeyEntry(entry ClientAPIKeyEntry) ClientAPIKeyEntry {
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	if isPlaceholderClientAPIKey(entry.APIKey) {
		return ClientAPIKeyEntry{}
	}
	entry.Note = strings.TrimSpace(entry.Note)
	entry.AllowedModels = NormalizeModelPatternList(entry.AllowedModels)
	entry.ExcludedModels = NormalizeModelPatternList(entry.ExcludedModels)
	entry.Quota = NormalizeClientAPIKeyQuota(entry.Quota)
	return entry
}

func isPlaceholderClientAPIKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	return normalized == "your-api-key" || strings.HasPrefix(normalized, "your-api-key-")
}

// NormalizeClientAPIKeys trims, deduplicates, and merges repeated api-key
// entries while preserving the order of first appearance. Known example
// placeholders are dropped so copied sample configs do not create real
// credentials.
func NormalizeClientAPIKeys(entries ClientAPIKeys) ClientAPIKeys {
	if len(entries) == 0 {
		return nil
	}
	out := make(ClientAPIKeys, 0, len(entries))
	indexByKey := make(map[string]int, len(entries))
	for _, raw := range entries {
		entry := normalizeClientAPIKeyEntry(raw)
		if entry.APIKey == "" {
			continue
		}
		key := entry.APIKey
		if index, exists := indexByKey[key]; exists {
			current := out[index]
			if current.Note == "" {
				current.Note = entry.Note
			}
			current.Disabled = current.Disabled || entry.Disabled
			current.AllowedModels = mergeModelPatternLists(current.AllowedModels, entry.AllowedModels)
			current.ExcludedModels = mergeModelPatternLists(current.ExcludedModels, entry.ExcludedModels)
			current.Quota = mergeClientAPIKeyQuota(current.Quota, entry.Quota)
			out[index] = current
			continue
		}
		indexByKey[key] = len(out)
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mergeModelPatternLists(base, extra []string) []string {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	return NormalizeModelPatternList(append(append([]string{}, base...), extra...))
}

func mergeClientAPIKeyQuota(base, extra ClientAPIKeyQuota) ClientAPIKeyQuota {
	base = NormalizeClientAPIKeyQuota(base)
	extra = NormalizeClientAPIKeyQuota(extra)
	return ClientAPIKeyQuota{
		DailyCost:       mergeClientAPIKeyQuotaLimit(base.DailyCost, extra.DailyCost),
		MonthlyCost:     mergeClientAPIKeyQuotaLimit(base.MonthlyCost, extra.MonthlyCost),
		TotalCost:       mergeClientAPIKeyQuotaLimit(base.TotalCost, extra.TotalCost),
		DailyTokens:     mergeClientAPIKeyQuotaCount(base.DailyTokens, extra.DailyTokens),
		MonthlyTokens:   mergeClientAPIKeyQuotaCount(base.MonthlyTokens, extra.MonthlyTokens),
		TotalTokens:     mergeClientAPIKeyQuotaCount(base.TotalTokens, extra.TotalTokens),
		DailyRequests:   mergeClientAPIKeyQuotaCount(base.DailyRequests, extra.DailyRequests),
		MonthlyRequests: mergeClientAPIKeyQuotaCount(base.MonthlyRequests, extra.MonthlyRequests),
		TotalRequests:   mergeClientAPIKeyQuotaCount(base.TotalRequests, extra.TotalRequests),
	}
}

func mergeClientAPIKeyQuotaLimit(base, extra float64) float64 {
	base = normalizeClientAPIKeyQuotaLimit(base)
	extra = normalizeClientAPIKeyQuotaLimit(extra)
	if base == 0 {
		return extra
	}
	if extra == 0 {
		return base
	}
	if extra < base {
		return extra
	}
	return base
}

func mergeClientAPIKeyQuotaCount(base, extra int64) int64 {
	base = normalizeClientAPIKeyQuotaCount(base)
	extra = normalizeClientAPIKeyQuotaCount(extra)
	if base == 0 {
		return extra
	}
	if extra == 0 || extra >= base {
		return base
	}
	return extra
}
