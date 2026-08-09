# Management API reference

The Management API controls live/persisted CLIProxyAPI state. It is separate from the public client API and uses a different secret.

## Availability and authentication

All routes use `/v0/management` and require either:

```text
Authorization: Bearer <management-key>
X-Management-Key: <management-key>
```

Use `CLIPROXY_MANAGEMENT_KEY`; never substitute `CLIPROXY_API_KEY` unless the deployment owner explicitly configured the same value.

Management routes return 404 when `remote-management.secret-key` and `MANAGEMENT_PASSWORD` are absent, when management routes are disabled, or when Home mode is active. `remote-management.allow-remote` defaults to false. Localhost still requires a key. `MANAGEMENT_PASSWORD` enables remote management as an override.

Five failed key attempts from one IP cause a 30-minute ban. Never brute-force or loop on 401/403.

```bash
export CLIPROXY_BASE_URL="${CLIPROXY_BASE_URL:-http://127.0.0.1:8317}"
MGMT="$CLIPROXY_BASE_URL/v0/management"

curl -sS "$MGMT/usage" \
  -H "Authorization: Bearer $CLIPROXY_MANAGEMENT_KEY"
```

Management responses include `X-CPA-VERSION`, `X-CPA-COMMIT`, and `X-CPA-BUILD-DATE`, which are useful when behavior differs by build.

## Safe agent workflow

1. Confirm the base URL is localhost unless the user intentionally configured remote management.
2. Start with the narrowest read-only endpoint; avoid fetching `/config` or `/config.yaml` merely to discover one setting because they can contain credentials.
3. For a mutation, GET the narrow setting first, execute one write, then GET it again.
4. Redact management keys, auth-file contents, upstream tokens, full configuration, and sensitive logs from output.
5. Treat full config replacement, credential upload/delete, log deletion, OAuth completion, and `/api-call` as high impact. State the exact target and preserve a recoverable snapshot when practical.

## Route catalog

### Usage and version

- `GET /usage`
- `GET /usage/details?recent=N&compact=true|false`
- `GET /usage/aggregated`
- `GET /usage/export`
- `GET /usage/export/details`
- `POST /usage/import`
- `GET /usage-queue?count=N` — destructive read: pops queue records
- `GET /latest-version`

`/usage` returns a summary under `usage`. Detailed views cap dashboard history; exports are intended for backup/forensics. Import accepts export versions 0–3 and merges or replaces data according to `source_id`.

### Configuration

- `GET /config`
- `GET /config.yaml`
- `PUT /config.yaml`
- `GET|PUT|PATCH /debug`
- `GET|PUT|PATCH /logging-to-file`
- `GET|PUT|PATCH /logs-max-total-size-mb`
- `GET|PUT|PATCH /error-logs-max-files`
- `GET|PUT|PATCH /usage-statistics-enabled`
- `GET|PUT|PATCH /usage-detail-retention-limit`
- `GET|PUT|PATCH /model-prices`
- `GET|PUT|PATCH|DELETE /proxy-url`
- `GET|PUT|PATCH /request-log`
- `GET|PUT|PATCH /ws-auth`
- `GET|PUT|PATCH /request-retry`
- `GET|PUT|PATCH /max-retry-interval`
- `GET|PUT|PATCH /force-model-prefix`
- `GET|PUT|PATCH /routing/strategy`
- `GET|PUT|PATCH|DELETE /api-keys`
- `GET /api-key-usage`
- `GET|PUT|PATCH|DELETE /claude-api-key`
- `GET|PUT|PATCH|DELETE /codex-api-key`
- `GET|PUT|PATCH|DELETE /openai-compatibility`
- `GET|PUT|PATCH|DELETE /oauth-excluded-models`
- `GET|PUT|PATCH|DELETE /oauth-model-alias`

Simple boolean, integer, and string setters accept a value wrapper:

```bash
curl -sS -X PUT "$MGMT/request-log" \
  -H "Authorization: Bearer $CLIPROXY_MANAGEMENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"value":true}'
```

List/object endpoints have route-specific bodies. Read the current value and inspect the matching handler in `internal/api/handlers/management/` before constructing an update. Do not infer PUT versus PATCH semantics from another endpoint.

`PUT /config.yaml` replaces the complete configuration with raw YAML, validates it, writes it with mode 0600, and reloads it. Preserve the existing file/comments and avoid this endpoint for a one-field edit.

### Logs

- `GET /logs?after=<cursor>&limit=N`
- `DELETE /logs`
- `GET /request-error-logs`
- `GET /request-error-logs/:name`
- `GET /request-log-by-id/:id`

Log bodies can contain prompts, responses, identifiers, and headers. Request logging attempts to redact secrets, but treat all returned material as sensitive. Do not delete logs unless explicitly requested.

### Auth files, models, and quota

- `GET /auth-files`
- `GET /auth-files/models?name=<file-or-id>`
- `GET /auth-files/codex-usage`
- `GET /auth-files/codex-rate-limit-reset-credits`
- `POST /auth-files/codex-rate-limit-reset-credits/consume`
- `GET /model-definitions/:channel`
- `GET /auth-files/preview?name=<file.json>`
- `GET /auth-files/download?name=<file.json>`
- `POST /auth-files?name=<file.json>` with raw JSON, or multipart file upload
- `DELETE /auth-files?name=<file.json>`; `?all=true` targets all files
- `PATCH /auth-files/status` with `{"name":"file.json","disabled":true}`
- `PATCH /auth-files/fields` with `name` plus supported editable fields
- `PATCH /auth-files/codex-auth-mode`

Prefer `/preview` over `/download` when inspection is enough; preview omits internal runtime metadata. Never reproduce auth-file contents in the final response. For deletion, resolve and display the exact filename; `all=true` is a broad destructive target.

### OAuth

- `GET /anthropic-auth-url`
- `GET /codex-auth-url` — device login by default; `?mode=browser` selects browser callback
- `GET /xai-auth-url`
- `POST /oauth-callback`
- `GET /get-auth-status?state=<state>`

Typical flow:

1. GET the provider auth URL endpoint.
2. Keep the returned `state`; open the returned URL for the user. Codex device mode may also return a user code and verification URL.
3. Let the provider redirect to `/anthropic/callback`, `/codex/callback`, or `/xai/callback`, or POST a received callback to `/oauth-callback`.
4. Poll `/get-auth-status?state=...` at a modest interval until `ok` or `error`; pending is `wait`.
5. Confirm the new credential through `/auth-files` without exposing tokens.

Do not automate entering passwords, MFA, or consent unless the user explicitly asks for browser automation. OAuth state is security-sensitive and expires.

Manual callback body accepts explicit fields or a redirect URL:

```json
{
  "provider": "codex",
  "redirect_url": "http://localhost/codex/callback?code=...&state=...",
  "state": "optional-when-present-in-redirect",
  "code": "optional-when-present-in-redirect",
  "error": "optional"
}
```

### Credential-aware upstream test call

`POST /api-call` performs an arbitrary outbound HTTP request and can substitute a selected stored credential token. Request fields are:

```json
{
  "auth_index": "AUTH_INDEX_FROM_AUTH_FILES",
  "provider": "optional-provider-hint",
  "method": "GET",
  "url": "https://api.example.com/v1/ping",
  "header": {"Authorization": "Bearer $TOKEN$"},
  "data": ""
}
```

The response contains `status_code`, `header`, and `body`. Because this can send stored credentials and create external side effects, call only a user-approved destination/method. Never use it as a generic web fetcher. Do not print the substituted request or sensitive upstream response headers.

## Source map

- Routes: `internal/api/server.go:registerManagementRoutes`
- Authentication: `internal/api/handlers/management/handler.go:Middleware`
- Config bodies: `config_basic.go`, `config_lists.go`, `model_prices.go`
- Usage: `usage.go`
- Auth files: `auth_files*.go`
- OAuth: `oauth_token_requests.go`, `oauth_callback.go`
- Credential-aware call: `api_tools.go`

When a route body is not documented here, inspect the one matching handler rather than guessing.
