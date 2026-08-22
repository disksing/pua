package session

// IsActivityEvent reports whether an event represents effective agent work
// within a Turn. It is shared by the session projection and the API activity
// stream so a watchdog and a UI monitor use the same semantic boundary.
func IsActivityEvent(event Event) bool {
	if event.TurnID == "" {
		return false
	}
	switch event.Type {
	case "turn.started",
		EventMessageInput,
		"message.assistant.delta",
		"message.reasoning.delta",
		"tool.event", "tool.call",
		"approval.requested",
		"approval.resolved",
		"provider.error",
		EventTurnCompleted,
		EventTurnFailed,
		EventTurnCancelled:
		return true
	default:
		return false
	}
}
