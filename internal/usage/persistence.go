package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const persistedStatisticsStateVersion = 1

type persistedStatisticsState struct {
	Version           int                              `json:"version"`
	SavedAt           time.Time                        `json:"saved_at"`
	Snapshot          StatisticsSnapshot               `json:"snapshot"`
	AggregateRecords  []persistedUsageAggregateRecord  `json:"aggregate_records,omitempty"`
	RolledUp          *AggregatedUsageSnapshot         `json:"rolled_up_aggregated,omitempty"`
	ClientAPIKeyQuota *persistedClientAPIKeyQuotaState `json:"client_api_key_quota,omitempty"`
}

type persistedUsageAggregateRecord struct {
	APIName   string        `json:"api_name"`
	ModelName string        `json:"model_name"`
	Detail    RequestDetail `json:"detail"`
}

// SavePersistedState writes the current statistics state to disk so it can be
// restored on the next process start.
func SavePersistedState(path string, stats *RequestStatistics) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if stats == nil {
		return fmt.Errorf("usage: request statistics is nil")
	}

	data, errMarshal := MarshalPersistedState(stats)
	if errMarshal != nil {
		return errMarshal
	}

	dir := filepath.Dir(path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("usage: create persistence directory: %w", errMkdir)
	}

	tmpPath := path + ".tmp"
	if errWrite := os.WriteFile(tmpPath, data, 0o600); errWrite != nil {
		return fmt.Errorf("usage: write persisted state: %w", errWrite)
	}
	if errRename := os.Rename(tmpPath, path); errRename != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("usage: replace persisted state: %w", errRename)
	}
	return nil
}

// LoadPersistedState restores statistics previously saved by SavePersistedState.
func LoadPersistedState(path string, stats *RequestStatistics) (bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return false, nil
	}
	if stats == nil {
		return false, fmt.Errorf("usage: request statistics is nil")
	}

	data, errRead := os.ReadFile(path)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return false, nil
		}
		return false, fmt.Errorf("usage: read persisted state: %w", errRead)
	}
	if len(data) == 0 {
		return false, nil
	}

	return LoadPersistedStateBytes(data, stats)
}

// MarshalPersistedState serializes the current statistics state for durable
// storage backends.
func MarshalPersistedState(stats *RequestStatistics) ([]byte, error) {
	if stats == nil {
		return nil, fmt.Errorf("usage: request statistics is nil")
	}
	state := stats.persistedState()
	data, errMarshal := json.Marshal(state)
	if errMarshal != nil {
		return nil, fmt.Errorf("usage: marshal persisted state: %w", errMarshal)
	}
	return data, nil
}

// LoadPersistedStateBytes restores statistics from a serialized persisted state.
func LoadPersistedStateBytes(data []byte, stats *RequestStatistics) (bool, error) {
	if len(data) == 0 {
		return false, nil
	}
	if stats == nil {
		return false, fmt.Errorf("usage: request statistics is nil")
	}

	var state persistedStatisticsState
	if errUnmarshal := json.Unmarshal(data, &state); errUnmarshal != nil {
		return false, fmt.Errorf("usage: unmarshal persisted state: %w", errUnmarshal)
	}
	if state.Version != 0 && state.Version != persistedStatisticsStateVersion {
		return false, fmt.Errorf("usage: unsupported persisted state version %d", state.Version)
	}

	stats.restorePersistedState(state)
	if state.ClientAPIKeyQuota != nil {
		defaultClientAPIKeyQuotaTracker.restorePersistedState(*state.ClientAPIKeyQuota, time.Now().UTC())
		seedCurrentClientAPIKeyQuotaStore()
	}
	return true, nil
}

func (s *RequestStatistics) persistedState() persistedStatisticsState {
	state := persistedStatisticsState{
		Version: persistedStatisticsStateVersion,
		SavedAt: time.Now().UTC(),
	}
	if s == nil {
		return state
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	state.Snapshot = s.snapshotWithDetailsLocked(true)
	if len(s.aggregateRecords) > 0 {
		state.AggregateRecords = make([]persistedUsageAggregateRecord, 0, len(s.aggregateRecords))
		for _, record := range s.aggregateRecords {
			detail := normalizeImportedRequestDetail(record.Detail)
			if detail.Timestamp.IsZero() {
				continue
			}
			apiName := strings.TrimSpace(record.APIName)
			if apiName == "" {
				apiName = "unknown"
			}
			modelName := strings.TrimSpace(record.ModelName)
			if modelName == "" {
				modelName = "unknown"
			}
			state.AggregateRecords = append(state.AggregateRecords, persistedUsageAggregateRecord{
				APIName:   apiName,
				ModelName: modelName,
				Detail:    detail,
			})
		}
	}
	if s.rolledUpAggregated != nil {
		cloned := cloneAggregatedUsageSnapshot(*s.rolledUpAggregated)
		state.RolledUp = &cloned
	}
	quotaState := defaultClientAPIKeyQuotaTracker.persistedState()
	if !quotaState.isZero() {
		state.ClientAPIKeyQuota = &quotaState
	}

	return state
}

func (s *RequestStatistics) restorePersistedState(state persistedStatisticsState) {
	if s == nil {
		return
	}

	snapshot := canonicalDetailedSnapshotForImport(state.Snapshot)
	now := time.Now().UTC()
	cutoff := now.Add(-aggregateRecordRetentionWindow)
	limit := DetailRetentionLimit()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.totalRequests.Store(snapshot.TotalRequests)
	s.successCount.Store(snapshot.SuccessCount)
	s.failureCount.Store(snapshot.FailureCount)
	s.totalTokens.Store(snapshot.TotalTokens)

	s.apis = make(map[string]*apiStats, len(snapshot.APIs))
	for apiName, apiSnapshot := range snapshot.APIs {
		apiName = strings.TrimSpace(apiName)
		if apiName == "" {
			apiName = "unknown"
		}
		stats := &apiStats{
			TotalRequests: apiSnapshot.TotalRequests,
			TotalTokens:   apiSnapshot.TotalTokens,
			Models:        make(map[string]*modelStats, len(apiSnapshot.Models)),
		}
		for modelName, modelSnapshot := range apiSnapshot.Models {
			modelName = strings.TrimSpace(modelName)
			if modelName == "" {
				modelName = "unknown"
			}
			details := make([]RequestDetail, 0, len(modelSnapshot.Details))
			for _, detail := range modelSnapshot.Details {
				details = append(details, normalizeImportedRequestDetail(detail))
			}
			trimRequestDetails(&details, limit)
			stats.Models[modelName] = &modelStats{
				TotalRequests:  modelSnapshot.TotalRequests,
				TotalTokens:    modelSnapshot.TotalTokens,
				TokenBreakdown: normaliseTokenStats(modelSnapshot.TokenBreakdown),
				Latency:        modelSnapshot.Latency,
				Details:        details,
			}
		}
		s.apis[apiName] = stats
	}

	s.requestsByDay = cloneStringInt64Map(snapshot.RequestsByDay)
	s.requestsByHour = make(map[int]int64, len(snapshot.RequestsByHour))
	for hourKey, count := range snapshot.RequestsByHour {
		hour, ok := parseSnapshotHour(hourKey)
		if !ok {
			continue
		}
		s.requestsByHour[hour] += count
	}

	s.tokensByDay = cloneStringInt64Map(snapshot.TokensByDay)
	s.tokensByHour = make(map[int]int64, len(snapshot.TokensByHour))
	for hourKey, count := range snapshot.TokensByHour {
		hour, ok := parseSnapshotHour(hourKey)
		if !ok {
			continue
		}
		s.tokensByHour[hour] += count
	}

	if state.RolledUp != nil {
		cloned := cloneAggregatedUsageSnapshot(*state.RolledUp)
		s.rolledUpAggregated = &cloned
	} else {
		s.rolledUpAggregated = nil
	}

	s.aggregateRecords = nil
	s.oldestAggregateRecordAt = time.Time{}
	s.newestAggregateRecordAt = time.Time{}

	seen := make(map[string]struct{}, len(state.AggregateRecords))
	appendRecord := func(apiName, modelName string, detail RequestDetail) {
		detail = normalizeImportedRequestDetail(detail)
		if detail.Timestamp.IsZero() {
			return
		}
		apiName = strings.TrimSpace(apiName)
		if apiName == "" {
			apiName = "unknown"
		}
		modelName = strings.TrimSpace(modelName)
		if modelName == "" {
			modelName = "unknown"
		}
		key := dedupKey(apiName, modelName, detail)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		s.aggregateRecords = append(s.aggregateRecords, usageAggregateRecord{
			APIName:   apiName,
			ModelName: modelName,
			Detail:    detail,
		})
		recordTime := detail.Timestamp.UTC()
		if s.oldestAggregateRecordAt.IsZero() || recordTime.Before(s.oldestAggregateRecordAt) {
			s.oldestAggregateRecordAt = recordTime
		}
		if s.newestAggregateRecordAt.IsZero() || recordTime.After(s.newestAggregateRecordAt) {
			s.newestAggregateRecordAt = recordTime
		}
	}

	for _, record := range state.AggregateRecords {
		appendRecord(record.APIName, record.ModelName, record.Detail)
	}

	for apiName, apiSnapshot := range snapshot.APIs {
		for modelName, modelSnapshot := range apiSnapshot.Models {
			for _, detail := range modelSnapshot.Details {
				normalized := normalizeImportedRequestDetail(detail)
				if normalized.Timestamp.IsZero() || normalized.Timestamp.Before(cutoff) {
					continue
				}
				appendRecord(apiName, modelName, normalized)
			}
		}
	}

	s.pruneAggregateRecordsLocked(now)

	s.importedSummary = nil
	s.importedAggregated = nil
	s.importedSummaryHashes = make(map[string]struct{})
	s.importedAggregateHashes = make(map[string]struct{})
	s.importedSummarySources = make(map[string]StatisticsSnapshot)
	s.importedDetailedSources = make(map[string]StatisticsSnapshot)
	s.importedAggregateSource = make(map[string]AggregatedUsageSnapshot)
}
