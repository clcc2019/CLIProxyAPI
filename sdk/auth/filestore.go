package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

	if auth.Disabled {
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
	root, err := os.OpenRoot(absDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	entries := make([]*cliproxyauth.Auth, 0)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
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
		auth, err := s.readAuthFileFromRoot(root, path, dir, relPath, info)
		if err != nil {
			return nil
		}
		if auth != nil {
			entries = append(entries, auth)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
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
	if provider == "antigravity" || provider == "gemini" {
		projectID := ""
		if pid, ok := metadata["project_id"].(string); ok {
			projectID = strings.TrimSpace(pid)
		}
		if projectID == "" {
			accessToken := extractAccessToken(metadata)
			// For gemini type, the stored access_token is likely expired (~1h lifetime).
			// Refresh it using the long-lived refresh_token before querying.
			if provider == "gemini" {
				if tokenMap, ok := metadata["token"].(map[string]any); ok {
					if refreshed, errRefresh := refreshGeminiAccessToken(tokenMap, http.DefaultClient); errRefresh == nil {
						accessToken = refreshed
					}
				}
			}
			if accessToken != "" {
				fetchedProjectID, errFetch := FetchAntigravityProjectID(context.Background(), accessToken, http.DefaultClient)
				if errFetch == nil && strings.TrimSpace(fetchedProjectID) != "" {
					metadata["project_id"] = strings.TrimSpace(fetchedProjectID)
					if raw, errMarshal := json.Marshal(metadata); errMarshal == nil {
						if file, errOpen := scopedPath.openFile(os.O_WRONLY|os.O_TRUNC, 0o600); errOpen == nil {
							_, _ = file.Write(raw)
							_ = file.Close()
						}
					}
				}
			}
		}
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

func extractAccessToken(metadata map[string]any) string {
	if at, ok := metadata["access_token"].(string); ok {
		if v := strings.TrimSpace(at); v != "" {
			return v
		}
	}
	if tokenMap, ok := metadata["token"].(map[string]any); ok {
		if at, ok := tokenMap["access_token"].(string); ok {
			if v := strings.TrimSpace(at); v != "" {
				return v
			}
		}
	}
	return ""
}

func refreshGeminiAccessToken(tokenMap map[string]any, httpClient *http.Client) (string, error) {
	refreshToken, _ := tokenMap["refresh_token"].(string)
	clientID, _ := tokenMap["client_id"].(string)
	clientSecret, _ := tokenMap["client_secret"].(string)
	tokenURI, _ := tokenMap["token_uri"].(string)

	if refreshToken == "" || clientID == "" || clientSecret == "" {
		return "", fmt.Errorf("missing refresh credentials")
	}
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}

	data := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}

	resp, err := httpClient.PostForm(tokenURI, data)
	if err != nil {
		return "", fmt.Errorf("refresh request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := util.ReadResponseBody(resp.Body)
	if errRead != nil {
		return "", fmt.Errorf("read refresh response: %w", errRead)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("refresh failed: status %d", resp.StatusCode)
	}

	var result map[string]any
	if errUnmarshal := json.Unmarshal(body, &result); errUnmarshal != nil {
		return "", fmt.Errorf("decode refresh response: %w", errUnmarshal)
	}

	newAccessToken, _ := result["access_token"].(string)
	if newAccessToken == "" {
		return "", fmt.Errorf("no access_token in refresh response")
	}

	tokenMap["access_token"] = newAccessToken
	return newAccessToken, nil
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
