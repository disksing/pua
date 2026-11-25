package serve

import (
	"context"
	"strings"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/localize"
)

type advisoryRenderer func(language string, compact bool) (string, error)

func renderWorkspaceAdvisory(ctx context.Context, workspace serveWorkspace, render advisoryRenderer) (string, error) {
	root, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		return "", err
	}
	language, err := root.Language()
	if err != nil {
		return "", err
	}
	compact, guidanceErr := root.HasCurrentTurnGuidanceContext(ctx)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return render(language, compact && guidanceErr == nil)
}

func queuePreviewRenderer(file *turnWorkFile, work *turnWork, session agentHubSession, message resourceMailboxMessage, mailbox resourceMailbox) advisoryRenderer {
	readIDs := []string{}
	if batchIneligibleReason(message, *file) == "" {
		readIDs = append(readIDs, message.ID)
		for _, other := range mailbox.Messages {
			if len(readIDs) >= 8 {
				break
			}
			if other.ID == message.ID || other.ResourceID != message.ResourceID || batchIneligibleReason(other, *file) != "" ||
				other.Sender.ID != message.Sender.ID || other.SenderWorkspaceInstanceID != message.SenderWorkspaceInstanceID ||
				!file.canPreview(work, other.ID) || work.PreviewAttempts[other.ID].MessageID != "" {
				continue
			}
			readIDs = append(readIDs, other.ID)
		}
	}
	role := message.Role
	if message.Type == "user_chat_input" {
		role = "user"
	}
	return func(language string, _ bool) (string, error) {
		if len(readIDs) > 1 {
			if work.GroupedPreviews == nil {
				work.GroupedPreviews = map[string]string{}
			}
			for _, id := range readIDs[1:] {
				work.GroupedPreviews[id] = message.ID
			}
		}
		canAct := work.State == "running" && work.Command == nil && len(session.PendingApprovalIDs) == 0
		body := message.Text
		if role != "user" {
			if excerpt := []rune(body); len(excerpt) > 120 {
				body = string(excerpt[:120]) + "…"
			}
		}
		return localize.Render(language, "queue-preview.md", map[string]any{
			"Party": providerMessageParty(role, message.Sender, language), "Count": max(1, len(readIDs)),
			"Read": canAct && !work.Context.ChatVisible && len(readIDs) > 0, "IDs": strings.Join(readIDs, ","),
			"Yield":  canAct && role == "user" && work.ParentID == "" && yieldInputSupported(message),
			"WorkID": work.ID, "MessageID": message.ID, "Standalone": !session.InputCapabilities.TurnScopedSteer, "Body": body,
		})
	}
}

func localizedChatState(language string, compact bool, snapshot turnChatSnapshot, work turnWork) (string, error) {
	hint, err := localize.Render(language, "advisory-hint.md", map[string]any{"Compact": compact})
	if err != nil {
		return "", err
	}
	return localize.Render(language, "chat-state.md", map[string]any{
		"Chat": snapshot, "Visible": work.Context.ChatVisible, "Hint": hint,
	})
}
