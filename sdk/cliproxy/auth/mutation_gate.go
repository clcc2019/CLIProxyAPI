package auth

import (
	"context"
	"reflect"
	"sync"

	"golang.org/x/sync/semaphore"
)

// lockAuthMutation acquires the store reload barrier and then the per-auth
// mutation gate. Keeping both until the mutation's persistence finishes makes
// a whole-store Load observe the same credential version that is visible in
// memory. Unrelated auth IDs can still mutate concurrently.
func (m *Manager) lockAuthMutation(id string) func() {
	release, _ := m.lockAuthMutationContext(context.Background(), id)
	return release
}

func (m *Manager) lockAuthMutationContext(ctx context.Context, id string) (func(), error) {
	if m == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if m.authLoadGate == nil {
		m.mu.Lock()
		if m.authLoadGate == nil {
			m.authLoadGate = newAuthLoadGate()
		}
		gate := m.authLoadGate
		m.mu.Unlock()
		return m.lockAuthMutationWithGate(ctx, id, gate)
	}
	return m.lockAuthMutationWithGate(ctx, id, m.authLoadGate)
}

func newAuthLoadGate() *semaphore.Weighted {
	return semaphore.NewWeighted(1<<63 - 1)
}

func (m *Manager) lockAuthMutationWithGate(ctx context.Context, id string, loadGate *semaphore.Weighted) (func(), error) {
	if errAcquire := loadGate.Acquire(ctx, 1); errAcquire != nil {
		return nil, errAcquire
	}
	key := id
	if key == "" {
		key = "<anonymous>"
	}
	value, _ := m.authMutationLocks.LoadOrStore(key, make(chan struct{}, 1))
	gate, ok := value.(chan struct{})
	if !ok || gate == nil {
		loadGate.Release(1)
		return nil, context.Canceled
	}
	select {
	case gate <- struct{}{}:
		if errContext := ctx.Err(); errContext != nil {
			<-gate
			loadGate.Release(1)
			return nil, errContext
		}
	case <-ctx.Done():
		loadGate.Release(1)
		return nil, ctx.Err()
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			<-gate
			loadGate.Release(1)
		})
	}, nil
}

// mergeAuthSaveDelta applies fields changed by Store.Save while preserving
// edits made by the manager during storage I/O. Map entries are treated as
// independent values, while nested values remain atomic. Identity and runtime
// bookkeeping fields are owned by the manager.
func mergeAuthSaveDelta(target, before, after *Auth, preserveConcurrent bool) {
	if target == nil || before == nil || after == nil {
		return
	}
	dst := reflect.ValueOf(target).Elem()
	base := reflect.ValueOf(before).Elem()
	next := reflect.ValueOf(after).Elem()
	for i := 0; i < dst.NumField(); i++ {
		field := dst.Type().Field(i)
		if field.PkgPath != "" || field.Name == "ID" {
			continue
		}
		d, b, n := dst.Field(i), base.Field(i), next.Field(i)
		if reflect.DeepEqual(b.Interface(), n.Interface()) {
			continue
		}
		if reflect.DeepEqual(d.Interface(), b.Interface()) {
			d.Set(n)
			continue
		}
		if d.Kind() != reflect.Map {
			if !preserveConcurrent {
				d.Set(n)
			}
			continue
		}

		keys := make(map[any]reflect.Value)
		for _, key := range b.MapKeys() {
			keys[key.Interface()] = key
		}
		for _, key := range n.MapKeys() {
			keys[key.Interface()] = key
		}
		for _, key := range keys {
			oldValue, newValue := b.MapIndex(key), n.MapIndex(key)
			if equalAuthSaveValue(oldValue, newValue) {
				continue
			}
			if preserveConcurrent && !equalAuthSaveValue(d.MapIndex(key), oldValue) {
				continue
			}
			if !newValue.IsValid() {
				if d.IsNil() {
					continue
				}
				d.SetMapIndex(key, reflect.Value{})
				continue
			}
			if d.IsNil() {
				d.Set(reflect.MakeMap(d.Type()))
			}
			d.SetMapIndex(key, newValue)
		}
	}
}

func equalAuthSaveValue(a, b reflect.Value) bool {
	if !a.IsValid() || !b.IsValid() {
		return a.IsValid() == b.IsValid()
	}
	return reflect.DeepEqual(a.Interface(), b.Interface())
}
