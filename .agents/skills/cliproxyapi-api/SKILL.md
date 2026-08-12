---
name: cliproxyapi-api
description: Safely call, configure, test, integrate, and troubleshoot the CLIProxyAPI HTTP interfaces in this repository. Use this skill whenever a user wants to connect an agent, CLI, curl command, OpenAI/Anthropic SDK, Codex, Claude Code, or another client to CLIProxyAPI; choose between Chat Completions, Responses, Messages, image, WebSocket, model-list, health, OAuth, or Management endpoints; diagnose 401/403/404/429/503 responses; or modify CLIProxyAPI through its Management API, even when the user only says “use the local proxy”, “point this client at port 8317”, or “check/manage the proxy”.
compatibility: Requires a shell with curl for live probes. CodeGraph is preferred when verifying this repository's current route implementation.
---

# CLIProxyAPI interface guide

Use CLIProxyAPI as a protocol-compatible gateway, not as a single upstream-provider API. The client chooses an HTTP protocol surface and a client-visible model; the server resolves the model/alias to an available credential and translates the request for the selected backend.

## Start with the user's intent

Classify the task before constructing a request:

1. Use the public inference API for model discovery, chat, Responses, Claude Messages, token counting, images, or realtime/WebSocket traffic.
2. Use the Management API for usage data, configuration, logs, credential metadata, auth-file lifecycle, or OAuth login.
3. Use the repository SDK only when the user is embedding the proxy server or adding an executor/translator in Go. For that case, read `docs/sdk-usage.md`, then `docs/sdk-access.md` or `docs/sdk-advanced.md` as needed.

Read [references/http-api.md](references/http-api.md) for inference routes, request examples, streaming, and SDK setup. Read [references/management-api.md](references/management-api.md) only for management or OAuth work.

## Establish connection details

Default to `http://127.0.0.1:8317` only when the user has not supplied a base URL. Prefer these environment variables in commands so secrets never appear in shell history or output:

```bash
export CLIPROXY_BASE_URL="http://127.0.0.1:8317"
export CLIPROXY_API_KEY="..."
export CLIPROXY_MANAGEMENT_KEY="..."
```

Do not ask the user to paste a key if an existing environment variable or client configuration can be used. Never print, log, commit, or interpolate a secret into a URL. Use `Authorization: Bearer "$CLIPROXY_API_KEY"` for public examples unless the client protocol naturally uses `X-Api-Key` or `X-Goog-Api-Key`.

## Probe before inference

For a live server task, run the bundled read-only probe from the repository root:

```bash
bash .agents/skills/cliproxyapi-api/scripts/probe.sh
```

Interpret the results in this order:

- `/healthz` proves the process is alive.
- `/readyz` proves bootstrap completed; a 503 here means wait or diagnose dependencies.
- `/v1/models` proves client authentication and shows the model IDs this client may use.

Select a model from the live `/v1/models` response. Do not invent a model name from provider marketing names, and preserve any configured prefix or alias exactly.

## Choose the protocol surface

Use the smallest endpoint matching the caller:

| Need | Method and path | Request discriminator |
|---|---|---|
| OpenAI chat | `POST /v1/chat/completions` | `model`, `messages`, optional `stream` |
| OpenAI legacy completion | `POST /v1/completions` | `model`, `prompt` |
| OpenAI Responses | `POST /v1/responses` | `model`, `input`, optional `stream` |
| Compact Responses context | `POST /v1/responses/compact` | Responses body; streaming is rejected |
| Claude Messages | `POST /v1/messages` | `model`, `max_tokens`, `messages` |
| Claude token count | `POST /v1/messages/count_tokens` | Claude Messages-shaped body |
| OpenAI image generation | `POST /v1/images/generations` | JSON with `prompt`; model defaults to `gpt-image-2` |
| OpenAI image edit | `POST /v1/images/edits` | JSON or multipart, depending on model/input |
| OpenAI image variation | `POST /v1/images/variations` | multipart image |
| Realtime WebSocket | `GET /v1/realtime?model=...` | WebSocket upgrade |
| Responses WebSocket | `GET /v1/responses` | WebSocket upgrade |
| Model discovery | `GET /v1/models` | response shape can depend on client headers |

The current `internal/api/server.go` also registers Codex aliases under `/backend-api/codex/responses` and `/backend-api/codex/responses/compact`.

This build does not register `/api/provider/{provider}/...` or Gemini `/v1beta/...` routes. Do not emit those paths; use the registered `/v1/*` surfaces and a model returned by `/v1/models`.

## Execute safely

Before a public request:

1. Show the endpoint, selected live model, protocol, and whether streaming is enabled.
2. Keep the API key in an environment variable or SDK option; redact it from all output.
3. Use `Content-Type: application/json` for JSON requests. For SSE, add `Accept: text/event-stream` and disable client-side buffering when appropriate.
4. Preserve tool calls and multimodal content in the chosen protocol instead of flattening them to text.
5. Report the HTTP status and the structured `error` payload on failure. Do not repeatedly retry authentication or management failures.

For Management API writes, the user's explicit request to change that setting is authorization to proceed. Otherwise, stay read-only. Snapshot the narrow current value before changing it, use the management key rather than the client API key, and verify the new value with a GET. Treat full config replacement, auth-file deletion, log deletion, and `/api-call` as high-impact operations; state the exact target before executing them.

## Diagnose by status

- `401`: missing or invalid client/management key. Check the header family and do not reveal the value.
- `403`: management access may be remote-disabled, no management secret exists, or the IP may be temporarily banned after repeated failures.
- `404` on `/v0/management/*`: Management API is disabled when no secret is configured, or Home mode is active. A provider-alias path can also simply be absent from this build.
- `429`: a client API-key request/token/cost quota was exceeded. Respect `Retry-After` and inspect the returned quota scope.
- `503` on `/readyz`: bootstrap is incomplete. `503` on inference can also be backpressure or an unavailable upstream/credential pool.
- A stream that never becomes JSON may be SSE or WebSocket traffic; use the protocol-aware client rather than parsing it as one JSON document.

## Keep route knowledge current

When this repository has changed, treat `internal/api/server.go:setupRoutes` and `registerManagementRoutes` as the route source of truth. Use CodeGraph context first for structural questions, then inspect only the surfaced symbols. Do not trust generated examples over the live `/v1/models` result or the checked-out route registration.

## Response style

Return a ready-to-run command or SDK snippet plus a short explanation of endpoint choice. State assumptions about base URL and model. Redact credentials as `$CLIPROXY_API_KEY` or `$CLIPROXY_MANAGEMENT_KEY`. If a live call was made, include the observed status and concise result; if it was not, clearly label the command as unexecuted.
