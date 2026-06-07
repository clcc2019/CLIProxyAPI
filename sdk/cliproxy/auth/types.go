package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	baseauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

// PostAuthHook defines a function that is called after an Auth record is created
// but before it is persisted to storage. This allows for modification of the
// Auth record (e.g., injecting metadata) based on external context.
type PostAuthHook func(context.Context, *Auth) error

// RequestInfo holds information extracted from the HTTP request.
// It is injected into the context passed to PostAuthHook.
type RequestInfo struct {
	Query   url.Values
	Headers http.Header
}

type requestInfoKey struct{}

// WithRequestInfo returns a new context with the given RequestInfo attached.
func WithRequestInfo(ctx context.Context, info *RequestInfo) context.Context {
	return context.WithValue(ctx, requestInfoKey{}, info)
}

// GetRequestInfo retrieves the RequestInfo from the context, if present.
func GetRequestInfo(ctx context.Context) *RequestInfo {
	if val, ok := ctx.Value(requestInfoKey{}).(*RequestInfo); ok {
		return val
	}
	return nil
}

// Auth encapsulates the runtime state and metadata associated with a single credential.
type Auth struct {
	// ID uniquely identifies the auth record across restarts.
	ID string `json:"id"`
	// Index is a stable runtime identifier derived from auth metadata (not persisted).
	Index string `json:"-"`
	// Provider is the upstream provider key (e.g. "claude", "codex").
	Provider string `json:"provider"`
	// Prefix optionally namespaces models for routing (e.g., "teamA/claude-sonnet-4-5").
	Prefix string `json:"prefix,omitempty"`
	// FileName stores the relative or absolute path of the backing auth file.
	FileName string `json:"-"`
	// Storage holds the token persistence implementation used during login flows.
	Storage baseauth.TokenStorage `json:"-"`
	// Label is an optional human readable label for logging.
	Label string `json:"label,omitempty"`
	// Status is the lifecycle status managed by the AuthManager.
	Status Status `json:"status"`
	// StatusMessage holds a short description for the current status.
	StatusMessage string `json:"status_message,omitempty"`
	// Disabled indicates the auth is intentionally disabled by operator.
	Disabled bool `json:"disabled"`
	// Unavailable flags transient provider unavailability (e.g. quota exceeded).
	Unavailable bool `json:"unavailable"`
	// ProxyURL overrides the global proxy setting for this auth if provided.
	ProxyURL string `json:"proxy_url,omitempty"`
	// Attributes stores provider specific metadata needed by executors (immutable configuration).
	Attributes map[string]string `json:"attributes,omitempty"`
	// Metadata stores runtime mutable provider state (e.g. tokens, cookies).
	Metadata map[string]any `json:"metadata,omitempty"`
	// Quota captures recent quota information for load balancers.
	Quota QuotaState `json:"quota"`
	// LastError stores the last failure encountered while executing or refreshing.
	LastError *Error `json:"last_error,omitempty"`
	// CreatedAt is the creation timestamp in UTC.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is the last modification timestamp in UTC.
	UpdatedAt time.Time `json:"updated_at"`
	// LastRefreshedAt records the last successful refresh time in UTC.
	LastRefreshedAt time.Time `json:"last_refreshed_at"`
	// NextRefreshAfter is the earliest time a refresh should retrigger.
	NextRefreshAfter time.Time `json:"next_refresh_after"`
	// NextRetryAfter is the earliest time a retry should retrigger.
	NextRetryAfter time.Time `json:"next_retry_after"`
	// ModelStates tracks per-model runtime availability data.
	ModelStates map[string]*ModelState `json:"model_states,omitempty"`

	// Runtime carries non-serialisable data used during execution (in-memory only).
	Runtime any `json:"-"`

	Success int64 `json:"-"`
	Failed  int64 `json:"-"`

	recentRequests *recentRequestRing `json:"-"`
	indexAssigned  bool               `json:"-"`
	runtimeMu      unsafe.Pointer     `json:"-"`
}

const (
	recentRequestBucketSeconds int64 = 10 * 60
	recentRequestBucketCount         = 20
)

type recentRequestBucket struct {
	bucketID int64
	success  int64
	failed   int64
}

type recentRequestRing struct {
	buckets [recentRequestBucketCount]recentRequestBucket
}

type RecentRequestBucket struct {
	Time    string `json:"time"`
	Success int64  `json:"success"`
	Failed  int64  `json:"failed"`
}

type RecentRequestState struct {
	BucketID int64 `json:"bucket_id"`
	Success  int64 `json:"success"`
	Failed   int64 `json:"failed"`
}

type AuthRuntimeState struct {
	Version        int                    `json:"version"`
	SavedAt        time.Time              `json:"saved_at"`
	Success        int64                  `json:"success"`
	Failed         int64                  `json:"failed"`
	RecentRequests []RecentRequestState   `json:"recent_requests,omitempty"`
	Status         Status                 `json:"status,omitempty"`
	StatusMessage  string                 `json:"status_message,omitempty"`
	Unavailable    bool                   `json:"unavailable,omitempty"`
	Quota          QuotaState             `json:"quota"`
	LastError      *Error                 `json:"last_error,omitempty"`
	NextRetryAfter time.Time              `json:"next_retry_after,omitempty"`
	ModelStates    map[string]*ModelState `json:"model_states,omitempty"`
	UpdatedAt      time.Time              `json:"updated_at,omitempty"`
}

const runtimeStateMetadataKey = "cliproxy_runtime_state"

// QuotaState contains limiter tracking data for a credential.
type QuotaState struct {
	// Exceeded indicates the credential recently hit a quota error.
	Exceeded bool `json:"exceeded"`
	// Reason provides an optional provider specific human readable description.
	Reason string `json:"reason,omitempty"`
	// NextRecoverAt is when the credential may become available again.
	NextRecoverAt time.Time `json:"next_recover_at"`
	// BackoffLevel is retained for backward compatibility with older persisted
	// runtime metadata; new quota cooldowns use NextRecoverAt directly.
	BackoffLevel int `json:"backoff_level,omitempty"`
	// AuthScope is set on the auth-level QuotaState (not per-model) when the
	// failure that tripped the quota was auth-scoped — e.g. an upstream shared
	// AGENTIC_REQUEST bucket. It tells the selector that every model on this
	// credential is exhausted, not just the model that triggered the 429,
	// so session affinity can move to the next auth instead of scattering
	// across the depleted one by model.
	AuthScope bool `json:"auth_scope,omitempty"`
}

// ModelState captures the execution state for a specific model under an auth entry.
type ModelState struct {
	// Status reflects the lifecycle status for this model.
	Status Status `json:"status"`
	// StatusMessage provides an optional short description of the status.
	StatusMessage string `json:"status_message,omitempty"`
	// Unavailable mirrors whether the model is temporarily blocked for retries.
	Unavailable bool `json:"unavailable"`
	// NextRetryAfter defines the per-model retry time.
	NextRetryAfter time.Time `json:"next_retry_after"`
	// LastError records the latest error observed for this model.
	LastError *Error `json:"last_error,omitempty"`
	// Quota retains quota information if this model hit rate limits.
	Quota QuotaState `json:"quota"`
	// UpdatedAt tracks the last update timestamp for this model state.
	UpdatedAt time.Time `json:"updated_at"`
}

func recentRequestBucketID(now time.Time) int64 {
	if now.IsZero() {
		return 0
	}
	return now.Unix() / recentRequestBucketSeconds
}

func recentRequestBucketIndex(bucketID int64) int {
	mod := bucketID % int64(recentRequestBucketCount)
	if mod < 0 {
		mod += int64(recentRequestBucketCount)
	}
	return int(mod)
}

func formatRecentRequestBucketLabel(bucketID int64) string {
	start := time.Unix(bucketID*recentRequestBucketSeconds, 0).In(time.Local)
	end := start.Add(time.Duration(recentRequestBucketSeconds) * time.Second)
	return start.Format("15:04") + "-" + end.Format("15:04")
}

func (a *Auth) recordRecentRequest(now time.Time, success bool) {
	if a == nil {
		return
	}
	mu := a.ensureRuntimeMu()
	mu.Lock()
	defer mu.Unlock()
	a.recordRecentRequestLocked(now, success)
}

func (a *Auth) recordRecentRequestLocked(now time.Time, success bool) {
	if a == nil {
		return
	}
	a.ensureRecentRequests()
	bucketID := recentRequestBucketID(now)
	idx := recentRequestBucketIndex(bucketID)
	bucket := &a.recentRequests.buckets[idx]
	if bucket.bucketID != bucketID {
		bucket.bucketID = bucketID
		bucket.success = 0
		bucket.failed = 0
	}
	if success {
		bucket.success++
		return
	}
	bucket.failed++
}

func (a *Auth) recordRuntimeResult(now time.Time, success bool) {
	if a == nil {
		return
	}
	mu := a.ensureRuntimeMu()
	mu.Lock()
	defer mu.Unlock()
	a.recordRecentRequestLocked(now, success)
	if success {
		a.Success++
		return
	}
	a.Failed++
}

func (a *Auth) RecentRequestsSnapshot(now time.Time) []RecentRequestBucket {
	out := make([]RecentRequestBucket, 0, recentRequestBucketCount)
	if a == nil {
		return out
	}
	mu := a.ensureRuntimeMu()
	mu.Lock()
	defer mu.Unlock()

	currentBucketID := recentRequestBucketID(now)
	for i := recentRequestBucketCount - 1; i >= 0; i-- {
		bucketID := currentBucketID - int64(i)
		idx := recentRequestBucketIndex(bucketID)
		var bucket recentRequestBucket
		if a.recentRequests != nil {
			bucket = a.recentRequests.buckets[idx]
		}
		entry := RecentRequestBucket{
			Time: formatRecentRequestBucketLabel(bucketID),
		}
		if bucket.bucketID == bucketID {
			entry.Success = bucket.success
			entry.Failed = bucket.failed
		}
		out = append(out, entry)
	}

	return out
}

func (a *Auth) RuntimeStateSnapshot() AuthRuntimeState {
	if a == nil {
		return AuthRuntimeState{}
	}
	mu := a.ensureRuntimeMu()
	mu.Lock()
	defer mu.Unlock()
	return AuthRuntimeState{
		Version:        1,
		SavedAt:        time.Now().UTC(),
		Success:        a.Success,
		Failed:         a.Failed,
		RecentRequests: a.recentRequestState(),
		Status:         a.Status,
		StatusMessage:  a.StatusMessage,
		Unavailable:    a.Unavailable,
		Quota:          a.Quota,
		LastError:      cloneError(a.LastError),
		NextRetryAfter: a.NextRetryAfter,
		ModelStates:    cloneModelStates(a.ModelStates),
		UpdatedAt:      a.UpdatedAt,
	}
}

func (a *Auth) SetRuntimeStateMetadata() {
	if a == nil {
		return
	}
	state := a.RuntimeStateSnapshot()
	if !runtimeStateHasPersistentContent(state) {
		if a.Metadata != nil {
			delete(a.Metadata, runtimeStateMetadataKey)
		}
		return
	}
	if a.Metadata == nil {
		a.Metadata = make(map[string]any)
	}
	a.Metadata[runtimeStateMetadataKey] = state
}

func (a *Auth) ApplyRuntimeStateFromMetadata() bool {
	state, ok := a.runtimeStateFromMetadata()
	if !ok {
		return false
	}
	a.ApplyRuntimeState(state)
	return true
}

func (a *Auth) runtimeStateFromMetadata() (AuthRuntimeState, bool) {
	if a == nil || len(a.Metadata) == 0 {
		return AuthRuntimeState{}, false
	}
	raw, ok := a.Metadata[runtimeStateMetadataKey]
	if !ok || raw == nil {
		return AuthRuntimeState{}, false
	}
	switch state := raw.(type) {
	case AuthRuntimeState:
		return state, runtimeStateHasPersistentContent(state)
	case *AuthRuntimeState:
		if state == nil {
			return AuthRuntimeState{}, false
		}
		return *state, runtimeStateHasPersistentContent(*state)
	default:
		data, err := json.Marshal(raw)
		if err != nil {
			return AuthRuntimeState{}, false
		}
		var decoded AuthRuntimeState
		if err := json.Unmarshal(data, &decoded); err != nil {
			return AuthRuntimeState{}, false
		}
		return decoded, runtimeStateHasPersistentContent(decoded)
	}
}

func runtimeStateHasPersistentContent(state AuthRuntimeState) bool {
	if state.Success != 0 || state.Failed != 0 || len(state.RecentRequests) > 0 {
		return true
	}
	if state.Status != "" && state.Status != StatusActive {
		return true
	}
	if state.StatusMessage != "" || state.Unavailable || state.LastError != nil || !state.NextRetryAfter.IsZero() {
		return true
	}
	if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 {
		return true
	}
	return len(state.ModelStates) > 0
}

func (a *Auth) ApplyRuntimeState(state AuthRuntimeState) {
	if a == nil {
		return
	}
	mu := a.ensureRuntimeMu()
	mu.Lock()
	defer mu.Unlock()
	a.Success = state.Success
	a.Failed = state.Failed
	a.applyRecentRequestState(state.RecentRequests)

	if a.Disabled || a.Status == StatusDisabled {
		return
	}
	if state.Status != "" && state.Status != StatusDisabled {
		a.Status = state.Status
	}
	a.StatusMessage = state.StatusMessage
	a.Unavailable = state.Unavailable
	a.Quota = state.Quota
	a.LastError = cloneError(state.LastError)
	a.NextRetryAfter = state.NextRetryAfter
	if state.Version > 0 {
		a.ModelStates = cloneModelStates(state.ModelStates)
	} else if len(state.ModelStates) > 0 {
		a.ModelStates = cloneModelStates(state.ModelStates)
	}
	if !state.UpdatedAt.IsZero() && state.UpdatedAt.After(a.UpdatedAt) {
		a.UpdatedAt = state.UpdatedAt
	}
}

func (a *Auth) recentRequestState() []RecentRequestState {
	if a == nil || a.recentRequests == nil {
		return nil
	}
	out := make([]RecentRequestState, 0, recentRequestBucketCount)
	for _, bucket := range a.recentRequests.buckets {
		if bucket.bucketID == 0 && bucket.success == 0 && bucket.failed == 0 {
			continue
		}
		out = append(out, RecentRequestState{
			BucketID: bucket.bucketID,
			Success:  bucket.success,
			Failed:   bucket.failed,
		})
	}
	return out
}

func (a *Auth) applyRecentRequestState(items []RecentRequestState) {
	if a == nil {
		return
	}
	if len(items) == 0 {
		a.recentRequests = nil
		return
	}
	a.recentRequests = &recentRequestRing{}
	for _, item := range items {
		if item.BucketID == 0 && item.Success == 0 && item.Failed == 0 {
			continue
		}
		idx := recentRequestBucketIndex(item.BucketID)
		a.recentRequests.buckets[idx] = recentRequestBucket{
			bucketID: item.BucketID,
			success:  item.Success,
			failed:   item.Failed,
		}
	}
}

func (a *Auth) ensureRecentRequests() {
	if a != nil && a.recentRequests == nil {
		a.recentRequests = &recentRequestRing{}
	}
}

func (a *Auth) ensureRuntimeMu() *sync.Mutex {
	if a == nil {
		return nil
	}
	if ptr := atomic.LoadPointer(&a.runtimeMu); ptr != nil {
		return (*sync.Mutex)(ptr)
	}
	mu := &sync.Mutex{}
	if atomic.CompareAndSwapPointer(&a.runtimeMu, nil, unsafe.Pointer(mu)) {
		return mu
	}
	return (*sync.Mutex)(atomic.LoadPointer(&a.runtimeMu))
}

func cloneRecentRequestRing(src *recentRequestRing) *recentRequestRing {
	if src == nil {
		return nil
	}
	copyRing := *src
	return &copyRing
}

// Clone shallow copies the Auth structure, duplicating maps to avoid accidental mutation.
func (a *Auth) Clone() *Auth {
	if a == nil {
		return nil
	}
	mu := a.ensureRuntimeMu()
	mu.Lock()
	defer mu.Unlock()
	copyAuth := *a
	copyAuth.recentRequests = cloneRecentRequestRing(a.recentRequests)
	copyAuth.runtimeMu = nil
	copyAuth.Attributes = cloneStringMap(a.Attributes)
	copyAuth.Metadata = cloneAnyMap(a.Metadata)
	copyAuth.ModelStates = cloneModelStates(a.ModelStates)
	copyAuth.Runtime = a.Runtime
	return &copyAuth
}

// CloneShallow copies the Auth struct while reusing nested maps and model state
// references. It is only safe for read-only snapshots.
func (a *Auth) CloneShallow() *Auth {
	if a == nil {
		return nil
	}
	mu := a.ensureRuntimeMu()
	mu.Lock()
	defer mu.Unlock()
	copyAuth := *a
	copyAuth.recentRequests = nil
	copyAuth.runtimeMu = nil
	copyAuth.Runtime = a.Runtime
	return &copyAuth
}

// CloneForManagementSummary copies only the fields needed by management list
// summaries. Full auth metadata can contain large tokens, so summary endpoints
// avoid cloning it for every row while still returning independent maps.
func (a *Auth) CloneForManagementSummary() *Auth {
	if a == nil {
		return nil
	}
	mu := a.ensureRuntimeMu()
	mu.Lock()
	defer mu.Unlock()

	copyAuth := a.cloneSnapshotBase()
	copyAuth.StatusMessage = a.StatusMessage
	copyAuth.Success = a.Success
	copyAuth.Failed = a.Failed
	copyAuth.recentRequests = cloneRecentRequestRing(a.recentRequests)
	copyAuth.Attributes = cloneAuthAttributesForManagementSummary(a.Attributes)
	copyAuth.Metadata = cloneAuthMetadataForManagementSummary(a.Metadata)
	return &copyAuth
}

func (a *Auth) cloneSnapshotBase() Auth {
	return Auth{
		ID:               a.ID,
		Index:            a.Index,
		Provider:         a.Provider,
		Prefix:           a.Prefix,
		FileName:         a.FileName,
		Label:            a.Label,
		Status:           a.Status,
		Disabled:         a.Disabled,
		Unavailable:      a.Unavailable,
		ProxyURL:         a.ProxyURL,
		Quota:            a.Quota,
		CreatedAt:        a.CreatedAt,
		UpdatedAt:        a.UpdatedAt,
		LastRefreshedAt:  a.LastRefreshedAt,
		NextRefreshAfter: a.NextRefreshAfter,
		NextRetryAfter:   a.NextRetryAfter,
		indexAssigned:    a.indexAssigned,
	}
}

func (a *Auth) cloneForExecution() *Auth {
	if a == nil {
		return nil
	}
	copyAuth := a.cloneSnapshotBase()
	copyAuth.Storage = a.Storage
	copyAuth.StatusMessage = a.StatusMessage
	copyAuth.LastError = a.LastError
	copyAuth.Runtime = a.Runtime
	copyAuth.Attributes = cloneStringMap(a.Attributes)
	copyAuth.Metadata = cloneAnyMap(a.Metadata)
	copyAuth.ModelStates = cloneModelStates(a.ModelStates)
	return &copyAuth
}

// CloneForScheduler copies only the fields the scheduler needs to retain.
// Most providers keep a narrowed routing/cooldown snapshot, while codex keeps
// full Attributes/Metadata because scheduler snapshots also back its execution
// fast path.
func (a *Auth) CloneForScheduler() *Auth {
	if a == nil {
		return nil
	}
	copyAuth := a.cloneSnapshotBase()
	if strings.EqualFold(strings.TrimSpace(a.Provider), "codex") {
		copyAuth.Attributes = cloneStringMap(a.Attributes)
		copyAuth.Metadata = cloneAnyMap(a.Metadata)
	} else {
		copyAuth.Attributes = cloneAuthAttributesForScheduler(a.Attributes)
		copyAuth.Metadata = cloneAuthMetadataForScheduler(a.Metadata)
	}
	copyAuth.ModelStates = cloneModelStatesForScheduler(a.ModelStates)
	return &copyAuth
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func cloneAnyMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func cloneModelStates(src map[string]*ModelState) map[string]*ModelState {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]*ModelState, len(src))
	for key, state := range src {
		dst[key] = state.Clone()
	}
	return dst
}

func cloneModelStatesForScheduler(src map[string]*ModelState) map[string]*ModelState {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]*ModelState, len(src))
	for key, state := range src {
		dst[key] = state.CloneForScheduler()
	}
	return dst
}

func cloneAuthAttributesForScheduler(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	var dst map[string]string
	add := func(key string) {
		value := src[key]
		if value == "" {
			return
		}
		if dst == nil {
			dst = make(map[string]string, 5)
		}
		dst[key] = value
	}
	add("priority")
	add("websockets")
	add("compat_name")
	add("provider_key")
	return dst
}

func cloneAuthAttributesForManagementSummary(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, 16)
	for _, key := range []string{
		"path",
		"runtime_only",
		"email",
		"account_email",
		"project_id",
		"projectId",
		"refresh_token",
		"refreshToken",
		"plan_type",
		"planType",
		"chatgpt_plan_type",
		"chatgptPlanType",
		"last_refresh",
		"lastRefresh",
		"last_refreshed_at",
		"lastRefreshedAt",
		"priority",
		"note",
		"header:User-Agent",
		"user_agent",
		"user-agent",
		AuthFileCodexOriginatorKey,
		"header:" + AuthFileCodexOriginatorHeader,
		"header:" + AuthFileCodexBetaFeaturesHeader,
		"header:" + AuthFileCodexInstallationIDHeader,
		"header:" + AuthFileCodexIncludeTimingMetricsHeader,
		"websockets",
		"api_key",
	} {
		if value := src[key]; value != "" {
			dst[key] = value
		}
	}
	if len(dst) == 0 {
		return nil
	}
	return dst
}

func cloneAuthMetadataForScheduler(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	value, ok := src["websockets"]
	if !ok {
		return nil
	}
	return map[string]any{"websockets": value}
}

func cloneAuthMetadataForManagementSummary(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]any, 24)
	copyKeys := []string{
		"email",
		"project_id",
		"projectId",
		"refresh_token",
		"refreshToken",
		"plan_type",
		"planType",
		"chatgpt_plan_type",
		"chatgptPlanType",
		"subscription_expires_at",
		"subscriptionExpiresAt",
		"chatgpt_subscription_active_until",
		"chatgptSubscriptionActiveUntil",
		"subscription_active_until",
		"subscriptionActiveUntil",
		"subscription_active_days",
		"subscriptionActiveDays",
		"chatgpt_subscription_active_start",
		"chatgptSubscriptionActiveStart",
		"subscription_active_start",
		"subscriptionActiveStart",
		"subscription_started_at",
		"subscriptionStartedAt",
		"subscription_start_date",
		"subscriptionStartDate",
		"current_period_start",
		"currentPeriodStart",
		"period_start",
		"periodStart",
		"started_at",
		"startedAt",
		"starts_at",
		"startsAt",
		"prefix",
		"proxy_url",
		"proxy-url",
		"proxyUrl",
		"priority",
		"note",
		"user_agent",
		"user-agent",
		AuthFileCodexOriginatorKey,
		AuthFileCodexOriginatorHeader,
		AuthFileCodexBetaFeaturesKey,
		"beta-features",
		"betaFeatures",
		AuthFileCodexInstallationIDKey,
		"installation-id",
		"installationId",
		AuthFileCodexIncludeTimingMetricsKey,
		"include-timing-metrics",
		"includeTimingMetrics",
		"websockets",
		"websocket",
		"disable_cooling",
		"disable-cooling",
		"last_refresh",
		"lastRefresh",
		"last_refreshed_at",
		"lastRefreshedAt",
		"disabled",
		runtimeStateMetadataKey,
	}
	for _, key := range copyKeys {
		copyManagementSummaryMetadataValue(dst, src, key)
	}

	nestedSubscriptionKeys := []string{
		"subscription_expires_at",
		"subscriptionExpiresAt",
		"chatgpt_subscription_active_until",
		"chatgptSubscriptionActiveUntil",
		"subscription_active_until",
		"subscriptionActiveUntil",
		"expires_at",
		"expiresAt",
		"current_period_end",
		"currentPeriodEnd",
		"period_end",
		"periodEnd",
		"subscription_active_days",
		"subscriptionActiveDays",
		"chatgpt_subscription_active_start",
		"chatgptSubscriptionActiveStart",
		"subscription_active_start",
		"subscriptionActiveStart",
		"subscription_started_at",
		"subscriptionStartedAt",
		"subscription_start_date",
		"subscriptionStartDate",
		"current_period_start",
		"currentPeriodStart",
		"period_start",
		"periodStart",
		"started_at",
		"startedAt",
		"starts_at",
		"startsAt",
	}
	for _, containerKey := range []string{"account", "entitlement", "subscription", "providerSpecificData"} {
		if nested := cloneSelectedManagementSummaryNestedMap(src[containerKey], nestedSubscriptionKeys); len(nested) > 0 {
			dst[containerKey] = nested
		}
	}

	for _, containerKey := range []string{"token", "tokens", "token_data", "tokenData"} {
		if nested := cloneSelectedManagementSummaryNestedMap(src[containerKey], []string{"refresh_token", "refreshToken"}); len(nested) > 0 {
			dst[containerKey] = nested
		}
	}

	if len(dst) == 0 {
		return nil
	}
	return dst
}

func copyManagementSummaryMetadataValue(dst map[string]any, src map[string]any, key string) {
	value, ok := src[key]
	if !ok || value == nil {
		return
	}
	dst[key] = cloneManagementSummaryMetadataValue(value)
}

func cloneManagementSummaryMetadataValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		copyMap := make(map[string]any, len(typed))
		for key, nested := range typed {
			copyMap[key] = nested
		}
		return copyMap
	case map[string]string:
		copyMap := make(map[string]string, len(typed))
		for key, nested := range typed {
			copyMap[key] = nested
		}
		return copyMap
	default:
		return value
	}
}

func cloneSelectedManagementSummaryNestedMap(value any, keys []string) map[string]any {
	if value == nil {
		return nil
	}
	dst := make(map[string]any, len(keys))
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range keys {
			if nested, ok := typed[key]; ok && nested != nil {
				dst[key] = cloneManagementSummaryMetadataValue(nested)
			}
		}
	case map[string]string:
		for _, key := range keys {
			if nested := strings.TrimSpace(typed[key]); nested != "" {
				dst[key] = nested
			}
		}
	}
	if len(dst) == 0 {
		return nil
	}
	return dst
}

func stableAuthIndex(seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:8])
}

func (a *Auth) indexSeed() string {
	if a == nil {
		return ""
	}

	provider := strings.ToLower(strings.TrimSpace(a.Provider))
	compatName := ""
	baseURL := ""
	apiKey := ""
	filePath := ""
	if a.Attributes != nil {
		compatName = strings.TrimSpace(a.Attributes["compat_name"])
		baseURL = strings.TrimSpace(a.Attributes["base_url"])
		apiKey = strings.TrimSpace(a.Attributes["api_key"])
		filePath = strings.TrimSpace(a.Attributes["path"])
		if filePath == "" {
			filePath = strings.TrimSpace(a.Attributes["source"])
		}
	}

	if filePath == "" {
		filePath = strings.TrimSpace(a.FileName)
	}
	if filePath == "" {
		filePath = strings.TrimSpace(a.ID)
	}

	if filePath != "" && util.HasJSONFileName(filePath) {
		abs, errAbs := filepath.Abs(filePath)
		if errAbs == nil && strings.TrimSpace(abs) != "" {
			filePath = abs
		}
		filePath = filepath.Clean(filePath)

		authType := ""
		if a.Metadata != nil {
			if rawType, ok := a.Metadata["type"].(string); ok {
				authType = strings.TrimSpace(rawType)
			}
		}
		if authType == "" {
			authType = strings.TrimSpace(provider)
		}
		authType = strings.ToLower(strings.TrimSpace(authType))
		if authType != "" {
			return authType + ":" + filePath
		}
	}

	apiPrefix := ""
	if apiKey != "" {
		switch {
		case compatName != "" || strings.EqualFold(provider, "openai-compatibility"):
			apiPrefix = "openai-compatibility"
		case strings.EqualFold(provider, "codex"):
			apiPrefix = "codex-api-key"
		case strings.EqualFold(provider, "claude"):
			apiPrefix = "claude-api-key"
		}
	}
	if apiPrefix != "" {
		return apiPrefix + ":" + strings.TrimSpace(baseURL) + "+" + strings.TrimSpace(apiKey)
	}

	if id := strings.TrimSpace(a.ID); id != "" {
		return "id:" + id
	}

	return ""
}

// EnsureIndex returns a stable index derived from the auth file name or credential identity.
func (a *Auth) EnsureIndex() string {
	if a == nil {
		return ""
	}
	if a.indexAssigned && a.Index != "" {
		return a.Index
	}

	seed := a.indexSeed()
	if seed == "" {
		return ""
	}

	idx := stableAuthIndex(seed)
	a.Index = idx
	a.indexAssigned = true
	return idx
}

// Clone duplicates a model state including nested error details.
func (m *ModelState) Clone() *ModelState {
	if m == nil {
		return nil
	}
	copyState := *m
	if m.LastError != nil {
		copyState.LastError = &Error{
			Code:       m.LastError.Code,
			Message:    m.LastError.Message,
			Retryable:  m.LastError.Retryable,
			HTTPStatus: m.LastError.HTTPStatus,
		}
	}
	return &copyState
}

// CloneForScheduler copies only the fields the scheduler needs for
// availability and cooldown decisions.
func (m *ModelState) CloneForScheduler() *ModelState {
	if m == nil {
		return nil
	}
	return &ModelState{
		Status:         m.Status,
		Unavailable:    m.Unavailable,
		NextRetryAfter: m.NextRetryAfter,
		Quota:          m.Quota,
	}
}

func (a *Auth) ProxyInfo() string {
	if a == nil {
		return ""
	}
	proxyStr := strings.TrimSpace(a.ProxyURL)
	if proxyStr == "" {
		return ""
	}
	if strings.EqualFold(proxyStr, "direct") || strings.EqualFold(proxyStr, "none") {
		return "direct"
	}
	if idx := strings.Index(proxyStr, "://"); idx > 0 {
		return "via " + proxyStr[:idx] + " proxy"
	}
	return "via proxy"
}

// DisableCoolingOverride returns the auth scoped disable_cooling override when present.
// The value is read from metadata key "disable_cooling" (or legacy "disable-cooling").
//
// NOTE: This override is intentionally "true-only". When the metadata value is false, it is treated
// as "not set" so the global disable-cooling flag can still take effect.
func (a *Auth) DisableCoolingOverride() (bool, bool) {
	if a == nil || a.Metadata == nil {
		return false, false
	}
	if val, ok := a.Metadata["disable_cooling"]; ok {
		if parsed, okParse := parseBoolAny(val); okParse {
			if !parsed {
				return false, false
			}
			return parsed, true
		}
	}
	if val, ok := a.Metadata["disable-cooling"]; ok {
		if parsed, okParse := parseBoolAny(val); okParse {
			if !parsed {
				return false, false
			}
			return parsed, true
		}
	}
	return false, false
}

// ToolPrefixDisabled returns whether the proxy_ tool name prefix should be
// skipped for this auth. When true, tool names are sent to Anthropic unchanged.
// The value is read from metadata key "tool_prefix_disabled" (or "tool-prefix-disabled").
func (a *Auth) ToolPrefixDisabled() bool {
	if a == nil || a.Metadata == nil {
		return false
	}
	for _, key := range []string{"tool_prefix_disabled", "tool-prefix-disabled"} {
		if val, ok := a.Metadata[key]; ok {
			if parsed, okParse := parseBoolAny(val); okParse {
				return parsed
			}
		}
	}
	return false
}

// ServiceTierPassthrough returns whether this auth is allowed to pass a
// client-provided Codex service_tier through to the upstream request.
func (a *Auth) ServiceTierPassthrough() bool {
	if a == nil {
		return false
	}
	for _, key := range authFileServiceTierPassthroughKeys {
		if a.Metadata != nil {
			if val, ok := a.Metadata[key]; ok {
				if parsed, okParse := parseBoolAny(val); okParse {
					return parsed
				}
			}
		}
		if a.Attributes != nil {
			if val, ok := a.Attributes[key]; ok {
				if parsed, okParse := parseBoolAny(val); okParse {
					return parsed
				}
			}
		}
	}
	return false
}

// RequestRetryOverride returns the auth-file scoped request_retry override when present.
// The value is read from metadata key "request_retry" (or legacy "request-retry").
func (a *Auth) RequestRetryOverride() (int, bool) {
	if a == nil || a.Metadata == nil {
		return 0, false
	}
	if val, ok := a.Metadata["request_retry"]; ok {
		if parsed, okParse := parseIntAny(val); okParse {
			if parsed < 0 {
				parsed = 0
			}
			return parsed, true
		}
	}
	if val, ok := a.Metadata["request-retry"]; ok {
		if parsed, okParse := parseIntAny(val); okParse {
			if parsed < 0 {
				parsed = 0
			}
			return parsed, true
		}
	}
	return 0, false
}

func parseBoolAny(val any) (bool, bool) {
	switch typed := val.(type) {
	case bool:
		return typed, true
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return false, false
		}
		parsed, err := strconv.ParseBool(trimmed)
		if err != nil {
			return false, false
		}
		return parsed, true
	case float64:
		return typed != 0, true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return false, false
		}
		return parsed != 0, true
	default:
		return false, false
	}
}

func parseIntAny(val any) (int, bool) {
	switch typed := val.(type) {
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		parsed, err := strconv.Atoi(trimmed)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func (a *Auth) AccountInfo() (string, string) {
	if a == nil {
		return "", ""
	}
	// Check metadata for email first (OAuth-style auth)
	if a.Metadata != nil {
		if v, ok := a.Metadata["email"].(string); ok {
			email := strings.TrimSpace(v)
			if email != "" {
				return "oauth", email
			}
		}
	}
	// Fall back to API key (API-key auth)
	if a.Attributes != nil {
		if v := a.Attributes["api_key"]; v != "" {
			return "api_key", v
		}
	}
	return "", ""
}

// ExpirationTime attempts to extract the credential expiration timestamp from metadata.
// It inspects common keys such as "expired", "expire", "expires_at", and also
// nested "token" objects to remain compatible with legacy auth file formats.
func (a *Auth) ExpirationTime() (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	if ts, ok := expirationFromMap(a.Metadata); ok {
		return ts, true
	}
	return time.Time{}, false
}

var (
	refreshLeadMu        sync.RWMutex
	refreshLeadFactories = make(map[string]func() *time.Duration)

	defaultAutoRefreshMu        sync.RWMutex
	defaultAutoRefreshProviders = make(map[string]defaultAutoRefreshConfig)
)

type defaultAutoRefreshConfig struct {
	intervalFactory func() time.Duration
}

func RegisterRefreshLeadProvider(provider string, factory func() *time.Duration) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" || factory == nil {
		return
	}
	refreshLeadMu.Lock()
	refreshLeadFactories[provider] = factory
	refreshLeadMu.Unlock()
}

func RegisterDefaultAutoRefreshProvider(provider string) {
	RegisterDefaultAutoRefreshProviderWithInterval(provider, nil)
}

func RegisterDefaultAutoRefreshProviderWithInterval(provider string, intervalFactory func() time.Duration) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return
	}
	defaultAutoRefreshMu.Lock()
	defaultAutoRefreshProviders[provider] = defaultAutoRefreshConfig{intervalFactory: intervalFactory}
	defaultAutoRefreshMu.Unlock()
}

var expireKeys = [...]string{"expired", "expire", "expires_at", "expiresAt", "expiry", "expires"}

func expirationFromMap(meta map[string]any) (time.Time, bool) {
	if meta == nil {
		return time.Time{}, false
	}
	for _, key := range expireKeys {
		if v, ok := meta[key]; ok {
			if ts, ok1 := parseTimeValue(v); ok1 {
				return ts, true
			}
		}
	}
	for _, nestedKey := range []string{"token", "Token"} {
		if nested, ok := meta[nestedKey]; ok {
			switch val := nested.(type) {
			case map[string]any:
				if ts, ok1 := expirationFromMap(val); ok1 {
					return ts, true
				}
			case map[string]string:
				temp := make(map[string]any, len(val))
				for k, v := range val {
					temp[k] = v
				}
				if ts, ok1 := expirationFromMap(temp); ok1 {
					return ts, true
				}
			}
		}
	}
	return time.Time{}, false
}

func ProviderRefreshLead(provider string, runtime any) *time.Duration {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if runtime != nil {
		if eval, ok := runtime.(interface{ RefreshLead() *time.Duration }); ok {
			if lead := eval.RefreshLead(); lead != nil && *lead > 0 {
				return lead
			}
		}
	}
	refreshLeadMu.RLock()
	factory := refreshLeadFactories[provider]
	refreshLeadMu.RUnlock()
	if factory == nil {
		return nil
	}
	if lead := factory(); lead != nil && *lead > 0 {
		return lead
	}
	return nil
}

func ProviderDefaultRefreshInterval(provider string) time.Duration {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return 0
	}
	defaultAutoRefreshMu.RLock()
	cfg, ok := defaultAutoRefreshProviders[provider]
	defaultAutoRefreshMu.RUnlock()
	if !ok || cfg.intervalFactory == nil {
		return 0
	}
	interval := cfg.intervalFactory()
	if interval <= 0 {
		return 0
	}
	return interval
}

func ProviderDefaultAutoRefresh(provider string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return false
	}
	defaultAutoRefreshMu.RLock()
	_, ok := defaultAutoRefreshProviders[provider]
	defaultAutoRefreshMu.RUnlock()
	return ok
}

func HasDefaultAutoRefreshProviders() bool {
	defaultAutoRefreshMu.RLock()
	ok := len(defaultAutoRefreshProviders) > 0
	defaultAutoRefreshMu.RUnlock()
	return ok
}

func parseTimeValue(v any) (time.Time, bool) {
	switch value := v.(type) {
	case string:
		s := strings.TrimSpace(value)
		if s == "" {
			return time.Time{}, false
		}
		layouts := []string{
			time.RFC3339,
			time.RFC3339Nano,
			"2006-01-02 15:04:05",
			"2006-01-02 15:04",
			"2006-01-02T15:04:05Z07:00",
		}
		for _, layout := range layouts {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts, true
			}
		}
		if unix, err := strconv.ParseInt(s, 10, 64); err == nil {
			return normaliseUnix(unix), true
		}
	case float64:
		return normaliseUnix(int64(value)), true
	case int64:
		return normaliseUnix(value), true
	case json.Number:
		if i, err := value.Int64(); err == nil {
			return normaliseUnix(i), true
		}
		if f, err := value.Float64(); err == nil {
			return normaliseUnix(int64(f)), true
		}
	}
	return time.Time{}, false
}

func normaliseUnix(raw int64) time.Time {
	if raw <= 0 {
		return time.Time{}
	}
	// Heuristic: treat values with millisecond precision (>1e12) accordingly.
	if raw > 1_000_000_000_000 {
		return time.UnixMilli(raw)
	}
	return time.Unix(raw, 0)
}
