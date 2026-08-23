# PUA Integration Gate

AgentHub includes a black-box contract gate for clients such as PUA. The
gate starts a race-enabled `agenthub serve` binary and a deterministic fake
ACP provider as separate operating-system processes. It uses a new
`AGENTHUB_HOME`, working directory, loopback port, daemon process group, and
provider process group for every case. It never discovers or reads the user's
default AgentHub data root.

Run the complete gate with:

```bash
go test -race -count=1 -tags=integration ./integration
```

The normal backend command, `go test -race ./...`, intentionally excludes this
process-level gate. CI runs the command above as a separate required job.

## Covered contract

The process-level scenarios cover:

- source metadata persistence, exact combined filters, replay, and daemon
  restart;
- per-session launch environment propagation, parallel isolation, native
  provider resume, and restart;
- the strict stopped boundary for startup failure, clean provider exit,
  provider crash, concurrent stop/exit, concurrent resume/stop, and archive;
- graceful daemon `SIGTERM`, ungraceful `SIGKILL`, orphan provider process
  cleanup, and deterministic closure of an open approval and turn;
- a 5,000-plus durable event backlog, interrupted SSE, subscriber overflow,
  REST catch-up, cursor-ahead errors, and restart while a turn is open;
- the API version, advertised capabilities, canonical turn terminals, and
  structured non-2xx error envelope.

The fake provider accepts fault controls only through the isolated session
launch environment. It can crash during startup or a turn, hold a prompt,
request an approval, exit normally, and emit a large event burst. These are
test fixtures, not public AgentHub configuration options.

SSE event and heartbeat writes are bounded to five seconds. A client that
stops reading therefore cannot pin a handler after its subscriber queue
overflows; it is disconnected and must resume from its last contiguous
durable cursor.

Every test registers cleanup before starting a process. Cleanup terminates
the daemon, and the runtime's strict stopped contract terminates and probes
the provider process group. Temporary roots are removed by the Go test
harness. Tests may run repeatedly or in parallel without sharing ports,
processes, or session directories.

## PUA responsibilities

AgentHub provides durable sessions, process lifecycle, replay cursors,
capability negotiation, and structured errors. PUA still owns:

- Workspace, Project, Task, and Agent Profile configuration;
- resource-scoped file-ownership instructions and GUI run projections;
- validating required capabilities before creating a session;
- generating a unique source identity and any caller-specific per-session
  launch environment;
- persisting its last contiguous event cursor and reconnecting after any SSE
  interruption or unknown event;
- deduplicating client retries according to each endpoint's documented
  idempotency, and reconciling ambiguous message delivery through events;
- deciding when a stopped session should be resumed or archived.

When a Session reports `ephemeralEnvironmentRequired: true`, PUA must attach a
fresh non-empty `ephemeralEnvironment` to every resume that can start a new
Provider process. AgentHub retains only that boolean requirement across stop,
archive, provider failure, and daemon replay; it never retains the overlay's
names or values, and implicit delivery retries remain stopped until PUA
explicitly supplies another overlay.

PUA no longer exposes resource Session Locks, AutoRun/Self-Driving, a
reserved Scheduler Profile, or public commands for manually creating,
binding, heartbeating, or ending its local Session projections. Those local
projections are created and reconciled internally by `pua serve`.

Source metadata is self-asserted correlation data, not authentication or
tenant isolation. Launch environment values are durable and visible through
the Session API, so PUA must not place secrets there unless persistence is
intended.
