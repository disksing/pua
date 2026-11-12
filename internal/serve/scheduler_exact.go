package serve

import (
	"context"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/localize"
)

func naturalLanguageSchedules(config app.SchedulerConfig) app.SchedulerConfig {
	config.Schedules = append([]app.Schedule{}, config.Schedules...)
	filtered := config.Schedules[:0]
	for _, schedule := range config.Schedules {
		if schedule.ExactTime == nil {
			filtered = append(filtered, schedule)
		}
	}
	config.Schedules = filtered
	return config
}

func (m *agentManager) reconcileScheduleExactDue(ctx context.Context, workspace serveWorkspace) error {
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		return err
	}
	instanceID, err := workspaceInstanceID(workspace.Path)
	if err != nil {
		return err
	}
	return puaWorkspace.DispatchExactTimeSchedules(m.now(), func(schedule app.Schedule) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		message, err := acceptGeneratedMailboxMessage(workspace.Path, exactScheduleMessage(instanceID, schedule))
		if err != nil {
			return err
		}
		if message.Status == resourceMessageQueued {
			m.requestReconcile(reconcileMailboxes)
		}
		return nil
	})
}

func exactScheduleMessage(instanceID string, schedule app.Schedule) resourceMailboxMessage {
	pending := schedule.ExactTime.Pending
	messageID := notificationMessageID(resourceMessageTypeScheduleExactDue, instanceID, schedule.ID, schedule.ExactTime.StartedAt, pending.Occurrence)
	text := localize.MustRender(pending.Language, "scheduler-exact-due.md", map[string]string{
		"MessageID": messageID, "ScheduleID": schedule.ID, "Description": pending.Description,
		"Occurrence": pending.Occurrence, "FiredAt": pending.FiredAt, "TimeZone": schedule.ExactTime.TimeZone,
	})
	return resourceMailboxMessage{
		ID: messageID, ResourceID: pending.Target, Text: text,
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
		Type: resourceMessageTypeScheduleExactDue,
		Causation: &resourceMessageCausation{
			Type: resourceMessageTypeScheduleExactDue, SourceWorkspaceInstanceID: instanceID,
			SourceResourceID: app.SchedulerResourceID, Reason: schedule.ID + "@" + pending.Occurrence,
		},
	}
}

func nextExactScheduleDeadline(config app.SchedulerConfig, now time.Time) time.Time {
	var earliest time.Time
	for _, schedule := range config.Schedules {
		occurrence, exists := schedule.NextExactTimeOccurrence(now)
		if !exists {
			continue
		}
		if !occurrence.After(now) {
			occurrence = now.Add(30 * time.Second)
		}
		if earliest.IsZero() || occurrence.Before(earliest) {
			earliest = occurrence
		}
	}
	return earliest
}
