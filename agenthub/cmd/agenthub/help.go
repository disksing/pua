package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// helpOutput is where help text is written. Tests may redirect it.
var helpOutput io.Writer = os.Stdout

// isHelpFlag reports whether arg is a conventional help flag.
func isHelpFlag(arg string) bool {
	switch arg {
	case "-h", "--help", "-help":
		return true
	}
	return false
}

// hasHelpFlag reports whether any argument is a help flag.
func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if isHelpFlag(arg) {
			return true
		}
	}
	return false
}

// printTopic writes a registered help topic to helpOutput.
func printTopic(name string) {
	fmt.Fprint(helpOutput, helpTopics[name])
}

// usageError builds an error for invalid arguments that points at the
// matching help topic.
func usageError(usage, topic string) error {
	return fmt.Errorf("usage: %s\nRun 'agenthub help %s' for usage.", usage, topic)
}

// flagParseError converts a flag.Parse error: on -h/--help it prints the
// named help topic and returns nil; otherwise it wraps the error with a
// pointer to the matching help topic.
func flagParseError(err error, topic string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, flag.ErrHelp) {
		printTopic(topic)
		return nil
	}
	return fmt.Errorf("%v\nRun 'agenthub help %s' for usage.", err, topic)
}

// runHelp implements "agenthub help [command [subcommand]]".
func runHelp(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(helpOutput, rootHelp)
		return nil
	}
	key := strings.Join(args, " ")
	if _, ok := helpTopics[key]; ok {
		printTopic(key)
		return nil
	}
	return fmt.Errorf("unknown help topic %q\nRun 'agenthub help' to list commands.", key)
}

const rootHelp = `AgentHub - local agent launcher and session hub

Usage:
  agenthub <command> [arguments]
  agenthub help [command [subcommand]]

AgentHub runs as a local daemon ("agenthub serve") that manages coding agents
installed on this machine. The CLI, the Web UI, and other clients all talk to
the same daemon over its HTTP API; the daemon is the single writer of all
session state and configuration.

Commands:
  serve       Start the AgentHub daemon (HTTP API and Web UI)
  status      Show daemon and runtime status as JSON
  agents      List providers, agents and availability probes
  run         Create a session, send one message, print the reply
  chat        Interactive chat on a new or existing session
  session     Manage sessions (create, list, show, events, attach, ...)
  version     Print the AgentHub version
  help        Show this help or help for a command

Use "agenthub help <command>" or "agenthub <command> --help" for details.

Core concepts:
  Provider   Adapter for a local agent runtime or protocol (Codex
             app-server, Kimi/OpenCode ACP, Pi RPC). Providers are declared
             in the config file and probed for executable availability.
  Agent      A named, runnable configuration bound to one provider. Its
             name is required and unique (case-insensitively) and is the
             only way sessions refer to it; there is no separate agent id.
             It carries default options (model, mode, approval, sandbox,
             ...) that are passed to the provider when a session starts.
  Session    An AgentHub conversation. It is always created with an
             explicitly selected agent (no implicit routing or fallback),
             and binds that agent, a working directory and a
             provider-native session/thread (providerSessionId). Sessions
             survive daemon restarts; the provider is re-attached on
             demand when the conversation continues.
  Turn       One inbound message processed by the agent. Turn lifecycle and
             assistant output are recorded as events.
  Provenance Message source metadata on inbound events. Roles are user,
             system, or agent; sender identity is descriptive only and is not
             authentication, permission, trust, or instruction priority.
  Approval   A provider request for permission (for example to run a
             command). Resolve it with "agenthub session approve" or the
             Web UI; the turn then continues. Approvals require the
             provider process to be running and do not survive restarts.
  Events     Every change to a session is appended to events.jsonl, the
             single source of truth; session.json is a rebuildable
             projection. The CLI polls events; the API also streams them
             over SSE.

Files:
  Config     ~/.agenthub/config.json (providers, agents)
  Sessions   ~/.agenthub/sessions/<id>/session.json + events.jsonl
  Archive    ~/.agenthub/sessions/Archive/<id>/ (archived sessions)
  Logs       ~/.agenthub/logs/ (service stdout/stderr when installed)
  State      ~/.agenthub/server.json (daemon endpoint discovery)

All persistent data lives under ~/.agenthub. This version reads only the
unified layout; migrate or back up data from older releases before starting
the daemon.

Environment:
  AGENTHUB_HOME         Isolate config, data and state into one directory
  AGENTHUB_ENDPOINT     Daemon endpoint used by the CLI (skips server.json)
  AGENTHUB_CODEX_CLI    Override the Codex executable
  AGENTHUB_OPENCODE_CLI Override the OpenCode executable
  AGENTHUB_KIMI_CLI     Override the Kimi executable
  AGENTHUB_PI_CLI       Override the Pi executable

Examples:
  agenthub serve                              Start the daemon (loopback only)
  agenthub run --agent "Kimi K3" "fix the tests"  One-shot run with an agent
  agenthub chat --agent "Kimi K3"                 Interactive chat with an agent
  agenthub session list                       List sessions
`

var helpTopics = map[string]string{
	"serve": `Start the AgentHub daemon.

Usage:
  agenthub serve [--addr host:port] [--web-dir path] [--allow-origin origin]...

The daemon serves the embedded Web UI at /agenthub/, the HTTP JSON/SSE API at
/agenthub/v1/, and the API reference at /agenthub/api.md. It is
the single writer of sessions and the config file; every other agenthub
command discovers it through server.json (or AGENTHUB_ENDPOINT) and talks to
this API. Only one daemon may run at a time (state/server.lock).

Options:
  --addr host:port   Listen address (default 127.0.0.1:4646, loopback only).
                     The host must be loopback, a local interface address, a
                     hostname that resolves to one, or a wildcard. IPv6 needs
                     brackets, e.g. [::1]:4646. Invalid addresses fail at
                     startup; there is no silent fallback.
  --web-dir path     Built Web UI directory (overrides the embedded UI).
  --allow-origin o   Trusted browser origin (scheme://host[:port]) for
                     mutating requests, in addition to the daemon's own
                     origin. Repeatable. Use when a reverse proxy exposes
                     the daemon under a public origin, e.g.
                     --allow-origin https://agenthub.example.com:8443.

AgentHub has no authentication: a non-loopback address prints a startup
warning and must only be used on trusted networks.

Examples:
  agenthub serve
  agenthub serve --addr 0.0.0.0:4646
  agenthub serve --addr '[::1]:4646'

The root URL redirects browsers to /agenthub/. The discovery endpoint written
to server.json is http://host:port/agenthub, so CLI and HTTP clients use the
same base path whether AgentHub runs standalone or inside PUA.

See also: agenthub help status
`,

	"status": `Show daemon and runtime status as JSON.

Usage:
  agenthub status

Requires a running daemon (see "agenthub help serve"). The CLI discovers the
endpoint through server.json or AGENTHUB_ENDPOINT. The report includes the
daemon version and uptime, the config, session store, archive and logs
paths, the runtime summary, the stable apiVersion and the capabilities this
daemon instance actually supports. API clients should check apiVersion and
all required capabilities before creating a session; a missing field means
the daemon is too old for capability negotiation.

See also: agenthub help agents, agenthub help session list
`,

	"agents": `List the effective agent configuration as JSON.

Usage:
  agenthub agents

Prints providers, agents and provider availability probes. Providers are
adapters for local agent runtimes; agents are named runnable configurations
bound to a provider, referenced everywhere by their unique name. The Web UI
settings panel shows each built-in provider's detected executable and edits
the executable paths and agents; anything more advanced is done in the
config file (see "agenthub help"). Agents whose provider executable cannot
be resolved are reported as unavailable and cannot create sessions.

See also: agenthub help run, agenthub help session create
`,

	"run": `Create a session, send one message and print the assistant reply.

Usage:
  agenthub run [--cwd dir] [--title title] --agent name <message>

Options:
  --cwd dir      Working directory of the session (default ".")
  --title title  Session title
  --agent name   Agent name from the configuration (required; sessions
                 always run with an explicit agent). Names match
                 case-insensitively; unknown names fail with a clear error.

The reply streams to stdout; the session id and agent are printed to
stderr. If the agent requests an approval, resolve it from the Web UI or
with "agenthub session approve".

Examples:
  agenthub run --agent "Kimi K3" "summarize this repository"
  agenthub run --agent "Kimi K3" --cwd ~/src/app "run the tests"

See also: agenthub help chat, agenthub help session create
`,

	"chat": `Chat interactively on a new or existing session.

Usage:
  agenthub chat [--session id | --cwd dir --title title --agent name]

Without --session a new session is created and --agent is required (same
selection rules as "agenthub run"). Type a message to start a turn; the
reply streams to stdout.

Options:
  --session id   Attach an existing session
  --cwd dir      Working directory for a new session (default ".")
  --title title  Session title for a new session
  --agent name   Agent name from the configuration (required for a new
                 session; sessions always run with an explicit agent)

Commands inside chat:
  /interrupt     Cancel the running turn
  /stop          Stop the provider process (session becomes stopped)
  /quit, /exit   Leave chat (the session is kept on the daemon)

Examples:
  agenthub chat --agent "Kimi K3" --cwd .
  agenthub chat --session ses_01HX...

See also: agenthub help run, agenthub help session attach
`,

	"version": `Print the AgentHub version.

Usage:
  agenthub version
`,

	"session": `Manage AgentHub sessions.

Usage:
  agenthub session <command> [arguments]

Commands:
  create      Create a session without sending a message
  list        List sessions
  show        Show one session as JSON
  events      Print the event log of a session as JSON
  attach      Attach an interactive chat to a session
  resume      Start the provider and re-attach its native session/thread
  interrupt   Cancel the running turn
  stop        Stop the provider process
  approve     Resolve a pending approval
  archive     Archive a session

Use "agenthub session <command> --help" or "agenthub help session <command>"
for details.

A session binds an agent, a working directory and a provider-native
session/thread. Its state lives in events.jsonl and session.json under the
data directory and survives daemon restarts.
`,

	"session create": `Create a session without sending a message.

Usage:
  agenthub session create [--cwd dir] [--title title] --agent name

Options:
  --cwd dir      Working directory of the session (default ".")
  --title title  Session title
  --agent name   Agent name from the configuration (required; sessions
                 always run with an explicit agent)

Prints the created session as JSON. Send messages with
"agenthub chat --session <id>" or "agenthub session attach <id>".

Examples:
  agenthub session create --agent "Kimi K3" --title "bug hunt"
  agenthub session create --agent "Kimi K3" --cwd ~/src/app

See also: agenthub help run, agenthub help session attach
`,

	"session list": `List sessions.

Usage:
  agenthub session list [--all] [--archived] [--json]

By default only active sessions are listed; archived sessions stay hidden.
Use --all to include them in the list, or --archived to list only archived
sessions. Archived sessions keep their full event log under
~/.agenthub/sessions/Archive/<session-id>/ and can be inspected with
"agenthub session show" and "agenthub session events".

Options:
  --all       Include archived sessions
  --archived  List only archived sessions
  --json      Print JSON instead of a table

Examples:
  agenthub session list
  agenthub session list --all --json
  agenthub session list --archived

See also: agenthub help session show, agenthub help session archive
`,

	"session show": `Show one session as JSON.

Usage:
  agenthub session show <session-id>

Prints the full session record: state, agent name, provider session/thread id,
current turn, pending approvals and the event cursor.

See also: agenthub help session events
`,

	"session events": `Print provider-neutral semantic event frames as JSON.

Usage:
  agenthub session events <session-id>

Frames cover state changes, turn lifecycle, canonical message.input events,
assistant messages, approvals, provider errors and normalized tool calls.
Existing raw provider logs are normalized on read and are never printed by
this command. The daemon streams the same "agenthub.semantic-events.v1"
protocol over SSE at GET /v1/sessions/{id}/events.

See also: agenthub help session show
`,

	"session attach": `Attach an interactive chat to an existing session.

Usage:
  agenthub session attach <session-id>

Equivalent to "agenthub chat --session <session-id>". Supports the same
in-chat commands: /interrupt, /stop, /quit.

See also: agenthub help chat
`,

	"session resume": `Start the provider for a session and re-attach its native session/thread.

Usage:
  agenthub session resume <session-id>

Sending a message also starts the provider on demand, so resume is mainly
useful to warm up a session or recover one that was stopped. The provider
thread is recovered from the persisted providerSessionId, which is how
conversations survive daemon restarts.

See also: agenthub help session stop, agenthub help chat
`,

	"session interrupt": `Cancel the turn currently running on a session.

Usage:
  agenthub session interrupt <session-id>

The provider process keeps running and the session stays usable; the turn
ends with a turn.cancelled event. Requires the provider to be running.

See also: agenthub help session stop
`,

	"session stop": `Stop the provider process of a session.

Usage:
  agenthub session stop <session-id>

The session first becomes stopping. This command returns only after the
provider process group has exited; the durable stopped event then carries
reason "requested". The full event log is kept. Continue later with
"agenthub session resume" or by sending a new message, which re-attaches the
provider-native session/thread on demand.

See also: agenthub help session resume
`,

	"session approve": `Resolve a pending approval request.

Usage:
  agenthub session approve [--decision decision] <session-id> <approval-id>

Options:
  --decision decision   accept (default), acceptForSession, decline or cancel

When a provider asks for permission (for example to run a command), the
session enters waiting_approval and an approval.requested event carries the
approval id. Resolving it lets the turn continue. The provider process must
be running. After a daemon crash, recovery cancels pending approvals and the
open turn before publishing stopped with reason "daemon_recovery".

Examples:
  agenthub session approve ses_01HX... apr_01HX...
  agenthub session approve --decision decline ses_01HX... apr_01HX...

See also: agenthub help session events
`,

	"session archive": `Archive a session.

Usage:
  agenthub session archive <session-id>

Archiving moves the whole session directory from the active store to
~/.agenthub/sessions/Archive/<session-id>/. Nothing is deleted:
session.json, events.jsonl and every other persisted file move along and
stay readable with "agenthub session show" and "agenthub session events".

Only inactive sessions can be archived: the provider must be stopped (see
"agenthub session stop") and no turn or approval may be open; otherwise the
command fails with a conflict and the session is left untouched. Archiving
is idempotent, so repeating it is safe. An archived session is hidden from
"agenthub session list" unless --all or --archived is given, and no longer
accepts messages, steer, resume, interrupt or approvals. Unarchiving is not
supported.

See also: agenthub help session list, agenthub help session stop
`,
}
