package kiro

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	KiroIDETokenFile       = ".aws/sso/cache/kiro-auth-token.json"
	KiroIDETokenLegacyFile = ".kiro/kiro-auth-token.json"
)

type TokenData struct {
	AccessToken  string
	RefreshToken string
	ProfileArn   string
	ExpiresAt    string
	AuthMethod   string
	Provider     string
	ClientID     string
	ClientSecret string
	ClientIDHash string
	Email        string
	StartURL     string
	Region       string
	MachineID    string
}

type tokenWire struct {
	AccessToken       string `json:"accessToken"`
	AccessTokenLegacy string `json:"access_token"`
	RefreshToken      string `json:"refreshToken"`
	RefreshTokenOld   string `json:"refresh_token"`
	ProfileArn        string `json:"profileArn"`
	ProfileArnOld     string `json:"profile_arn"`
	ExpiresAt         string `json:"expiresAt"`
	ExpiresAtOld      string `json:"expires_at"`
	AuthMethod        string `json:"authMethod"`
	AuthMethodOld     string `json:"auth_method"`
	Provider          string `json:"provider"`
	ClientID          string `json:"clientId"`
	ClientIDOld       string `json:"client_id"`
	ClientSecret      string `json:"clientSecret"`
	ClientSecretOld   string `json:"client_secret"`
	ClientIDHash      string `json:"clientIdHash"`
	ClientIDHashOld   string `json:"client_id_hash"`
	Email             string `json:"email"`
	StartURL          string `json:"startUrl"`
	StartURLOld       string `json:"start_url"`
	Region            string `json:"region"`
	MachineID         string `json:"machineId"`
	MachineIDOld      string `json:"machine_id"`
	DeviceID          string `json:"deviceId"`
	DeviceIDOld       string `json:"device_id"`
}

func LoadKiroIDEToken() (*TokenData, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("kiro: failed to get home directory: %w", err)
	}

	data, tokenPath, err := readKiroIDETokenFile(homeDir)
	if err != nil {
		return nil, err
	}
	token, err := ParseTokenData(data)
	if err != nil {
		return nil, fmt.Errorf("kiro: failed to parse Kiro IDE token (%s): %w", tokenPath, err)
	}
	if token.AccessToken == "" {
		return nil, fmt.Errorf("kiro: access token is empty in Kiro IDE token file")
	}
	token.AuthMethod = strings.ToLower(strings.TrimSpace(token.AuthMethod))
	if token.Email == "" {
		token.Email = ExtractEmailFromJWT(token.AccessToken)
	}
	if token.ClientIDHash != "" && token.ClientID == "" {
		_ = loadDeviceRegistration(homeDir, token.ClientIDHash, token)
	}
	return token, nil
}

func ParseTokenData(data []byte) (*TokenData, error) {
	var wire tokenWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	return &TokenData{
		AccessToken:  firstNonEmpty(wire.AccessToken, wire.AccessTokenLegacy),
		RefreshToken: firstNonEmpty(wire.RefreshToken, wire.RefreshTokenOld),
		ProfileArn:   firstNonEmpty(wire.ProfileArn, wire.ProfileArnOld),
		ExpiresAt:    firstNonEmpty(wire.ExpiresAt, wire.ExpiresAtOld),
		AuthMethod:   firstNonEmpty(wire.AuthMethod, wire.AuthMethodOld),
		Provider:     strings.TrimSpace(wire.Provider),
		ClientID:     firstNonEmpty(wire.ClientID, wire.ClientIDOld),
		ClientSecret: firstNonEmpty(wire.ClientSecret, wire.ClientSecretOld),
		ClientIDHash: firstNonEmpty(wire.ClientIDHash, wire.ClientIDHashOld),
		Email:        strings.TrimSpace(wire.Email),
		StartURL:     firstNonEmpty(wire.StartURL, wire.StartURLOld),
		Region:       strings.TrimSpace(wire.Region),
		MachineID:    firstNonEmpty(wire.MachineID, wire.MachineIDOld, wire.DeviceID, wire.DeviceIDOld),
	}, nil
}

func readKiroIDETokenFile(homeDir string) ([]byte, string, error) {
	candidates := []string{
		filepath.Join(homeDir, KiroIDETokenFile),
		filepath.Join(homeDir, KiroIDETokenLegacyFile),
	}
	errs := make([]string, 0, len(candidates))
	for _, tokenPath := range candidates {
		data, err := os.ReadFile(tokenPath)
		if err == nil {
			return data, tokenPath, nil
		}
		if os.IsNotExist(err) {
			errs = append(errs, tokenPath+" (not found)")
			continue
		}
		return nil, "", fmt.Errorf("kiro: failed to read Kiro IDE token file (%s): %w", tokenPath, err)
	}
	return nil, "", fmt.Errorf("kiro: failed to read Kiro IDE token file; checked: %s", strings.Join(errs, ", "))
}

func loadDeviceRegistration(homeDir, clientIDHash string, token *TokenData) error {
	if clientIDHash == "" {
		return fmt.Errorf("clientIdHash is empty")
	}
	if strings.Contains(clientIDHash, "/") || strings.Contains(clientIDHash, "\\") || strings.Contains(clientIDHash, "..") {
		return fmt.Errorf("invalid clientIdHash")
	}
	deviceRegPath := filepath.Join(homeDir, ".aws", "sso", "cache", clientIDHash+".json")
	data, err := os.ReadFile(deviceRegPath)
	if err != nil {
		return err
	}
	var deviceReg struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.Unmarshal(data, &deviceReg); err != nil {
		return err
	}
	token.ClientID = strings.TrimSpace(deviceReg.ClientID)
	token.ClientSecret = strings.TrimSpace(deviceReg.ClientSecret)
	return nil
}

func ExtractEmailFromJWT(accessToken string) string {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		decoded, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}
	var claims struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		Sub               string `json:"sub"`
	}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return ""
	}
	for _, candidate := range []string{claims.Email, claims.PreferredUsername, claims.Sub} {
		if strings.Contains(candidate, "@") {
			return candidate
		}
	}
	return ""
}

func SanitizeEmailForFilename(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	replacer := strings.NewReplacer(
		"%2F", "_", "%2f", "_", "%5C", "_", "%5c", "_",
		"%2E", "_", "%2e", "_", "%00", "_", "%", "_",
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_", " ", "_", "\x00", "_",
	)
	value = replacer.Replace(value)
	parts := strings.Split(value, "_")
	for i, part := range parts {
		for strings.HasPrefix(part, ".") {
			part = "_" + part[1:]
		}
		parts[i] = part
	}
	return strings.Join(parts, "_")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
