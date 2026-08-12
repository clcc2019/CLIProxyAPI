# Public HTTP API reference

This reference reflects the routes registered in `internal/api/server.go` in this checkout. The server translates OpenAI- or Claude-shaped requests to configured OAuth/API-key backends, so a protocol surface does not by itself pin one provider.

## Base URL and authentication

The example configuration listens on port `8317`. Use:

```bash
export CLIPROXY_BASE_URL="${CLIPROXY_BASE_URL:-http://127.0.0.1:8317}"
```

Public inference routes accept configured client keys from any of these locations:

- `Authorization: Bearer <key>`
- `X-Api-Key: <key>` for Anthropic-shaped clients
- `X-Goog-Api-Key: <key>` for Google-shaped clients
- `?key=<key>` or `?auth_token=<key>` when a constrained client cannot set headers

Prefer a header. Query-string credentials leak more easily through logs and URLs. The client key comes from `api-keys`; it is not an upstream OAuth access token. When no access providers/keys are configured, the middleware retains legacy unauthenticated behavior.

Unauthenticated utility routes are `GET|HEAD /healthz`, `GET|HEAD /readyz`, `GET /`, and browser OAuth callbacks. Never use a callback route as an inference endpoint.

## Route inventory

| Method | Path | Notes |
|---|---|---|
| `GET`, `HEAD` | `/healthz` | Liveness; JSON `{"status":"ok"}` for GET |
| `GET`, `HEAD` | `/readyz` | 200 ready or 503 `not_ready` |
| `GET` | `/v1/models` | Authenticated, client-filtered model discovery |
| `POST` | `/v1/chat/completions` | OpenAI Chat Completions; stream or non-stream |
| `POST` | `/v1/completions` | Legacy completion; translated through chat internally |
| `POST` | `/v1/responses` | OpenAI Responses; stream or non-stream |
| `POST` | `/v1/responses/compact` | Context compaction; rejects `stream: true` |
| `GET` | `/v1/responses` | Responses WebSocket upgrade on the same path |
| `GET` | `/v1/realtime` | OpenAI-compatible realtime WebSocket; pass `model` query |
| `POST` | `/v1/messages` | Anthropic Messages |
| `POST` | `/v1/messages/count_tokens` | Anthropic token counting |
| `POST` | `/v1/images/generations` | JSON image generation |
| `POST` | `/v1/images/edits` | JSON or multipart edit |
| `POST` | `/v1/images/variations` | Multipart variation |
| `GET`, `POST` | `/backend-api/codex/responses` | Codex CLI direct Responses alias/WebSocket |
| `POST` | `/backend-api/codex/responses/compact` | Codex CLI direct compact alias |

An optional relay WebSocket route defaults to `/v1/ws` when attached by the host. Its authentication is conditional on `ws-auth`; prove that it is attached before relying on it.

This build does not register `/api/provider/{provider}/...` or Gemini `/v1beta/...` routes. Use only the registered surfaces above.

## Discover models first

```bash
curl -sS "$CLIPROXY_BASE_URL/v1/models" \
  -H "Authorization: Bearer $CLIPROXY_API_KEY"
```

Normally the result is OpenAI-shaped:

```json
{"object":"list","data":[{"id":"model-id","object":"model"}]}
```

Two client-sensitive variants matter:

- A `User-Agent` beginning with `claude-cli` returns Claude's list envelope with `data`, `has_more`, `first_id`, and `last_id`.
- A `client_version` query parameter returns the Codex client model-list shape.

Configured client-key restrictions filter the visible list. Prefixes and aliases in this response are part of the callable model ID. Because this build exposes unified protocol surfaces only, use unique aliases/prefixes when strict backend selection matters.

## OpenAI Chat Completions

```bash
curl -sS "$CLIPROXY_BASE_URL/v1/chat/completions" \
  -H "Authorization: Bearer $CLIPROXY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "MODEL_FROM_V1_MODELS",
    "messages": [{"role": "user", "content": "Reply with one sentence."}],
    "stream": false
  }'
```

The endpoint also recognizes a Responses-shaped body containing `input` or `instructions` but no `messages` and converts it to Chat Completions. Prefer the native `/v1/responses` path when the client supports it; this fallback is for compatibility.

For streaming, set `"stream": true` and use `curl -N`. Process each SSE `data:` event and stop at the protocol's terminal event rather than concatenating the stream and parsing it as one JSON object.

## OpenAI Responses

```bash
curl -sS "$CLIPROXY_BASE_URL/v1/responses" \
  -H "Authorization: Bearer $CLIPROXY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "MODEL_FROM_V1_MODELS",
    "input": "Explain why readiness differs from liveness.",
    "stream": false
  }'
```

Use `previous_response_id`, tools, multimodal input, reasoning options, and other Responses fields in their normal OpenAI shape; the gateway preserves/ translates supported fields. `/v1/responses/compact` accepts a Responses body but rejects streaming.

Codex-compatible direct base URL:

```text
http://127.0.0.1:8317/backend-api/codex
```

That base exposes `/responses` and `/responses/compact`; it is not a general OpenAI `/v1` base.

## Anthropic Messages

```bash
curl -sS "$CLIPROXY_BASE_URL/v1/messages" \
  -H "X-Api-Key: $CLIPROXY_API_KEY" \
  -H "Anthropic-Version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "MODEL_FROM_V1_MODELS",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "Summarize this gateway."}],
    "stream": false
  }'
```

Token counting uses the same body shape:

```bash
curl -sS "$CLIPROXY_BASE_URL/v1/messages/count_tokens" \
  -H "X-Api-Key: $CLIPROXY_API_KEY" \
  -H "Anthropic-Version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_FROM_V1_MODELS","messages":[{"role":"user","content":"Count me"}]}'
```

## Images

Generation requires `prompt`. If `model` is omitted it defaults to `gpt-image-2`, and `response_format` defaults to `b64_json`.

```bash
curl -sS "$CLIPROXY_BASE_URL/v1/images/generations" \
  -H "Authorization: Bearer $CLIPROXY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-image-2","prompt":"A precise technical cutaway diagram","size":"1024x1024"}'
```

Edits accept JSON or multipart depending on the selected model/input path. Variations accept multipart. Multipart requests are capped at 128 MiB and individual decoded images at 32 MiB. The configuration can disable all image routes, producing 404.

## SDK configuration

OpenAI Python SDK:

```python
import os
from openai import OpenAI

base = os.getenv("CLIPROXY_BASE_URL", "http://127.0.0.1:8317").rstrip("/")
client = OpenAI(base_url=f"{base}/v1", api_key=os.environ["CLIPROXY_API_KEY"])
models = client.models.list()
response = client.responses.create(model=models.data[0].id, input="Hello")
```

Anthropic Python SDK:

```python
import os
from anthropic import Anthropic

base = os.getenv("CLIPROXY_BASE_URL", "http://127.0.0.1:8317").rstrip("/")
client = Anthropic(base_url=base, api_key=os.environ["CLIPROXY_API_KEY"])
message = client.messages.create(
    model="MODEL_FROM_V1_MODELS",
    max_tokens=256,
    messages=[{"role": "user", "content": "Hello"}],
)
```

Avoid double-appending `/v1`: OpenAI SDK receives a base ending in `/v1`; Anthropic SDK receives the server origin and appends `/v1/messages` itself.

## Error handling

Public JSON errors generally use an `error` field or OpenAI-style `{"error":{"message":...,"type":...}}`. Preserve status, headers, and body during diagnosis.

- Invalid body: 400.
- Invalid/missing configured key: 401.
- Client key quota: 429 with `Retry-After` when a reset is known and a `quota` object describing scope/resource/limit/used.
- Server backpressure or unavailable dependency: often 503.
- Unsupported/disabled image route: 400 or 404 depending on the condition.

Retry only transient failures. The server already rotates credentials and retries configured upstream statuses, so aggressive client retries can multiply traffic.
