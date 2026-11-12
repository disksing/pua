package pua

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func TestSchedulerExactTimeCLI(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		output := run(t, "scheduler", "add", "--description=Daily", "--target=workspace", "--daily=09:00", "--timezone=Asia/Shanghai")
		var created app.Schedule
		if err := json.Unmarshal([]byte(output), &created); err != nil || created.ExactTime == nil || created.Condition != "" {
			t.Fatalf("created %s error %v", output, err)
		}
		if listed := run(t, "scheduler", "list"); !strings.Contains(listed, "daily 09:00 (Asia/Shanghai)") {
			t.Fatal(listed)
		}
		output = run(t, "scheduler", "update", "--id="+created.ID, "--timezone=UTC", "--condition=")
		var updated app.Schedule
		if err := json.Unmarshal([]byte(output), &updated); err != nil || updated.ExactTime.TimeOfDay != "09:00" || updated.ExactTime.TimeZone != "UTC" {
			t.Fatalf("partial exact update %s error %v", output, err)
		}
		run(t, "scheduler", "update", "--id="+created.ID, "--expires=2027-01-01T00:00Z")
		output = run(t, "scheduler", "update", "--id="+created.ID, "--expires=")
		if strings.Contains(output, `"expiresAt"`) {
			t.Fatal(output)
		}
		for _, options := range [][]string{
			{"update", "--id=" + created.ID, "--no-exact-time"},
			{"update", "--id=" + created.ID, "--no-exact-time", "--daily=10:00"},
			{"update", "--id=" + created.ID, "--no-exact-time", "--no-exact-time"},
			{"add", "--description=Bad", "--target=workspace", "--at=2026-11-11T00:00Z", "--daily=09:00"},
			{"add", "--description=Bad", "--target=workspace", "--daily=25:00"},
			{"add", "--description=Bad", "--target=workspace", "--condition="},
		} {
			if _, err := runErr(t, append([]string{"scheduler"}, options...)...); err == nil {
				t.Fatalf("invalid arguments accepted %v", options)
			}
		}
		output = run(t, "scheduler", "update", "--id="+created.ID, "--no-exact-time", "--condition=ready")
		if strings.Contains(output, `"exactTime"`) {
			t.Fatal(output)
		}
	})
}
