package app_test

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func exactInstant(t *testing.T, value string) time.Time {
	t.Helper()
	result, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestExactScheduleOccurrence(t *testing.T) {
	for _, test := range []struct {
		name, zone, start, last, now, expires, want string
	}{
		{"next day after creation", "Asia/Shanghai", "2026-11-11T10:00:00+08:00", "", "2026-11-11T12:00:00+08:00", "", "2026-11-12T09:00:00+08:00"},
		{"latest missed", "Asia/Shanghai", "2026-11-01T08:00:00+08:00", "2026-11-01T09:00:00+08:00", "2026-11-11T12:00:00+08:00", "", "2026-11-11T09:00:00+08:00"},
		{"before today", "Asia/Shanghai", "2026-11-01T08:00:00+08:00", "", "2026-11-11T08:00:00+08:00", "", "2026-11-10T09:00:00+08:00"},
		{"same occurrence consumed", "Asia/Shanghai", "2026-11-01T08:00:00+08:00", "2026-11-11T09:00:00+08:00", "2026-11-11T12:00:00+08:00", "", "2026-11-12T09:00:00+08:00"},
		{"catch up before expiry", "Asia/Shanghai", "2026-11-01T08:00:00+08:00", "", "2026-11-11T12:00:00+08:00", "2026-11-10T09:00:00+08:00", "2026-11-10T09:00:00+08:00"},
		{"expired and consumed", "Asia/Shanghai", "2026-11-01T08:00:00+08:00", "2026-11-10T09:00:00+08:00", "2026-11-11T12:00:00+08:00", "2026-11-10T09:00:00+08:00", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			schedule := app.Schedule{ExactTime: &app.ScheduleExactTime{
				ScheduleExactTimeSpec: app.ScheduleExactTimeSpec{TimeOfDay: "09:00", TimeZone: test.zone, ExpiresAt: test.expires},
				StartedAt:             test.start, LastFiredAt: test.last,
			}}
			got, exists := schedule.NextExactTimeOccurrence(exactInstant(t, test.now))
			if test.want == "" {
				if exists {
					t.Fatalf("unexpected occurrence %s", got)
				}
			} else if !exists || !got.Equal(exactInstant(t, test.want)) {
				t.Fatalf("occurrence %s, exists %v, want %s", got, exists, test.want)
			}
		})
	}
}

func TestExactScheduleDST(t *testing.T) {
	for _, test := range []struct{ daily, start, last, now, want string }{
		{"02:30", "2026-03-07T00:00:00Z", "2026-03-07T07:30:00Z", "2026-03-08T08:00:00Z", "2026-03-09T06:30:00Z"},
		{"01:30", "2026-10-31T00:00:00Z", "", "2026-11-01T07:00:00Z", "2026-11-01T05:30:00Z"},
		{"01:30", "2026-10-31T00:00:00Z", "2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z", "2026-11-02T06:30:00Z"},
	} {
		schedule := app.Schedule{ExactTime: &app.ScheduleExactTime{
			ScheduleExactTimeSpec: app.ScheduleExactTimeSpec{TimeOfDay: test.daily, TimeZone: "America/New_York"},
			StartedAt:             test.start, LastFiredAt: test.last,
		}}
		got, exists := schedule.NextExactTimeOccurrence(exactInstant(t, test.now))
		if !exists || !got.Equal(exactInstant(t, test.want)) {
			t.Fatalf("DST occurrence %s, exists %v, want %s", got, exists, test.want)
		}
	}
}

func TestExactScheduleValidation(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []app.ScheduleExactTimeSpec{
		{}, {At: "bad"}, {TimeOfDay: "25:00"}, {TimeOfDay: "9:00"}, {TimeOfDay: "09:00", TimeZone: "No/Zone"},
		{At: "2026-11-12T09:00", TimeOfDay: "09:00"}, {TimeZone: "UTC"},
		{At: "2026-03-08T02:30", TimeZone: "America/New_York"},
		{At: "2026-11-12T09:00Z", ExpiresAt: "2026-11-11T09:00Z"},
		{TimeOfDay: "09:00", ExpiresAt: "bad"},
	} {
		if _, err := workspace.AddSchedule(app.CreateScheduleInput{Description: "Bad", Target: "workspace", ExactTime: &spec}); err == nil {
			t.Fatalf("invalid spec accepted: %#v", spec)
		}
	}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{Description: "Once", Target: "workspace",
		ExactTime: &app.ScheduleExactTimeSpec{At: "2026-11-12T09:00", TimeZone: "Asia/Shanghai"}})
	if err != nil {
		t.Fatal(err)
	}
	if created.Condition != "" || created.ExactTime.At != "2026-11-12T01:00:00Z" {
		t.Fatalf("normalized schedule %#v", created)
	}
	if _, err := workspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, ExactTime: &app.ScheduleExactTimeSpec{}}); err == nil {
		t.Fatal("removing exactTime without condition accepted")
	}
	condition := "when ready"
	if _, err := workspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, Condition: &condition, ExactTime: &app.ScheduleExactTimeSpec{}}); err != nil {
		t.Fatal(err)
	}
}

func TestExactScheduleDurablePendingAndEdits(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{Description: "Daily", Target: "workspace",
		ExactTime: &app.ScheduleExactTimeSpec{TimeOfDay: "09:00", TimeZone: "UTC"}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().AddDate(0, 0, 3)
	var pending app.ScheduleExactDelivery
	failure := errors.New("simulated crash after target acceptance")
	err = workspace.DispatchExactTimeSchedules(now, func(schedule app.Schedule) error {
		pending = *schedule.ExactTime.Pending
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("dispatch error %v", err)
	}
	config, err := workspace.Scheduler()
	if err != nil || !reflect.DeepEqual(config.Schedules[0].ExactTime.Pending, &pending) {
		t.Fatalf("pending not durable: %#v, %v", config, err)
	}
	description := "Updated description"
	updated, err := workspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, Description: &description,
		ExactTime: &created.ExactTime.ScheduleExactTimeSpec})
	if err != nil || updated.ExactTime.Pending == nil || updated.ExactTime.Pending.Description != "Daily" {
		t.Fatalf("unchanged timing lost frozen delivery: %#v, %v", updated, err)
	}
	reopened, _ := app.OpenWorkspace(workspace.Root())
	received := 0
	if err := reopened.DispatchExactTimeSchedules(now.AddDate(0, 0, 1), func(schedule app.Schedule) error {
		received++
		if !reflect.DeepEqual(*schedule.ExactTime.Pending, pending) {
			t.Fatalf("pending changed across restart: %#v", schedule.ExactTime.Pending)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if received != 1 {
		t.Fatalf("pending delivered %d times", received)
	}
	config, _ = reopened.Scheduler()
	cursor := config.Schedules[0].ExactTime.LastFiredAt
	updated, err = reopened.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, ExactTime: &created.ExactTime.ScheduleExactTimeSpec})
	if err != nil || updated.ExactTime.LastFiredAt != cursor {
		t.Fatalf("same timing reset cursor: %#v %v", updated, err)
	}
}

func TestExactScheduleConcurrentOneTimeDispatch(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := workspace.AddSchedule(app.CreateScheduleInput{Description: "Once", Target: "workspace",
		ExactTime: &app.ScheduleExactTimeSpec{At: now.Add(-time.Minute).Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	accepted := 0
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			if err := workspace.DispatchExactTimeSchedules(now, func(schedule app.Schedule) error {
				accepted++
				return nil
			}); err != nil {
				t.Error(err)
			}
		})
	}
	wait.Wait()
	config, err := workspace.Scheduler()
	if err != nil || accepted != 1 || len(config.Schedules) != 0 {
		t.Fatalf("one-time accepted %d; config %#v; error %v", accepted, config, err)
	}
}
