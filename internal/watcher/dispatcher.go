// dispatcher.go implements auth update dispatching and queue management.
// It batches, deduplicates, and delivers auth updates to registered consumers.
package watcher

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

var snapshotCoreAuthsFunc = snapshotCoreAuths

func (w *Watcher) setAuthUpdateQueue(queue chan<- AuthUpdate) {
	w.clientsMutex.Lock()
	defer w.clientsMutex.Unlock()
	w.authQueue = queue
	if w.dispatchCond == nil {
		w.dispatchCond = sync.NewCond(&w.dispatchMu)
	}
	if w.dispatchCancel != nil {
		w.dispatchCancel()
		if w.dispatchCond != nil {
			w.dispatchMu.Lock()
			w.dispatchCond.Broadcast()
			w.dispatchMu.Unlock()
		}
		w.dispatchCancel = nil
	}
	if queue != nil {
		ctx, cancel := context.WithCancel(context.Background())
		w.dispatchCancel = cancel
		go w.dispatchLoop(ctx)
	}
}

func (w *Watcher) dispatchRuntimeAuthUpdate(update AuthUpdate) bool {
	if w == nil {
		return false
	}
	w.clientsMutex.Lock()
	if w.runtimeAuths == nil {
		w.runtimeAuths = make(map[string]*coreauth.Auth)
	}
	switch update.Action {
	case AuthUpdateActionAdd, AuthUpdateActionModify:
		if update.Auth != nil && update.Auth.ID != "" {
			clone := update.Auth.Clone()
			w.runtimeAuths[clone.ID] = clone
			if w.currentAuths == nil {
				w.currentAuths = make(map[string]*coreauth.Auth)
			}
			w.currentAuths[clone.ID] = clone.Clone()
			update.Auth = clone.Clone()
		}
	case AuthUpdateActionDelete:
		id := update.ID
		if id == "" && update.Auth != nil {
			id = update.Auth.ID
		}
		if id != "" {
			delete(w.runtimeAuths, id)
			if w.currentAuths != nil {
				delete(w.currentAuths, id)
			}
		}
	}
	updates := []AuthUpdate{update}
	w.stampAuthUpdatesLocked(updates)
	w.clientsMutex.Unlock()
	if w.getAuthQueue() == nil {
		return false
	}
	w.dispatchAuthUpdates(updates)
	return true
}

func (w *Watcher) refreshAuthState(force bool) {
	w.clientsMutex.RLock()
	cfg := w.config
	authDir := w.authDir
	previous := w.snapshotRevisionsLocked()
	w.clientsMutex.RUnlock()
	auths := snapshotCoreAuthsFunc(cfg, authDir)
	w.refreshAuthStateFromSnapshot(force, auths, previous)
}

// refreshAuthStateFromFileAuths combines config-backed auths with a file
// snapshot already produced by scanFileClients. This keeps a full reload to a
// single auth-directory pass instead of asking FileSynthesizer to read every
// file again.
func (w *Watcher) refreshAuthStateFromFileAuths(force bool, fileAuths []*coreauth.Auth, previous authSnapshotRevisions) {
	w.clientsMutex.RLock()
	cfg := w.config
	authDir := w.authDir
	w.clientsMutex.RUnlock()
	auths := snapshotConfigAuths(cfg, authDir)
	auths = append(auths, fileAuths...)
	w.refreshAuthStateFromSnapshot(force, auths, previous)
}

type authSnapshotRevisions struct {
	auths map[string]uint64
	files map[string]uint64
}

func (w *Watcher) snapshotRevisionsLocked() authSnapshotRevisions {
	return authSnapshotRevisions{auths: maps.Clone(w.authRevisions), files: maps.Clone(w.fileRevisions)}
}

func (w *Watcher) refreshAuthStateFromSnapshot(force bool, auths []*coreauth.Auth, previous authSnapshotRevisions) {
	w.clientsMutex.Lock()
	// Reading a directory is intentionally outside clientsMutex. Preserve newer
	// observations, including deletes and files that had no auth when scanning began.
	changedDuringScan := func(auth *coreauth.Auth) bool {
		path := auth.Attributes["path"]
		if path == "" {
			path = auth.Attributes["source"]
		}
		path = w.normalizeAuthPath(path)
		return previous.auths[auth.ID] != w.authRevisions[auth.ID] || previous.files[path] != w.fileRevisions[path]
	}
	for i, auth := range auths {
		if auth != nil && changedDuringScan(auth) {
			auths[i] = nil
		}
	}
	for _, auth := range w.currentAuths {
		if auth != nil && changedDuringScan(auth) {
			auths = append(auths, auth.Clone())
		}
	}
	if len(w.runtimeAuths) > 0 {
		for _, a := range w.runtimeAuths {
			if a != nil {
				auths = append(auths, a.Clone())
			}
		}
	}
	updates := w.prepareAuthUpdatesLocked(auths, force)
	w.clientsMutex.Unlock()
	w.dispatchAuthUpdates(updates)
}

func (w *Watcher) prepareAuthUpdatesLocked(auths []*coreauth.Auth, force bool) []AuthUpdate {
	newState := make(map[string]*coreauth.Auth, len(auths))
	for _, auth := range auths {
		if auth == nil || auth.ID == "" {
			continue
		}
		newState[auth.ID] = auth.Clone()
	}
	updates := make([]AuthUpdate, 0, len(newState)+len(w.currentAuths))
	for id, auth := range newState {
		if existing, ok := w.currentAuths[id]; !ok {
			updates = append(updates, AuthUpdate{Action: AuthUpdateActionAdd, ID: id, Auth: auth.Clone()})
		} else if force || !authEqual(existing, auth) {
			updates = append(updates, AuthUpdate{Action: AuthUpdateActionModify, ID: id, Auth: auth.Clone()})
		}
	}
	for id := range w.currentAuths {
		if _, ok := newState[id]; !ok {
			updates = append(updates, AuthUpdate{Action: AuthUpdateActionDelete, ID: id})
		}
	}
	w.currentAuths = newState
	w.stampAuthUpdatesLocked(updates)
	if w.authQueue == nil {
		return nil
	}
	return updates
}

// stampAuthUpdatesLocked assigns revisions while the corresponding state is
// published, before an update can be delayed on its way to the dispatch queue.
func (w *Watcher) stampAuthUpdatesLocked(updates []AuthUpdate) {
	if w.authRevisions == nil {
		w.authRevisions = make(map[string]uint64)
	}
	for i := range updates {
		update := &updates[i]
		if update.Auth != nil && update.Action != AuthUpdateActionDelete {
			update.ID = update.Auth.ID
		} else if update.ID == "" && update.Auth != nil {
			update.ID = update.Auth.ID
		}
		if update.ID == "" {
			continue
		}
		w.authRevisions[update.ID]++
		update.revision = w.authRevisions[update.ID]
	}
}

func (w *Watcher) dispatchAuthUpdates(updates []AuthUpdate) {
	if len(updates) == 0 {
		return
	}
	// Keep validation and enqueue atomic with respect to newer observations.
	w.clientsMutex.RLock()
	defer w.clientsMutex.RUnlock()
	if w.authQueue == nil {
		return
	}
	baseTS := time.Now().UnixNano()
	w.dispatchMu.Lock()
	if w.pendingUpdates == nil {
		w.pendingUpdates = make(map[string]AuthUpdate)
	}
	for idx, update := range updates {
		if update.revision != 0 && update.revision < w.authRevisions[update.ID] {
			continue
		}
		key := w.authUpdateKey(update, baseTS+int64(idx))
		if _, exists := w.pendingUpdates[key]; !exists {
			w.pendingOrder = append(w.pendingOrder, key)
		}
		w.pendingUpdates[key] = update
	}
	if w.dispatchCond != nil {
		w.dispatchCond.Signal()
	}
	w.dispatchMu.Unlock()
}

func (w *Watcher) authUpdateKey(update AuthUpdate, ts int64) string {
	if update.ID != "" {
		return update.ID
	}
	return fmt.Sprintf("%s:%d", update.Action, ts)
}

func (w *Watcher) dispatchLoop(ctx context.Context) {
	for {
		batch, ok := w.nextPendingBatch(ctx)
		if !ok {
			return
		}
		queue := w.getAuthQueue()
		if queue == nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		for _, update := range batch {
			select {
			case queue <- update:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (w *Watcher) nextPendingBatch(ctx context.Context) ([]AuthUpdate, bool) {
	w.dispatchMu.Lock()
	defer w.dispatchMu.Unlock()
	for len(w.pendingOrder) == 0 {
		if ctx.Err() != nil {
			return nil, false
		}
		w.dispatchCond.Wait()
		if ctx.Err() != nil {
			return nil, false
		}
	}
	batch := make([]AuthUpdate, 0, len(w.pendingOrder))
	for _, key := range w.pendingOrder {
		batch = append(batch, w.pendingUpdates[key])
		delete(w.pendingUpdates, key)
	}
	w.pendingOrder = w.pendingOrder[:0]
	return batch, true
}

func (w *Watcher) getAuthQueue() chan<- AuthUpdate {
	w.clientsMutex.RLock()
	defer w.clientsMutex.RUnlock()
	return w.authQueue
}

func (w *Watcher) stopDispatch() {
	// Acquire locks in the same order as setAuthUpdateQueue/dispatchAuthUpdates
	// (clientsMutex → dispatchMu) to avoid a circular wait if Stop and a reload
	// race. Cancel + clear authQueue under clientsMutex; clear pending state
	// under dispatchMu afterwards.
	w.clientsMutex.Lock()
	cancel := w.dispatchCancel
	w.dispatchCancel = nil
	w.authQueue = nil
	w.clientsMutex.Unlock()

	if cancel != nil {
		cancel()
	}

	w.dispatchMu.Lock()
	w.pendingOrder = nil
	w.pendingUpdates = nil
	if w.dispatchCond != nil {
		w.dispatchCond.Broadcast()
	}
	w.dispatchMu.Unlock()
}

func authEqual(a, b *coreauth.Auth) bool {
	return reflect.DeepEqual(normalizeAuth(a), normalizeAuth(b))
}

func normalizeAuth(a *coreauth.Auth) *coreauth.Auth {
	if a == nil {
		return nil
	}
	clone := a.Clone()
	clone.CreatedAt = time.Time{}
	clone.UpdatedAt = time.Time{}
	clone.LastRefreshedAt = time.Time{}
	clone.NextRefreshAfter = time.Time{}
	clone.Runtime = nil
	clone.Quota.NextRecoverAt = time.Time{}
	return clone
}

func snapshotCoreAuths(cfg *config.Config, authDir string) []*coreauth.Auth {
	out := snapshotConfigAuths(cfg, authDir)
	ctx := &synthesizer.SynthesisContext{
		Config:      cfg,
		AuthDir:     authDir,
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	}

	fileSynth := synthesizer.NewFileSynthesizer()
	if auths, err := fileSynth.Synthesize(ctx); err == nil {
		out = append(out, auths...)
	}

	return out
}

func snapshotConfigAuths(cfg *config.Config, authDir string) []*coreauth.Auth {
	ctx := &synthesizer.SynthesisContext{
		Config:      cfg,
		AuthDir:     authDir,
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	}
	configSynth := synthesizer.NewConfigSynthesizer()
	auths, err := configSynth.Synthesize(ctx)
	if err != nil {
		return nil
	}
	return auths
}
