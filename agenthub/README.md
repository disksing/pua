# AgentHub

[简体中文](README.zh-CN.md)

AgentHub is a local agent launcher and session hub. A single Go daemon manages Codex, Kimi, Pi/Grok, and OpenCode on your machine, and both the Web UI and the CLI work through the same HTTP API and SSE event stream.

## Capabilities

- Listens on loopback only by default; LAN/wildcard/IPv6 addresses can be configured explicitly. No accounts, tokens, or API authentication.
- Independent provider and agent configuration.
- Sessions are always created with an explicitly selected agent; there is no implicit routing or fallback.
- Real provider adapters:
  - Codex app-server
  - Kimi / OpenCode ACP v1
  - Pi JSONL RPC, including models such as Kimi K3 and Grok
- Session creation, chat, steer, interrupt, stop, resume, archive, and approvals.
- On-demand recovery of provider-native sessions/threads after a daemon restart.
- Same-origin Web UI: session list, real-time chat, status, approvals, stop, structured settings, and a floating activity/quota companion with optional Web Audio beeps.
- Provider model enumeration: each built-in provider reports its currently usable models through its official interface, normalized into one read-only API.
- CLI: one-shot runs, interactive chat, attach, event queries, and session management.
- Each session stores an append-only `events.jsonl` source of truth, rebuildable `session.json`, and rebuildable compact `turns.jsonl`; approvals remain canonical Events rather than a separate authority.

## Build and Run

AgentHub now lives in the PUA repository and shares its Go module and release
build. Requires Go 1.26+ and Node.js. From the repository root:

```bash
scripts/build
bin/agenthub serve
```

Open <http://127.0.0.1:4646/agenthub/>. The standalone root URL redirects
there; the API is rooted at `/agenthub/v1`, and the API reference is
`/agenthub/api.md`. The binary embeds the Web UI and does not need external
frontend files at runtime.

PUA embeds this same application by default at
`http://127.0.0.1:4936/agenthub`. Run the standalone binary only when AgentHub
must operate without PUA or when PUA is explicitly started with
`--agenthub-mode=external`. Both forms keep `~/.agenthub` as the data root and
must not hold its daemon lock at the same time.

### Listen Address

By default the daemon listens on loopback only; `agenthub serve` is equivalent to `agenthub serve --addr 127.0.0.1:4646`. To make the daemon reachable from other devices on the LAN, explicitly choose a local address with `--addr host:port`:

```bash
agenthub serve --addr 192.168.2.150:4646   # a specific LAN IPv4 address
agenthub serve --addr 0.0.0.0:4646         # all IPv4 interfaces (wildcard)
agenthub serve --addr '[::]:4646'          # all IPv6 interfaces (wildcard)
agenthub serve --addr '[::1]:4646'         # IPv6 loopback only
agenthub serve --addr myhost.local:4646    # a hostname/domain that resolves to a local interface
```

The hostname must resolve to a local network interface or loopback. Unresolvable hostnames, addresses of non-local interfaces, malformed values, and invalid ports all fail at startup with an error; there is no silent fallback to another address. IPv6 addresses must be enclosed in square brackets.

> **Security warning**: AgentHub has no accounts, tokens, or API authentication. When listening on a non-loopback or wildcard address, any device that can reach that address gets full control of the daemon (running agents, modifying sessions and configuration). Only use this on trusted networks, and never expose it directly to the public internet. The startup log prints the same warning.

When listening on a non-loopback address, the local CLI still discovers the daemon automatically through the loopback endpoint in `server.json`. Browser write requests still require the `Origin` to match the request `Host`, and the `Host` must be a local interface address or the local hostname (to prevent DNS rebinding); arbitrary origins are not accepted.

### Reverse Proxy

When the daemon sits behind a reverse proxy that terminates TLS (Caddy, nginx, ...), browser write requests carry the public https `Origin`, which does not match the daemon's own origin and would be rejected with `403 origin_rejected`. Trust the public origin explicitly (repeatable):

```bash
agenthub serve --addr 0.0.0.0:4646 --allow-origin https://agenthub.example.com:8443
```

Browsers forbid forging the `Origin` header, so the allowlist only admits the exact origins you configure; all other cross-origin writes stay rejected. Keep the proxy rewriting `Host` to the upstream address (the default for Caddy's `reverse_proxy`) so the DNS-rebinding Host guard keeps passing.

During development you can run the two parts separately:

```bash
go run ./cmd/agenthub serve
cd ../web && npm run dev -- --config vite.agenthub.config.ts
```

Vite proxies `/agenthub/v1` to the default daemon port.

## CLI

The CLI ships layered help: `agenthub help` prints an overview plus a concept guide (providers, agents, sessions, turns, approvals, events), `agenthub help <command>` (optionally `agenthub help session <subcommand>`) prints per-command usage, options, defaults and examples, and `agenthub <command> --help` does the same inline. Unknown commands and invalid arguments exit non-zero with a pointer to the matching help topic.

```bash
agenthub help
agenthub help session approve
agenthub serve --help

agenthub status
agenthub agents

agenthub run --agent "Kimi K3" --cwd . "Investigate why the tests fail"
agenthub run --agent "Codex" --cwd . "Implement this feature and run the tests"

agenthub chat --agent "Codex GPT" --cwd .
agenthub session create --agent "Kimi K3" --title "bug hunt"
agenthub session attach <session-id>
agenthub session list
agenthub session list --archived
agenthub session show <session-id>
agenthub session events <session-id>
agenthub session resume <session-id>
agenthub session interrupt <session-id>
agenthub session stop <session-id>
agenthub session approve --decision accept <session-id> <approval-id>
agenthub session archive <session-id>
```

Interactive chat supports `/interrupt`, `/stop`, and `/quit`. The CLI automatically discovers the endpoint from the daemon's `server.json`; you can also set `AGENTHUB_ENDPOINT`.

## Configuration

The default config file is:

```text
$HOME/.agenthub/config.json
```

On first startup, if the config does not exist, AgentHub generates its own minimal default configuration; it does not read or migrate any other program's config. Config structure:

```json
{
  "version": 1,
  "agentProviders": [
    { "id": "codex", "name": "Codex app-server", "type": "codex", "enabled": true },
    { "id": "kimi", "name": "Kimi Code", "type": "kimi", "enabled": true },
    { "id": "pi", "name": "Pi Coding Agent", "type": "pi", "enabled": true }
  ],
  "agents": [
    {
      "name": "Kimi K3",
      "providerId": "pi",
      "options": { "mode": "build", "model": "kimi-coding/k3" }
    }
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
```

The floating companion can be dragged anywhere inside the viewport. Its
normalized position is stored in browser-local storage, survives reloads and
stays visible after a resize. Opening it chooses an upward/downward and
leftward/rightward direction from that position so the card remains on-screen.
The open card can be resized from its outward corner; the saved size is clamped
to the available viewport and its controls, waveform, quota columns, and
scrolling layout respond to narrower, shorter, or wider dimensions.
Its header can open the same interface in a new tab at `/agenthub/beeper`. That
standalone monitor uses the whole viewport as a dark surface, keeps quota
providers in one column in portrait orientation, and still provides settings
access. Its activity waveform is driven by the global AgentHub activity SSE
stream. Each Session creates one pulse per one-second frame regardless of its
underlying event count; concurrent Session pulses enter at the right, share the
same four 250ms subdivisions as their activity beeps, and scroll smoothly to
the left. One to four active Sessions use deterministic density patterns;
additional chord tones stack on rotating subdivisions instead of shortening
the grid. Active Session labels use one full-width row each and brighten on new
activity before fading back over ten seconds. A successfully finished Turn
leaves its row yellow for five minutes, while failed and cancelled Turns leave
it red; a new Turn restores the active styling. Its assigned tone is released
ten seconds after the terminal event even though the row remains visible.
The Activity settings are browser-local preferences stored in `localStorage`;
they are not sent to the daemon or included in AgentHub configuration. They can
hold one selected chord or use the built-in Canon in C
progression (`C - G - Am - Em - F - C - F - G`). Each chord independently
holds for a random one to six one-second frames. At a boundary, every Session
immediately switches to a discrete tone from the new chord without pitch bends
or staggered migration. Completion playback uses the six bundled Codex Beeper
MP3 sounds.

A provider wraps a local agent runtime or protocol; an agent references one provider and holds its concrete launch options. An agent has no separate id: its `name` is required (up to 80 characters), is unique case-insensitively after trimming surrounding whitespace, and is the only reference key — the config, the API, the CLI and session records all use it. Every session is created with an explicit agent name (`POST /v1/sessions` requires `agentName`, and the CLI requires `--agent`); name matching is case-insensitive and sessions record the canonical configured spelling. An unknown, missing, or disabled-provider agent fails with a clear error instead of being routed elsewhere.

Renaming an agent is safe: when a config save replaces a name with exactly one otherwise identical agent, the daemon appends a `session.agent` event to every active session that referenced the old name, so those sessions follow the rename. An ambiguous rename (several identical candidates) is rejected with an actionable error, and deleting or renaming so that no unique target exists leaves old sessions failing with a clear "unknown agent" error rather than guessing.

Codex agents accept the options `model`, `sandbox`, `approval`, and `reasoning_effort`. `reasoning_effort` controls the Codex reasoning ("thinking") effort: it is sent as the `model_reasoning_effort` config override on `thread/start` and `thread/resume`, and the daemon validates the value against the efforts the selected model advertises via `model/list` (for example `low`, `medium`, `high`, `xhigh`; some models add `max` and `ultra`). An unsupported value fails session creation with the list of valid values; an empty value inherits the Codex default. Codex Resume restores the native thread while excluding historical turns from the JSON-RPC response because AgentHub already owns the durable normalized event history.

### Per-session launch environment

`POST /v1/sessions` accepts an optional string map named `launchEnvironment`. It is merged over the daemon environment for that session's Provider process, so a session value wins when both define the same variable. Codex receives the merged process environment and each session entry as `shell_environment_policy.set.<KEY>` on both `thread/start` and `thread/resume`; ACP and Pi receive the merged process environment. The value is part of the durable `session.created` event, so it survives event replay, daemon restarts, and Provider resume without leaking into another concurrent session. Older sessions without the field continue to inherit the daemon environment unchanged.

The map is deliberately persisted in the Session's `events.jsonl` and rebuildable `session.json`, and is returned by Session API responses. Do not put credentials or any other value in `launchEnvironment` that you do not want stored on disk. Session files remain private (`0600`), but that is not a substitute for secret storage.

When the daemon advertises `session.ephemeral-environment`, create and resume
requests may include an `ephemeralEnvironment` string map. It is merged last
for that Provider process only and discarded when the process exits. The first
non-empty overlay used to start a Provider sets the durable, non-secret
`ephemeralEnvironmentRequired: true` Session marker, which Session responses
return. That marker survives stop, archive, event replay, and daemon restart,
but the overlay's names and values are never written to Session events,
`session.json`, API responses, or history.

Once the marker is set, every future Provider start requires a fresh non-empty
overlay. Implicit delivery retries and Provider restarts remain stopped because
they cannot supply one. While the Session is stopped, Resume with an empty body,
`{}`, or an explicitly empty `ephemeralEnvironment` map returns `409
runtime_operation_failed` without starting a Provider. Resume remains
idempotent while a Provider is already running: it does not start or alter that
process, so no fresh overlay is required or consumed. Clients that require this
behavior must fail closed when the capability is absent; they must never put the
ephemeral values in `launchEnvironment` as a fallback.

### Agent environment variables

An agent configuration may also carry an optional string map named `environment`, editable in the Web UI's **Agents** panel or in `config.json`. When the daemon starts a Provider process for that agent, the agent environment is merged under the daemon environment and the session's `launchEnvironment` is merged on top, so the precedence is `daemon < agent < session launchEnvironment` (a per-session value wins over the agent default). Codex receives the merged process environment and each entry as `shell_environment_policy.set.<KEY>` on both `thread/start` and `thread/resume`; ACP and Pi receive the merged process environment.

Unlike the durable per-session `launchEnvironment`, the agent environment is live configuration: a session only records the agent name, so starting or resuming a session re-reads the agent's current environment. This makes it the right place for defaults shared by every session of that agent. Values are stored in `config.json` (private, `0600`) and returned by `GET /v1/config`; do not put credentials there.

### Model Enumeration

Each built-in provider can report the models currently usable on this machine through its official interface — no provider session is created and nothing is written to provider configuration:

- Codex: the app-server `model/list` request (account-scoped, with display names, a default flag, and hidden-model filtering).
- Kimi: `kimi provider list --json` (the configured model registry, with display names).
- Pi: the RPC `get_available_models` command in `--no-session` mode (every configured upstream; the model Pi would use by default is flagged).
- OpenCode: `opencode models --verbose` (configured providers plus OpenCode Zen free models, with display names).

`GET /v1/providers/{id}/models` normalizes all four into `{ "provider": {...}, "models": [{ "id", "label", "default" }] }`, where `id` is exactly the value to put into an agent's `model` option. Results are deduplicated, kept in provider order, cached briefly (5 minutes for successes, 15 seconds for failures) with concurrent lookups deduplicated, and the cache is dropped on every configuration change (whole-config save or provider toggle). Failures are classified so clients can render them differently: `404 unknown_provider`, `409 provider_disabled`, `503 provider_unavailable` (CLI missing or not startable), `504 provider_timeout`, and `502 provider_error` (upstream or parse failure); an empty list is a successful `200` with `"models": []`. The endpoint is read-only: it never creates a provider session and never changes configuration.

In the Web settings, the agent **Model** field is a dropdown fed by this endpoint instead of a free-text input: pick the provider first, then choose a model. The empty "Provider default" choice simply omits the `model` option. A previously saved model that is not in the current list is kept as an explicit "saved, not currently listed" option until you pick a replacement, and loading, retry, empty, and disabled-provider states are shown inline.

The Web UI's **Settings** panel is the recommended way to manage this configuration. The **Providers** section is intentionally minimal: exactly four switches enable or disable the built-in providers (Codex, Kimi, Grok/Pi, OpenCode). There is no provider add/delete and no editing of commands, arguments, environment variables or other advanced fields. A toggle flips only the `enabled` flag through `PUT /v1/config/providers/{id}`, so the underlying configuration survives a disable/enable cycle; a built-in provider missing from an old config is created with canonical defaults when it is first enabled. The **Agents** section keeps structured, validated forms, and provider command availability probes distinguish *enabled* from *CLI available*. All changes go through the daemon API, which remains the only writer of the config file — no manual JSON editing is required.

The **General** section configures the daemon-backed OnWatch integration. Provider quota is fetched only by the daemon from OnWatch, normalized, cached for the configured interval, and exposed through `GET /v1/quota`; Basic Auth passwords are stored in the local `0600` config but redacted from every API response. The **Activity** section contains browser-local preferences for activity, sounds, and per-quota visibility, stored under the current browser origin and never sent through `GET` or `PUT /v1/config`. It lists the quota rows currently returned by `/v1/quota`; hiding one removes that row from both the expanded Beeper card and the collapsed quota rotation. Balance-style quota rows (e.g. DeepSeek credit balance) also expose a per-provider balance total: remaining share is re-derived as current balance / total (default `100`). Session activity comes only from AgentHub's own durably appended, user-visible Turn events and is aggregated per Session into one-second frames at `GET /v1/activity/events`. Messages, reasoning, tools, approvals, Turn errors, and Turn terminals count as activity; Session/process lifecycle bookkeeping, message-delivery bookkeeping, raw Provider notifications, background metadata, and stderr do not. This prevents daemon restarts and idle Provider maintenance from making old Sessions appear active. The browser keeps one global EventSource and synthesizes optional activity beeps with Web Audio. Each active Session receives the first available tone from the selected major or minor triad in octave-band order 5, 4, 6, 3, then 7, prioritizing the lower bands before the highest tone; the tone remains stable until ten seconds after the current Turn ends or until the Session leaves the active window, and the first 15 concurrent Sessions use distinct slots. Terminal rows remain visible for five minutes: successful Turns are yellow, failed or cancelled Turns are red, and a new Turn restores the active state and obtains a tone again. Playback is quantized to four 250ms subdivisions with deterministic patterns and rotation; above four Sessions, chord tones share subdivisions with normalized gain instead of creating irregular fractions of a second. The Activity settings can switch between holding the selected chord and the built-in Canon in C progression (`C - G - Am - Em - F - C - F - G`). Each progression chord independently lasts a random one to six one-second frames; all Sessions switch immediately at the boundary, using discrete pitches without bends or staggered migration. The selected local MP3 completion sound remains independent of the activity chord. AgentHub never scans Codex or another Provider's native Session files for this feature.

A disabled provider's agents are reported as unavailable (`available: false` with a reason in `GET /v1/agents`), are hidden from the new-session choices, and are rejected by the daemon on session creation and resume even when a client bypasses the Web UI. Disabling never interrupts an already running session, and existing session history stays readable.

### Removed Older Formats

Sessions now use the explicit Agent name as their only identity. Agent Profiles, tag routing, `defaultChatAgentId`, Agent `id` fields, and `POST /v1/sessions`'s `agentId` field are no longer accepted. The daemon does not rewrite old config files, create an id-mapping sidecar, or replay event payloads that use the removed identity fields. Convert or back up older config and session data before starting this version.

Command discovery order: the provider's `command`, `AGENTHUB_*_CLI`, then `PATH`. Supported:

- `AGENTHUB_CODEX_CLI`
- `AGENTHUB_OPENCODE_CLI`
- `AGENTHUB_KIMI_CLI`
- `AGENTHUB_PI_CLI`

`AGENTHUB_HOME=/path` isolates all config, data, and runtime state into a single directory; the config file then lives at `/path/config/config.json`, which is useful for testing. The layout is explicit and is read as-is.

## API

The daemon serves a complete Markdown API reference at **`GET /agenthub/api.md`**
(`text/markdown; charset=utf-8`): every public endpoint with parameters,
request and response bodies, error codes, curl examples and the SSE event
contract. It is embedded in the binary, needs no frontend build, and is kept
in sync with the registered routes by automated tests. Fetch it with:

```bash
curl -s http://127.0.0.1:4646/agenthub/api.md
```

The canonical client endpoint is `http://host:port/agenthub`; every API path
below is relative to that endpoint. For example, `GET /v1/status` resolves to
`GET /agenthub/v1/status` and is the compatibility handshake. It returns
`"apiVersion": "1"` and only the capabilities this daemon instance can
actually exercise: `session.source`, `session.launch-environment`,
`session.source-metadata`, `session.idempotent-create`,
`session.input-capabilities`, `messages.idempotent`, `messages.at-least-once`,
`messages.opaque-payload-v2`, `turns.stable-index`, `turns.materialized`, `turns.activity-items`,
`session.launch-environment-update`, `session.ephemeral-environment`,
`session.strict-stopped`, `events.lossless-replay`, `events.delta-merge`,
`activity.global-sse`,
`events.canonical-turn-terminals`, and `recovery.closed-turns`. Recovery closes interrupted delivered Turns, while a durably accepted input still awaiting Provider delivery remains on the at-least-once retry path. A client
must reject an unsupported API version or a missing required capability
before creating a session; older daemons with neither field are explicitly
incompatible. Unknown additional capabilities, response fields and event
types must be ignored.

Every non-2xx public API response uses the same JSON error envelope with a
stable `code`, human-readable `message`, boolean `retryable`, optional
`details`, and `requestId`. Unknown routes and unsupported methods are JSON
errors too. The three provider-independent turn terminal events are
`turn.completed`, `turn.failed`, and `turn.cancelled`; provider-native
completion events are diagnostic and must not close a client turn.

Main endpoints:

```text
GET    /v1/health
GET    /v1/status
GET    /v1/config
PUT    /v1/config
POST   /v1/onwatch/test
GET    /v1/quota
GET    /v1/activity/events
GET    /v1/agents
GET    /v1/providers/{id}/models

POST   /v1/sessions
GET    /v1/sessions
GET    /v1/sessions/{id}
DELETE /v1/sessions/{id}
POST   /v1/sessions/{id}/messages
POST   /v1/sessions/{id}/resume
POST   /v1/sessions/{id}/interrupt
POST   /v1/sessions/{id}/stop
POST   /v1/sessions/{id}/approvals/{approvalId}
GET    /v1/sessions/{id}/events
GET    /v1/sessions/{id}/event/{sourceEventId}
GET    /v1/sessions/{id}/turns
```

The events endpoint returns provider-neutral `agenthub.semantic-events.v1` frames in paginated JSON with an exclusive `after` cursor, `nextAfter`, `hasMore`, and the latest durable cursor. With `Accept: text/event-stream` or `?stream=true` it returns the same frames over SSE and uses the exclusive cursor through `Last-Event-ID`. The daemon subscribes before capturing a high-water mark, replays the entire durable backlog in pages, and then switches to live delivery; overflow closes the stream so reconnecting from the last contiguous id can recover from `events.jsonl`, including after a daemon restart. The singular event endpoint is the only public route that returns an exact raw source Event for diagnostics.

Clients may attach optional caller-defined correlation metadata when creating a session:

```json
{
  "agentName": "Codex",
  "cwd": "/path/to/project",
  "source": {
    "app": "pua",
    "instanceId": "mac-mini",
    "externalId": "project7.task26/1",
    "metadata": {"resourceId": "project7.task26", "generationId": "gen-1"}
  },
  "idempotencyKey": "gen-1"
}
```

An identical create retry with the same `idempotencyKey` returns the original
Session, including after daemon restart. Inbound messages can similarly carry
a stable `messageId`. Session responses advertise `inputCapabilities.steer`,
so orchestrators can queue instead of steering providers that do not support
active-turn input. The Turn endpoint exposes a compact index whose references
are stable event IDs and remains readable after archival.

The `source` object is persisted in `session.created`, rebuilt into `session.json` on replay, and returned by session GET/list responses. It is deliberately self-asserted metadata: AgentHub does not register applications, reserve names, authenticate values, enforce uniqueness, or isolate tenants. Any client may submit any values and duplicates are valid. `GET /v1/sessions` accepts exact, case-sensitive `sourceApp`, `sourceInstanceId`, and `sourceExternalId` filters in any combination; they also compose with `includeArchived`, `archived`, and `state`. Sessions created without source metadata remain compatible and do not match source filters. See the complete [HTTP API reference](http://127.0.0.1:4646/agenthub/api.md) served by the daemon.

### Opaque message payloads

Schema-v2 inputs contain provider-facing `text` and an optional caller-owned
JSON `payload`. AgentHub stores and returns the payload without interpreting
it, while forwarding `text` to the Provider byte-for-byte. `steer` and
`messageId` remain AgentHub delivery controls. Application provenance,
correlation, and presentation metadata belong inside `payload`.

Inputs without `schemaVersion` retain the legacy contract: AgentHub accepts
`role`, `sender`, `replyTo`, and `correlationId`, and constructs the historical
Provider prompt header. Existing durable `message.input`, `message.user`, and
`message.user.steer` events continue to replay without rewriting session logs.

Examples:

```json
{"schemaVersion":2,"text":"Message from agent \"Review Agent\":\nThe worker finished its scan.","payload":{"schema":"my-app.message.v1","text":"The worker finished its scan.","role":"agent","sender":{"name":"Review Agent"}},"messageId":"msg-42"}
{"text":"Legacy input","role":"agent","sender":{"name":"Old Client"}}
```

### Semantic Events

`GET /v1/sessions/{id}/events` exposes the provider-neutral
`agenthub.semantic-events.v1` protocol to every client. AgentHub Web keeps its
presentation projector locally and depends only on that public protocol; it
does not publish or require a shared timeline package. Existing Provider raw
events are normalized when read, while new tool activity is persisted as
canonical `tool.call` source events. Use the singular
`GET /v1/sessions/{id}/event/{sourceEventId}` endpoint only for exact raw
diagnostics.

### Archiving Sessions

`DELETE /v1/sessions/{id}` archives a session: the daemon appends a durable `session.archived` event and then moves the whole session directory into the store's `Archive/` subdirectory (`sessions/Archive/<session-id>/`). Nothing is deleted — `session.json`, `events.jsonl` and all other files move along.

- Only strictly stopped sessions can be archived, with no open turn or pending approval. The endpoint never force-stops a session; stop it first with `POST /v1/sessions/{id}/stop`.
- Status codes: `200` archived (repeating an archive is idempotent), `404` unknown session, `409 session_active` the session still has active work, `409 session_archive_conflict` the archive target is occupied, `500 session_archive_failed` a filesystem error (the session's data stays intact and a retry or a daemon restart completes the move).
- `GET /v1/sessions` hides archived sessions by default; use `?includeArchived=true` to include them or `?archived=true` to list only archived sessions. `GET /v1/sessions/{id}` and the events endpoint keep working for archived sessions.
- Archived sessions are read-only: `messages`, `resume`, `interrupt`, `stop` and approval writes return `409 session_archived`. Unarchiving is not supported.

The CLI equivalents are `agenthub session archive <id>`, `agenthub session list --all` and `agenthub session list --archived`; the Web UI offers an Archive action with an in-app confirmation and an "Archived Sessions" view.

### Provider Startup Failures

Session creation starts the provider synchronously: the handshake requests (`initialize`, `session/new` / `thread/start`, and their resume/load variants) must answer within a 2-minute startup timeout. A provider that cannot answer — for example a process stuck reading the session working directory because the operating system is holding a privacy permission prompt — fails the request instead of hanging it:

- The API returns `502 provider_start_failed` with the provider's real error and, on timeout, an actionable hint (on macOS this points at System Settings > Privacy & Security prompts, e.g. the Downloads folder or Full Disk Access). The Web New Session dialog shows this message.
- The session is kept for diagnostics with `provider.error`, any open turn is failed, and the session converges to `stopped` with `stopReason: "startup_error"` only after process exit is confirmed. It can then be inspected, resumed, archived, or left alone.

### Strict stopped lifecycle and crash recovery

`stopped` is the single trustworthy provider-release boundary. A stop request
first appends `stopping`; the stop call returns and the final
`session.state {"state":"stopped","reason":"requested"}` event is appended
only after the adapter Wait path and process-group probe confirm that the
provider and its descendants cannot write to the working directory.

All exit paths use the same terminal sequence. Clean provider exit uses
`completed`; a crash records `provider.error`, closes approvals and the open
turn, then uses `provider_error`; startup failure uses `startup_error`;
explicit stop and graceful daemon shutdown use `requested`. If the daemon is
killed, the next daemon uses durable `provider.process.started` evidence to
terminate any surviving process group, deterministically cancels pending
approvals and the open turn, and finishes with `daemon_recovery`.

Active provider turns have no fixed wall-clock deadline. ACP `session/prompt`
and Pi `prompt`/`steer` requests wait for the provider's real terminal result,
even when reasoning or tool work runs longer than 15 minutes. Users can still
end work explicitly with interrupt or stop, and provider exit or daemon
shutdown releases the pending request. Startup handshakes keep their 2-minute
bound, while ordinary control requests keep a separate bounded timeout.

## Data and Security

All persistent user data lives under a single root, `$HOME/.agenthub`:

```text
~/.agenthub/
├── config.json                 (providers and agents)
├── sessions/<session-id>/
│     session.json
│     events.jsonl
├── sessions/Archive/<session-id>/   (archived sessions, same files)
├── logs/                       (service stdout/stderr when installed as a service)
├── server.json                 (transient daemon endpoint discovery)
└── server.lock                 (transient single-daemon lock)
```

`events.jsonl` is the single source of truth; `session.json` and compact `turns.jsonl` are rebuildable projections. Writes persist the canonical Event before updating either projection. A partial final line caused by an interrupted current write is repaired at startup or query time. The archive is a plain directory move inside the same store: if the daemon stops between the archived event and the move, startup completes the move, so the physical location always matches the event log. Directories are created `0700` and sensitive files `0600`.

`agenthub status` (and `GET /v1/status`) reports the effective config, session store, archive and logs paths, so you can confirm the layout after an upgrade.

### Data Layout

The daemon reads only the unified `~/.agenthub` layout. Older releases may have stored sessions under an operating-system data directory (for example `~/Library/Application Support/agenthub` on macOS) and logs under `~/Library/Logs/AgentHub`; those paths are no longer read or migrated automatically. Before upgrading, perform a one-time, verified copy or export into the current layout and keep a backup. The daemon never merges two roots or chooses a winner.

The no-authentication mode is only suitable for the local machine and trusted networks: it listens on loopback only by default, sends no permissive CORS headers, rejects cross-origin browser write requests, and verifies that the request Host points to a local address.

## Verification

Backend tests compile directly from a clean PUA checkout; they do not require
building the frontend first. PUA and AgentHub share the Svelte/TypeScript
project under `../web`, while keeping separate Vite entries, embedded assets,
and binaries. The repository-level `scripts/build` command builds both app
entries and enforces their generated entrypoints for release binaries.

```bash
go test -race ./...
go test -race -count=1 -tags=integration ./integration
go vet ./...
cd ../web
npm ci
npm run check
npm run build:agenthub
npm test
npm run test:sites
```

The separately invoked real-process
[PUA integration gate](docs/pua-integration-gate.md) launches an
isolated daemon and fake ACP provider subprocesses, injects lifecycle and
streaming failures, and verifies cleanup, recovery, replay, capabilities,
and structured errors across the process boundary.

The implementation has also been integration-tested locally against real providers: Codex app-server, Kimi ACP, Pi/Kimi K3, Pi/Grok, Codex native thread recovery across restarts, and Kimi creating and writing files in the workspace.

## License

This project is released under the [BSD 3-Clause License](LICENSE) (New BSD License / Revised BSD License).
