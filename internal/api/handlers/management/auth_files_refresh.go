package management

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const authFilesCodexPageRefreshConcurrency = 4

func (h *Handler) refreshAuthFileEntryPageFromManager(ctx context.Context, files []gin.H, auths []*coreauth.Auth, opts authFileEntryBuildOptions) []gin.H {
	if h == nil || len(files) == 0 || len(auths) == 0 {
		return files
	}
	type refreshTask struct {
		index int
		auth  *coreauth.Auth
	}
	authsByKey := make(map[string]*coreauth.Auth, len(auths)*3)
	for _, auth := range auths {
		for _, key := range authFileListAuthKeys(auth) {
			if _, exists := authsByKey[key]; !exists {
				authsByKey[key] = auth
			}
		}
	}
	refreshed := make([]gin.H, len(files))
	copy(refreshed, files)
	tasks := make([]refreshTask, 0, len(refreshed))
	for i, entry := range refreshed {
		auth := authsByKey[authFileEntryLookupKey(entry)]
		if auth == nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") && !strings.EqualFold(authFileEntryString(entry, "type", "provider"), "codex") {
			continue
		}
		tasks = append(tasks, refreshTask{index: i, auth: auth})
	}
	runAuthFilePageRefreshTasks(len(tasks), func(taskIndex int) {
		task := tasks[taskIndex]
		refreshAuth := task.auth
		if h.authManager != nil && task.auth != nil && strings.TrimSpace(task.auth.ID) != "" {
			if current, ok := h.authManager.GetByID(task.auth.ID); ok && current != nil {
				refreshAuth = current
			}
		}
		updated := h.enrichCodexSubscriptionInfo(ctx, refreshAuth, codexSubscriptionListRefresh)
		if updatedEntry := h.buildAuthFileEntryWithOptions(updated, opts); updatedEntry != nil {
			refreshed[task.index] = updatedEntry
		}
	})
	return refreshed
}

func (h *Handler) refreshAuthFileEntryPageFromDisk(ctx context.Context, files []gin.H) []gin.H {
	if h == nil || h.cfg == nil || strings.TrimSpace(h.cfg.AuthDir) == "" || len(files) == 0 {
		return files
	}
	root, err := os.OpenRoot(strings.TrimSpace(h.cfg.AuthDir))
	if err != nil {
		return files
	}
	defer func() { _ = root.Close() }()

	type refreshTask struct {
		index int
		entry gin.H
	}
	refreshed := make([]gin.H, len(files))
	copy(refreshed, files)
	tasks := make([]refreshTask, 0, len(refreshed))
	for i, entry := range refreshed {
		if !strings.EqualFold(authFileEntryString(entry, "type", "provider"), "codex") {
			continue
		}
		tasks = append(tasks, refreshTask{index: i, entry: entry})
	}
	runAuthFilePageRefreshTasks(len(tasks), func(taskIndex int) {
		task := tasks[taskIndex]
		if updatedEntry := h.refreshAuthFileEntryFromDiskWithRoot(ctx, root, task.entry); updatedEntry != nil {
			refreshed[task.index] = updatedEntry
		}
	})
	return refreshed
}

func runAuthFilePageRefreshTasks(total int, run func(index int)) {
	if total <= 0 || run == nil {
		return
	}
	workerCount := authFilesCodexPageRefreshConcurrency
	if workerCount > total {
		workerCount = total
	}
	if workerCount < 1 {
		workerCount = 1
	}
	tasks := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range tasks {
				run(index)
			}
		}()
	}
	for i := 0; i < total; i++ {
		tasks <- i
	}
	close(tasks)
	wg.Wait()
}

func (h *Handler) refreshAuthFileEntryFromDisk(ctx context.Context, entry gin.H) gin.H {
	if h == nil || h.cfg == nil || strings.TrimSpace(h.cfg.AuthDir) == "" {
		return nil
	}
	root, err := os.OpenRoot(strings.TrimSpace(h.cfg.AuthDir))
	if err != nil {
		return nil
	}
	defer func() { _ = root.Close() }()
	return h.refreshAuthFileEntryFromDiskWithRoot(ctx, root, entry)
}

func (h *Handler) refreshAuthFileEntryFromDiskWithRoot(ctx context.Context, root *os.Root, entry gin.H) gin.H {
	name := strings.TrimSpace(valueAsString(entry["name"]))
	if name == "" || h == nil || h.cfg == nil || root == nil {
		return nil
	}
	path := filepath.Join(h.cfg.AuthDir, name)
	data, err := readAuthRootFile(root, name)
	if err != nil {
		return nil
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil
	}
	updated := h.enrichCodexSubscriptionInfo(ctx, &coreauth.Auth{
		ID:       name,
		Provider: "codex",
		FileName: name,
		Metadata: metadata,
		Attributes: map[string]string{
			"path": path,
		},
	}, codexSubscriptionListRefresh)
	if updated == nil || len(updated.Metadata) == 0 {
		return nil
	}
	updatedEntry := cloneGinH(entry)
	if authFileMetadataHasRefreshToken(updated.Metadata) {
		updatedEntry["has_refresh_token"] = true
	}
	if email := strings.TrimSpace(valueAsString(updated.Metadata["email"])); email != "" {
		updatedEntry["email"] = email
	}
	if planType := strings.TrimSpace(valueAsString(updated.Metadata["plan_type"])); planType != "" {
		updatedEntry["plan_type"] = planType
	}
	if until, ok := codexSubscriptionUntilValue(updated.Metadata); ok {
		updatedEntry["subscription_expires_at"] = until
	}
	if claims := extractCodexIDTokenClaims(updated); claims != nil {
		updatedEntry["id_token"] = claims
		applyCodexSubscriptionFromClaims(updatedEntry, claims)
	}
	return updatedEntry
}
