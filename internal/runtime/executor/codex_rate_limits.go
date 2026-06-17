package executor

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func codexPublishRateLimitsFromHeaders(ctx context.Context, auth *cliproxyauth.Auth, headers http.Header) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" || len(headers) == 0 {
		return
	}
	snapshots := codexRateLimitSnapshotsFromHeaders(headers)
	if len(snapshots) == 0 {
		return
	}
	cliproxyauth.PublishRateLimitUpdate(ctx, auth.ID, snapshots)
}

func codexPublishRateLimitsFromEvent(ctx context.Context, auth *cliproxyauth.Auth, payload []byte) bool {
	if auth == nil || strings.TrimSpace(auth.ID) == "" || len(payload) == 0 {
		return false
	}
	snapshot, ok := codexRateLimitSnapshotFromEvent(payload)
	if !ok {
		return false
	}
	cliproxyauth.PublishRateLimitUpdate(ctx, auth.ID, []cliproxyauth.RateLimitSnapshot{snapshot})
	return true
}

func codexPublishRateLimitsFromErrorBody(ctx context.Context, auth *cliproxyauth.Auth, body []byte) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" || len(body) == 0 {
		return
	}
	snapshots := codexRateLimitSnapshotsFromErrorBody(body)
	if len(snapshots) == 0 {
		return
	}
	cliproxyauth.PublishRateLimitUpdate(ctx, auth.ID, snapshots)
}

func codexRateLimitSnapshotsFromHeaders(headers http.Header) []cliproxyauth.RateLimitSnapshot {
	if len(headers) == 0 {
		return nil
	}
	var snapshots []cliproxyauth.RateLimitSnapshot
	if snapshot, ok := codexRateLimitSnapshotForLimit(headers, "codex"); ok {
		snapshots = append(snapshots, snapshot)
	}
	limitIDs := make(map[string]struct{})
	for key := range headers {
		if limitID := codexRateLimitHeaderLimitID(strings.ToLower(key)); limitID != "" && limitID != "codex" {
			limitIDs[limitID] = struct{}{}
		}
	}
	if len(limitIDs) == 0 {
		return snapshots
	}
	ordered := make([]string, 0, len(limitIDs))
	for limitID := range limitIDs {
		ordered = append(ordered, limitID)
	}
	sort.Strings(ordered)
	for _, limitID := range ordered {
		if snapshot, ok := codexRateLimitSnapshotForLimit(headers, limitID); ok {
			snapshots = append(snapshots, snapshot)
		}
	}
	return snapshots
}

func codexRateLimitSnapshotForLimit(headers http.Header, limitID string) (cliproxyauth.RateLimitSnapshot, bool) {
	limitID = codexNormalizeRateLimitID(limitID)
	if limitID == "" {
		limitID = "codex"
	}
	prefix := "x-" + strings.ReplaceAll(limitID, "_", "-")
	snapshot := cliproxyauth.RateLimitSnapshot{
		LimitID:   limitID,
		LimitName: strings.TrimSpace(headers.Get(prefix + "-limit-name")),
		Primary: codexRateLimitWindowFromHeaders(
			headers,
			prefix+"-primary-used-percent",
			prefix+"-primary-window-minutes",
			prefix+"-primary-reset-at",
		),
		Secondary: codexRateLimitWindowFromHeaders(
			headers,
			prefix+"-secondary-used-percent",
			prefix+"-secondary-window-minutes",
			prefix+"-secondary-reset-at",
		),
		Credits: codexCreditsSnapshotFromHeaders(headers),
	}
	if !codexRateLimitSnapshotHasData(snapshot) {
		return cliproxyauth.RateLimitSnapshot{}, false
	}
	return snapshot, true
}

func codexRateLimitWindowFromHeaders(headers http.Header, usedHeader, windowHeader, resetHeader string) *cliproxyauth.RateLimitWindow {
	used, ok := codexHeaderFloat(headers, usedHeader)
	if !ok {
		return nil
	}
	window := &cliproxyauth.RateLimitWindow{UsedPercent: used}
	if value, ok := codexHeaderInt64(headers, windowHeader); ok {
		window.WindowMinutes = &value
	}
	if value, ok := codexHeaderInt64(headers, resetHeader); ok {
		window.ResetsAt = &value
	}
	if window.UsedPercent == 0 && window.WindowMinutes == nil && window.ResetsAt == nil {
		return nil
	}
	return window
}

func codexCreditsSnapshotFromHeaders(headers http.Header) *cliproxyauth.CreditsSnapshot {
	hasCredits, okHasCredits := codexHeaderBool(headers, "x-codex-credits-has-credits")
	unlimited, okUnlimited := codexHeaderBool(headers, "x-codex-credits-unlimited")
	if !okHasCredits || !okUnlimited {
		return nil
	}
	return &cliproxyauth.CreditsSnapshot{
		HasCredits: hasCredits,
		Unlimited:  unlimited,
		Balance:    strings.TrimSpace(headers.Get("x-codex-credits-balance")),
	}
}

func codexRateLimitSnapshotFromEvent(payload []byte) (cliproxyauth.RateLimitSnapshot, bool) {
	root := gjson.ParseBytes(payload)
	if strings.TrimSpace(root.Get("type").String()) != "codex.rate_limits" {
		return cliproxyauth.RateLimitSnapshot{}, false
	}
	limitID := firstNonEmptyString(root.Get("metered_limit_name").String(), root.Get("limit_name").String(), "codex")
	snapshot := cliproxyauth.RateLimitSnapshot{
		LimitID:   codexNormalizeRateLimitID(limitID),
		Primary:   codexRateLimitWindowFromResult(root.Get("rate_limits.primary")),
		Secondary: codexRateLimitWindowFromResult(root.Get("rate_limits.secondary")),
		PlanType:  strings.TrimSpace(root.Get("plan_type").String()),
	}
	if credits := root.Get("credits"); credits.IsObject() {
		snapshot.Credits = &cliproxyauth.CreditsSnapshot{
			HasCredits: credits.Get("has_credits").Bool(),
			Unlimited:  credits.Get("unlimited").Bool(),
			Balance:    strings.TrimSpace(credits.Get("balance").String()),
		}
	}
	if !codexRateLimitSnapshotHasData(snapshot) {
		return cliproxyauth.RateLimitSnapshot{}, false
	}
	return snapshot, true
}

func codexRateLimitSnapshotsFromErrorBody(body []byte) []cliproxyauth.RateLimitSnapshot {
	root := gjson.ParseBytes(body)
	for _, path := range []string{"rate_limits", "error.rate_limits"} {
		value := root.Get(path)
		if !value.Exists() {
			continue
		}
		if value.IsArray() {
			var snapshots []cliproxyauth.RateLimitSnapshot
			value.ForEach(func(_, item gjson.Result) bool {
				if snapshot, ok := codexRateLimitSnapshotFromResult(item); ok {
					snapshots = append(snapshots, snapshot)
				}
				return true
			})
			return snapshots
		}
		if snapshot, ok := codexRateLimitSnapshotFromResult(value); ok {
			return []cliproxyauth.RateLimitSnapshot{snapshot}
		}
	}
	return nil
}

func codexRateLimitSnapshotFromResult(value gjson.Result) (cliproxyauth.RateLimitSnapshot, bool) {
	if !value.IsObject() {
		return cliproxyauth.RateLimitSnapshot{}, false
	}
	snapshot := cliproxyauth.RateLimitSnapshot{
		LimitID:              codexNormalizeRateLimitID(firstNonEmptyString(value.Get("limit_id").String(), value.Get("limitId").String(), "codex")),
		LimitName:            firstNonEmptyString(value.Get("limit_name").String(), value.Get("limitName").String()),
		Primary:              codexRateLimitWindowFromResult(value.Get("primary")),
		Secondary:            codexRateLimitWindowFromResult(value.Get("secondary")),
		PlanType:             firstNonEmptyString(value.Get("plan_type").String(), value.Get("planType").String()),
		RateLimitReachedType: firstNonEmptyString(value.Get("rate_limit_reached_type").String(), value.Get("rateLimitReachedType").String()),
	}
	if credits := value.Get("credits"); credits.IsObject() {
		snapshot.Credits = &cliproxyauth.CreditsSnapshot{
			HasCredits: credits.Get("has_credits").Bool() || credits.Get("hasCredits").Bool(),
			Unlimited:  credits.Get("unlimited").Bool(),
			Balance:    strings.TrimSpace(credits.Get("balance").String()),
		}
	}
	if !codexRateLimitSnapshotHasData(snapshot) {
		return cliproxyauth.RateLimitSnapshot{}, false
	}
	if updatedAt := firstNonEmptyString(value.Get("updated_at").String(), value.Get("updatedAt").String()); updatedAt != "" {
		if ts, err := time.Parse(time.RFC3339, updatedAt); err == nil {
			snapshot.UpdatedAt = ts
		}
	}
	return snapshot, true
}

func codexRateLimitWindowFromResult(value gjson.Result) *cliproxyauth.RateLimitWindow {
	if !value.IsObject() {
		return nil
	}
	usedResult := firstExistingResult(value.Get("used_percent"), value.Get("usedPercent"))
	if !usedResult.Exists() {
		return nil
	}
	used := usedResult.Float()
	if math.IsNaN(used) || math.IsInf(used, 0) {
		return nil
	}
	window := &cliproxyauth.RateLimitWindow{UsedPercent: used}
	if minutes := firstExistingResult(value.Get("window_minutes"), value.Get("windowMinutes"), value.Get("window_duration_mins"), value.Get("windowDurationMins")); minutes.Exists() {
		v := minutes.Int()
		window.WindowMinutes = &v
	}
	if resetsAt := firstExistingResult(value.Get("reset_at"), value.Get("resetAt"), value.Get("resets_at"), value.Get("resetsAt")); resetsAt.Exists() {
		v := resetsAt.Int()
		window.ResetsAt = &v
	}
	return window
}

func codexRateLimitHeaderLimitID(headerName string) string {
	const suffix = "-primary-used-percent"
	prefix, ok := strings.CutSuffix(headerName, suffix)
	if !ok {
		return ""
	}
	limit, ok := strings.CutPrefix(prefix, "x-")
	if !ok {
		return ""
	}
	return codexNormalizeRateLimitID(limit)
}

func codexNormalizeRateLimitID(value string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "-", "_")
}

func codexRateLimitSnapshotHasData(snapshot cliproxyauth.RateLimitSnapshot) bool {
	return snapshot.Primary != nil ||
		snapshot.Secondary != nil ||
		snapshot.Credits != nil ||
		strings.TrimSpace(snapshot.LimitName) != "" ||
		strings.TrimSpace(snapshot.PlanType) != "" ||
		strings.TrimSpace(snapshot.RateLimitReachedType) != ""
}

func codexHeaderFloat(headers http.Header, name string) (float64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(headers.Get(name)), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

func codexHeaderInt64(headers http.Header, name string) (int64, bool) {
	value, err := strconv.ParseInt(strings.TrimSpace(headers.Get(name)), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func codexHeaderBool(headers http.Header, name string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(headers.Get(name))) {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default:
		return false, false
	}
}

func firstExistingResult(values ...gjson.Result) gjson.Result {
	for _, value := range values {
		if value.Exists() {
			return value
		}
	}
	return gjson.Result{}
}
