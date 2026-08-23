package pua

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/disksing/pua/internal/serve"
)

const serviceUsage = "usage: pua service list|show|apply|remove|enable|disable|start|stop|restart|logs|exports|validate"

func runService(args []string) error {
	if len(args) > 0 && isHelpCommand(args[0]) {
		printServiceHelp()
		return nil
	}
	if len(args) == 0 {
		return errors.New("service requires a subcommand")
	}
	switch args[0] {
	case "list":
		return runServiceList(args[1:])
	case "show":
		return runServiceShow(args[1:])
	case "apply":
		return runServiceApply(args[1:])
	case "remove":
		return runServiceMutation(args[1:], "remove")
	case "enable":
		return runServiceMutation(args[1:], "enable")
	case "disable":
		return runServiceMutation(args[1:], "disable")
	case "start":
		return runServiceMutation(args[1:], "start")
	case "stop":
		return runServiceMutation(args[1:], "stop")
	case "restart":
		return runServiceMutation(args[1:], "restart")
	case "logs":
		return runServiceLogs(args[1:])
	case "exports":
		return runServiceExports(args[1:])
	case "validate":
		return runServiceValidate(args[1:])
	default:
		return fmt.Errorf("unknown service subcommand %q", args[0])
	}
}

func serviceClient(args []string, usage string) ([]string, *resourceServerClient, error) {
	remaining, serverURL, err := splitServerArg(args, usage)
	if err != nil {
		return nil, nil, err
	}
	client, _, err := newResourceServerClient(serverURL)
	if err != nil {
		return nil, nil, err
	}
	return remaining, client, nil
}

func runServiceList(args []string) error {
	remaining, client, err := serviceClient(args, "usage: pua service list [--server=<url>] [--json]")
	if err != nil {
		return err
	}
	jsonOutput := false
	for _, arg := range remaining {
		if arg == "--json" && !jsonOutput {
			jsonOutput = true
		} else {
			return errors.New("usage: pua service list [--server=<url>] [--json]")
		}
	}
	var response struct {
		Services []serve.ServiceStatus `json:"services"`
	}
	if err := client.request(context.Background(), http.MethodGet, "/api/workspaces/"+client.workspaceID+"/services", nil, &response); err != nil {
		return err
	}
	if jsonOutput {
		return printJSON(response)
	}
	for _, service := range response.Services {
		fmt.Fprintf(os.Stdout, "%s\t%s\n", service.ID, service.State)
	}
	return nil
}

func runServiceShow(args []string) error {
	remaining, client, err := serviceClient(args, "usage: pua service show <id> [--server=<url>] [--json]")
	if err != nil {
		return err
	}
	if len(remaining) < 1 || len(remaining) > 2 {
		return errors.New("usage: pua service show <id> [--server=<url>] [--json]")
	}
	id := remaining[0]
	jsonOutput := len(remaining) == 2 && remaining[1] == "--json"
	if len(remaining) == 2 && !jsonOutput {
		return errors.New("usage: pua service show <id> [--server=<url>] [--json]")
	}
	var response serve.ServiceStatus
	if err := client.request(context.Background(), http.MethodGet, "/api/workspaces/"+client.workspaceID+"/services/"+id, nil, &response); err != nil {
		return err
	}
	if jsonOutput {
		return printJSON(response)
	}
	fmt.Fprintf(os.Stdout, "%s\t%s\n", response.ID, response.State)
	return nil
}

func runServiceApply(args []string) error {
	remaining, client, err := serviceClient(args, "usage: pua service apply --file=<config> [--server=<url>]")
	if err != nil {
		return err
	}
	file := ""
	for i := 0; i < len(remaining); i++ {
		if strings.HasPrefix(remaining[i], "--file=") {
			file = strings.TrimPrefix(remaining[i], "--file=")
		} else if remaining[i] == "--file" && i+1 < len(remaining) {
			i++
			file = remaining[i]
		} else {
			return errors.New("usage: pua service apply --file=<config> [--server=<url>]")
		}
	}
	if strings.TrimSpace(file) == "" {
		return errors.New("usage: pua service apply --file=<config> [--server=<url>]")
	}
	cfg, err := serve.LoadServiceConfig(file)
	if err != nil {
		return err
	}
	var response serve.ServiceStatus
	if err := client.request(context.Background(), http.MethodPut, "/api/workspaces/"+client.workspaceID+"/services/"+cfg.ID, cfg, &response); err != nil {
		return err
	}
	return printJSON(response)
}

func runServiceMutation(args []string, action string) error {
	remaining, client, err := serviceClient(args, fmt.Sprintf("usage: pua service %s <id> [--server=<url>]", action))
	if err != nil {
		return err
	}
	if len(remaining) != 1 {
		return fmt.Errorf("usage: pua service %s <id> [--server=<url>]", action)
	}
	id := remaining[0]
	method := http.MethodPost
	path := "/api/workspaces/" + client.workspaceID + "/services/" + id + "/" + action
	if action == "remove" {
		method = http.MethodDelete
		path = "/api/workspaces/" + client.workspaceID + "/services/" + id
	}
	var response serve.ServiceStatus
	if err := client.request(context.Background(), method, path, nil, &response); err != nil {
		return err
	}
	if action != "remove" {
		return printJSON(response)
	}
	return nil
}

func runServiceLogs(args []string) error {
	remaining, client, err := serviceClient(args, "usage: pua service logs <id> [--follow] [--stream=stdout|stderr] [--server=<url>]")
	if err != nil {
		return err
	}
	if len(remaining) < 1 || len(remaining) > 3 {
		return errors.New("usage: pua service logs <id> [--follow] [--stream=stdout|stderr] [--server=<url>]")
	}
	id := remaining[0]
	follow := false
	stream := "stdout"
	for _, arg := range remaining[1:] {
		switch {
		case arg == "--follow":
			follow = true
		case strings.HasPrefix(arg, "--stream="):
			stream = strings.TrimPrefix(arg, "--stream=")
			if stream != "stdout" && stream != "stderr" {
				return errors.New("usage: pua service logs <id> [--follow] [--stream=stdout|stderr] [--server=<url>]")
			}
		default:
			return errors.New("usage: pua service logs <id> [--follow] [--stream=stdout|stderr] [--server=<url>]")
		}
	}
	path := "/api/workspaces/" + client.workspaceID + "/services/" + id + "/logs"
	path += "?stream=" + stream
	if follow {
		path += "&follow=true"
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, client.baseURL+path, nil)
	if err != nil {
		return err
	}
	response, err := client.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(response.Body)
		return fmt.Errorf("PUA Server returned %s: %s", response.Status, strings.TrimSpace(string(data)))
	}
	_, err = io.Copy(os.Stdout, response.Body)
	return err
}

func runServiceExports(args []string) error {
	remaining, client, err := serviceClient(args, "usage: pua service exports <id> [--json] [--server=<url>]")
	if err != nil {
		return err
	}
	if len(remaining) < 1 || len(remaining) > 2 {
		return errors.New("usage: pua service exports <id> [--json] [--server=<url>]")
	}
	id := remaining[0]
	jsonOutput := len(remaining) == 2 && remaining[1] == "--json"
	if len(remaining) == 2 && !jsonOutput {
		return errors.New("usage: pua service exports <id> [--json] [--server=<url>]")
	}
	var response serve.ServiceExports
	if err := client.request(context.Background(), http.MethodGet, "/api/workspaces/"+client.workspaceID+"/services/"+id+"/exports", nil, &response); err != nil {
		return err
	}
	if jsonOutput {
		return printJSON(response)
	}
	variableNames := make([]string, 0, len(response.Variables))
	for key := range response.Variables {
		variableNames = append(variableNames, key)
	}
	sort.Strings(variableNames)
	for _, key := range variableNames {
		value := response.Variables[key]
		fmt.Fprintf(os.Stdout, "%s=%s\n", key, value)
	}
	for _, secret := range response.Secrets {
		fmt.Fprintf(os.Stdout, "%s=<secret:%s>\n", secret.Name, secret.Name)
	}
	return nil
}

func runServiceValidate(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: pua service validate")
	}
	workspace, err := openApplicationWorkspace()
	if err != nil {
		return err
	}
	if err := serve.ValidateServices(workspace.Root()); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "services valid")
	return nil
}

func printServiceHelp() {
	fmt.Print(`Usage:
  pua service list [--server=<url>] [--json]
  pua service show <id> [--server=<url>] [--json]
  pua service apply --file=<config> [--server=<url>]
  pua service remove|enable|disable|start|stop|restart <id> [--server=<url>]
  pua service logs <id> [--follow] [--stream=stdout|stderr] [--server=<url>]
  pua service exports <id> [--server=<url>] [--json]
  pua service validate

Service definitions are stored in .pua/services/<id>.json and are owned by
the pua serve process. Every service that writes PUA_SERVICE_EXPORT_PATH must
set "exports": true, regardless of readiness. Readiness-only services omit the
flag. Secret values are never printed or persisted by PUA.
`)
}
