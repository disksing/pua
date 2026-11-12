package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

type ScheduleExactTimeSpec struct {
	At        string `json:"at,omitempty"`
	TimeOfDay string `json:"timeOfDay,omitempty"`
	TimeZone  string `json:"timeZone,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

type ScheduleExactTime struct {
	ScheduleExactTimeSpec
	StartedAt   string                 `json:"startedAt"`
	LastFiredAt string                 `json:"lastFiredAt,omitempty"`
	Pending     *ScheduleExactDelivery `json:"pending,omitempty"`
}

type ScheduleExactDelivery struct {
	Occurrence  string `json:"occurrence"`
	FiredAt     string `json:"firedAt"`
	Language    string `json:"language"`
	Description string `json:"description"`
	Target      string `json:"target"`
}

func defaultScheduleTimeZone() string {
	name := time.Local.String()
	if name == "Local" {
		if path, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
			if _, zone, found := strings.Cut(path, "zoneinfo/"); found {
				if _, err := time.LoadLocation(zone); err == nil {
					return zone
				}
			}
		}
		if zone := strings.TrimPrefix(os.Getenv("TZ"), ":"); zone != "" {
			if _, err := time.LoadLocation(zone); err == nil {
				return zone
			}
		}
	}
	return name
}

func scheduleWallTime(year int, month time.Month, day, hour, minute, second, nano int, location *time.Location) (time.Time, bool) {
	wall := time.Date(year, month, day, hour, minute, second, nano, time.UTC)
	var earliest time.Time
	for _, hours := range []int{-48, 0, 48} {
		_, offset := wall.Add(time.Duration(hours) * time.Hour).In(location).Zone()
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		local := candidate.In(location)
		if local.Year() == year && local.Month() == month && local.Day() == day &&
			local.Hour() == hour && local.Minute() == minute && local.Second() == second &&
			(earliest.IsZero() || candidate.Before(earliest)) {
			earliest = candidate
		}
	}
	return earliest.In(location), !earliest.IsZero()
}

func parseScheduleTime(value string, location *time.Location) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04Z07:00"} {
		if instant, err := time.Parse(layout, value); err == nil {
			return instant, nil
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04"} {
		if wall, err := time.Parse(layout, value); err == nil {
			if instant, exists := scheduleWallTime(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), wall.Nanosecond(), location); exists {
				return instant, nil
			}
			return time.Time{}, errors.New("local time does not exist in this time zone")
		}
	}
	return time.Time{}, errors.New("time must be RFC3339 or YYYY-MM-DDTHH:MM[:SS]")
}

func normalizeScheduleExactTime(spec *ScheduleExactTimeSpec) (*ScheduleExactTime, error) {
	if spec == nil || *spec == (ScheduleExactTimeSpec{}) {
		return nil, nil
	}
	value := *spec
	value.At, value.TimeOfDay = strings.TrimSpace(value.At), strings.TrimSpace(value.TimeOfDay)
	value.TimeZone, value.ExpiresAt = strings.TrimSpace(value.TimeZone), strings.TrimSpace(value.ExpiresAt)
	if (value.At == "") == (value.TimeOfDay == "") {
		return nil, errors.New("exactTime requires exactly one of at and timeOfDay")
	}
	if value.TimeZone == "" {
		value.TimeZone = defaultScheduleTimeZone()
	}
	location, err := time.LoadLocation(value.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("invalid exactTime timeZone: %w", err)
	}
	if value.TimeOfDay != "" {
		parsed, err := time.Parse("15:04", value.TimeOfDay)
		if err != nil || parsed.Format("15:04") != value.TimeOfDay {
			return nil, errors.New("exactTime timeOfDay must be HH:MM")
		}
	}
	var at time.Time
	if value.At != "" {
		at, err = parseScheduleTime(value.At, location)
		if err != nil {
			return nil, fmt.Errorf("invalid exactTime at: %w", err)
		}
		value.At = at.UTC().Format(time.RFC3339Nano)
	}
	if value.ExpiresAt != "" {
		expires, err := parseScheduleTime(value.ExpiresAt, location)
		if err != nil {
			return nil, fmt.Errorf("invalid exactTime expiresAt: %w", err)
		}
		if !at.IsZero() && expires.Before(at) {
			return nil, errors.New("exactTime expiresAt must not precede at")
		}
		value.ExpiresAt = expires.UTC().Format(time.RFC3339Nano)
	}
	return &ScheduleExactTime{ScheduleExactTimeSpec: value}, nil
}

func validateStoredExactTime(exact *ScheduleExactTime) error {
	if exact == nil {
		return nil
	}
	if normalized, err := normalizeScheduleExactTime(&exact.ScheduleExactTimeSpec); err != nil {
		return err
	} else if normalized == nil {
		return errors.New("empty exactTime")
	}
	for name, value := range map[string]string{"startedAt": exact.StartedAt, "lastFiredAt": exact.LastFiredAt} {
		if value == "" && name == "lastFiredAt" {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			return errors.New("exactTime startedAt and lastFiredAt must be RFC3339")
		}
	}
	if exact.Pending != nil {
		for _, value := range []string{exact.Pending.Occurrence, exact.Pending.FiredAt} {
			if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
				return errors.New("invalid pending exactTime timestamp")
			}
		}
		if exact.Pending.Description == "" || exact.Pending.Target == "" ||
			(exact.Pending.Language != "en" && exact.Pending.Language != "zh-CN") {
			return errors.New("invalid pending exactTime delivery")
		}
	}
	return nil
}

func (s Schedule) NextExactTimeOccurrence(now time.Time) (time.Time, bool) {
	exact := s.ExactTime
	if exact == nil {
		return time.Time{}, false
	}
	if exact.Pending != nil {
		pending, err := time.Parse(time.RFC3339Nano, exact.Pending.Occurrence)
		return pending, err == nil
	}
	last, _ := time.Parse(time.RFC3339Nano, exact.LastFiredAt)
	if exact.At != "" {
		instant, err := time.Parse(time.RFC3339Nano, exact.At)
		return instant, err == nil && last.IsZero()
	}
	location, err := time.LoadLocation(exact.TimeZone)
	if err != nil {
		return time.Time{}, false
	}
	start, err := time.Parse(time.RFC3339Nano, exact.StartedAt)
	if err != nil {
		return time.Time{}, false
	}
	expires, _ := time.Parse(time.RFC3339Nano, exact.ExpiresAt)
	limit := now
	if !expires.IsZero() && expires.Before(limit) {
		limit = expires
	}
	hour, _ := strconv.Atoi(exact.TimeOfDay[:2])
	minute, _ := strconv.Atoi(exact.TimeOfDay[3:])
	local := limit.In(location)
	for day := 0; day > -3; day-- {
		date := time.Date(local.Year(), local.Month(), local.Day()+day, 12, 0, 0, 0, location)
		candidate, exists := scheduleWallTime(date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, location)
		if exists && !candidate.After(limit) {
			if !candidate.Before(start) && candidate.After(last) {
				return candidate, true
			}
			break
		}
	}
	base := now
	if start.After(base) {
		base = start
	}
	if last.After(base) {
		base = last
	}
	local = base.In(location)
	for day := 0; day < 4; day++ {
		date := time.Date(local.Year(), local.Month(), local.Day()+day, 12, 0, 0, 0, location)
		candidate, exists := scheduleWallTime(date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, location)
		if exists && !candidate.Before(start) && candidate.After(now) && candidate.After(last) {
			return candidate, expires.IsZero() || !candidate.After(expires)
		}
	}
	return time.Time{}, false
}

func (w *Workspace) DispatchExactTimeSchedules(now time.Time, accept func(Schedule) error) error {
	if err := w.require(); err != nil {
		return err
	}
	return withWorkspaceMutationLock(w.root, func() error {
		config, err := readSchedulerJSON(schedulerJSONPath(w.root))
		if err != nil {
			return err
		}
		workspaceConfig, err := readWorkspaceConfig(w.root)
		if err != nil {
			return err
		}
		var failures []error
		for index := 0; index < len(config.Schedules); index++ {
			schedule := &config.Schedules[index]
			occurrence, exists := schedule.NextExactTimeOccurrence(now)
			if !exists || occurrence.After(now) {
				continue
			}
			exact := schedule.ExactTime
			if exact.Pending == nil {
				exact.Pending = &ScheduleExactDelivery{
					Occurrence: occurrence.UTC().Format(time.RFC3339Nano),
					FiredAt:    now.UTC().Format(time.RFC3339Nano), Language: workspaceConfig.Language,
					Description: schedule.Description, Target: schedule.Target,
				}
				if err := writeSchedulerJSON(schedulerJSONPath(w.root), config); err != nil {
					return err
				}
			}
			if err := accept(*schedule); err != nil {
				failures = append(failures, fmt.Errorf("schedule %s: %w", schedule.ID, err))
				continue
			}
			exact.LastFiredAt, exact.Pending = exact.Pending.Occurrence, nil
			if _, next := schedule.NextExactTimeOccurrence(now); !next {
				config.Schedules = append(config.Schedules[:index], config.Schedules[index+1:]...)
				index--
			}
			if err := writeSchedulerJSON(schedulerJSONPath(w.root), config); err != nil {
				return err
			}
		}
		return errors.Join(failures...)
	})
}

func updatedScheduleExactTime(previous *ScheduleExactTime, spec *ScheduleExactTimeSpec, now time.Time, targetChanged bool) (*ScheduleExactTime, error) {
	next := previous
	if spec != nil {
		normalized, err := normalizeScheduleExactTime(spec)
		if err != nil {
			return nil, err
		}
		if previous == nil || normalized == nil || previous.ScheduleExactTimeSpec != normalized.ScheduleExactTimeSpec {
			next = normalized
			if next != nil {
				next.StartedAt = now.UTC().Format(time.RFC3339Nano)
			}
		}
	}
	if next != nil && targetChanged {
		cloned := *next
		cloned.StartedAt, cloned.LastFiredAt, cloned.Pending = now.UTC().Format(time.RFC3339Nano), "", nil
		next = &cloned
	}
	return next, nil
}
