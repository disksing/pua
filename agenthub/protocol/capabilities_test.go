package protocol

import (
	"slices"
	"testing"
)

func TestBaselineV1IsFixed(t *testing.T) {
	expected := []string{
		"session.source", "session.source-metadata", "session.idempotent-create", "session.input-capabilities",
		"messages.idempotent", "messages.at-least-once", "messages.opaque-payload-v2",
		"turns.stable-index", "turns.materialized", "session.launch-environment",
		"session.launch-environment-update", "session.strict-stopped", "events.lossless-replay",
		"events.canonical-turn-terminals", "events.semantic-v1", "event.raw-v1", "recovery.closed-turns",
	}
	if !slices.Equal(BaselineV1[:], expected) {
		t.Fatalf("API v1 baseline changed; add feature-specific negotiation or bump the API major: %v", BaselineV1)
	}
}
