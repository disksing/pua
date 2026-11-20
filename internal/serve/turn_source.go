package serve

type turnResumeSource struct {
	Role             string                 `json:"role,omitempty"`
	Type             string                 `json:"type,omitempty"`
	Sender           *agentHubMessageSender `json:"sender,omitempty"`
	SourceResourceID string                 `json:"sourceResourceId,omitempty"`
}

func turnSourceResourceID(cause *resourceMessageCausation) string {
	if cause == nil {
		return ""
	}
	return cause.SourceResourceID
}

func turnSourceMessage(mailbox resourceMailbox, id string) resourceMailboxMessage {
	for _, message := range mailbox.Messages {
		if id != "" && message.ID == id {
			return message
		}
	}
	return resourceMailboxMessage{ID: id}
}

func resumedTurnSource(resourceID string, message resourceMailboxMessage, mailbox resourceMailbox, works turnWorkFile) *turnResumeSource {
	source := &turnResumeSource{}
	seen := make(map[string]bool)
	for !seen[message.ID] {
		seen[message.ID] = true
		work := works.find(message.ID)
		context := message.ProviderContext
		if context == nil && work != nil {
			context = &work.Context
		}
		if context != nil && !context.OpenerUnknown {
			source.Role = firstNonEmpty(source.Role, puaMessageRole(context.OpenerRole, context.OpenerType))
			if context.OpenerType != "turn_resume" {
				source.Type = firstNonEmpty(source.Type, context.OpenerType)
			}
			if source.Sender == nil {
				source.Sender = normalizedMessageSender(context.OpenerSender)
			}
		}
		if message.ID != "" && (message.Type == "turn_resume" || (message.Type == "" && work != nil && source.Type == "")) {
			parentID := resumeParentID(resourceID, message, works)
			if parentID != "" {
				message = turnSourceMessage(mailbox, parentID)
				continue
			}
		}
		if message.Type != "turn_resume" {
			source.Role = firstNonEmpty(source.Role, puaMessageRole(message.Role, message.Type))
			source.Type = firstNonEmpty(source.Type, message.Type)
			if source.Sender == nil {
				source.Sender = normalizedMessageSender(message.Sender)
			}
			source.SourceResourceID = turnSourceResourceID(message.Causation)
		}
		break
	}
	return source
}

func resumeParentID(resourceID string, message resourceMailboxMessage, works turnWorkFile) string {
	if cause := message.Causation; cause != nil {
		if cause.Type != "turn_resume" || cause.SourceResourceID != resourceID || cause.MessageID == "" {
			return ""
		}
		parent := works.find(cause.MessageID)
		if parent != nil && cause.TurnReference != "" && parent.History != "" && cause.TurnReference != parent.History {
			return ""
		}
		return cause.MessageID
	}
	for _, parent := range works.Works {
		if parent.ResumeID == message.ID {
			return parent.ID
		}
	}
	return ""
}
