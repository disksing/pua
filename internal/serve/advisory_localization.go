package serve

import (
	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/localize"
)

type advisoryRenderer func(language string, compact bool) (string, error)

func renderWorkspaceAdvisory(workspace serveWorkspace, render advisoryRenderer) (string, error) {
	root, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		return "", err
	}
	language, err := root.Language()
	if err != nil {
		return "", err
	}
	compact, guidanceErr := root.HasCurrentTurnGuidance()
	return render(language, compact && guidanceErr == nil)
}

func localizedQueuePreview(language string, compact bool, message resourceMailboxMessage, snapshot *mailboxQueueSnapshot) (string, error) {
	role := message.Role
	if message.Type == "user_chat_input" {
		role = "user"
	}
	queue, err := localizedQueueSnapshot(language, snapshot)
	if err != nil {
		return "", err
	}
	hint, err := localize.Render(language, "advisory-hint.md", map[string]any{"Compact": compact})
	if err != nil {
		return "", err
	}
	return localize.Render(language, "queue-preview.md", map[string]any{
		"MessageID": message.ID, "Party": providerMessageParty(role, message.Sender, language),
		"Body": message.Text, "Queue": queue, "Hint": hint, "Compact": compact,
	})
}

func localizedQueueSnapshot(language string, snapshot *mailboxQueueSnapshot) (string, error) {
	if snapshot == nil || snapshot.WaitingTotal == 0 && snapshot.SameSourceWaiting == 0 &&
		snapshot.BatchEligibleFromSource == 0 && snapshot.Delivering == 0 &&
		snapshot.Interrupting == 0 && snapshot.DeliveryUnknown == 0 {
		return "", nil
	}
	return localize.Render(language, "queue-notice.md", snapshot)
}

func localizedChatState(language string, compact bool, snapshot turnChatSnapshot, work turnWork) (string, error) {
	hint, err := localize.Render(language, "advisory-hint.md", map[string]any{"Compact": compact})
	if err != nil {
		return "", err
	}
	return localize.Render(language, "chat-state.md", map[string]any{
		"Chat": snapshot, "OriginWorkID": work.ID, "Visible": work.Context.ChatVisible,
		"Epoch": work.Context.ChatEpoch, "Compact": compact, "Hint": hint,
	})
}
