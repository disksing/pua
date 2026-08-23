package pua

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
)

const (
	schedulerAddUsage    = "usage: pua scheduler add --description=<text> --condition=<text> --target=<resource> [--guard=<text>] (--at=<rfc3339>|--every=<duration> --anchor=<rfc3339>|--cron=<six-fields> --timezone=<iana>) [--server=<url>]"
	schedulerShowUsage   = "usage: pua scheduler show --id=<schedule> [--server=<url>]"
	schedulerUpdateUsage = "usage: pua scheduler update --id=<schedule> --revision=<n> [--description=<text>] [--condition=<text>] [--guard=<text>] [--target=<resource>] (--at=<rfc3339>|--every=<duration> --anchor=<rfc3339>|--cron=<six-fields> --timezone=<iana>) [--server=<url>]"
	schedulerRemoveUsage = "usage: pua scheduler remove --id=<schedule> [--server=<url>]"
)

type schedulerChangePayload struct {
	Operation        string               `json:"operation"`
	ID               string               `json:"id,omitempty"`
	ExpectedRevision uint64               `json:"expectedRevision,omitempty"`
	Description      *string              `json:"description,omitempty"`
	Condition        *string              `json:"condition,omitempty"`
	Guard            *string              `json:"guard,omitempty"`
	Target           *string              `json:"target,omitempty"`
	Trigger          *app.ScheduleTrigger `json:"trigger,omitempty"`
}

func runScheduler(args []string) error {
	if len(args) > 0 && isHelpCommand(args[0]) {
		printSchedulerHelp()
		return nil
	}
	if len(args) == 0 {
		return errors.New("scheduler requires a subcommand")
	}
	subcommand := args[0]
	usage := "usage: pua scheduler " + subcommand + " [--server=<url>]"
	switch subcommand {
	case "add":
		usage = schedulerAddUsage
	case "update":
		usage = schedulerUpdateUsage
	case "show":
		usage = schedulerShowUsage
	case "remove":
		usage = schedulerRemoveUsage
	}
	remaining, serverURL, err := splitServerArg(args[1:], usage)
	if err != nil {
		return err
	}
	var updatePayload schedulerChangePayload
	if subcommand == "update" {
		updatePayload, err = parseSchedulerUpdatePayload(remaining)
		if err != nil {
			return err
		}
	}
	client, _, err := newResourceServerClient(serverURL)
	if err != nil {
		return err
	}
	basePath := fmt.Sprintf("/api/workspaces/%s/scheduler", url.PathEscape(client.workspaceID))

	switch subcommand {
	case "list":
		jsonOutput := false
		for _, arg := range remaining {
			if arg != "--json" || jsonOutput {
				return errors.New("usage: pua scheduler list [--json] [--server=<url>]")
			}
			jsonOutput = true
		}
		snapshot, err := schedulerSnapshot(client, basePath)
		if err != nil {
			return err
		}
		if jsonOutput {
			return printJSON(snapshot)
		}
		for _, schedule := range snapshot.Schedules {
			nextRun, lastOutcome := schedule.NextRunAt, schedule.LastOutcome
			if nextRun == "" {
				nextRun = "-"
			}
			if lastOutcome == "" {
				lastOutcome = "-"
			}
			fmt.Fprintf(os.Stdout, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", schedule.ID, schedule.Revision, schedule.EffectiveState, scheduleTriggerSummary(schedule.Trigger), nextRun, lastOutcome, schedule.Description, schedule.Target)
		}
		return nil
	case "show":
		values, err := parseSchedulerOptions(remaining, map[string]bool{"id": true})
		if err != nil || values["id"] == "" {
			return errors.New(schedulerShowUsage)
		}
		snapshot, err := schedulerSnapshot(client, basePath)
		if err != nil {
			return err
		}
		for _, schedule := range snapshot.Schedules {
			if schedule.ID == values["id"] {
				return printJSON(schedule)
			}
		}
		return fmt.Errorf("schedule not found: %s", values["id"])
	case "add":
		values, err := parseSchedulerOptions(remaining, schedulerMutationOptions(false))
		if err != nil || values["description"] == "" || values["condition"] == "" || values["target"] == "" {
			return errors.New(schedulerAddUsage)
		}
		trigger, present, err := schedulerTriggerFromOptions(values)
		if err != nil || !present {
			if err != nil {
				return err
			}
			return errors.New(schedulerAddUsage)
		}
		description, condition, target := values["description"], values["condition"], values["target"]
		payload := schedulerChangePayload{Operation: "create", Description: &description, Condition: &condition, Target: &target, Trigger: trigger}
		if guard, ok := values["guard"]; ok {
			payload.Guard = &guard
		}
		return schedulerChangeRequest(client, basePath, payload)
	case "update":
		return schedulerChangeRequest(client, basePath, updatePayload)
	case "pause", "resume", "remove":
		values, err := parseSchedulerOptions(remaining, map[string]bool{"id": true})
		if err != nil || values["id"] == "" {
			return errors.New("usage: pua scheduler " + subcommand + " --id=<schedule> [--server=<url>]")
		}
		return schedulerChangeRequest(client, basePath, schedulerChangePayload{Operation: subcommand, ID: values["id"]})
	default:
		return fmt.Errorf("unknown scheduler subcommand %q", subcommand)
	}
}

func parseSchedulerUpdatePayload(args []string) (schedulerChangePayload, error) {
	values, err := parseSchedulerOptions(args, schedulerMutationOptions(true))
	if err != nil || values["id"] == "" || values["revision"] == "" {
		return schedulerChangePayload{}, errors.New(schedulerUpdateUsage)
	}
	revision, err := strconv.ParseUint(values["revision"], 10, 64)
	if err != nil || revision == 0 {
		return schedulerChangePayload{}, errors.New(schedulerUpdateUsage)
	}
	trigger, triggerPresent, err := schedulerTriggerFromOptions(values)
	if err != nil {
		return schedulerChangePayload{}, err
	}
	if !triggerPresent {
		return schedulerChangePayload{}, errors.New(schedulerUpdateUsage)
	}
	payload := schedulerChangePayload{Operation: "update", ID: values["id"], ExpectedRevision: revision, Trigger: trigger}
	for name, target := range map[string]**string{"description": &payload.Description, "condition": &payload.Condition, "guard": &payload.Guard, "target": &payload.Target} {
		if value, ok := values[name]; ok {
			copy := value
			*target = &copy
		}
	}
	return payload, nil
}

func schedulerSnapshot(client *resourceServerClient, path string) (app.SchedulerSnapshot, error) {
	var snapshot app.SchedulerSnapshot
	err := client.request(context.Background(), http.MethodGet, path, nil, &snapshot)
	return snapshot, err
}

func schedulerChangeRequest(client *resourceServerClient, path string, payload schedulerChangePayload) error {
	var schedule app.Schedule
	if err := client.request(context.Background(), http.MethodPost, path+"/changes", payload, &schedule); err != nil {
		return err
	}
	return printJSON(schedule)
}

func schedulerMutationOptions(update bool) map[string]bool {
	result := map[string]bool{
		"description": true, "condition": true, "guard": true, "target": true,
		"at": true, "every": true, "anchor": true, "cron": true, "timezone": true,
	}
	if update {
		result["id"], result["revision"] = true, true
	}
	return result
}

func schedulerTriggerFromOptions(values map[string]string) (*app.ScheduleTrigger, bool, error) {
	at, hasAt := values["at"]
	everyValue, hasEvery := values["every"]
	anchor, hasAnchor := values["anchor"]
	expression, hasCron := values["cron"]
	timeZone, hasTimeZone := values["timezone"]
	forms := 0
	if hasAt {
		forms++
	}
	if hasEvery || hasAnchor {
		forms++
	}
	if hasCron || hasTimeZone {
		forms++
	}
	if forms == 0 {
		return nil, false, nil
	}
	if forms != 1 {
		return nil, true, errors.New("exactly one trigger form is required")
	}
	var trigger app.ScheduleTrigger
	switch {
	case hasAt:
		trigger = app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at}
	case hasEvery || hasAnchor:
		if !hasEvery || !hasAnchor {
			return nil, true, errors.New("--every and --anchor are required together")
		}
		duration, err := time.ParseDuration(everyValue)
		if err != nil || duration%time.Second != 0 {
			return nil, true, errors.New("--every must be a whole-second duration such as 5m")
		}
		trigger = app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: int64(duration / time.Second), AnchorAt: anchor}
	case hasCron || hasTimeZone:
		if !hasCron || !hasTimeZone {
			return nil, true, errors.New("--cron and --timezone are required together")
		}
		trigger = app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: expression, TimeZone: timeZone}
	}
	if err := app.ValidateScheduleTrigger(trigger); err != nil {
		return nil, true, err
	}
	return &trigger, true, nil
}

func scheduleTriggerSummary(trigger *app.ScheduleTrigger) string {
	if trigger == nil {
		return "needs compilation"
	}
	switch trigger.Type {
	case app.ScheduleTriggerAt:
		return "at " + trigger.At
	case app.ScheduleTriggerInterval:
		return fmt.Sprintf("every %ds from %s", trigger.EverySeconds, trigger.AnchorAt)
	case app.ScheduleTriggerCron:
		return trigger.Cron + " [" + trigger.TimeZone + "]"
	default:
		return trigger.Type
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
		if !allowed[name] {
			return nil, fmt.Errorf("unknown option --%s", name)
		}
		if _, exists := values[name]; exists {
			return nil, fmt.Errorf("duplicate option --%s", name)
		}
		value := ""
		explicitEmpty := len(nameValue) == 2 && nameValue[1] == ""
		if len(nameValue) == 2 {
			value = nameValue[1]
		} else if index+1 < len(args) && !strings.HasPrefix(args[index+1], "--") {
			index++
			value = args[index]
		}
		value = strings.TrimSpace(value)
		if value == "" && !(name == "guard" && explicitEmpty) {
			return nil, fmt.Errorf("option --%s requires a value", name)
		}
		values[name] = value
	}
	return values, nil
}
