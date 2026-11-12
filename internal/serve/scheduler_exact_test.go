package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func TestExactScheduleServerCatchUpAndRestart(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	store, _ := app.OpenWorkspace(workspace.Path)
	created, err := store.AddSchedule(app.CreateScheduleInput{Description: "Daily reminder", Target: "workspace",
		ExactTime: &app.ScheduleExactTimeSpec{TimeOfDay: "09:00", TimeZone: "Asia/Shanghai"}})
	if err != nil {
		t.Fatal(err)
	}
	start, _ := time.Parse(time.RFC3339Nano, created.ExactTime.StartedAt)
	location, _ := time.LoadLocation("Asia/Shanghai")
	date := start.In(location).AddDate(0, 0, 3)
	now := time.Date(date.Year(), date.Month(), date.Day(), 10, 0, 0, 0, location)
	manager.now = func() time.Time { return now }
	if err := manager.reconcileScheduleExactDue(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	mailbox, err := loadResourceMailboxForResource(workspace.Path, "workspace")
	if err != nil || len(mailbox.Messages) != 1 {
		t.Fatalf("mailbox %#v %v", mailbox, err)
	}
	message := mailbox.Messages[0]
	expectedOccurrence := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	if message.Status != resourceMessageQueued || message.Type != resourceMessageTypeScheduleExactDue ||
		!strings.Contains(message.Text, expectedOccurrence) || !strings.Contains(message.Text, created.ID) {
		t.Fatalf("trigger %#v", message)
	}
	if manager.takeReconcileRequests()&reconcileMailboxes == 0 {
		t.Fatal("target mailbox reconciliation not requested")
	}
	restarted := newAgentManager(manager.server)
	restarted.now = manager.now
	if err := restarted.reconcileScheduleExactDue(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	mailbox, _ = loadResourceMailboxForResource(workspace.Path, "workspace")
	if len(mailbox.Messages) != 1 {
		t.Fatalf("restart duplicated trigger: %#v", mailbox.Messages)
	}
	config, _ := store.Scheduler()
	if config.Schedules[0].ExactTime.LastFiredAt != expectedOccurrence || config.Schedules[0].ExactTime.Pending != nil {
		t.Fatalf("cursor %#v", config.Schedules[0])
	}
	deadline := nextExactScheduleDeadline(config, now)
	if !deadline.Equal(now.AddDate(0, 0, 1).Add(-time.Hour)) {
		t.Fatalf("deadline %s", deadline)
	}
}

func TestExactScheduleCrashAfterMailboxAcceptance(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	store, _ := app.OpenWorkspace(workspace.Path)
	now := time.Now().UTC()
	created, err := store.AddSchedule(app.CreateScheduleInput{Description: "Once", Target: "workspace",
		ExactTime: &app.ScheduleExactTimeSpec{At: now.Add(-time.Minute).Format(time.RFC3339)}})
	if err != nil {
		t.Fatal(err)
	}
	instanceID, _ := workspaceInstanceID(workspace.Path)
	failure := errors.New("crash after acceptance before cursor checkpoint")
	err = store.DispatchExactTimeSchedules(now, func(schedule app.Schedule) error {
		if _, err := acceptGeneratedMailboxMessage(workspace.Path, exactScheduleMessage(instanceID, schedule)); err != nil {
			t.Fatal(err)
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return now.AddDate(0, 0, 1) }
	if err := manager.reconcileScheduleExactDue(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	mailbox, _ := loadResourceMailboxForResource(workspace.Path, "workspace")
	config, _ := store.Scheduler()
	if len(mailbox.Messages) != 1 || len(config.Schedules) != 0 || !strings.Contains(mailbox.Messages[0].Text, created.ID) {
		t.Fatalf("crash recovery mailbox %#v config %#v", mailbox, config)
	}
}

func TestExactSchedulesDoNotWakeSchedulerAgent(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	client, _ := newAgentHubClient(hub.URL, nil)
	store, _ := app.OpenWorkspace(workspace.Path)
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if _, err := store.AddSchedule(app.CreateScheduleInput{Description: "Once", Target: "workspace",
		ExactTime: &app.ScheduleExactTimeSpec{At: now.Add(-time.Minute).Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace, client); err != nil {
		t.Fatal(err)
	}
	if ticks := schedulerMessages(t, workspace.Path); len(ticks) != 0 {
		t.Fatalf("exact schedule created LLM tick %#v", ticks)
	}
	if _, err := store.AddSchedule(app.CreateScheduleInput{Description: "Review", Condition: "when ready", Target: "workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace, client); err != nil {
		t.Fatal(err)
	}
	before := schedulerMessages(t, workspace.Path)
	if len(before) != 1 {
		t.Fatalf("natural language tick %#v", before)
	}
	if _, err := store.AddSchedule(app.CreateScheduleInput{Description: "Another once", Target: "workspace",
		ExactTime: &app.ScheduleExactTimeSpec{At: now.Add(-time.Second).Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace, client); err != nil {
		t.Fatal(err)
	}
	mailbox, _ := loadResourceMailboxForResource(workspace.Path, "workspace")
	if len(mailbox.Messages) != 2 || len(schedulerMessages(t, workspace.Path)) != 1 {
		t.Fatalf("coexistence mailbox %#v", mailbox)
	}
}

func TestExactScheduleHTTPContract(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	server := manager.server
	request := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		server.handleWorkspace(recorder, httptest.NewRequest(method, "/api/workspaces/"+workspace.ID+"/scheduler"+path, strings.NewReader(body)))
		return recorder
	}
	response := request(http.MethodPost, "", `{"description":"Daily","target":"workspace","exactTime":{"timeOfDay":"09:00","timeZone":"Asia/Shanghai"}}`)
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	var created app.Schedule
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.ExactTime == nil {
		t.Fatal(err, response.Body.String())
	}
	for _, body := range []string{
		`{"description":"Bad","target":"workspace","exactTime":{"timeOfDay":"25:00"}}`,
		`{"description":"Bad","target":"workspace","exactTime":{"timeOfDay":"09:00","lastFiredAt":"2026-11-11T00:00:00Z"}}`,
	} {
		if result := request(http.MethodPost, "", body); result.Code != http.StatusBadRequest {
			t.Fatalf("invalid body accepted %s", result.Body.String())
		}
	}
	if result := request(http.MethodPut, "/"+created.ID, `{"condition":"ready","exactTime":{}}`); result.Code != http.StatusOK {
		t.Fatal(result.Body.String())
	}
	response = request(http.MethodGet, "", "")
	if strings.Contains(response.Body.String(), `"exactTime"`) {
		t.Fatal("exactTime not removed", response.Body.String())
	}
}
