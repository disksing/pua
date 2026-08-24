# AgentHub HTTP API Reference

AgentHub is a local agent launcher and session hub. A single Go daemon manages
the agents installed on the machine (Codex, Kimi, Pi/Grok, OpenCode) and
exposes configuration, session management, approvals and event streams to the
Web UI, the CLI and any other HTTP client through one HTTP JSON + SSE API.

This document is served by the daemon itself at `GET /agenthub/api.md`
(`text/markdown; charset=utf-8`) and is verified against the registered routes
by automated tests, so it always matches the running implementation.

## Base URL and Security Boundary

The daemon listens on **loopback only** by default:

```text
http://127.0.0.1:4646/agenthub
```

All examples below use `$BASE` for the base URL and `$SESSION` for a session
id (session ids look like `ses_<timestamp><random hex>`):

```bash
BASE=http://127.0.0.1:4646/agenthub
SESSION=ses_1753502400000010203040506070809
```

Security model — read this before exposing the daemon anywhere:

- **No authentication.** There are no accounts, tokens or API keys. Any
  process that can reach the listen address has full control of the daemon:
  it can run agents, execute tools through them, and modify sessions and
  configuration.
- **Local-first.** The default loopback binding is only reachable from the
  same machine. `agenthub serve --addr` can explicitly bind a LAN interface
  address, a wildcard address or a hostname that resolves to a local
  interface. Binding a non-loopback address means **every device that can
  reach that address controls the daemon** — only do this on a trusted
  network and never expose the port to the public internet.
- **Host header validation.** Requests whose `Host` header does not name a
  local address of the daemon are rejected with `403 host_rejected`. This
  blocks DNS-rebinding attacks from browsers on the local network.
- **Cross-origin write protection.** Mutating requests (`POST`, `PUT`,
  `PATCH`, `DELETE`) that carry an `Origin` header are rejected with
  `403 origin_rejected` unless the origin matches the daemon's own origin
  or was explicitly trusted with `agenthub serve --allow-origin` (for
  reverse proxy deployments whose public origin differs from the daemon
  address). The daemon sends no permissive CORS headers, so browsers on
  other origins cannot issue writes. Non-browser clients (curl, the CLI)
  simply omit the `Origin` header.
- **JSON writes only.** Mutating requests must send
  `Content-Type: application/json` (`415 json_required` otherwise). Bodies
  are limited to 1 MiB and unknown JSON fields are rejected.

## Conventions

### Error responses

Every non-2xx response uses a single error envelope:

```json
{
  "error": {
    "code": "session_not_found",
    "message": "session not found",
    "retryable": false,
    "details": null,
    "requestId": "req_1753502400001a2b3c4d5e6f7a8b9c"
  }
}
```

`code` is a stable machine-readable identifier; `message` is human-readable
and may change; `retryable` says whether retrying the same operation later
can be useful without first changing the request; `details` carries optional
structured context (for example the session id after a provider start
failure); `requestId` correlates the response with daemon logs. Clients must
branch on `code`, not on `message`. A false `retryable` does not mean the
condition is permanent: the caller may need to change state or construct a
new request first. In particular, create and message failures are never
marked retryable because the daemon may already have persisted a session or
turn.

Unknown API paths and unsupported methods use this envelope too
(`route_not_found` and `method_not_allowed`); no public API error is plain
text or an empty body.

### Compatibility and capability negotiation

Call `GET /v1/status` before creating a session. `apiVersion` is the major
public contract version; a client must reject a value it does not support.
`capabilities` is a set of independently testable behaviors. Require every
behavior your workflow needs and reject a daemon that omits one. Older
daemons that return neither field are therefore explicitly incompatible,
not silently assumed to support the current contract. Ignore unknown
capabilities, response fields and event types so a compatible daemon can add
information without breaking the client.

Current runtime-backed daemon instances advertise:

| Capability | Guaranteed behavior |
| --- | --- |
| `session.source` | Durable caller source metadata and exact list filters. |
| `session.source-metadata` | Durable opaque string metadata entries inside `source.metadata`; AgentHub stores but does not interpret them. |
| `session.idempotent-create` | A stable `idempotencyKey` creates at most one Session across retries and daemon restarts. |
| `session.input-capabilities` | Every Session reports whether its selected provider supports active-Turn steer. |
| `messages.idempotent` | A non-empty `messageId` creates at most one canonical `message.input` per Session; conflicting reuse is rejected. |
| `messages.at-least-once` | Once a message request is durably accepted, AgentHub retains provider delivery responsibility across ambiguous responses, provider failures, and daemon restarts. A crash after provider acceptance but before durable acknowledgement can cause a limited duplicate attempt. |
| `messages.delivery-result` | Message success responses distinguish durable input acceptance from Provider acceptance through `delivery.state: pending|delivered`. |
| `messages.opaque-payload-v2` | Schema-v2 inputs persist caller-owned JSON payloads opaquely and forward provider-facing text without transformation. |
| `turns.stable-index` | Turn pages expose event ranges, trigger/final-reply event references, status and forward/backward cursors. |
| `turns.materialized` | Closed Turns and ordered compact items are read from rebuildable `turns.jsonl`; single-Turn queries repair projection lag and Event ranges provide bounded detail expansion. |
| `turns.activity-items` | Compact Turn projections combine every uninterrupted thinking/tool run into one `activity` item with independent phase, update and tool-call counts. |
| `session.launch-environment` | Durable per-session provider environment, including provider resume. |
| `session.launch-environment-update` | Resume accepts a `launchEnvironment` overlay, persisted before provider start. |
| `session.ephemeral-environment` | Create/resume accepts a one-shot `ephemeralEnvironment` overlay passed only to one Provider process. Names and values are never stored; after first use, the non-secret `ephemeralEnvironmentRequired: true` Session marker requires a fresh non-empty overlay for every later Provider start. |
| `session.strict-stopped` | `stopped` is published only after provider exit is confirmed. |
| `events.lossless-replay` | Durable exclusive cursors, paginated REST catch-up and gap-free SSE replay. |
| `events.delta-merge` | Consecutive same-message text deltas are folded into one durable source Event; SSE delivers live folds as SemanticFrame append patches (`mode: "append"`) under the same cursor and re-sends the full replace frame on reconnect. |
| `events.backward-pagination` | JSON frame pages can also read the source log backwards with `latest=true` or an exclusive `before` cursor. |
| `events.semantic-v1` | `/events` JSON, range and SSE expose only provider-neutral `agenthub.semantic-events.v1` frames. |
| `event.raw-v1` | Singular `/event/{sourceEventId}` returns one exact raw source Event together with its normalized frame for diagnostics. |
| `activity.global-sse` | Best-effort one-second activity frames across all Sessions, with no historical beep replay. |
| `events.canonical-turn-terminals` | Provider-independent `turn.completed`, `turn.failed` and `turn.cancelled`. |
| `recovery.closed-turns` | Daemon recovery closes interrupted delivered turns before publishing `stopped`; an accepted input still pending Provider delivery remains recoverable and is retried. |

Store-only test/diagnostic server instances omit runtime-backed capabilities;
the daemon never advertises them merely because their names are compiled
into the binary.

### Sessions

A session object looks like:

```json
{
  "id": "ses_1753502400000010203040506070809",
  "title": "Fix the flaky test",
  "cwd": "/path/to/project",
  "agentName": "Codex",
  "launchEnvironment": {"SESSION_CONTEXT_ID": "context-123"},
  "source": {
    "app": "pua",
    "instanceId": "mac-mini",
    "externalId": "project7.task26"
  },
  "provider": "codex",
  "providerSessionId": "thread_abc123",
  "state": "ready",
  "currentTurnId": "turn_1753502400002b2b3c4d5e6f7a8b9c0d",
  "pendingApprovalIds": ["approval-1"],
  "lastEventId": 42,
  "createdAt": "2026-07-26T12:00:00Z",
  "updatedAt": "2026-07-26T12:05:00Z"
}
```

`state` is one of `starting`, `ready`, `running`, `waiting_approval`,
`stopping`, `stopped` or `archived`. A stopped session may include `stopReason`:
`requested`, `completed`, `provider_error`, `startup_error` or
`daemon_recovery`. Optional fields (`agentName`, `source`,
`launchEnvironment`, `provider`, `stopReason`, `providerSessionId`,
`currentTurnId`, `pendingApprovalIds`) are omitted when absent. `source` is
unverified, caller-supplied correlation metadata; AgentHub does not register
source applications, reserve names, authenticate the values, or require them
to be unique. `launchEnvironment` is durable session data and may be visible
to any client that can read the session or its events. `lastEventId` is the id
of the newest event in the session log and doubles as the resume cursor for
the events endpoint.

### Agents and providers

Agents are referenced **by name only**. An agent has a `name` (required,
unique case-insensitively after trimming, at most 80 characters), a
`providerId` naming one configured provider, and provider-specific `options`.
There are no agent ids, agent profiles or tag-based routing; creating a
session always requires an explicit agent name. The built-in provider ids
are `codex`, `kimi`, `pi` and `opencode`; each can be enabled or disabled,
and a disabled provider makes its agents unavailable for new work without
disturbing sessions that are already running.

### Durable source Events

Every state change is appended to the session's internal durable event log. A
source Event looks like:

```json
{
  "id": 42,
  "time": "2026-07-26T12:05:00Z",
  "type": "message.assistant.delta",
  "sessionId": "ses_1753502400000010203040506070809",
  "turnId": "turn_1753502400002b2b3c4d5e6f7a8b9c0d",
  "data": {"text": "..."}
}
```

`id` is a durable, per-session, monotonically increasing integer cursor. A
committed id is never reused, including across daemon restarts. `turnId` is
present on Events that belong to a Turn. These source payloads are storage and
  diagnostic records, not the public timeline contract: `/events` always
  normalizes them into SemanticFrames, and only singular
  `/event/{sourceEventId}` returns one exact source Event.

Consecutive `message.assistant.delta` or `message.reasoning.delta` fragments
of one provider message (same type, turn, and provider method) are folded
into a single durable source Event instead of one Event per fragment. The
source Event keeps its original id, accumulates `text`, moves `time` to the
newest fragment, and records the first fragment in optional `startTime`.
Folding stops at a 32 KiB accumulated payload, so one long message may span
several source Events. Public REST and replay responses always contain a full
`mode: "replace"` SemanticFrame; only a live folded fragment uses
`mode: "append"` under the same cursor. See the `/events` contract below.

Source Event `type` and `data` are intentionally not a versioned public
payload contract. Current writers persist provider-neutral `tool.call`
records and may retain Provider input in a diagnostic `raw` sidecar. Existing
logs can still contain provider-specific `tool.event`, legacy `message.user`
and `message.user.steer`, and other private Provider records. AgentHub
normalizes those records on read without rewriting `events.jsonl`; clients
must consume the SemanticEvents contract rather than reproduce this adapter
logic. The singular `/event/{sourceEventId}` endpoint is the only API that
exposes one exact source payload for diagnostics.

## Endpoints

### GET /v1/status

Daemon status, effective data paths and runtime summary.

- **Query parameters:** none.
- **Success `200`:**

```json
{
  "apiVersion": "1",
  "capabilities": [
    "events.lossless-replay",
    "events.delta-merge",
    "events.backward-pagination",
    "events.semantic-v1",
    "event.raw-v1",
    "activity.global-sse",
    "session.source",
    "session.source-metadata",
    "session.idempotent-create",
    "session.input-capabilities",
    "messages.idempotent",
    "messages.at-least-once",
    "messages.delivery-result",
    "messages.opaque-payload-v2",
    "turns.stable-index",
    "turns.materialized",
    "turns.activity-items",
    "events.canonical-turn-terminals",
    "recovery.closed-turns",
    "session.launch-environment",
    "session.launch-environment-update",
    "session.ephemeral-environment",
    "session.strict-stopped"
  ],
  "version": "0.1.0",
  "startedAt": "2026-07-26T11:00:00Z",
  "uptimeSeconds": 3600,
  "paths": {
    "config": "/home/user/.agenthub/config.json",
    "sessions": "/home/user/.agenthub/sessions",
    "archive": "/home/user/.agenthub/sessions/Archive",
    "logs": "/home/user/.agenthub/logs"
  },
  "sessionStore": {
    "path": "/home/user/.agenthub/sessions",
    "archivePath": "/home/user/.agenthub/sessions/Archive",
    "sessionCount": 3
  },
  "runtime": {"available": true, "summary": "2 running sessions"}
}
```

  `runtime` is `{"available": false}` when the daemon runs without a
  session runtime. Such an instance also omits the runtime-backed
  capabilities described under
  [Compatibility and capability negotiation](#compatibility-and-capability-negotiation).

```bash
curl -s "$BASE/v1/status"
```

### GET /v1/config

Read the effective configuration.

- **Success `200`:** `{"config": {...}}` — the configuration object
  (`version`, `agentProviders`, `agents`, `onWatch`; see
  `PUT /v1/config` for the schema and constraints). `onWatch.password` is
  always returned as an empty string; the stored secret never leaves the
  daemon.
- **Errors:** `503 runtime_unavailable`.

```bash
curl -s "$BASE/v1/config"
```

### PUT /v1/config

Replace the whole configuration. The daemon validates the new
configuration, writes it to the config file atomically (the daemon is the
only writer) and applies it in memory.

- **Request body:**

```json
{
  "config": {
    "version": 1,
    "agentProviders": [
      {"id": "codex", "name": "Codex app-server", "type": "codex", "enabled": true, "command": "codex"}
    ],
    "agents": [
      {"name": "Codex", "providerId": "codex", "options": {"approval": "never", "sandbox": "danger-full-access"}, "environment": {"FOO": "bar"}}
    ],
    "onWatch": {
      "enabled": false,
      "serverUrl": "http://127.0.0.1:9211",
      "authMode": "trusted_proxy",
      "username": "admin",
      "password": "",
      "refreshIntervalSeconds": 60
    }
  }
}
```

- **Constraints:** provider `id` unique and `type` one of `codex`, `kimi`,
  `pi`, `opencode`; agent `name` required, at most 80 characters, unique
  case-insensitively after trimming; every agent's `providerId` must
  reference a configured provider. An agent's optional `environment` map is
  merged into the Provider process environment when a session starts or
  resumes (the session `launchEnvironment` wins on conflicts); names cannot
  be empty or contain `=` or NUL, and values cannot contain NUL. Unknown
  fields are rejected.
- **OnWatch constraints:** `serverUrl` is an absolute HTTP(S) URL without
  embedded credentials; `authMode` is `trusted_proxy`, `basic` or `none`;
  refresh is 30, 60 or 300 seconds. Basic Auth requires a username and, when
  enabled, a password. Because GET responses redact the password, an empty
  Basic password in PUT preserves the currently stored value.
- **Browser preferences:** Activity Monitor controls are stored only in the
  browser's `localStorage` and are absent from this API. For upgrade
  compatibility, a legacy top-level `companion` object is accepted and
  discarded; it is omitted from responses and from the next config-file write.
- **Rename semantics:** if an agent disappears while exactly one new agent
  with an identical provider, options and environment appears, the change is
  treated as a rename and active sessions referencing the old name are
  migrated with a `session.agent` event. Ambiguous renames are rejected.
- **Success `200`:** `{"config": {...}}` (the applied configuration).
- **Errors:** `400 invalid_request` (malformed JSON or unknown fields,
  including removed profile/tag fields), `415 json_required`,
  `422 invalid_config` (validation failed), `422 ambiguous_rename`,
  `500 agent_rename_failed`, `503 runtime_unavailable`.

```bash
curl -s -X PUT "$BASE/v1/config" \
  -H "Content-Type: application/json" \
  -d @config.json
```

### PUT /v1/config/providers/{id}

Enable or disable one built-in provider without touching the rest of the
configuration. This is the contract behind the four switches of the Web
settings UI. The provider's other fields survive a disable/enable cycle; a
built-in provider missing from an old configuration is created with its
canonical defaults.

- **Path parameters:** `id` — one of `codex`, `kimi`, `pi`, `opencode`.
- **Request body:** `{"enabled": true}` — `enabled` is required.
- **Success `200`:** `{"provider": {"id", "name", "type", "enabled", "command?"}}`.
- **Errors:** `400 invalid_request` (missing `enabled`), `415 json_required`,
  `404 unknown_provider`, `422 invalid_config`, `503 runtime_unavailable`.
- **Effect:** agents of a disabled provider report `available: false` from
  `GET /v1/agents` and are rejected for new sessions; running sessions keep
  running.

```bash
curl -s -X PUT "$BASE/v1/config/providers/kimi" \
  -H "Content-Type: application/json" \
  -d '{"enabled": false}'
```

### POST /v1/onwatch/test

Test an OnWatch connection without saving the supplied settings. The request
body is `{"onWatch": {...}}` using the same object as `PUT /v1/config`.
An empty Basic Auth password reuses the currently stored secret. The daemon
requests OnWatch `/api/providers`; it never returns credentials.

- **Success `200`:** `{"connected": true, "providers": ["codex", ...]}`.
- **Errors:** `400 invalid_request`, `415 json_required`,
  `422 invalid_config`, `502 onwatch_unavailable`, `503 runtime_unavailable`.

```bash
curl -s -X POST "$BASE/v1/onwatch/test" \
  -H "Content-Type: application/json" \
  -d '{"onWatch":{"enabled":true,"serverUrl":"http://127.0.0.1:9211","authMode":"trusted_proxy","username":"admin","refreshIntervalSeconds":60}}'
```

### GET /v1/quota

Read the companion's normalized Provider quota snapshot. The browser never
contacts OnWatch directly. The daemon discovers Provider order from
`/api/providers`, reads each `/api/current?provider=...` response, normalizes
remaining/used percentages, reset data and status, and caches the result for
the configured refresh interval.

- **Success `200`:** `{"quota": {"configured", "connected", "stale?",
  "error?", "updatedAt", "staleAfterSeconds", "providers": [...]}}`.
  Each Provider carries `provider`, `label`, optional `planLabel`, `status`,
  `capturedAt`, optional stale/error state, and `quotas`. Each quota carries
  `kind`, `label`, `remainingPercent`, `usedPercent`, reset/window fields when
  available, `status`, and optional `used`, `limit`, rate and projection data.
  Providers that report a `balance` payload instead of a `quotas` array (e.g.
  DeepSeek credit balance) are normalized into one quota row with
  `kind:"balance"`, whose remaining share defaults to current balance / 100.
  The raw balance is also exposed as `value` so browsers can re-derive
  percentages against a per-provider balance total configured in the Activity
  settings.
  `windowPositionPercent` is the percentage of the known reset window still
  remaining: `100` is the right edge at the start of a window and it moves
  toward the left edge (`0`) as reset approaches.
- When an upstream refresh fails after a successful read, the last cached
  snapshot is returned with `connected:false`, `stale:true` and an error.
- **Errors:** `503 runtime_unavailable`. Upstream availability is represented
  inside the successful snapshot so the UI can retain stale data.

```bash
curl -s "$BASE/v1/quota"
```

### GET /v1/providers/{id}/models

Enumerate the models currently usable by one built-in provider through the
provider's official interface. Read-only: it never creates a provider
session and never changes configuration. Enumeration may start a short-lived
provider CLI process and can take several seconds; the whole request is
bounded by a 45-second timeout. Results are cached until the configuration
changes.

- **Path parameters:** `id` — one of `codex`, `kimi`, `pi`, `opencode`.
- **Success `200`:** `{"provider": {"id", "name", "type"}, "models": [...]}`;
  each model is `{"id", "label", "default?"}`, where `id` is the value the
  agent `model` option accepts. An empty `models` array is a valid result.
- **Errors:** `404 unknown_provider`, `409 provider_disabled`,
  `503 provider_unavailable` (provider CLI missing or failed to start),
  `504 provider_timeout`, `502 provider_error` (provider ran but reported an
  error or returned unreadable data), `503 runtime_unavailable`.

```bash
curl -s "$BASE/v1/providers/codex/models"
```

### GET /v1/agents

List configured providers and agents with their effective availability, plus
CLI availability probes for enabled providers.

- **Success `200`:**

```json
{
  "providers": [{"id": "codex", "name": "Codex app-server", "type": "codex", "enabled": true}],
  "agents": [
    {
      "name": "Codex",
      "providerId": "codex",
      "options": {"approval": "never"},
      "environment": {"FOO": "bar"},
      "available": true
    }
  ],
  "probes": [{"providerId": "codex", "type": "codex", "command": "/usr/local/bin/codex", "available": true}]
}
```

  An agent whose provider is missing or disabled reports `available: false`
  with an `unavailableReason`.
- **Errors:** `503 runtime_unavailable`.

```bash
curl -s "$BASE/v1/agents"
```

### GET /v1/sessions

List sessions, most recently updated first. Archived sessions are hidden by
default.

- **Query parameters:**
  - `includeArchived=true` — include archived sessions in the list.
  - `archived=true` — list only archived sessions.
  - `state=<state>[,<state>...]` — keep only sessions in one of the given
    states (see [Sessions](#sessions)).
  - `sourceApp=<value>` — exact, case-sensitive `source.app` match.
  - `sourceInstanceId=<value>` — exact, case-sensitive
    `source.instanceId` match.
  - `sourceExternalId=<value>` — exact, case-sensitive
    `source.externalId` match.
    Source filters can be combined with each other and with archive and state
    filters. Sessions created without `source` never match a source filter.
  - `limit=<n>` — opt in to cursor pagination with a page size from 1 to 500.
    When `cursor` is present and `limit` is omitted, the page size is 50.
  - `cursor=<opaque>` — continue from the `page.nextCursor` returned by the
    previous response. Cursors are opaque and should not be parsed or edited.
- **Success `200`:** `{"sessions": [...], "page": {"limit": 50,
  "hasMore": true, "nextCursor": "..."}}`. Results have a stable
  `updatedAt` descending, Session ID descending order. Calls that omit both
  pagination parameters retain the historical unbounded result for existing
  clients; their `page.hasMore` is false.
- **Errors:** `400 invalid_session_limit`, `400 invalid_session_cursor`.

```bash
curl -s "$BASE/v1/sessions"
curl -s "$BASE/v1/sessions?archived=true"
curl -s "$BASE/v1/sessions?state=running,waiting_approval"
curl -s "$BASE/v1/sessions?sourceApp=pua&sourceInstanceId=mac-mini&state=ready"
curl -s "$BASE/v1/sessions?limit=50"
```

### POST /v1/sessions

Create a session with an explicit agent and start its provider
synchronously. Optionally send an initial message, which starts the first
turn before the response returns.

- **Request body:**

```json
{
  "title": "Fix the flaky test",
  "cwd": "/path/to/project",
  "agentName": "Codex",
  "idempotencyKey": "pua-workspace-1-project7-task26-generation-1",
  "launchEnvironment": {"SESSION_CONTEXT_ID": "context-123"},
  "ephemeralEnvironment": {"PUA_SERVICE_TOKEN": "one-shot-value"},
  "source": {
    "app": "pua",
    "instanceId": "mac-mini",
    "externalId": "project7.task26/1",
    "metadata": {
      "resourceId": "project7.task26",
      "generationId": "01HZZ...",
      "previousSessionId": "ses_..."
    }
  },
  "initialMessage": {"text": "Reproduce the failure first.", "role": "user"}
}
```

- **Fields:**
  - `agentName` (required) — the unique name of a configured agent; matched
    case-insensitively, and the session records the canonical configured
    spelling. The agent's provider must be enabled.
  - `cwd` (required) — working directory for the agent; must exist and be a
    directory (symlinks are resolved).
  - `title` (optional) — display title.
  - `idempotencyKey` (optional) — caller-stable creation key, trimmed and
    limited to 4096 bytes. Repeating the same immutable creation request with
    this key returns the original Session with `200` and `created: false`,
    including after daemon restart. Reusing the key with different title,
    cwd, Agent, source, environment, provider or input capability returns
    `409 idempotency_conflict`.
  - `source` (optional) — caller-supplied correlation metadata containing
    optional string fields `app`, `instanceId`, and `externalId`, plus an
    optional string-to-string `metadata` map. The values
    are stored verbatim in `session.created`, survive event replay, and are
    returned by session GET/list responses. They are not authenticated or
    unique: any client may submit any values, and duplicate values are
    allowed.
  - `launchEnvironment` (optional) — string-to-string environment overrides
    for this session's provider process. Session values override daemon
    variables with the same name. Codex also receives every entry as a
    `shell_environment_policy.set.<KEY>` config override on both
    `thread/start` and `thread/resume`; ACP and Pi receive the merged process
    environment. The map is stored in the durable `session.created` event and
    remains in effect after event replay, daemon restart and provider resume.
    **It is persisted in `events.jsonl` and `session.json` and returned by the
    Session API, so never put a secret here unless you intend it to be stored.**
  - `ephemeralEnvironment` (optional) — one-shot string-to-string overrides
    passed only to the Provider process. This field is accepted only when the
    `session.ephemeral-environment` capability is advertised. Its names and
    values are never persisted or returned. After an overlay is first used,
    the Session persists and returns only
    `ephemeralEnvironmentRequired: true`; every later Provider start requires
    another non-empty overlay. Clients must fail closed when the capability is
    absent and must not copy these values into `launchEnvironment`.
  - `initialMessage` (optional) — first inbound message; it accepts the same
    `text`, `role`, `sender`, `steer`, and reserved correlation fields as the
    messages endpoint. An omitted role means `user`; when non-empty the first
    turn starts immediately. `assistant` is rejected. When creation itself
    is retried, use a stable `initialMessage.messageId` as well so delivery
    can be retried idempotently.
- **Success `201`:** `{"session": {...}, "created": true}` with a
  `Location: /v1/sessions/{id}` header.
- **Idempotent replay `200`:** `{"session": {...}, "created": false}`.
- **Errors:** `400 invalid_request` (malformed body or removed/unknown fields), `415 json_required`, `422 agent_required`,
  `422 invalid_agent` (unknown agent or disabled provider),
  `422 invalid_cwd`,
  `422 invalid_launch_environment` (an environment name is empty or contains
  `=`/NUL, or a value contains NUL),
  `422 invalid_ephemeral_environment` (an environment name is empty or
  contains `=`/NUL, or a value contains NUL),
  `409 idempotency_conflict`,
  `500 session_create_failed`,
  `502 provider_start_failed` (the provider handshake failed; the response
  `details.sessionId` names the session kept for diagnostics in the
  `stopped` state with `stopReason: "startup_error"`),
  `502 turn_start_failed` (the provider started but the initial
  message could not be sent; `details.sessionId` likewise).

```bash
curl -s -X POST "$BASE/v1/sessions" \
  -H "Content-Type: application/json" \
  -d '{
    "title": "Fix the flaky test",
    "cwd": "/path/to/project",
    "agentName": "Codex",
    "launchEnvironment": {"SESSION_CONTEXT_ID": "context-123"},
    "source": {
      "app": "pua",
      "instanceId": "mac-mini",
      "externalId": "project7.task26"
    },
    "initialMessage": {"text": "Reproduce the failure first.", "role": "user"}
  }'
```

### GET /v1/sessions/{id}

Read one session. Works for active and archived sessions.

- **Path parameters:** `id` — session id.
- **Success `200`:** `{"session": {...}}`.
- **Errors:** `404 session_not_found`, `500 session_store_failed`.

```bash
curl -s "$BASE/v1/sessions/$SESSION"
```

### DELETE /v1/sessions/{id}

Archive a session. Archiving appends a durable `session.archived` event and
then atomically moves the whole session directory into the store's
`Archive/` subdirectory. Nothing is deleted; `GET /v1/sessions/{id}` and the
events endpoint keep working for archived sessions. Archived sessions are
read-only: all write operations below return `409 session_archived`, and
unarchiving is not supported.

- **Preconditions:** the session must be `stopped`, with no open turn or
  pending approval. Stop it first with
  `POST /v1/sessions/{id}/stop`; this endpoint never force-stops a provider.
- **Success `200`:** `{"session": {...}}` (state `archived`). Repeating an
  archive is idempotent.
- **Errors:** `404 session_not_found`, `400 invalid_session_id`,
  `409 session_active` (active work remains),
  `409 session_archive_conflict` (archive target occupied),
  `500 session_archive_failed` (filesystem error; the session's data stays
  intact and a retry or daemon restart completes the move).

```bash
curl -s -X DELETE "$BASE/v1/sessions/$SESSION" \
  -H "Content-Type: application/json" -d '{}'
```

### GET /v1/sessions/{id}/events

以 JSON 或 Server-Sent Events 读取 provider-neutral SemanticFrame。接口适用于活动和已归档 Session；原始 Provider payload 永远不会出现在该接口中。

#### JSON mode (default)

Plain requests return a JSON snapshot of the log after a cursor.

- **Query parameters:**
  - `after=<event-id>` — only source frames with a cursor greater than this id
    (default `0`, i.e. from the beginning).
  - `before=<event-id>` — backward pagination: the last `limit` source frames
    with a cursor smaller than this exclusive id. A `before` value past the
    durable head is clamped to `latestCursor+1` instead of rejected, so a
    tail read can never silently skip future frames. Mutually exclusive
    with `after` and `latest`.
  - `latest=true` — equivalent to `before=<latestCursor+1>`: returns the
    last `limit` source frames of the log. Mutually exclusive with `after` and
    `before`.
  - `limit=<n>` — maximum number of source frames returned (default `500`,
    values above `1000` are clamped to the page size).
  - `start=<event-id>&end=<event-id>` — inclusive stable Event bounds for
    expanding one compact Turn item. Both are required together; paginate
    within the range with the normal exclusive `after` cursor. Ranges do not
    support backward reads or SSE.
- **Success `200`:** frames are in ascending cursor order in both directions.
  `after` and `nextAfter` are exclusive cursors; `latestCursor` is the
  durable head captured for this response.

  ```json
  {
    "schema": "agenthub.semantic-events.v1",
    "frames": [],
    "page": {
      "after": 100,
      "limit": 200,
      "nextAfter": 100,
      "hasMore": false
    },
    "latestCursor": 100
  }
  ```

  Page forward with `page.nextAfter` while `page.hasMore` is true. Clients
  that need a stable catch-up target should retain the first response's
  `latestCursor`; frames appended later can be consumed by a subsequent
  request or SSE.

  Backward pages (requested with `before` or `latest`) additionally carry
  `before` (the clamped exclusive cursor used), `nextBefore` and
  `hasMoreBefore` in `page`; forward pages never include these fields.

  ```json
  {
    "schema": "agenthub.semantic-events.v1",
    "frames": [],
    "page": {
      "after": 0,
      "limit": 100,
      "nextAfter": 1030,
      "hasMore": false,
      "before": 1031,
      "nextBefore": 931,
      "hasMoreBefore": true
    },
    "latestCursor": 1030
  }
  ```

  Page backward with `page.nextBefore` while `page.hasMoreBefore` is true.
  `nextAfter`, `hasMore` and `latestCursor` stay populated, so a tail page
  can hand `page.nextAfter` to a subsequent `after` request or SSE stream
  directly.
- **Errors:** `400 invalid_event_cursor` (malformed cursor, or `before` /
  `latest` combined with each other or with an explicit `after`),
  `404 session_not_found`, `409 event_cursor_ahead` (the supplied `after`
  cursor is newer than this session's durable head; the error details
  include `latestCursor`).

每条持久化 source Event 对应一个 frame，`frame.cursor` 等于 source Event ID。`frame.events` 可以为空、包含一条或多条 semantic events；空 frame 仍推进 cursor。REST 和 reconnect replay 返回 `mode: "replace"`，live folded delta 使用 `mode: "append"` 且不推进 cursor。

#### SemanticFrame 与 SemanticEvent

`agenthub.semantic-events.v1` 的稳定 envelope 如下。未知字段必须忽略；
`id` 是 opaque stable ID，client 不得解析其格式：

```text
SemanticFrame {
  schema: "agenthub.semantic-events.v1"
  cursor: integer
  mode: "replace" | "append"
  source: {eventId, type, sessionId, turnId?, time, startTime?}
  events: SemanticEvent[]
}

SemanticEvent {
  id: string
  sourceEventId: integer
  index: integer
  time: string
  startTime?: string
  type: string
  sessionId: string
  turnId?: string
  data?: object
}
```

`source` 只提供 cursor 来源和诊断元数据，不包含 source Event 的 `data`。
同一 source Event 可以投影为零个、一个或多个 semantic facts；因此 client
必须保存空 frame 以推进 cursor，并以 semantic `id`（而不是 cursor）区分
同一 frame 内的 facts。`mode` 描述整个 source frame 的 revision：重复 cursor
的 `replace` 完整替换已有 frame，`append` 只合并实时增量。

稳定 semantic types 与 `data`：

| Type | Stable `data` fields |
| --- | --- |
| `message.input` | `role`, `text`, `steer`, optional opaque `payload`, `sender`, `messageId`, `replyTo`, `correlationId` |
| `message.assistant.delta`, `message.reasoning.delta` | `text` |
| `tool.call` | `schemaVersion: 1`, `callId`, `operation`, optional `toolKind`, `name`, `summary`, `status`, `output`, `error` |
| `approval.requested` | `approvalId`, `kind`, `title`, `detail`, `question`, `options[]` |
| `approval.resolved` | `approvalId`, `decision`, optional `optionId`, `text`, `reason` |
| `provider.error` | optional `message`, `details`, `reason`, `code`, `retryable` |
| `turn.started`, `turn.completed`, `turn.failed`, `turn.cancelled` | lifecycle fields such as `error`, `message`, or `reason` when applicable |
| `session.created`, `session.provider`, `session.state`, `session.archived`, `session.agent`, `session.launch-environment` | provider-neutral Session lifecycle fields |
| `unknown` | `sourceType`, `reason`, optional `code` |

`tool.call.operation` 是 `start | update | finish`；`status` 是
`running | completed | failed`。省略字段表示保留此前同一 `(turnId, callId)`
的值，显式 `null` 表示清除。`output` 为
`{mode: "append" | "replace", text, truncated?}`，这里的 mode 只控制工具
输出文本合并，与外层 SemanticFrame `mode` 相互独立；`error` 只包含稳定的
`message` 和可选 `code`。Provider `rawInput`、私有 method/notification、
完整工具参数和 raw approval params 都不属于该协议。

Provider 噪声和重复 envelope 投影为空 frame；无法识别但可能有诊断价值的
source type 投影为脱敏 `unknown`。client 必须安全忽略未知 semantic type。
Turn 只由 `turn.completed`、`turn.failed` 或 `turn.cancelled` 结束，不能使用
`provider.turn.completed` 等 source type 推断终态。

```bash
curl -s "$BASE/v1/sessions/$SESSION/events"
curl -s "$BASE/v1/sessions/$SESSION/events?after=100&limit=200"
curl -s "$BASE/v1/sessions/$SESSION/events?latest=true&limit=100"
curl -s "$BASE/v1/sessions/$SESSION/events?before=931&limit=100"
```

#### SSE mode

Send `Accept: text/event-stream` (or append `?stream=true`) to keep the
connection open and receive frames as they happen. Backward pagination is
a JSON-mode feature: streams reject `before` and `latest` with
`400 invalid_event_cursor`.

- **Headers:** `Accept: text/event-stream`; optional `Last-Event-ID` with
  the id of the last event already processed (the standard SSE resume
  mechanism; `?after=` is an equivalent query form).
- **Connection lifecycle:**
  1. The daemon installs the live subscription and captures a durable
     high-water mark.
  2. It pages through **all** stored source Events after the exclusive cursor up to
     that high-water mark, with no 1000-event backlog limit.
  3. It then consumes the live subscription. When a text delta folds into
     the tail source Event, the live SemanticFrame is an append patch: it
     reuses the folded cursor, sets `mode: "append"`, and its semantic Event
     carries only the new fragment. Consumers merge it into their stored
     frame; only a new cursor advances the stream cursor.
  4. Every 15 seconds without traffic the daemon sends a `: heartbeat`
     comment line to keep proxies and clients alive.
  5. The stream ends when the client disconnects or when the daemon shuts
     down (daemon shutdown closes streams promptly so restarts are fast).
     A subscriber queue overflow also ends the stream immediately; the
     daemon never continues sending a queue known to contain a gap. Event
     and heartbeat writes have a five-second deadline, so a client that
     stops reading cannot pin the handler and prevent that terminal close.
- **Recovery:** reconnect with `Last-Event-ID` set to the id of the last
  contiguous frame processed. The replay first re-sends that cursor frame
  with its current durable content — append patches never move the cursor,
  so this heals fragments that folded into the tail event while the client
  was disconnected — and then continues with frames after it, replayed from
  `events.jsonl` rather than the in-memory subscriber queue, so overflow
  and daemon restart are recoverable. A frame whose `cursor` is at or below
  the last processed cursor is either an append patch (`mode: "append"`) or
  a full replacement (`mode: "replace"`); neither moves the cursor. Only a
  cursor greater than `last_processed_id + 1` is a gap; a client that observes one must
  stop projection and catch up through REST before resuming SSE.
- **Frame format:** every SemanticFrame uses the default SSE message channel
  (no per-type `event:` field). SSE `id:` equals `frame.cursor`; `data:` is
  the complete `agenthub.semantic-events.v1` frame, including empty frames.
  Consumers inspect each nested semantic Event's `type` and safely ignore
  unknown types.

```text
id: 43
data: {"schema":"agenthub.semantic-events.v1","cursor":43,"mode":"replace","source":{"eventId":43,"type":"message.assistant.delta","sessionId":"ses_...","turnId":"turn_...","time":"2026-07-26T12:05:01Z"},"events":[{"id":"sem_43_0","sourceEventId":43,"index":0,"time":"2026-07-26T12:05:01Z","type":"message.assistant.delta","sessionId":"ses_...","turnId":"turn_...","data":{"text":"Hello"}}]}

: heartbeat

id: 44
data: {"schema":"agenthub.semantic-events.v1","cursor":44,"mode":"replace","source":{"eventId":44,"type":"turn.completed","sessionId":"ses_...","turnId":"turn_...","time":"2026-07-26T12:05:02Z"},"events":[{"id":"sem_44_0","sourceEventId":44,"index":0,"time":"2026-07-26T12:05:02Z","type":"turn.completed","sessionId":"ses_...","turnId":"turn_...","data":{}}]}
```

- **Errors:** `400 invalid_event_cursor`, `404 session_not_found`,
  `409 event_cursor_ahead`, `500 stream_unsupported` (all before the stream
  starts).

```bash
curl -N -H "Accept: text/event-stream" "$BASE/v1/sessions/$SESSION/events"
curl -N -H "Accept: text/event-stream" -H "Last-Event-ID: 100" \
  "$BASE/v1/sessions/$SESSION/events"
```

### GET /v1/sessions/{id}/event/{sourceEventId}

按正整数 source Event ID 精确读取一条诊断记录。该接口不接受查询参数，不支持分页、range 或 SSE。成功响应同时包含未经过 normalizer 的持久化 `sourceEvent` 和同一 Event 的 provider-neutral `frame`：

```json
{
  "schema": "agenthub.event-detail.v1",
  "sourceEvent": {
    "id": 301,
    "time": "2026-08-22T00:00:00Z",
    "type": "tool.call",
    "sessionId": "ses_example",
    "turnId": "turn_example",
    "data": {"schemaVersion": 1, "callId": "call_1", "operation": "start", "toolKind": "command", "name": "Command", "status": "running", "raw": {}}
  },
  "frame": {
    "schema": "agenthub.semantic-events.v1",
    "cursor": 301,
    "mode": "replace",
    "source": {"eventId": 301, "type": "tool.call", "sessionId": "ses_example", "turnId": "turn_example", "time": "2026-08-22T00:00:00Z"},
    "events": [{
      "id": "sem_301_0",
      "sourceEventId": 301,
      "index": 0,
      "time": "2026-08-22T00:00:00Z",
      "type": "tool.call",
      "sessionId": "ses_example",
      "turnId": "turn_example",
      "data": {"schemaVersion": 1, "callId": "call_1", "operation": "start", "toolKind": "command", "name": "Command", "status": "running"}
    }]
  }
}
```

`sourceEvent.data` 是不稳定的诊断数据，可能包含 Provider raw payload，client 不得依赖其内部 schema。`frame` 使用与 `/events` 完全相同的稳定协议。找不到精确 ID 时返回 `404 event_not_found`；非法 ID 返回 `400 invalid_event_id`。

### GET /v1/activity/events

Subscribe to a process-local, best-effort view of new activity across every
AgentHub Session. This stream is derived only from Events published after they
are durably appended to AgentHub's own `events.jsonl`; it does not scan or
read Provider-native Session files.

- Events are grouped into one-second windows and then by `sessionId`. A
  Session appears at most once per frame with `eventCount`, `provider`,
  `title`, `turnId`, `lastEventAt`, and `completed`. `turnId` is the latest
  non-empty Turn id in the frame. `completed` remains true when the window
  contains any canonical Turn terminal event for backward compatibility.
  Such a frame also contains `turnTerminal` with the exact `turnId`, `endedAt`,
  and a `status` of `completed`, `failed`, or `cancelled`. If a different Turn
  starts later in the same frame, its activity supersedes the earlier terminal
  marker.
- Frames carry a daemon-wide monotonic `sequence`, `windowStartedAt`,
  `windowEndedAt`, and `sessions`. They use the default SSE message channel.
- There is no historical replay and `Last-Event-ID` is ignored: reconnecting
  starts with current activity so stale work never produces delayed beeps.
  Clients use `sequence` only to detect a dropped frame and reset transient
  visualization state.
- The stream flushes immediately, sends a heartbeat every 15 seconds, uses
  bounded writes, and terminates on shutdown, client cancellation, or bounded
  subscriber overflow. EventSource can then reconnect automatically.

```text
id: 1842
data: {"sequence":1842,"windowStartedAt":"2026-08-10T15:06:23Z","windowEndedAt":"2026-08-10T15:06:24Z","sessions":[{"sessionId":"ses_abc123","provider":"codex","title":"Fix quota polling race","turnId":"turn_abc123","eventCount":18,"completed":true,"turnTerminal":{"turnId":"turn_abc123","status":"failed","endedAt":"2026-08-10T15:06:23.921Z"},"lastEventAt":"2026-08-10T15:06:23.921Z"}]}
```

```bash
curl -N -H "Accept: text/event-stream" "$BASE/v1/activity/events"
```

### GET /v1/sessions/{id}/turns

Read a compact, rebuildable Turn index for an active or archived Session.
Each entry contains the Turn id, status, `closed`, full timing,
`startEventId`, `turnStartedEventId`, `lastEventId`, terminal `endEventId`,
and ordered compact `items`. Message items retain complete text and
provenance. Activity items combine uninterrupted thinking/tool work and retain
stable Event ranges, thinking phase count, reasoning update count, tool-call
count, and duration for bounded expansion; approval, error, and lifecycle items
retain their display data directly. Materialized legacy `thinking` and `tool`
items are normalized to `activity` in memory when read, without requiring a
canonical Event scan or an eager `turns.jsonl` rewrite.
All references are stable event IDs in `events.jsonl`; byte offsets and paths
are never exposed.

- **Query parameters:** `after`, `before`, `latest=true`, and `limit` use the
  same mutually exclusive, forward/backward semantics as event pagination.
  A Turn's `firstEventId` is its cursor key.
- **Success `200`:** `{"turns": [...], "page": {...}, "latestCursor": n, "latestEventId": n}`.
- **Errors:** `400 invalid_turn_cursor`, `404 session_not_found`.

```bash
curl -s "$BASE/v1/sessions/$SESSION/turns?latest=true&limit=50"
```

### GET /v1/sessions/{id}/turns/{turnId}

Read one compact active or closed Turn. If its terminal Event is durable but
the materialized record is missing or incomplete, AgentHub repairs
`turns.jsonl` from `events.jsonl` before responding.

- **Success `200`:** `{"turn": {...}, "latestEventId": n}`.
- **Errors:** `404 session_not_found`, `404 turn_not_found`.

```bash
curl -s "$BASE/v1/sessions/$SESSION/turns/$TURN"
```

### POST /v1/sessions/{id}/messages

Send an inbound message. Without an active turn this starts a new turn; with
an active turn the message is rejected unless `steer` is set. Schema v2 sends
`text` to the selected Provider byte-for-byte and persists `payload` as opaque
caller-owned JSON. AgentHub does not derive prompt text, display metadata, or
trust semantics from that payload.

- **Request body:** `{"schemaVersion":2,"text":"...","payload":{...},"steer":false,"messageId":"msg-..."}`
  - `schemaVersion` — use `2` for the opaque payload contract.
  - `text` (required) — the complete provider-facing prompt; blank text is rejected.
  - `payload` (optional) — any valid JSON value. It is stored and returned
    unchanged in meaning and is never interpreted by AgentHub.
  - `steer` (optional) — inject the message into the currently active turn
    instead of starting a new one. Providers that cannot steer an active
    prompt (the ACP providers: Kimi and OpenCode) reject steer requests.
  - `messageId` (optional) — caller-stable idempotency key for this Session.
    Once the canonical input is durable, concurrent retries and retries after
    daemon restart never append it again. A success response distinguishes a
    durable input still pending Provider acceptance from one the Provider has
    accepted; the former stays on the recoverable delivery path. A crash after Provider acceptance but before
    the acceptance Event is durable can produce one or more limited duplicate
    attempts, which is the intentional at-least-once tradeoff.
    Reusing the id with different canonical text, payload, or steer input
    is rejected.
  - Legacy compatibility — if `schemaVersion` is omitted, `role`, `sender`,
    `replyTo`, and `correlationId` retain their historical behavior, including
    AgentHub's provenance header construction. Schema v2 rejects these fields
    so application metadata cannot leak back into the transport abstraction.
- **Success `202`:** `{"session": {...},"delivery":{"messageId":"msg-...","state":"pending|delivered"}}`.
  The request is durably accepted in both states. `pending` means a caller with
  a stable `messageId` may retry the identical request to confirm delivery;
  `delivered` means the Provider accepted it. The turn runs asynchronously;
  watch it through the events endpoint.
- **Errors:** `400 invalid_request`, `415 json_required`,
  `400 invalid_message_schema`, `400 mixed_message_schema`,
  `400 invalid_message_payload`,
  `400 invalid_message_role`, `400 assistant_message_forbidden`,
  `400 invalid_message_sender`, `400 invalid_message_reference`,
  `404 session_not_found`, `409 session_archived`,
  `409 session_stopping`, `409 turn_active` (an active turn exists without
  `steer=true`), `409 runtime_operation_failed` (the provider rejected the
  prompt),
  `503 runtime_unavailable`.

```bash
curl -s -X POST "$BASE/v1/sessions/$SESSION/messages" \
  -H "Content-Type: application/json" \
  -d '{"text": "Now fix the root cause."}'

curl -s -X POST "$BASE/v1/sessions/$SESSION/messages" \
  -H "Content-Type: application/json" \
  -d '{"text": "Skip the refactor and patch the test only.", "steer": true}'

curl -s -X POST "$BASE/v1/sessions/$SESSION/messages" \
  -H "Content-Type: application/json" \
  -d '{"text":"Resume the queued work.","role":"system","sender":{"name":"Workflow Coordinator"}}'

curl -s -X POST "$BASE/v1/sessions/$SESSION/messages" \
  -H "Content-Type: application/json" \
  -d '{"text":"The worker finished its scan.","role":"agent","sender":{"name":"Review Agent","sessionId":"ses_worker"}}'
```

### POST /v1/sessions/{id}/resume

Start (or restart) a stopped session's provider without sending a message. When
the session recorded a provider-native session/thread id, the provider
resumes that native conversation, so context survives daemon and provider
restarts. Safe to call when the provider is already running.

- **Request body:** optional. An empty body (or `{}`) resumes with the
  session's recorded launch environment. `launchEnvironment` (optional) is a
  string-to-string overlay onto that durable environment: supplied entries
  replace same-named values, keys the overlay omits are kept, and nothing is
  deleted. The merged map is validated and persisted as a
  `session.launch-environment` event (carrying the full merged
  `environment` map) **before** the provider starts, so the provider resume
  picks it up; the historical `session.created` snapshot is never rewritten.
  The update stays durable even if the provider then fails to start, and it
  is visible to any client that can read the session — never put secrets
  here. When the provider is already running, the overlay is recorded and
  takes effect on the next provider start.
  `ephemeralEnvironment` (optional) is a one-shot string-to-string overlay
  passed only to the Provider process. It requires the
  `session.ephemeral-environment` capability. Its names and values are never
  persisted or returned; clients must not downgrade it to
  `launchEnvironment`. A Session with `ephemeralEnvironmentRequired: true`
  requires a fresh non-empty `ephemeralEnvironment` on every Provider start.
  An empty body, `{}`, and an explicitly empty map all fail closed without
  changing the stopped Session or invoking the Provider factory. Stop and
  archive retain the marker; archived Sessions remain read-only.
- **Success `200`:** `{"session": {...}}`.
- **Errors:** `415 json_required`, `400 invalid_request` (malformed JSON or
  unknown fields), `404 session_not_found`,
  `422 invalid_launch_environment` (an empty variable name, `=` or NUL in a
  name, or NUL in a value), `422 invalid_ephemeral_environment` (the
  one-shot map contains an invalid name or NUL value), `409 session_archived`,
  `409 session_stopping`,
  `409 runtime_operation_failed` (the provider could not start, for example
  because the recorded agent no longer exists, or a Session marked
  `ephemeralEnvironmentRequired` was resumed without a non-empty overlay),
  `503 runtime_unavailable`.

```bash
curl -s -X POST "$BASE/v1/sessions/$SESSION/resume" \
  -H "Content-Type: application/json" -d '{}'

curl -s -X POST "$BASE/v1/sessions/$SESSION/resume" \
  -H "Content-Type: application/json" \
  -d '{"launchEnvironment": {"SESSION_CONTEXT_ID": "context-456"}}'
```

### POST /v1/sessions/{id}/interrupt

Cancel the session's active turn without stopping the provider. Appends a
`turn.cancelled` event; the provider process stays alive for the next
message.

- **Request body:** empty (`{}`).
- **Success `200`:** `{"session": {...}}`.
- **Errors:** `415 json_required`, `404 session_not_found`,
  `409 session_archived`, `409 turn_not_active`,
  `409 runtime_operation_failed` (the provider rejected interruption),
  `503 runtime_unavailable`.

```bash
curl -s -X POST "$BASE/v1/sessions/$SESSION/interrupt" \
  -H "Content-Type: application/json" -d '{}'
```

### POST /v1/sessions/{id}/stop

Stop the session's provider process. The durable transition is `stopping`
followed by `stopped` with `reason: "requested"` only after the adapter Wait
path and process-group probe confirm that the provider can no longer write
to the working directory. The request does not return early. The session
keeps its full history and can be resumed later. Stop is the
required precondition for archiving a session whose provider is running.

- **Request body:** empty (`{}`).
- **Success `200`:** `{"session": {...}}`.
- **Errors:** `415 json_required`, `404 session_not_found`,
  `409 session_archived`, `409 runtime_operation_failed`,
  `503 runtime_unavailable`.

```bash
curl -s -X POST "$BASE/v1/sessions/$SESSION/stop" \
  -H "Content-Type: application/json" -d '{}'
```

### POST /v1/sessions/{id}/approvals/{approvalId}

Resolve a pending approval. When a provider needs confirmation (for example
to run a command outside its sandbox) it emits an `approval.requested`
event and the session enters `waiting_approval` with the id in
`pendingApprovalIds`.

- **Path parameters:** `id` — session id; `approvalId` — the id from the
  `approval.requested` event.
- **Request body:** exactly one of the following reply modes:
  - `{"decision": "..."}` — a coarse decision, one of:
    - `accept` — approve this request once.
    - `acceptForSession` — approve and allow matching requests for the rest
      of the session where the provider supports it.
    - `decline` — reject the request.
    - `cancel` — cancel the operation that asked for approval.
  - `{"optionId": "..."}` — select one specific option offered by the
    request (the `optionId` values come from
    `approval.requested` → `data.options`). Unknown option ids are
    rejected and the approval stays pending.
  - `{"text": "..."}` — answer a question with custom free text. Provider
    protocols cannot carry free text inside an approval response, so the
    question is dismissed and the text is delivered as a regular user
    message once the current turn closes; the `approval.resolved` event
    records decision `text` with the reply.
- **Success `200`:** `{"session": {...}}`; an `approval.resolved` event is
  appended.
- **Errors:** `400 invalid_request`, `415 json_required`,
  `404 session_not_found`, `409 session_archived`,
  `400 invalid_approval_decision`, `409 approval_not_pending`,
  `409 runtime_operation_failed` (the provider is not running or rejected
  the response). Pending approvals do not survive a daemon restart: recovery
  appends `approval.resolved` with decision `cancel`, closes the open turn,
  and then publishes `stopped` with reason `daemon_recovery`,
  `503 runtime_unavailable`.

```bash
curl -s -X POST "$BASE/v1/sessions/$SESSION/approvals/approval-1" \
  -H "Content-Type: application/json" \
  -d '{"decision": "accept"}'
```

## Notes for Client Authors

- The Web UI served by the daemon, the `agenthub` CLI and this document all
  use the same API; anything the UI can do is available through the
  endpoints above.
- `GET /v1/health` exists for process-level health checks only and is not
  part of the public API surface documented here.
- The daemon writes all state; session files under the data root
  (`events.jsonl`, `session.json`, `turns.jsonl`) are its private storage — read them for
  diagnostics if needed, but never write them.
- JSON request objects are strict: unknown fields, malformed JSON, multiple
  top-level JSON values and bodies larger than 1 MiB are rejected with
  `invalid_request`. JSON response objects are extensible: clients must
  ignore fields they do not recognize.

### Idempotency and conflicts

AgentHub does not interpret an `Idempotency-Key` header. Callers use the JSON
`idempotencyKey` and `messageId` fields for durable retries:

| Operation | Idempotency contract | Important conflict |
| --- | --- | --- |
| Create | Without `idempotencyKey`, every accepted request creates a Session. With a key, identical retries return the original Session across daemon restarts; reuse with different immutable creation input fails. | `idempotency_conflict`; provider startup is 502. |
| Message | Without `messageId`, retry outcome may be ambiguous. With a stable id, concurrent or restarted identical retries append at most one canonical `message.input` event; `delivery.state` distinguishes durable acceptance (`pending`) from Provider acceptance (`delivered`). | Reusing an id with different canonical input is rejected; also `turn_active`, `session_stopping`, `session_archived`. |
| Steer (`messages` with `steer=true`) | The same `messageId` rule applies. Active-turn steer is accepted only when `session.inputCapabilities.steer` is true. With no active turn it starts a normal turn. | `session_stopping`, `session_archived`; unsupported steer and provider rejection use `runtime_operation_failed`. |
| Approval | Non-idempotent. Once resolved, the same id is no longer pending. | `approval_not_pending`, `session_archived`. |
| Interrupt | Not retry-idempotent: a successful call creates one `turn.cancelled`; a repeat sees no active turn. | `turn_not_active`, `session_archived`. |
| Stop | Idempotent for an already stopped session. It blocks through confirmed provider exit. | `session_archived`; process cleanup failure uses `runtime_operation_failed`. |
| Resume | Idempotent while the provider is already running; one provider instance remains associated with the session. | `session_stopping`, `session_archived`. |
| Archive | Idempotent for an already archived session. | `session_active`, `session_archive_conflict`. |

### Complete session flow

The following sequence negotiates the contract, creates a PUA-owned
session, catches up through the durable cursor, sends a message, resolves an
approval if one appears, waits for a canonical terminal event, then stops
and resumes the session. Shell snippets use `jq` for brevity:

```bash
STATUS=$(curl -fsS "$BASE/v1/status")
test "$(jq -r .apiVersion <<<"$STATUS")" = "1"
for capability in session.source session.source-metadata \
  session.idempotent-create session.input-capabilities \
  session.launch-environment session.launch-environment-update \
  session.ephemeral-environment \
  session.strict-stopped messages.idempotent messages.at-least-once \
  messages.delivery-result \
  messages.opaque-payload-v2 turns.stable-index turns.materialized \
  events.lossless-replay events.semantic-v1 event.raw-v1 \
  events.canonical-turn-terminals recovery.closed-turns; do
  jq -e --arg c "$capability" '.capabilities | index($c) != null' \
    <<<"$STATUS" >/dev/null
done

CREATED=$(curl -fsS -X POST "$BASE/v1/sessions" \
  -H "Content-Type: application/json" \
  -d "{
    \"title\":\"PUA task\",
    \"cwd\":\"$PWD\",
    \"agentName\":\"Codex\",
    \"idempotencyKey\":\"project7.task30:generation-1\",
    \"source\":{\"app\":\"pua\",\"instanceId\":\"mac-mini\",\"externalId\":\"project7.task30\",\"metadata\":{\"generationId\":\"generation-1\"}},
    \"launchEnvironment\":{\"SESSION_CONTEXT_ID\":\"context-123\"}
  }")
SESSION=$(jq -r .session.id <<<"$CREATED")

PAGE=$(curl -fsS "$BASE/v1/sessions/$SESSION/events?after=0&limit=500")
CURSOR=$(jq -r .page.nextAfter <<<"$PAGE")

curl -fsS -X POST "$BASE/v1/sessions/$SESSION/messages" \
  -H "Content-Type: application/json" \
  -d '{"schemaVersion":2,"text":"Implement the requested change.","messageId":"msg-example","payload":{"source":"example-client"}}'

# Reconnect with Last-Event-ID=$CURSOR. Process each adjacent frame cursor,
# merge append patches, swap in full replacements for repeated cursors, and
# ignore unknown semantic types. If approval.requested arrives, resolve its
# approvalId:
curl -fsS -X POST "$BASE/v1/sessions/$SESSION/approvals/approval-1" \
  -H "Content-Type: application/json" \
  -d '{"decision":"accept"}'

# End the turn only on turn.completed, turn.failed or turn.cancelled, never
# on provider.turn.completed. Then release and later restore the provider:
curl -fsS -X POST "$BASE/v1/sessions/$SESSION/stop" \
  -H "Content-Type: application/json" -d '{}'
curl -fsS -X POST "$BASE/v1/sessions/$SESSION/resume" \
  -H "Content-Type: application/json" -d '{}'
```
