package executor

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

const (
	codexAgentIdentityTaskRegistrationBaseURL = cliproxyauth.CodexAgentIdentityProductionAuthAPIBaseURL
	codexAgentIdentityTaskRegistrationTimeout = 30 * time.Second
	codexAgentIdentityTaskResponseMaxBytes    = 64 << 10
	codexAgentIdentityRegistrationMaxAttempts = 3
)

// This variable is intentionally package-scoped so the registration protocol
// can be exercised against an httptest server without changing production
// configuration or allowing credential files to redirect private-key proofs.
var codexAgentIdentityTaskRegistrationURL = codexAgentIdentityTaskRegistrationBaseURL

// Task state is shared by the HTTP and websocket executor instances. The auto
// executor owns one of each, and both can receive stale auth snapshots while a
// concurrent request is rotating the same task.
var codexAgentIdentityTasks sync.Map

type codexAgentIdentityKey struct {
	runtimeID  string
	privateKey ed25519.PrivateKey
	taskID     string
}

type codexAgentIdentityTaskRegistrationResponse struct {
	TaskID               string `json:"task_id"`
	TaskIDCamel          string `json:"taskId"`
	EncryptedTaskID      string `json:"encrypted_task_id"`
	EncryptedTaskIDCamel string `json:"encryptedTaskId"`
}

type codexAgentIdentityTaskState struct {
	mu     sync.Mutex
	taskID string
}

type codexReplayReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *codexReplayReadCloser) Close() error {
	if r == nil || r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

func peekCodexAgentIdentityErrorBody(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, nil
	}
	original := resp.Body
	prefix, err := io.ReadAll(io.LimitReader(original, helps.MaxErrorResponseBodyBytes))
	if err != nil {
		return nil, err
	}
	resp.Body = &codexReplayReadCloser{
		Reader: io.MultiReader(bytes.NewReader(prefix), original),
		closer: original,
	}
	return prefix, nil
}

func codexIsAgentIdentityAuth(auth *cliproxyauth.Auth) bool {
	return cliproxyauth.CodexAuthUsesAgentIdentity(auth)
}

func codexAgentIdentityPrivateKey(auth *cliproxyauth.Auth) (ed25519.PrivateKey, error) {
	if auth == nil {
		return nil, errors.New("codex agent identity auth is nil")
	}
	raw := strings.TrimSpace(metadataString(auth.Metadata, "agent_private_key", "agentPrivateKey"))
	if raw == "" {
		return nil, errors.New("codex agent identity private key is missing")
	}
	der, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("codex agent identity private key is not valid base64")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, errors.New("codex agent identity private key is not valid PKCS#8")
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("codex agent identity private key is not Ed25519")
	}
	return privateKey, nil
}

func codexAgentIdentityKeyFromAuth(auth *cliproxyauth.Auth) (codexAgentIdentityKey, error) {
	privateKey, err := codexAgentIdentityPrivateKey(auth)
	if err != nil {
		return codexAgentIdentityKey{}, err
	}
	runtimeID := strings.TrimSpace(metadataString(auth.Metadata, "agent_runtime_id", "agentRuntimeId", "agentRuntimeID"))
	if runtimeID == "" {
		return codexAgentIdentityKey{}, errors.New("codex agent identity runtime id is missing")
	}
	return codexAgentIdentityKey{
		runtimeID:  runtimeID,
		privateKey: privateKey,
		taskID:     codexAgentIdentityTaskID(auth),
	}, nil
}

func codexAgentIdentityTaskID(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	return strings.TrimSpace(metadataString(auth.Metadata, "task_id", "taskId"))
}

func buildCodexAgentAssertion(key codexAgentIdentityKey, now time.Time) (string, error) {
	if key.runtimeID == "" || key.taskID == "" {
		return "", errors.New("codex agent identity runtime or task id is missing")
	}
	timestamp := now.UTC().Format(time.RFC3339)
	signingText := []byte(key.runtimeID + ":" + key.taskID + ":" + timestamp)
	signature, err := key.privateKey.Sign(nil, signingText, crypto.Hash(0))
	if err != nil {
		return "", errors.New("failed to sign codex agent assertion")
	}
	envelope := map[string]string{
		"agent_runtime_id": key.runtimeID,
		"task_id":          key.taskID,
		"timestamp":        timestamp,
		"signature":        base64.StdEncoding.EncodeToString(signature),
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", errors.New("failed to serialize codex agent assertion")
	}
	return "AgentAssertion " + base64.RawURLEncoding.EncodeToString(encoded), nil
}

func signCodexAgentTaskRegistration(key codexAgentIdentityKey, now time.Time) (timestamp, signature string, err error) {
	if key.runtimeID == "" {
		return "", "", errors.New("codex agent identity runtime id is missing")
	}
	timestamp = now.UTC().Format(time.RFC3339)
	signed, err := key.privateKey.Sign(nil, []byte(key.runtimeID+":"+timestamp), crypto.Hash(0))
	if err != nil {
		return "", "", errors.New("failed to sign codex agent task registration")
	}
	return timestamp, base64.StdEncoding.EncodeToString(signed), nil
}

func decryptCodexAgentTaskID(key codexAgentIdentityKey, encoded string) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", errors.New("encrypted codex agent task id is not valid base64")
	}
	digest := sha512.Sum512(key.privateKey.Seed())
	var curvePrivate [32]byte
	copy(curvePrivate[:], digest[:32])
	curvePrivate[0] &= 248
	curvePrivate[31] &= 127
	curvePrivate[31] |= 64
	curvePublicBytes, err := curve25519.X25519(curvePrivate[:], curve25519.Basepoint)
	if err != nil {
		return "", errors.New("failed to derive codex agent identity decryption key")
	}
	var curvePublic [32]byte
	copy(curvePublic[:], curvePublicBytes)
	plaintext, ok := box.OpenAnonymous(nil, ciphertext, &curvePublic, &curvePrivate)
	if !ok {
		return "", errors.New("failed to decrypt encrypted codex agent task id")
	}
	taskID := strings.TrimSpace(string(plaintext))
	if taskID == "" {
		return "", errors.New("decrypted codex agent task id is empty")
	}
	return taskID, nil
}

func (e *CodexExecutor) registerCodexAgentIdentityTask(ctx context.Context, auth *cliproxyauth.Auth, key codexAgentIdentityKey) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, codexAgentIdentityTaskRegistrationTimeout)
	defer cancel()
	registrationBaseURL := codexAgentIdentityTaskRegistrationBaseURLForAuth(auth)
	registrationURL := strings.TrimRight(registrationBaseURL, "/") + "/v1/agent/" + url.PathEscape(key.runtimeID) + "/task/register"
	client := helps.NewCodexHTTPClient(requestCtx, e.cfg, auth, 0)
	for attempt := 1; attempt <= codexAgentIdentityRegistrationMaxAttempts; attempt++ {
		timestamp, signature, err := signCodexAgentTaskRegistration(key, time.Now())
		if err != nil {
			return "", err
		}
		body, err := json.Marshal(map[string]string{"timestamp": timestamp, "signature": signature})
		if err != nil {
			return "", errors.New("failed to serialize codex agent task registration")
		}
		req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, registrationURL, bytes.NewReader(body))
		if err != nil {
			return "", errors.New("failed to build codex agent task registration request")
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			if attempt < codexAgentIdentityRegistrationMaxAttempts && requestCtx.Err() == nil {
				continue
			}
			return "", errors.New("codex agent task registration request failed")
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			statusCode := resp.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, codexAgentIdentityTaskResponseMaxBytes))
			_ = resp.Body.Close()
			if attempt < codexAgentIdentityRegistrationMaxAttempts && codexAgentIdentityRegistrationRetryableStatus(statusCode) {
				continue
			}
			return "", fmt.Errorf("codex agent task registration returned status %d", statusCode)
		}
		limited := io.LimitReader(resp.Body, codexAgentIdentityTaskResponseMaxBytes+1)
		responseBody, readErr := io.ReadAll(limited)
		_ = resp.Body.Close()
		if readErr != nil || len(responseBody) > codexAgentIdentityTaskResponseMaxBytes {
			return "", errors.New("codex agent task registration response is invalid")
		}
		var result codexAgentIdentityTaskRegistrationResponse
		if err := json.Unmarshal(responseBody, &result); err != nil {
			return "", errors.New("codex agent task registration response is invalid")
		}
		for _, taskID := range []string{result.TaskID, result.TaskIDCamel} {
			if taskID = strings.TrimSpace(taskID); taskID != "" {
				return taskID, nil
			}
		}
		encrypted := strings.TrimSpace(result.EncryptedTaskID)
		if encrypted == "" {
			encrypted = strings.TrimSpace(result.EncryptedTaskIDCamel)
		}
		if encrypted == "" {
			return "", errors.New("codex agent task registration response omitted task id")
		}
		return decryptCodexAgentTaskID(key, encrypted)
	}
	return "", errors.New("codex agent task registration request failed")
}

func codexAgentIdentityTaskRegistrationBaseURLForAuth(auth *cliproxyauth.Auth) string {
	if override := strings.TrimSpace(codexAgentIdentityTaskRegistrationURL); override != "" && override != codexAgentIdentityTaskRegistrationBaseURL {
		return override
	}
	return cliproxyauth.CodexAgentIdentityAuthAPIBaseURL(auth)
}

func codexAgentIdentityRegistrationRetryableStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError
}

func (e *CodexExecutor) codexAgentIdentityState(auth *cliproxyauth.Auth, key codexAgentIdentityKey) *codexAgentIdentityTaskState {
	publicKey := key.privateKey.Public().(ed25519.PublicKey)
	fingerprint := sha256.Sum256(append([]byte(key.runtimeID+"\x00"), publicKey...))
	stateKey := fmt.Sprintf("%s\x00%x", strings.TrimSpace(auth.ID), fingerprint)
	state := &codexAgentIdentityTaskState{}
	actual, _ := codexAgentIdentityTasks.LoadOrStore(stateKey, state)
	return actual.(*codexAgentIdentityTaskState)
}

func (e *CodexExecutor) ensureCodexAgentIdentityTask(ctx context.Context, auth *cliproxyauth.Auth, expectedTaskID string) (string, bool, error) {
	if !codexIsAgentIdentityAuth(auth) {
		return "", false, nil
	}
	key, err := codexAgentIdentityKeyFromAuth(auth)
	if err != nil {
		return "", false, err
	}
	state := e.codexAgentIdentityState(auth, key)
	state.mu.Lock()
	defer state.mu.Unlock()

	currentTaskID := codexAgentIdentityTaskID(auth)
	if state.taskID == "" && currentTaskID != "" {
		state.taskID = currentTaskID
	}
	if state.taskID != "" && (expectedTaskID == "" || state.taskID != expectedTaskID) {
		codexSetAgentIdentityTaskID(auth, state.taskID)
		return state.taskID, false, nil
	}
	if currentTaskID != "" && (expectedTaskID == "" || currentTaskID != expectedTaskID) {
		state.taskID = currentTaskID
		return currentTaskID, false, nil
	}

	newTaskID, err := e.registerCodexAgentIdentityTask(ctx, auth, key)
	if err != nil {
		return "", false, err
	}
	state.taskID = newTaskID
	codexSetAgentIdentityTaskID(auth, newTaskID)
	cliproxyauth.PublishAuthUpdate(ctx, auth)
	CloseCodexWebsocketSessionsForAuthID(auth.ID, "agent_identity_task_rotated")
	return newTaskID, true, nil
}

func codexSetAgentIdentityTaskID(auth *cliproxyauth.Auth, taskID string) {
	if auth == nil {
		return
	}
	metadata := make(map[string]any, len(auth.Metadata)+1)
	for key, value := range auth.Metadata {
		metadata[key] = value
	}
	metadata["task_id"] = strings.TrimSpace(taskID)
	delete(metadata, "taskId")
	auth.Metadata = metadata
}

func (e *CodexExecutor) codexAuthorization(ctx context.Context, auth *cliproxyauth.Auth, token string) (string, error) {
	if !codexIsAgentIdentityAuth(auth) {
		token = strings.TrimSpace(token)
		if token == "" {
			return "", nil
		}
		return "Bearer " + token, nil
	}
	taskID, _, err := e.ensureCodexAgentIdentityTask(ctx, auth, "")
	if err != nil {
		return "", err
	}
	key, err := codexAgentIdentityKeyFromAuth(auth)
	if err != nil {
		return "", err
	}
	key.taskID = taskID
	return buildCodexAgentAssertion(key, time.Now())
}

func isCodexAgentIdentityTaskInvalid(statusCode int, body []byte) bool {
	if statusCode != http.StatusUnauthorized {
		return false
	}
	lower := strings.ToLower(string(body))
	compact := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(lower)
	for _, marker := range []string{
		`"code":"invalid_task_id"`,
		`"code":"task_not_found"`,
		`"code":"task_expired"`,
		`"error":"invalid_task_id"`,
	} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	for _, marker := range []string{
		"invalid task_id", "invalid task id", "task_id is invalid", "task id is invalid",
		"task not found", "task expired", "unknown task_id", "unknown task id",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func (e *CodexExecutor) recoverCodexAgentIdentityTask(ctx context.Context, auth *cliproxyauth.Auth, statusCode int, body []byte) (bool, error) {
	if !codexIsAgentIdentityAuth(auth) || !isCodexAgentIdentityTaskInvalid(statusCode, body) {
		return false, nil
	}
	expectedTaskID := codexAgentIdentityTaskID(auth)
	if expectedTaskID == "" {
		return false, nil
	}
	_, _, err := e.ensureCodexAgentIdentityTask(ctx, auth, expectedTaskID)
	if err != nil {
		return false, err
	}
	return true, nil
}
