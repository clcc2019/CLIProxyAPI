package auth

import "strings"

// AuthLookupSnapshot contains stable scalar fields used by management lookup
// paths. It excludes credentials, metadata maps, model state, quotas, and
// request-history buffers.
type AuthLookupSnapshot struct {
	ID       string
	Index    string
	FileName string
	Path     string
	Source   string
}

// AuthLookupSnapshots returns lightweight lookup records for all managed
// auths. The slice and its values are owned by the caller.
func (m *Manager) AuthLookupSnapshots() []AuthLookupSnapshot {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	snapshots := make([]AuthLookupSnapshot, 0, len(m.auths))
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		index := strings.TrimSpace(auth.Index)
		if index == "" {
			index = auth.Clone().EnsureIndex()
		}
		path := ""
		source := ""
		if auth.Attributes != nil {
			path = auth.Attributes["path"]
			source = auth.Attributes["source"]
		}
		snapshots = append(snapshots, AuthLookupSnapshot{
			ID:       auth.ID,
			Index:    index,
			FileName: auth.FileName,
			Path:     path,
			Source:   source,
		})
	}
	return snapshots
}
