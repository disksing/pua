package serve

import "strings"

func turnChatSender(root, resource string, work turnWork) string {
	if work.GenerationID == "" || work.SessionID == "" {
		return ""
	}
	matches := func(record generationRecord) bool {
		return record.GenerationID == work.GenerationID && record.AgentHubSessionID == work.SessionID
	}
	record, found, err := currentResourceGeneration(root, resource)
	if err == nil && found && matches(record) {
		return generationChatSender(record)
	}
	records, err := resourceHistoryGenerations(root, resource)
	if err == nil {
		for _, record := range records {
			if matches(record) {
				return generationChatSender(record)
			}
		}
	}
	return ""
}

func generationChatSender(record generationRecord) string {
	for _, name := range []string{record.AgentHubAgentName, record.ResolvedProfile} {
		if name = strings.TrimSpace(name); name != "" {
			return name
		}
	}
	if record.BindingKind == "profile" || record.BindingKind == "agent" {
		return strings.TrimSpace(record.BindingName)
	}
	return ""
}
