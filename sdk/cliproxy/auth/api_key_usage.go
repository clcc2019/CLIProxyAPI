package auth

import (
	"strings"
	"time"
)

// APIKeyUsageSnapshot contains only the fields required to aggregate recent
// API-key usage. It deliberately omits general auth metadata, tokens, model
// state, and persistence details.
type APIKeyUsageSnapshot struct {
	Provider       string
	APIKey         string
	BaseURL        string
	Success        int64
	Failed         int64
	RecentRequests []RecentRequestBucket
}

// APIKeyUsageSnapshots returns lightweight snapshots for API-key auths. Auths
// are filtered before copying so OAuth and unrelated credentials do not cause
// token-bearing metadata allocations.
func (m *Manager) APIKeyUsageSnapshots(now time.Time) []APIKeyUsageSnapshot {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	labels := recentRequestBucketLabels(now)
	snapshots := make([]APIKeyUsageSnapshot, 0, len(m.auths))
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		kind, apiKey := auth.AccountInfo()
		if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
			continue
		}
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" {
			continue
		}
		baseURL := ""
		if auth.Attributes != nil {
			baseURL = strings.TrimSpace(auth.Attributes["base_url"])
			if baseURL == "" {
				baseURL = strings.TrimSpace(auth.Attributes["base-url"])
			}
		}
		snapshots = append(snapshots, APIKeyUsageSnapshot{
			Provider:       auth.Provider,
			APIKey:         apiKey,
			BaseURL:        baseURL,
			Success:        auth.Success,
			Failed:         auth.Failed,
			RecentRequests: auth.recentRequestsSnapshotWithLabels(now, &labels),
		})
	}
	return snapshots
}
