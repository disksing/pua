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
