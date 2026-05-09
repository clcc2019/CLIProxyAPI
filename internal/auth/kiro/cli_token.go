package kiro

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	_ "modernc.org/sqlite"
)

const (
	KiroCLIAuthKVTokenKey   = "kirocli:social:token"
	KiroCLIProfileStateKey  = "api.codewhisperer.profile"
	KiroCLIDataFileName     = "data.sqlite3"
	kiroCLIDataDirName      = "kiro-cli"
	kiroCLISocialAuthMethod = "kiro-cli-social"
	kiroCLIDataPathEnv      = "KIRO_CLI_DATA_PATH"
	kiroCLIDataDirEnv       = "KIRO_CLI_DATA_DIR"
	xdgDataHomeEnv          = "XDG_DATA_HOME"
	localAppDataEnv         = "LOCALAPPDATA"
)

var ErrKiroCLITokenNotFound = errors.New("kiro CLI token not found")

// LoadKiroCLIToken imports the local Kiro CLI social-login token.
func LoadKiroCLIToken() (*TokenData, error) {
	paths := candidateKiroCLIDataPaths()
	if len(paths) == 0 {
		return nil, ErrKiroCLITokenNotFound
	}

	var checked []string
	var firstErr error
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		checked = append(checked, path)
		token, err := LoadKiroCLITokenFromPath(path)
		if err == nil {
			return token, nil
		}
		if !errors.Is(err, ErrKiroCLITokenNotFound) {
			if firstErr == nil {
				firstErr = err
			}
			break
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	if len(checked) == 0 {
		return nil, ErrKiroCLITokenNotFound
	}
	return nil, fmt.Errorf("%w; checked: %s", ErrKiroCLITokenNotFound, strings.Join(checked, ", "))
}

// LoadKiroCLITokenFromPath imports a Kiro CLI token from data.sqlite3.
func LoadKiroCLITokenFromPath(dbPath string) (*TokenData, error) {
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" {
		return nil, ErrKiroCLITokenNotFound
	}
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrKiroCLITokenNotFound, dbPath)
		}
		return nil, fmt.Errorf("kiro: failed to stat Kiro CLI database (%s): %w", dbPath, err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("kiro: failed to open Kiro CLI database: %w", err)
	}
	defer func() { _ = db.Close() }()

	var rawToken string
	if err := db.QueryRow(`select value from auth_kv where key = ?`, KiroCLIAuthKVTokenKey).Scan(&rawToken); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrKiroCLITokenNotFound, KiroCLIAuthKVTokenKey)
		}
		return nil, fmt.Errorf("kiro: failed to read Kiro CLI token: %w", err)
	}

	token, err := ParseTokenData([]byte(rawToken))
	if err != nil {
		return nil, fmt.Errorf("kiro: failed to parse Kiro CLI token: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return nil, fmt.Errorf("kiro: access token is empty in Kiro CLI token")
	}
	token.AuthMethod = kiroCLISocialAuthMethod
	if strings.TrimSpace(token.Provider) == "" {
		token.Provider = "kiro-cli"
	}
	if strings.TrimSpace(token.ProfileArn) == "" {
		token.ProfileArn = loadKiroCLIProfileArn(db)
	}
	if strings.TrimSpace(token.Email) == "" {
		token.Email = ExtractEmailFromJWT(token.AccessToken)
	}
	return token, nil
}

func loadKiroCLIProfileArn(db *sql.DB) string {
	if db == nil {
		return ""
	}
	var rawProfile []byte
	if err := db.QueryRow(`select value from state where key = ?`, KiroCLIProfileStateKey).Scan(&rawProfile); err != nil {
		return ""
	}
	var profile struct {
		Arn        string `json:"arn"`
		ProfileArn string `json:"profileArn"`
	}
	if err := json.Unmarshal(rawProfile, &profile); err != nil {
		return ""
	}
	return firstNonEmpty(profile.ProfileArn, profile.Arn)
}

func candidateKiroCLIDataPaths() []string {
	var candidates []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		for _, existing := range candidates {
			if existing == path {
				return
			}
		}
		candidates = append(candidates, path)
	}

	add(os.Getenv(kiroCLIDataPathEnv))
	if dir := strings.TrimSpace(os.Getenv(kiroCLIDataDirEnv)); dir != "" {
		add(filepath.Join(dir, KiroCLIDataFileName))
	}

	if xdgDataHome := strings.TrimSpace(os.Getenv(xdgDataHomeEnv)); xdgDataHome != "" {
		add(filepath.Join(xdgDataHome, kiroCLIDataDirName, KiroCLIDataFileName))
	}

	homeDir, err := os.UserHomeDir()
	if err == nil && strings.TrimSpace(homeDir) != "" {
		add(filepath.Join(homeDir, ".local", "share", kiroCLIDataDirName, KiroCLIDataFileName))
		if runtime.GOOS == "darwin" {
			add(filepath.Join(homeDir, "Library", "Application Support", kiroCLIDataDirName, KiroCLIDataFileName))
		}
	}

	if runtime.GOOS == "windows" {
		if localAppData := strings.TrimSpace(os.Getenv(localAppDataEnv)); localAppData != "" {
			add(filepath.Join(localAppData, kiroCLIDataDirName, KiroCLIDataFileName))
		}
	}

	return candidates
}
