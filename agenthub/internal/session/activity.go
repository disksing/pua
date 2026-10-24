package session

// IsActivityEvent reports Turn activity, including incoming messages, for the
// session projection and the API activity stream. Watchdogs use IsProgressEvent
// so receiving more work cannot hide an agent that is not making progress.
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

// IsProgressEvent records the initial Turn boundary and subsequent work, but
// excludes input: accepting or queuing a message does not prove its consumption.
func IsProgressEvent(event Event) bool {
	return event.Type != EventMessageInput && IsActivityEvent(event)
}
