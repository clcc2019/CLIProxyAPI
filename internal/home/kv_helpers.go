package home

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

func HashKeyPart(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func CurrentKVClient() (*Client, bool, error) {
	client := Current()
	if client == nil {
		return nil, false, nil
	}
	if !client.Enabled() {
		return nil, true, fmt.Errorf("home kv store unavailable: %w", ErrDisabled)
	}
	if !client.HeartbeatOK() {
		return nil, true, fmt.Errorf("home kv store unavailable: %w", ErrNotConnected)
	}
	return client, true, nil
}

func KVGetJSONRequired(ctx context.Context, key string, out any) (bool, bool, error) {
	client, homeMode, errClient := CurrentKVClient()
	if !homeMode || errClient != nil {
		return homeMode, false, errClient
	}
	raw, found, errGet := client.KVGet(ctx, key)
	if errGet != nil || !found {
		return true, false, errGet
	}
	if errUnmarshal := json.Unmarshal(raw, out); errUnmarshal != nil {
		return true, false, errUnmarshal
	}
	return true, true, nil
}

func KVSetJSONBestEffort(ctx context.Context, key string, value any, ttl time.Duration) bool {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		log.Errorf("home kv best-effort set failed prefix=%s: %v", kvLogPrefix(key), errMarshal)
		return false
	}
	client, homeMode, errClient := CurrentKVClient()
	if !homeMode {
		return false
	}
	if errClient != nil {
		log.Errorf("home kv best-effort set failed prefix=%s: %v", kvLogPrefix(key), errClient)
		return false
	}
	written, errSet := client.KVSet(ctx, key, raw, KVSetOptions{EX: ttl})
	if errSet != nil {
		log.Errorf("home kv best-effort set failed prefix=%s: %v", kvLogPrefix(key), errSet)
		return false
	}
	return written
}

func kvLogPrefix(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "unknown"
	}
	parts := strings.Split(key, ":")
	if len(parts) >= 2 {
		return parts[0] + ":" + parts[1] + ":*"
	}
	return parts[0] + ":*"
}
