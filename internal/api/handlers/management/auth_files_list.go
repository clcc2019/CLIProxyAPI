package management

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type authFilesListQuery struct {
	Paginated    bool
	Page         int
	PageSize     int
	Type         string
	Search       string
	SearchParts  []string
	Sort         string
	Summary      bool
	ProblemOnly  bool
	DisabledOnly bool
	PremiumOnly  bool
}

type authFileEntryBuildOptions struct {
	Summary   bool
	AuthDir   string
	AuthRoot  *os.Root
	StatCache map[string]authFileStatResult
}

type authFileStatResult struct {
	Info os.FileInfo
	Err  error
}

func authFilesListQueryFromRequest(c *gin.Context) authFilesListQuery {
	q := authFilesListQuery{
		Page:     1,
		PageSize: 0,
		Type:     strings.ToLower(strings.TrimSpace(firstNonEmptyQueryValue(c, "type", "provider"))),
		Search:   strings.ToLower(strings.TrimSpace(firstNonEmptyQueryValue(c, "q", "search"))),
		Sort:     strings.ToLower(strings.TrimSpace(firstNonEmptyQueryValue(c, "sort", "sort_mode", "sortMode"))),
	}
	if q.Sort == "" {
		q.Sort = "default"
	}
	if strings.Contains(q.Search, "*") {
		q.SearchParts = strings.Split(q.Search, "*")
	}
	q.Summary = isTruthyQueryValue(firstNonEmptyQueryValue(c, "summary", "lite", "compact", "summary_only", "summaryOnly"))
	if page := parsePositiveQueryInt(firstNonEmptyQueryValue(c, "page")); page > 0 {
		q.Page = page
	}
	if pageSize := parsePositiveQueryInt(firstNonEmptyQueryValue(c, "page_size", "pageSize", "limit")); pageSize > 0 {
		if pageSize > maxAuthFilesListPageSize {
			pageSize = maxAuthFilesListPageSize
		}
		q.PageSize = pageSize
		q.Paginated = true
	}
	q.ProblemOnly = isTruthyQueryValue(firstNonEmptyQueryValue(c, "problem", "problem_only", "problemOnly"))
	q.DisabledOnly = isTruthyQueryValue(firstNonEmptyQueryValue(c, "disabled", "disabled_only", "disabledOnly"))
	q.PremiumOnly = isTruthyQueryValue(firstNonEmptyQueryValue(c, "premium", "premium_only", "premiumOnly"))
	return q
}

func parsePositiveQueryInt(raw string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
}

func (q authFilesListQuery) active() bool {
	return q.Paginated || q.Type != "" || q.Search != "" || q.Sort != "default" || q.Summary || q.ProblemOnly || q.DisabledOnly || q.PremiumOnly
}

func (q authFilesListQuery) offset() int {
	if !q.Paginated || q.Page <= 1 || q.PageSize <= 0 {
		return 0
	}
	return (q.Page - 1) * q.PageSize
}

func authFilesListPayload(files []gin.H, total int, q authFilesListQuery, typeCounts map[string]int) gin.H {
	payload := gin.H{"files": files, "total": total}
	if q.Paginated {
		payload["page"] = q.Page
		payload["page_size"] = q.PageSize
		payload["has_more"] = q.offset()+len(files) < total
	}
	if len(typeCounts) > 0 {
		payload["type_counts"] = typeCounts
	}
	return payload
}

func clampAuthFilesListPage(q authFilesListQuery, total int) authFilesListQuery {
	if !q.Paginated || q.PageSize <= 0 {
		return q
	}
	totalPages := (total + q.PageSize - 1) / q.PageSize
	if totalPages < 1 {
		totalPages = 1
	}
	if q.Page > totalPages {
		q.Page = totalPages
	}
	return q
}

func cloneGinH(entry gin.H) gin.H {
	if entry == nil {
		return nil
	}
	cloned := make(gin.H, len(entry))
	for key, value := range entry {
		cloned[key] = cloneGinValue(value)
	}
	return cloned
}

func cloneGinValue(value any) any {
	switch v := value.(type) {
	case gin.H:
		return cloneGinH(v)
	case map[string]any:
		return cloneGinH(gin.H(v))
	case map[string]string:
		cloned := make(map[string]string, len(v))
		for key, value := range v {
			cloned[key] = value
		}
		return cloned
	case []any:
		cloned := make([]any, len(v))
		for i, value := range v {
			cloned[i] = cloneGinValue(value)
		}
		return cloned
	case []gin.H:
		cloned := make([]gin.H, len(v))
		for i, value := range v {
			cloned[i] = cloneGinH(value)
		}
		return cloned
	case []map[string]any:
		cloned := make([]map[string]any, len(v))
		for i, value := range v {
			cloned[i] = cloneGinH(gin.H(value))
		}
		return cloned
	case []string:
		return append([]string(nil), v...)
	default:
		return value
	}
}

func authFileListAuthKeys(auth *coreauth.Auth) []string {
	if auth == nil {
		return nil
	}
	keys := make([]string, 0, 3)
	for _, value := range []string{auth.ID, auth.FileName} {
		key := authFileListKey(value)
		if key == "" {
			continue
		}
		keys = append(keys, key)
	}
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		if base := filepath.Base(fileName); base != "." && base != string(filepath.Separator) {
			if key := authFileListKey(base); key != "" {
				keys = append(keys, key)
			}
		}
	}
	return keys
}

func authFileEntryLookupKey(entry gin.H) string {
	for _, key := range []string{"id", "name", "file_name", "fileName"} {
		if value := authFileListKey(valueAsString(entry[key])); value != "" {
			return value
		}
	}
	return ""
}

func authFileListKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func authFileEntryTypeCounts(files []gin.H, q authFilesListQuery) map[string]int {
	counts := map[string]int{"all": 0}
	for _, file := range files {
		if !authFileEntryMatchesDisplayQuery(file, q) {
			continue
		}
		counts["all"]++
		provider := authFileEntryString(file, "type", "provider")
		if provider != "" {
			counts[provider]++
		}
	}
	return counts
}

func authFileTypeCountEntry(auth *coreauth.Auth) gin.H {
	if auth == nil {
		return nil
	}
	runtimeOnly := isRuntimeOnlyAuth(auth)
	path := strings.TrimSpace(authAttribute(auth, "path"))
	if path == "" && !runtimeOnly {
		return nil
	}
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		name = auth.ID
	}
	entry := gin.H{
		"id":           auth.ID,
		"name":         name,
		"type":         strings.TrimSpace(auth.Provider),
		"provider":     strings.TrimSpace(auth.Provider),
		"disabled":     auth.Disabled,
		"status":       auth.Status,
		"runtime_only": runtimeOnly,
		"source":       "memory",
	}
	if path != "" {
		entry["path"] = path
		entry["source"] = "file"
	}
	if !auth.UpdatedAt.IsZero() {
		entry["modtime"] = auth.UpdatedAt
		entry["updated_at"] = auth.UpdatedAt
	}
	return entry
}

func dedupeAuthFileEntries(entries []gin.H) []gin.H {
	if len(entries) <= 1 {
		return entries
	}
	groups := make(map[string][]gin.H, len(entries))
	order := make([]string, 0, len(entries))
	ungrouped := make([]gin.H, 0)
	for _, entry := range entries {
		name := strings.ToLower(strings.TrimSpace(valueAsString(entry["name"])))
		if name == "" {
			ungrouped = append(ungrouped, entry)
			continue
		}
		if _, ok := groups[name]; !ok {
			order = append(order, name)
		}
		groups[name] = append(groups[name], entry)
	}

	out := make([]gin.H, 0, len(order)+len(ungrouped))
	for _, name := range order {
		out = append(out, mergeAuthFileEntryGroup(groups[name]))
	}
	out = append(out, ungrouped...)
	return out
}

func mergeAuthFileEntryGroup(entries []gin.H) gin.H {
	if len(entries) == 0 {
		return nil
	}
	bestIndex := 0
	for i := 1; i < len(entries); i++ {
		if compareAuthFileEntryMergePriority(entries[i], entries[bestIndex]) < 0 {
			bestIndex = i
		}
	}
	merged := make(gin.H, len(entries[bestIndex]))
	for key, value := range entries[bestIndex] {
		merged[key] = value
	}
	for i, entry := range entries {
		if i == bestIndex {
			continue
		}
		for key, value := range entry {
			if key == "recent_requests" {
				if authFileRecentRequestsTotal(value) > authFileRecentRequestsTotal(merged[key]) {
					merged[key] = value
				}
				continue
			}
			if !hasAuthFileEntryMeaningfulValue(merged[key]) && hasAuthFileEntryMeaningfulValue(value) {
				merged[key] = value
			}
		}
	}
	return merged
}

func authFileRecentRequestsTotal(value any) int64 {
	var total int64
	switch buckets := value.(type) {
	case []coreauth.RecentRequestBucket:
		for _, bucket := range buckets {
			total += bucket.Success + bucket.Failed
		}
	case []coreauth.RecentRequestState:
		for _, bucket := range buckets {
			total += bucket.Success + bucket.Failed
		}
	case []gin.H:
		for _, bucket := range buckets {
			total += authFileRecentRequestBucketTotal(bucket)
		}
	case []map[string]any:
		for _, bucket := range buckets {
			total += authFileRecentRequestBucketTotal(bucket)
		}
	case []any:
		for _, bucket := range buckets {
			total += authFileRecentRequestBucketTotal(bucket)
		}
	}
	return total
}

func authFileRecentRequestBucketTotal(value any) int64 {
	switch bucket := value.(type) {
	case coreauth.RecentRequestBucket:
		return bucket.Success + bucket.Failed
	case coreauth.RecentRequestState:
		return bucket.Success + bucket.Failed
	case gin.H:
		return authFileRecentRequestCountValue(bucket["success"]) + authFileRecentRequestCountValue(bucket["failed"]) + authFileRecentRequestCountValue(bucket["failure"])
	case map[string]any:
		return authFileRecentRequestCountValue(bucket["success"]) + authFileRecentRequestCountValue(bucket["failed"]) + authFileRecentRequestCountValue(bucket["failure"])
	default:
		return 0
	}
}

func authFileRecentRequestCountValue(value any) int64 {
	switch v := value.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	case float32:
		return int64(v)
	case json.Number:
		parsed, _ := v.Int64()
		return parsed
	case string:
		parsed, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return parsed
	default:
		return 0
	}
}

func compareAuthFileEntryMergePriority(left, right gin.H) int {
	if cmp := authFileEntryMergeScore(right) - authFileEntryMergeScore(left); cmp != 0 {
		return cmp
	}
	if cmp := authFileEntryDateMs(right) - authFileEntryDateMs(left); cmp != 0 {
		if cmp > 0 {
			return 1
		}
		return -1
	}
	if cmp := authFileEntryMeaningfulFieldCount(right) - authFileEntryMeaningfulFieldCount(left); cmp != 0 {
		return cmp
	}
	return 0
}

func authFileEntryMergeScore(entry gin.H) int {
	score := 0
	if authFileEntryString(entry, "source") == "file" {
		score += 32
	}
	if strings.TrimSpace(valueAsString(entry["path"])) != "" {
		score += 16
	}
	if !authFileEntryRuntimeOnly(entry) {
		score += 8
	}
	if !authFileEntryDisabled(entry) {
		score += 4
	}
	if authFileEntryDateMs(entry) > 0 {
		score += 2
	}
	return score
}

func authFileEntryDateMs(entry gin.H) int64 {
	for _, key := range []string{"modtime", "modified", "updated_at", "last_refresh", "lastRefresh", "last_refreshed_at", "runtime_updated_at"} {
		if value, ok := entry[key]; ok {
			if ms := authFileListTimestampMs(value); ms > 0 {
				return ms
			}
		}
	}
	return 0
}

func authFileEntryMeaningfulFieldCount(entry gin.H) int {
	count := 0
	for _, value := range entry {
		if hasAuthFileEntryMeaningfulValue(value) {
			count++
		}
	}
	return count
}

func hasAuthFileEntryMeaningfulValue(value any) bool {
	if value == nil {
		return false
	}
	if str, ok := value.(string); ok {
		return strings.TrimSpace(str) != ""
	}
	if arr, ok := value.([]any); ok {
		return len(arr) > 0
	}
	return true
}

func authFileMatchesListDisplayQuery(auth *coreauth.Auth, q authFilesListQuery) bool {
	if auth == nil {
		return false
	}
	if isRuntimeOnlyAuth(auth) && authFileListDisabled(auth) {
		return false
	}
	if q.ProblemOnly && strings.TrimSpace(auth.StatusMessage) == "" {
		return false
	}
	if q.DisabledOnly && !authFileListDisabled(auth) {
		return false
	}
	return true
}

func authFileMatchesListPreQuery(auth *coreauth.Auth, q authFilesListQuery) bool {
	if !authFileMatchesListDisplayQuery(auth, q) {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if q.Type != "" && provider != q.Type {
		return false
	}
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		name = auth.ID
	}
	return authFileMatchesNormalizedSearch(q.Search, q.SearchParts, strings.ToLower(strings.TrimSpace(name)), provider)
}

func authFileEntryMatchesListQuery(file gin.H, q authFilesListQuery) bool {
	if !authFileEntryMatchesDisplayQuery(file, q) {
		return false
	}
	provider := authFileEntryString(file, "type", "provider")
	if q.Type != "" && provider != q.Type {
		return false
	}
	return authFileMatchesNormalizedSearch(q.Search, q.SearchParts, authFileEntryString(file, "name"), provider)
}

func authFileEntryMatchesDisplayQuery(file gin.H, q authFilesListQuery) bool {
	if q.ProblemOnly && authFileEntryString(file, "status_message", "statusMessage") == "" {
		return false
	}
	if q.DisabledOnly && !authFileEntryDisabled(file) {
		return false
	}
	if q.PremiumOnly && !authFileEntryHasPremiumPlan(file) {
		return false
	}
	return true
}

func authFileEntryHasPremiumPlan(file gin.H) bool {
	planType := authFileEntryPlanType(file)
	if planType != "" && planType != "free" {
		return true
	}
	return authFileEntrySubscriptionExpiryMs(file) > 0
}

func authFileMatchesNormalizedSearch(search string, wildcardParts []string, values ...string) bool {
	if search == "" {
		return true
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		if wildcardParts != nil {
			if wildcardTextMatchParts(search, wildcardParts, value) {
				return true
			}
			continue
		}
		if strings.Contains(value, search) {
			return true
		}
	}
	return false
}

func wildcardTextMatchParts(pattern string, parts []string, value string) bool {
	pos := 0
	matchedAny := false
	for _, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(value[pos:], part)
		if idx < 0 {
			return false
		}
		matchedAny = true
		pos += idx + len(part)
	}
	return matchedAny || pattern == "*"
}

type authFileListSortEntry struct {
	file               gin.H
	disabled           bool
	name               string
	provider           string
	priority           int
	subscriptionRank   int
	subscriptionExpiry int64
}

func sortAuthFileEntriesByName(files []gin.H) {
	if len(files) < 2 {
		return
	}
	entries := make([]authFileListSortEntry, len(files))
	for i, file := range files {
		name, _ := file["name"].(string)
		entries[i] = authFileListSortEntry{
			file: file,
			name: strings.ToLower(name),
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].name < entries[j].name
	})
	for i := range entries {
		files[i] = entries[i].file
	}
}

func sortAuthFileEntriesForList(files []gin.H, sortMode string) {
	if len(files) < 2 {
		return
	}
	entries := make([]authFileListSortEntry, len(files))
	for i, file := range files {
		entry := authFileListSortEntry{
			file:     file,
			disabled: authFileEntryDisabled(file),
			name:     authFileEntryString(file, "name"),
		}
		switch sortMode {
		case "priority":
			entry.priority = authFileEntryPriority(file)
		case "subscription_expiry", "subscription-expiry", "subscription":
			entry.subscriptionRank, entry.subscriptionExpiry = authFileEntrySubscriptionSortValue(file)
			entry.provider = authFileEntryString(file, "type", "provider")
		default:
			if sortMode != "az" && sortMode != "name" {
				entry.provider = authFileEntryString(file, "type", "provider")
			}
		}
		entries[i] = entry
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return compareAuthFileListSortEntries(entries[i], entries[j], sortMode) < 0
	})
	for i := range entries {
		files[i] = entries[i].file
	}
}

func compareAuthFileListSortEntries(left, right authFileListSortEntry, sortMode string) int {
	if cmp := compareBoolLast(left.disabled, right.disabled); cmp != 0 {
		return cmp
	}
	switch sortMode {
	case "az", "name":
		return strings.Compare(left.name, right.name)
	case "priority":
		if cmp := right.priority - left.priority; cmp != 0 {
			return cmp
		}
		return strings.Compare(left.name, right.name)
	case "subscription_expiry", "subscription-expiry", "subscription":
		if left.subscriptionRank != right.subscriptionRank {
			return left.subscriptionRank - right.subscriptionRank
		}
		if left.subscriptionRank == 0 && left.subscriptionExpiry != right.subscriptionExpiry {
			if left.subscriptionExpiry < right.subscriptionExpiry {
				return -1
			}
			return 1
		}
		if cmp := strings.Compare(left.provider, right.provider); cmp != 0 {
			return cmp
		}
		return strings.Compare(left.name, right.name)
	default:
		if cmp := strings.Compare(left.provider, right.provider); cmp != 0 {
			return cmp
		}
		return strings.Compare(left.name, right.name)
	}
}

func authFileEntryPageSlice(files []gin.H, q authFilesListQuery) []gin.H {
	if !q.Paginated || q.PageSize <= 0 {
		return files
	}
	start := q.offset()
	if start >= len(files) {
		return []gin.H{}
	}
	end := start + q.PageSize
	if end > len(files) {
		end = len(files)
	}
	return files[start:end]
}

func authFileListDisabled(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	return auth.Disabled || auth.Status == coreauth.StatusDisabled
}

func authFileListTimestampMs(value any) int64 {
	switch v := value.(type) {
	case nil:
		return 0
	case int:
		return normalizeAuthFileTimestampMs(int64(v))
	case int64:
		return normalizeAuthFileTimestampMs(v)
	case float64:
		return normalizeAuthFileTimestampMs(int64(v))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return normalizeAuthFileTimestampMs(i)
		}
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return 0
		}
		if parsed, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return normalizeAuthFileTimestampMs(parsed)
		}
		if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
			return parsed.UnixMilli()
		}
		if parsed, err := time.Parse("2006-01-02", trimmed); err == nil {
			return parsed.UnixMilli()
		}
	case time.Time:
		if !v.IsZero() {
			return v.UnixMilli()
		}
	}
	return 0
}

func normalizeAuthFileTimestampMs(value int64) int64 {
	if value <= 0 {
		return 0
	}
	if value < 1_000_000_000_000 {
		return value * 1000
	}
	return value
}

func authFileEntryString(file gin.H, keys ...string) string {
	for _, key := range keys {
		if value, ok := file[key]; ok {
			if str := strings.TrimSpace(valueAsString(value)); str != "" {
				return strings.ToLower(str)
			}
		}
	}
	return ""
}

func authFileEntryDisabled(file gin.H) bool {
	if value, ok := file["disabled"]; ok {
		if disabled, okBool := value.(bool); okBool {
			return disabled
		}
	}
	return strings.EqualFold(strings.TrimSpace(valueAsString(file["status"])), string(coreauth.StatusDisabled))
}

func authFileEntryRuntimeOnly(file gin.H) bool {
	value := file["runtime_only"]
	if value == nil {
		value = file["runtimeOnly"]
	}
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	default:
		return false
	}
}

func authFileEntryPriority(file gin.H) int {
	return intFromAny(file["priority"])
}

func authFileEntrySubscriptionSortValue(file gin.H) (rank int, expiryMs int64) {
	expiryMs = authFileEntrySubscriptionExpiryMs(file)
	if expiryMs > 0 {
		return 0, expiryMs
	}
	planType := authFileEntryPlanType(file)
	if planType == "" || planType == "free" {
		return 2, 0
	}
	return 1, 0
}

func authFileEntrySubscriptionExpiryMs(file gin.H) int64 {
	for _, key := range []string{
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
	} {
		if ms := authFileListTimestampMs(file[key]); ms > 0 {
			return ms
		}
	}
	return 0
}

func authFileEntryPlanType(file gin.H) string {
	return authFileEntryString(file, "plan_type", "planType", "chatgpt_plan_type", "chatgptPlanType")
}

func intFromAny(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		if parsed, err := v.Int64(); err == nil {
			return int(parsed)
		}
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return parsed
		}
	}
	return 0
}

func compareBoolLast(left, right bool) int {
	if left == right {
		return 0
	}
	if left {
		return 1
	}
	return -1
}
