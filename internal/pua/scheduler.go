package pua

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/disksing/pua/internal/app"
)

const (
	schedulerAddUsage    = "usage: pua scheduler add --description=<text> --target=<resource> [--condition=<text>] [--at=<time>|--daily=<HH:MM>] [--timezone=<IANA>] [--expires=<time>]"
	schedulerShowUsage   = "usage: pua scheduler show --id=<schedule>"
	schedulerUpdateUsage = "usage: pua scheduler update --id=<schedule> [--description=<text>] [--condition=<text>] [--target=<resource>] [--at=<time>|--daily=<HH:MM>] [--timezone=<IANA>] [--expires=<time>] [--no-exact-time]"
	schedulerRemoveUsage = "usage: pua scheduler remove --id=<schedule>"
)

func runScheduler(args []string) error {
	if len(args) > 0 && isHelpCommand(args[0]) {
		printSchedulerHelp()
		return nil
	}
	if len(args) == 0 {
		return errors.New("scheduler requires a subcommand")
	}
	switch args[0] {
	case "list":
		jsonOutput := false
		for _, arg := range args[1:] {
			if arg != "--json" || jsonOutput {
				return errors.New("usage: pua scheduler list [--json]")
			}
			jsonOutput = true
		}
		workspace, err := openApplicationWorkspace()
		if err != nil {
			return err
		}
		config, err := workspace.Scheduler()
		if err != nil {
			return err
		}
		if jsonOutput {
			return printJSON(config)
		}
		for _, schedule := range config.Schedules {
			trigger := schedule.Condition
			if schedule.ExactTime != nil {
				trigger = schedule.ExactTime.At
				if trigger == "" {
					trigger = "daily " + schedule.ExactTime.TimeOfDay
				}
				trigger += " (" + schedule.ExactTime.TimeZone + ")"
			}
			fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%s\n", schedule.ID, schedule.Description, trigger, schedule.Target)
		}
		return nil
	case "show":
		values, err := parseSchedulerOptions(args[1:], map[string]bool{"id": true})
		if err != nil || values["id"] == "" {
			return errors.New(schedulerShowUsage)
		}
		workspace, err := openApplicationWorkspace()
		if err != nil {
			return err
		}
		config, err := workspace.Scheduler()
		if err != nil {
			return err
		}
		for _, schedule := range config.Schedules {
			if schedule.ID == values["id"] {
				return printJSON(schedule)
			}
		}
		return fmt.Errorf("schedule not found: %s", values["id"])
	case "add":
		values, err := parseSchedulerOptions(args[1:], map[string]bool{"description": true, "condition": true, "target": true, "at": true, "daily": true, "timezone": true, "expires": true})
		if err != nil || values["description"] == "" || values["target"] == "" {
			return errors.New(schedulerAddUsage)
		}
		exact, err := schedulerExactTimeOptions(values, nil)
		if err != nil {
			return err
		}
		workspace, err := openApplicationWorkspace()
		if err != nil {
			return err
		}
		created, err := workspace.AddSchedule(app.CreateScheduleInput{Description: values["description"], Condition: values["condition"], Target: values["target"], ExactTime: exact})
		if err != nil {
			return err
		}
		return printJSON(created)
	case "update":
		values, err := parseSchedulerOptions(args[1:], map[string]bool{"id": true, "description": true, "condition": true, "target": true, "at": true, "daily": true, "timezone": true, "expires": true, "no-exact-time": true})
		if err != nil || values["id"] == "" {
			return errors.New(schedulerUpdateUsage)
		}
		input := app.UpdateScheduleInput{ID: values["id"]}
		if value, exists := values["description"]; exists {
			input.Description = &value
		}
		if value, exists := values["condition"]; exists {
			input.Condition = &value
		}
		if value, exists := values["target"]; exists {
			input.Target = &value
		}
		if len(values) == 1 {
			return errors.New(schedulerUpdateUsage)
		}
		workspace, err := openApplicationWorkspace()
		if err != nil {
			return err
		}
		config, err := workspace.Scheduler()
		if err != nil {
			return err
		}
		var previous *app.ScheduleExactTimeSpec
		for _, schedule := range config.Schedules {
			if schedule.ID == input.ID && schedule.ExactTime != nil {
				previous = &schedule.ExactTime.ScheduleExactTimeSpec
			}
		}
		input.ExactTime, err = schedulerExactTimeOptions(values, previous)
		if err != nil {
			return err
		}
		updated, err := workspace.UpdateSchedule(input)
		if err != nil {
			return err
		}
		return printJSON(updated)
	case "remove":
		values, err := parseSchedulerOptions(args[1:], map[string]bool{"id": true})
		if err != nil || values["id"] == "" {
			return errors.New(schedulerRemoveUsage)
		}
		workspace, err := openApplicationWorkspace()
		if err != nil {
			return err
		}
		removed, err := workspace.RemoveSchedule(values["id"])
		if err != nil {
			return err
		}
		return printJSON(removed)
	default:
		return fmt.Errorf("unknown scheduler subcommand %q", args[0])
	}
}

func parseSchedulerOptions(args []string, allowed map[string]bool) (map[string]string, error) {
	values := make(map[string]string)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "--") {
			return nil, fmt.Errorf("unexpected argument %q", arg)
		}
		nameValue := strings.SplitN(strings.TrimPrefix(arg, "--"), "=", 2)
		name := nameValue[0]
		_, duplicate := values[name]
		if !allowed[name] || duplicate {
			return nil, fmt.Errorf("unknown or duplicate option --%s", name)
		}
		value := ""
		if name == "no-exact-time" && len(nameValue) == 1 {
			values[name] = "true"
			continue
		}
		if len(nameValue) == 2 {
			value = nameValue[1]
		} else if index+1 < len(args) && !strings.HasPrefix(args[index+1], "--") {
			index++
			value = args[index]
		}
		value = strings.TrimSpace(value)
		if value == "" && !(len(nameValue) == 2 && (name == "condition" || name == "expires")) {
			return nil, fmt.Errorf("option --%s requires a value", name)
		}
		values[name] = value
	}
	return values, nil
}

func schedulerExactTimeOptions(values map[string]string, previous *app.ScheduleExactTimeSpec) (*app.ScheduleExactTimeSpec, error) {
	hasExact := false
	for _, name := range []string{"at", "daily", "timezone", "expires"} {
		if _, exists := values[name]; exists {
			hasExact = true
		}
	}
	if values["no-exact-time"] != "" {
		if values["no-exact-time"] != "true" || hasExact {
			return nil, errors.New("--no-exact-time conflicts with exact-time options")
		}
		return &app.ScheduleExactTimeSpec{}, nil
	}
	if !hasExact {
		return nil, nil
	}
	if values["at"] != "" && values["daily"] != "" {
		return nil, errors.New("--at and --daily are mutually exclusive")
	}
	var exact app.ScheduleExactTimeSpec
	if previous != nil {
		exact = *previous
	}
	if at, exists := values["at"]; exists {
		exact.At, exact.TimeOfDay = at, ""
	}
	if daily, exists := values["daily"]; exists {
		exact.TimeOfDay, exact.At = daily, ""
	}
	if zone, exists := values["timezone"]; exists {
		exact.TimeZone = zone
	}
	if expires, exists := values["expires"]; exists {
		exact.ExpiresAt = expires
	}
	return &exact, nil
}
