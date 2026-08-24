// Package protocol defines the stable AgentHub contract shared by the daemon
// and clients. Capability names live here so producers and consumers never
// duplicate wire strings.
package protocol

const APIMajor = "1"

const (
	CapabilitySessionSource                  = "session.source"
	CapabilitySessionSourceMetadata          = "session.source-metadata"
	CapabilitySessionIdempotentCreate        = "session.idempotent-create"
	CapabilitySessionInputCapabilities       = "session.input-capabilities"
	CapabilityMessageIdempotency             = "messages.idempotent"
	CapabilityMessageAtLeastOnce             = "messages.at-least-once"
	CapabilityMessageOpaquePayloadV2         = "messages.opaque-payload-v2"
	CapabilityTurnsStableIndex               = "turns.stable-index"
	CapabilityTurnsMaterialized              = "turns.materialized"
	CapabilityTurnsActivityItems             = "turns.activity-items"
	CapabilitySessionLaunchEnvironment       = "session.launch-environment"
	CapabilitySessionLaunchEnvironmentUpdate = "session.launch-environment-update"
	CapabilitySessionStrictStopped           = "session.strict-stopped"
	CapabilityEventsLosslessReplay           = "events.lossless-replay"
	CapabilityEventsDeltaMerge               = "events.delta-merge"
	CapabilityEventsBackwardPagination       = "events.backward-pagination"
	CapabilityEventsCanonicalTerminal        = "events.canonical-turn-terminals"
	CapabilityEventsSemanticV1               = "events.semantic-v1"
	CapabilityEventRawV1                     = "event.raw-v1"
	CapabilityActivityGlobalSSE              = "activity.global-sse"
	CapabilityRecoveryClosedTurns            = "recovery.closed-turns"
)

// BaselineV1 is the fixed safety baseline required by PUA for AgentHub API v1.
// It must not grow. New independently negotiable behavior belongs in a
// feature-specific requirement at its call site; breaking baseline changes
// require a new API major.
var BaselineV1 = [...]string{
	CapabilitySessionSource,
	CapabilitySessionSourceMetadata,
	CapabilitySessionIdempotentCreate,
	CapabilitySessionInputCapabilities,
	CapabilityMessageIdempotency,
	CapabilityMessageAtLeastOnce,
	CapabilityMessageOpaquePayloadV2,
	CapabilityTurnsStableIndex,
	CapabilityTurnsMaterialized,
	CapabilitySessionLaunchEnvironment,
	CapabilitySessionLaunchEnvironmentUpdate,
	CapabilitySessionStrictStopped,
	CapabilityEventsLosslessReplay,
	CapabilityEventsCanonicalTerminal,
	CapabilityEventsSemanticV1,
	CapabilityEventRawV1,
	CapabilityRecoveryClosedTurns,
}
