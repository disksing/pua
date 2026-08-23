package app

import (
	"errors"
	"testing"
	"time"
)

func TestValidateScheduleTriggerForMutationRejectsAtEqualNow(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 123, time.UTC)
	trigger := ScheduleTrigger{Type: ScheduleTriggerAt, At: now.Format(time.RFC3339Nano)}
	if err := validateScheduleTriggerForMutation(trigger, now); !errors.Is(err, ErrScheduleTriggerAtNotFuture) {
		t.Fatalf("at-equals-now error = %v", err)
	}

	interval := ScheduleTrigger{Type: ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: now.Add(-time.Hour).Format(time.RFC3339Nano)}
	if err := validateScheduleTriggerForMutation(interval, now); err != nil {
		t.Fatalf("past interval anchor error = %v", err)
	}
}

func TestValidateScheduleTriggerForUpdateStillValidatesIdenticalTrigger(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	invalid := ScheduleTrigger{Type: ScheduleTriggerAt, At: now.Add(-time.Hour).Format(time.RFC3339Nano), EverySeconds: 60}
	if err := validateScheduleTriggerForUpdate(invalid, &invalid, now); err == nil || err.Error() != "at trigger must contain only at" {
		t.Fatalf("identical invalid trigger error = %v", err)
	}
}
