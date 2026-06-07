package management

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const authFileRuntimeStateJSONKey = "cliproxy_runtime_state"

func authFileRuntimeStateTimes(meta map[string]any) (time.Time, bool, time.Time, bool) {
	state, ok := authFileRuntimeState(meta)
	if !ok {
		return time.Time{}, false, time.Time{}, false
	}
	updatedOK := !state.UpdatedAt.IsZero()
	savedOK := !state.SavedAt.IsZero()
	return state.UpdatedAt.UTC(), updatedOK, state.SavedAt.UTC(), savedOK
}

func authFileRuntimeState(meta map[string]any) (coreauth.AuthRuntimeState, bool) {
	if len(meta) == 0 {
		return coreauth.AuthRuntimeState{}, false
	}
	raw, ok := meta[authFileRuntimeStateJSONKey]
	if !ok || raw == nil {
		return coreauth.AuthRuntimeState{}, false
	}
	var state coreauth.AuthRuntimeState
	switch v := raw.(type) {
	case coreauth.AuthRuntimeState:
		state = v
	case *coreauth.AuthRuntimeState:
		if v == nil {
			return coreauth.AuthRuntimeState{}, false
		}
		state = *v
	default:
		data, err := json.Marshal(raw)
		if err != nil {
			return coreauth.AuthRuntimeState{}, false
		}
		if err := json.Unmarshal(data, &state); err != nil {
			return coreauth.AuthRuntimeState{}, false
		}
	}
	if state.Status == "" && state.StatusMessage == "" && !state.Unavailable && state.LastError == nil &&
		state.Success == 0 && state.Failed == 0 && len(state.RecentRequests) == 0 && len(state.ModelStates) == 0 &&
		state.Quota.Reason == "" && !state.Quota.Exceeded && state.Quota.BackoffLevel == 0 && state.Quota.NextRecoverAt.IsZero() &&
		state.NextRetryAfter.IsZero() && state.UpdatedAt.IsZero() && state.SavedAt.IsZero() {
		return coreauth.AuthRuntimeState{}, false
	}
	return state, true
}

func serializeAuthError(err *coreauth.Error) gin.H {
	if err == nil {
		return nil
	}
	entry := gin.H{}
	if code := strings.TrimSpace(err.Code); code != "" {
		entry["code"] = code
	}
	if message := strings.TrimSpace(err.Message); message != "" {
		entry["message"] = message
	}
	entry["retryable"] = err.Retryable
	if err.HTTPStatus > 0 {
		entry["http_status"] = err.HTTPStatus
	}
	if len(entry) == 0 {
		return nil
	}
	return entry
}

func applyAuthFileRuntimeStateEntry(entry gin.H, state coreauth.AuthRuntimeState, overwrite bool) {
	if entry == nil {
		return
	}
	set := func(key string, value any) {
		if overwrite {
			entry[key] = value
			return
		}
		if _, exists := entry[key]; !exists {
			entry[key] = value
		}
	}
	if state.Status != "" {
		set("status", state.Status)
	}
	if state.StatusMessage != "" {
		set("status_message", state.StatusMessage)
	}
	if state.Unavailable {
		set("unavailable", true)
	}
	if serialized := serializeAuthError(state.LastError); serialized != nil {
		set("last_error", serialized)
	}
	if serialized := serializeModelStates(state.ModelStates); len(serialized) > 0 {
		set("model_states", serialized)
	}
	if state.Success != 0 {
		set("success", state.Success)
	}
	if state.Failed != 0 {
		set("failed", state.Failed)
	}
	if !state.NextRetryAfter.IsZero() {
		set("next_retry_after", state.NextRetryAfter.UTC())
	}
	if !state.UpdatedAt.IsZero() {
		set("runtime_updated_at", state.UpdatedAt.UTC())
	}
	if !state.SavedAt.IsZero() {
		set("runtime_saved_at", state.SavedAt.UTC())
	}
	if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 || state.Quota.AuthScope {
		set("quota", gin.H{
			"exceeded":        state.Quota.Exceeded,
			"reason":          state.Quota.Reason,
			"next_recover_at": state.Quota.NextRecoverAt,
			"backoff_level":   state.Quota.BackoffLevel,
			"auth_scope":      state.Quota.AuthScope,
		})
	}
}

func applyAuthFileRuntimeStateSummaryEntry(entry gin.H, state coreauth.AuthRuntimeState, overwrite bool) {
	if entry == nil {
		return
	}
	set := func(key string, value any) {
		if overwrite {
			entry[key] = value
			return
		}
		if _, exists := entry[key]; !exists {
			entry[key] = value
		}
	}
	if state.Status != "" {
		set("status", state.Status)
	}
	if state.StatusMessage != "" {
		set("status_message", state.StatusMessage)
	}
	if state.Unavailable {
		set("unavailable", true)
	}
	if state.Success != 0 {
		set("success", state.Success)
	}
	if state.Failed != 0 {
		set("failed", state.Failed)
	}
	if recent := authFileRecentRequestsFromRuntimeState(state, time.Now()); len(recent) > 0 {
		set("recent_requests", recent)
	}
	if !state.NextRetryAfter.IsZero() {
		set("next_retry_after", state.NextRetryAfter.UTC())
	}
	if !state.UpdatedAt.IsZero() {
		set("runtime_updated_at", state.UpdatedAt.UTC())
	}
	if !state.SavedAt.IsZero() {
		set("runtime_saved_at", state.SavedAt.UTC())
	}
}

func authFileRecentRequestsFromRuntimeState(state coreauth.AuthRuntimeState, now time.Time) []coreauth.RecentRequestBucket {
	const (
		bucketSeconds int64 = 10 * 60
		bucketCount         = 20
	)
	if len(state.RecentRequests) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	byBucket := make(map[int64]coreauth.RecentRequestState, len(state.RecentRequests))
	for _, item := range state.RecentRequests {
		if item.BucketID == 0 && item.Success == 0 && item.Failed == 0 {
			continue
		}
		byBucket[item.BucketID] = item
	}
	if len(byBucket) == 0 {
		return nil
	}
	currentBucketID := now.Unix() / bucketSeconds
	out := make([]coreauth.RecentRequestBucket, 0, bucketCount)
	for i := bucketCount - 1; i >= 0; i-- {
		bucketID := currentBucketID - int64(i)
		start := time.Unix(bucketID*bucketSeconds, 0).In(time.Local)
		end := start.Add(time.Duration(bucketSeconds) * time.Second)
		entry := coreauth.RecentRequestBucket{
			Time: start.Format("15:04") + "-" + end.Format("15:04"),
		}
		if item, ok := byBucket[bucketID]; ok {
			entry.Success = item.Success
			entry.Failed = item.Failed
		}
		out = append(out, entry)
	}
	return out
}

func serializeModelStates(states map[string]*coreauth.ModelState) map[string]gin.H {
	if len(states) == 0 {
		return nil
	}
	result := make(map[string]gin.H, len(states))
	for model, state := range states {
		model = strings.TrimSpace(model)
		if model == "" || state == nil {
			continue
		}
		entry := gin.H{
			"status":         state.Status,
			"status_message": state.StatusMessage,
			"unavailable":    state.Unavailable,
		}
		if !state.NextRetryAfter.IsZero() {
			entry["next_retry_after"] = state.NextRetryAfter
		}
		if serialized := serializeAuthError(state.LastError); serialized != nil {
			entry["last_error"] = serialized
		}
		if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 {
			entry["quota"] = gin.H{
				"exceeded":        state.Quota.Exceeded,
				"reason":          state.Quota.Reason,
				"next_recover_at": state.Quota.NextRecoverAt,
				"backoff_level":   state.Quota.BackoffLevel,
			}
		}
		if !state.UpdatedAt.IsZero() {
			entry["updated_at"] = state.UpdatedAt
		}
		result[model] = entry
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
