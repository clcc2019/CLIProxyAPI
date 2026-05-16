// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// DisableImageGeneration controls whether the built-in image_generation tool is injected/allowed.
	//
	// Supported values:
	//   - false (default): image_generation is enabled everywhere (normal behavior).
	//   - true: image_generation is disabled everywhere. The server stops injecting it, removes it from request payloads,
	//     and returns 404 for /v1/images/generations, /v1/images/edits, and /v1/images/variations.
	//   - "chat": disable image_generation injection for all non-images endpoints (e.g. /v1/responses, /v1/chat/completions),
	//     while keeping /v1/images/generations, /v1/images/edits, and /v1/images/variations enabled and preserving image_generation there.
	DisableImageGeneration DisableImageGenerationMode `yaml:"disable-image-generation" json:"disable-image-generation"`

	// EnableGeminiCLIEndpoint controls whether Gemini CLI internal endpoints (/v1internal:*) are enabled.
	// Default is false for safety; when false, /v1internal:* requests are rejected.
	EnableGeminiCLIEndpoint bool `yaml:"enable-gemini-cli-endpoint" json:"enable-gemini-cli-endpoint"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// APIKeys is the list of keys for authenticating clients to this proxy server.
	// Entries may be plain strings for the legacy format or structured objects with
	// model restrictions and USD spend quota limits.
	APIKeys ClientAPIKeys `yaml:"api-keys" json:"api-keys"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// OAuthRefresh controls background refresh behavior for OAuth / auth-file credentials.
	OAuthRefresh OAuthRefreshConfig `yaml:"oauth-refresh" json:"oauth-refresh"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`

	// ImageStreamDataIntervalTimeoutSeconds controls how long image SSE streams may wait without upstream data.
	// <= 0 disables image stream idle timeouts. Value is in seconds.
	ImageStreamDataIntervalTimeoutSeconds int `yaml:"image-stream-data-interval-timeout-seconds,omitempty" json:"image-stream-data-interval-timeout-seconds,omitempty"`

	// ImageStreamKeepAliveSeconds controls how often image SSE streams emit ":" heartbeats while idle.
	// <= 0 disables image stream keep-alives. Value is in seconds.
	ImageStreamKeepAliveSeconds int `yaml:"image-stream-keepalive-seconds,omitempty" json:"image-stream-keepalive-seconds,omitempty"`

	// Limits configures server-side resource limits and backpressure.
	Limits LimitsConfig `yaml:"limits,omitempty" json:"limits,omitempty"`
}

// LimitsConfig configures server-side resource limits.
type LimitsConfig struct {
	// MaxInFlightRequests caps how many requests the proxy will serve concurrently.
	// 0 (default) disables the limiter, preserving legacy behavior. When set,
	// excess requests fail fast with HTTP 503 + Retry-After rather than
	// queuing inside the proxy. A reasonable production value is 256–1024
	// depending on upstream concurrency budget and available memory.
	MaxInFlightRequests int `yaml:"max-in-flight-requests,omitempty" json:"max-in-flight-requests,omitempty"`

	// RetryAfterSeconds is the value advertised in the Retry-After header on a
	// 503 from the concurrency limiter. 0 disables the header.
	RetryAfterSeconds int `yaml:"retry-after-seconds,omitempty" json:"retry-after-seconds,omitempty"`
}

// OAuthRefreshConfig controls background refresh scheduling for OAuth/file-backed auths.
type OAuthRefreshConfig struct {
	// Enabled controls whether the background auto-refresh loop runs for OAuth / auth-file credentials.
	// Nil preserves the new default (disabled).
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`

	// OnStartup controls whether the auto-refresh loop immediately checks credentials when the service starts.
	// Nil preserves the legacy default (enabled) once auto-refresh itself is enabled.
	OnStartup *bool `yaml:"on-startup,omitempty" json:"on-startup,omitempty"`

	// BatchSize limits how many due credentials may be dispatched in one scheduler pass.
	// Additional due credentials are drained in short follow-up batches.
	// <= 0 preserves the legacy behavior (no per-pass cap).
	BatchSize int `yaml:"batch-size,omitempty" json:"batch-size,omitempty"`
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n").
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`
}
