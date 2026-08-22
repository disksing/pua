package pua

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/disksing/pua/internal/app"
)

const (
	workspaceBindingUsage = "usage: pua workspace binding set (--profile=<name>|--agent=<name>) [--server=<url>]"
	projectBindingUsage   = "usage: pua project binding set [--project=<project>] (--profile=<name>|--agent=<name>) [--server=<url>]"
	taskBindingUsage      = "usage: pua task binding set [--project=<project>] [--task=<task>] (--profile=<name>|--agent=<name>) [--server=<url>]"
)

type bindingFlagOptions struct {
	Binding    app.AgentBinding
	BindingSet bool
}

// parseAgentBindingOption consumes one --profile or --agent option. It is
// shared by binding set commands and task create so both surfaces enforce the
// same mutually-exclusive forms.
func parseAgentBindingOption(args []string, index *int, binding *app.AgentBinding, bindingSet *bool, usage string) (bool, error) {
	arg := args[*index]
	kind := ""
	value := ""
	switch {
	case strings.HasPrefix(arg, "--profile="):
		kind = "profile"
		value = strings.TrimPrefix(arg, "--profile=")
	case arg == "--profile":
		kind = "profile"
		if *index+1 >= len(args) || strings.HasPrefix(args[*index+1], "--") {
			return true, errors.New(usage)
		}
		*index = *index + 1
		value = args[*index]
	case strings.HasPrefix(arg, "--agent="):
		kind = "agent"
		value = strings.TrimPrefix(arg, "--agent=")
	case arg == "--agent":
		kind = "agent"
		if *index+1 >= len(args) || strings.HasPrefix(args[*index+1], "--") {
			return true, errors.New(usage)
		}
		*index = *index + 1
		value = args[*index]
	default:
		return false, nil
	}
	if *bindingSet {
		if binding.Kind == kind {
			return true, errors.New(usage)
		}
		return true, errors.New("--profile and --agent are mutually exclusive")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return true, errors.New(usage)
	}
	*binding = app.AgentBinding{Kind: kind, Name: value}
	*bindingSet = true
	return true, nil
}

func parseBindingFlags(args []string, usage string) (bindingFlagOptions, []string, error) {
	var options bindingFlagOptions
	remaining := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		handled, err := parseAgentBindingOption(args, &index, &options.Binding, &options.BindingSet, usage)
		if err != nil {
			return bindingFlagOptions{}, nil, err
		}
		if handled {
			continue
		}
		remaining = append(remaining, args[index])
	}
	return options, remaining, nil
}

func runWorkspaceBinding(args []string) error {
	if len(args) == 0 || args[0] != "set" {
		return errors.New(workspaceBindingUsage)
	}
	remaining, serverURL, err := splitServerArg(args[1:], workspaceBindingUsage)
	if err != nil {
		return err
	}
	options, remaining, err := parseBindingFlags(remaining, workspaceBindingUsage)
	if err != nil {
		return err
	}
	if len(remaining) != 0 || !options.BindingSet {
		return errors.New(workspaceBindingUsage)
	}
	return setResourceAgentBinding("workspace", options.Binding, serverURL)
}

func runProjectBinding(args []string) error {
	if len(args) == 0 || args[0] != "set" {
		return errors.New(projectBindingUsage)
	}
	remaining, serverURL, err := splitServerArg(args[1:], projectBindingUsage)
	if err != nil {
		return err
	}
	options, remaining, err := parseBindingFlags(remaining, projectBindingUsage)
	if err != nil {
		return err
	}
	if !options.BindingSet {
		return errors.New(projectBindingUsage)
	}
	projectID, err := resolveProjectArg(remaining, "binding set")
	if err != nil {
		return err
	}
	return setResourceAgentBinding(projectID, options.Binding, serverURL)
}

func runTaskBinding(args []string) error {
	if len(args) == 0 || args[0] != "set" {
		return errors.New(taskBindingUsage)
	}
	remaining, serverURL, err := splitServerArg(args[1:], taskBindingUsage)
	if err != nil {
		return err
	}
	options, remaining, err := parseBindingFlags(remaining, taskBindingUsage)
	if err != nil {
		return err
	}
	if !options.BindingSet {
		return errors.New(taskBindingUsage)
	}
	taskID, err := resolveTaskArg(remaining, "binding set")
	if err != nil {
		return err
	}
	return setResourceAgentBinding(taskID, options.Binding, serverURL)
}

func setResourceAgentBinding(resourceID string, binding app.AgentBinding, serverURL string) error {
	client, _, err := newResourceServerClient(serverURL)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/workspaces/%s/resources/%s/agent-binding", url.PathEscape(client.workspaceID), url.PathEscape(resourceID))
	var response map[string]any
	if err := client.request(context.Background(), http.MethodPut, path, binding, &response); err != nil {
		return err
	}
	return printJSON(response)
}
