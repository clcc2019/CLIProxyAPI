package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// FileTokenStore persists token records and auth metadata using the filesystem as backing storage.
type FileTokenStore struct {
	mu      sync.Mutex
	dirLock sync.RWMutex
	baseDir string
}

type scopedAuthFilePath struct {
	rootDir string
	relPath string
	root    *os.Root
}

type authFileCandidate struct {
	index   int
	path    string
	relPath string
	info    os.FileInfo
}

// NewFileTokenStore creates a token store that saves credentials to disk through the
// TokenStorage implementation embedded in the token record.
func NewFileTokenStore() *FileTokenStore {
	return &FileTokenStore{}
}

// SetBaseDir updates the default directory used for auth JSON persistence when no explicit path is provided.
func (s *FileTokenStore) SetBaseDir(dir string) {
	s.dirLock.Lock()
	s.baseDir = strings.TrimSpace(dir)
	s.dirLock.Unlock()
}

// Save persists token storage and metadata to the resolved auth file path.
func (s *FileTokenStore) Save(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("auth filestore: auth is nil")
	}

	path, err := s.resolveAuthPath(auth)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("auth filestore: missing file path attribute for %s", auth.ID)
	}
	scopedPath, err := s.scopedPath(path)
	if err != nil {
		return "", err
	}

	if auth.IsDisabled() {
		if _, statErr := scopedPath.stat(); os.IsNotExist(statErr) {
			return "", nil
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err = scopedPath.mkdirParent(0o700); err != nil {
		return "", fmt.Errorf("auth filestore: create dir failed: %w", err)
	}
	if err = scopedPath.validateExistingTarget(); err != nil {
		return "", fmt.Errorf("auth filestore: validate target failed: %w", err)
	}
	existingAuthFile, _ := scopedPath.readFile()
	cliproxyauth.PrepareCodexInstallationIDForSave(auth, existingAuthFile)

	// metadataSetter is a private interface for TokenStorage implementations that support metadata injection.
	type metadataSetter interface {
		SetMetadata(map[string]any)
	}

	switch {
	case auth.Storage != nil:
		metadata := cliproxyauth.PrepareAuthFileMetadataForSave(auth)
		if setter, ok := auth.Storage.(metadataSetter); ok {
			setter.SetMetadata(metadata)
		}
		if err = auth.Storage.SaveTokenToFile(path); err != nil {
			return "", err
		}
	case auth.Metadata != nil:
		metadata := cliproxyauth.PrepareAuthFileMetadataForSave(auth)
		raw, errMarshal := json.Marshal(metadata)
		if errMarshal != nil {
			return "", fmt.Errorf("auth filestore: marshal metadata failed: %w", errMarshal)
		}
		if existing, errRead := scopedPath.readFile(); errRead == nil {
			if jsonEqual(existing, raw) {
				return path, nil
			}
			file, errOpen := scopedPath.openFile(os.O_WRONLY|os.O_TRUNC, 0o600)
			if errOpen != nil {
				return "", fmt.Errorf("auth filestore: open existing failed: %w", errOpen)
			}
			if _, errWrite := file.Write(raw); errWrite != nil {
				_ = file.Close()
				return "", fmt.Errorf("auth filestore: write existing failed: %w", errWrite)
			}
			if errClose := file.Close(); errClose != nil {
				return "", fmt.Errorf("auth filestore: close existing failed: %w", errClose)
			}
			return path, nil
		} else if !os.IsNotExist(errRead) {
			return "", fmt.Errorf("auth filestore: read existing failed: %w", errRead)
		}
		if errWrite := scopedPath.writeFile(raw, 0o600); errWrite != nil {
			return "", fmt.Errorf("auth filestore: write file failed: %w", errWrite)
		}
	default:
		return "", fmt.Errorf("auth filestore: nothing to persist for %s", auth.ID)
	}

	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["path"] = path

	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = auth.ID
	}

	return path, nil
}

// List enumerates all auth JSON files under the configured directory.
func (s *FileTokenStore) List(ctx context.Context) ([]*cliproxyauth.Auth, error) {
	dir := s.baseDirSnapshot()
	if dir == "" {
		return nil, fmt.Errorf("auth filestore: directory not configured")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("auth filestore: resolve auth dir: %w", err)
	}
	candidates := make([]authFileCandidate, 0)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		if d.IsDir() {
			return nil
		}
		if !util.HasJSONFileName(d.Name()) {
			return nil
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			return nil
		}
		relPath, err := filepath.Rel(absDir, absPath)
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		candidates = append(candidates, authFileCandidate{
			index:   len(candidates),
			path:    path,
			relPath: relPath,
			info:    info,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	results, err := s.readAuthFileCandidates(ctx, dir, absDir, candidates)
	if err != nil {
		return nil, err
	}
	entries := make([]*cliproxyauth.Auth, 0, len(results))
	for _, auth := range results {
		if auth != nil {
			entries = append(entries, auth)
		}
	}
	return entries, nil
}

func (s *FileTokenStore) readAuthFileCandidates(ctx context.Context, dir, absDir string, candidates []authFileCandidate) ([]*cliproxyauth.Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	workerCount := authFileListWorkerCount(len(candidates))
	if workerCount <= 1 {
		root, err := os.OpenRoot(absDir)
		if err != nil {
			return nil, err
		}
		defer func() { _ = root.Close() }()

		results := make([]*cliproxyauth.Auth, len(candidates))
		for _, candidate := range candidates {
			if ctx != nil {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				default:
				}
			}
			auth, err := s.readAuthFileFromRoot(root, candidate.path, dir, candidate.relPath, candidate.info)
			if err != nil {
				return nil, err
			}
			results[candidate.index] = auth
		}
		return results, nil
	}

	ctxRead, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan authFileCandidate)
	results := make([]*cliproxyauth.Auth, len(candidates))
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	setErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		errMu.Unlock()
	}

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			root, err := os.OpenRoot(absDir)
			if err != nil {
				setErr(err)
				return
			}
			defer func() { _ = root.Close() }()

			for candidate := range jobs {
				select {
				case <-ctxRead.Done():
					return
				default:
				}
				auth, err := s.readAuthFileFromRoot(root, candidate.path, dir, candidate.relPath, candidate.info)
				if err != nil {
					setErr(err)
					return
				}
				results[candidate.index] = auth
			}
		}()
	}

sendJobs:
	for _, candidate := range candidates {
		select {
		case <-ctxRead.Done():
			break sendJobs
		case jobs <- candidate:
		}
	}
	close(jobs)
	wg.Wait()

	errMu.Lock()
	defer errMu.Unlock()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctxRead.Err(); err != nil && ctx != nil && ctx.Err() != nil {
		return nil, err
	}
	return results, nil
}

func authFileListWorkerCount(count int) int {
	if count <= 1 {
		return 1
	}
	workers := runtime.GOMAXPROCS(0) * 2
	if workers < 2 {
		workers = 2
	}
	if workers > 16 {
		workers = 16
	}
	if workers > count {
		workers = count
	}
	return workers
}

// Delete removes the auth file.
func (s *FileTokenStore) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("auth filestore: id is empty")
	}
	path, err := s.resolveDeletePath(id)
	if err != nil {
		return err
	}
	scopedPath, err := s.scopedPath(path)
	if err != nil {
		return err
	}
	if err = scopedPath.remove(); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("auth filestore: delete failed: %w", err)
	}
	return nil
}

func (s *FileTokenStore) resolveDeletePath(id string) (string, error) {
	if strings.ContainsRune(id, os.PathSeparator) || filepath.IsAbs(id) {
		return id, nil
	}
	dir := s.baseDirSnapshot()
	if dir == "" {
		return "", fmt.Errorf("auth filestore: directory not configured")
	}
	return filepath.Join(dir, id), nil
}

func (s *FileTokenStore) readAuthFile(path, baseDir string) (*cliproxyauth.Auth, error) {
	scopedPath, err := scopedAuthPath(path, baseDir)
	if err != nil {
		return nil, err
	}
	return s.readAuthFileFromScoped(scopedPath, path, baseDir, nil)
}

func (s *FileTokenStore) readAuthFileFromRoot(root *os.Root, path, baseDir, relPath string, info os.FileInfo) (*cliproxyauth.Auth, error) {
	if root == nil {
		return nil, fmt.Errorf("auth filestore: root is nil")
	}
	scopedPath := scopedAuthFilePath{
		rootDir: strings.TrimSpace(baseDir),
		relPath: relPath,
		root:    root,
	}
	return s.readAuthFileFromScoped(scopedPath, path, baseDir, info)
}

func (s *FileTokenStore) readAuthFileFromScoped(scopedPath scopedAuthFilePath, path, baseDir string, info os.FileInfo) (*cliproxyauth.Auth, error) {
	data, err := scopedPath.readFile()
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	metadata, err := cliproxyauth.DecodeAuthFileMetadata(data)
	if err != nil {
		return nil, fmt.Errorf("unmarshal auth json: %w", err)
	}
	provider, _ := metadata["type"].(string)
	if provider == "" {
		provider = "unknown"
	}
	if info == nil {
		info, err = scopedPath.stat()
		if err != nil {
			return nil, fmt.Errorf("stat file: %w", err)
		}
	}
	auth := cliproxyauth.NewAuthFromAuthFileMetadata(metadata, cliproxyauth.AuthFileProjectionOptions{
		Path:      path,
		BaseDir:   baseDir,
		CreatedAt: info.ModTime(),
		UpdatedAt: info.ModTime(),
	})
	return auth, nil
}

func (s *FileTokenStore) resolveAuthPath(auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("auth filestore: auth is nil")
	}
	if auth.Attributes != nil {
		if p := strings.TrimSpace(auth.Attributes["path"]); p != "" {
			return p, nil
		}
	}
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		if filepath.IsAbs(fileName) {
			return fileName, nil
		}
		if dir := s.baseDirSnapshot(); dir != "" {
			return filepath.Join(dir, fileName), nil
		}
		return fileName, nil
	}
	if auth.ID == "" {
		return "", fmt.Errorf("auth filestore: missing id")
	}
	if filepath.IsAbs(auth.ID) {
		return auth.ID, nil
	}
	dir := s.baseDirSnapshot()
	if dir == "" {
		return "", fmt.Errorf("auth filestore: directory not configured")
	}
	return filepath.Join(dir, auth.ID), nil
}

func (s *FileTokenStore) baseDirSnapshot() string {
	s.dirLock.RLock()
	defer s.dirLock.RUnlock()
	return s.baseDir
}

func (s *FileTokenStore) scopedPath(path string) (scopedAuthFilePath, error) {
	return scopedAuthPath(path, s.baseDirSnapshot())
}

func scopedAuthPath(path, baseDir string) (scopedAuthFilePath, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return scopedAuthFilePath{}, fmt.Errorf("auth filestore: path is empty")
	}
	baseDir = strings.TrimSpace(baseDir)
	if baseDir == "" {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return scopedAuthFilePath{}, fmt.Errorf("auth filestore: resolve path: %w", err)
		}
		return scopedAuthFilePath{
			rootDir: filepath.Dir(absPath),
			relPath: filepath.Base(absPath),
		}, nil
	}

	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return scopedAuthFilePath{}, fmt.Errorf("auth filestore: resolve auth dir: %w", err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return scopedAuthFilePath{}, fmt.Errorf("auth filestore: resolve path: %w", err)
	}
	relPath, err := filepath.Rel(absBase, absPath)
	if err != nil {
		return scopedAuthFilePath{}, fmt.Errorf("auth filestore: relate path to auth dir: %w", err)
	}
	if relPath == "." || relPath == "" || filepath.IsAbs(relPath) ||
		relPath == ".." || strings.HasPrefix(relPath, ".."+string(os.PathSeparator)) {
		return scopedAuthFilePath{}, fmt.Errorf("auth filestore: path %s is outside auth directory %s", path, baseDir)
	}
	return scopedAuthFilePath{
		rootDir: absBase,
		relPath: relPath,
	}, nil
}

func (p scopedAuthFilePath) openRoot() (*os.Root, error) {
	if p.root != nil {
		return p.root, nil
	}
	root, err := os.OpenRoot(p.rootDir)
	if err != nil {
		return nil, err
	}
	return root, nil
}

func (p scopedAuthFilePath) mkdirParent(perm os.FileMode) error {
	if err := os.MkdirAll(p.rootDir, perm); err != nil {
		return err
	}
	parent := filepath.Dir(p.relPath)
	if parent == "." || parent == "" {
		return nil
	}
	root, err := p.openRoot()
	if err != nil {
		return err
	}
	if p.root == nil {
		defer func() { _ = root.Close() }()
	}
	return root.MkdirAll(parent, perm)
}

func (p scopedAuthFilePath) readFile() ([]byte, error) {
	root, err := p.openRoot()
	if err != nil {
		return nil, err
	}
	if p.root == nil {
		defer func() { _ = root.Close() }()
	}
	return root.ReadFile(p.relPath)
}

func (p scopedAuthFilePath) writeFile(data []byte, perm os.FileMode) error {
	root, err := p.openRoot()
	if err != nil {
		return err
	}
	if p.root == nil {
		defer func() { _ = root.Close() }()
	}
	return root.WriteFile(p.relPath, data, perm)
}

func (p scopedAuthFilePath) openFile(flag int, perm os.FileMode) (*os.File, error) {
	root, err := p.openRoot()
	if err != nil {
		return nil, err
	}
	if p.root == nil {
		defer func() { _ = root.Close() }()
	}
	return root.OpenFile(p.relPath, flag, perm)
}

func (p scopedAuthFilePath) stat() (os.FileInfo, error) {
	root, err := p.openRoot()
	if err != nil {
		return nil, err
	}
	if p.root == nil {
		defer func() { _ = root.Close() }()
	}
	return root.Stat(p.relPath)
}

func (p scopedAuthFilePath) validateExistingTarget() error {
	if _, err := p.stat(); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (p scopedAuthFilePath) remove() error {
	root, err := p.openRoot()
	if err != nil {
		return err
	}
	if p.root == nil {
		defer func() { _ = root.Close() }()
	}
	return root.Remove(p.relPath)
}

// jsonEqual compares two JSON blobs by parsing them into Go objects and deep comparing.
func jsonEqual(a, b []byte) bool {
	var objA any
	var objB any
	if err := json.Unmarshal(a, &objA); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &objB); err != nil {
		return false
	}
	return deepEqualJSON(objA, objB)
}

func deepEqualJSON(a, b any) bool {
	switch valA := a.(type) {
	case map[string]any:
		valB, ok := b.(map[string]any)
		if !ok || len(valA) != len(valB) {
			return false
		}
		for key, subA := range valA {
			subB, ok1 := valB[key]
			if !ok1 || !deepEqualJSON(subA, subB) {
				return false
			}
		}
		return true
	case []any:
		sliceB, ok := b.([]any)
		if !ok || len(valA) != len(sliceB) {
			return false
		}
		for i := range valA {
			if !deepEqualJSON(valA[i], sliceB[i]) {
				return false
			}
		}
		return true
	case float64:
		valB, ok := b.(float64)
		if !ok {
			return false
		}
		return valA == valB
	case string:
		valB, ok := b.(string)
		if !ok {
			return false
		}
		return valA == valB
	case bool:
		valB, ok := b.(bool)
		if !ok {
			return false
		}
		return valA == valB
	case nil:
		return b == nil
	default:
		return false
	}
}
