package pua

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/workspacepath"
)

const (
	puaWorkspaceRootEnvironment     = "PUA_WORKSPACE_ROOT"
	puaWorkspaceInstanceEnvironment = "PUA_WORKSPACE_INSTANCE_ID"
	puaResourceIDEnvironment        = "PUA_RESOURCE_ID"
	workspaceStatusUsage            = "usage: pua workspace status [--server=<url>]"
	projectStatusUsage              = "usage: pua project status [--project=<project>] [--server=<url>]"
	taskStatusUsage                 = "usage: pua task status [--project=<project>] [--task=<task>] [--server=<url>]"
	taskStateUsage                  = "usage: pua task state [--project=<project>] [--task=<task>] [--server=<url>] | pua task state set <waiting|blocked|paused|completed> [--note=<text>] [--project=<project>] [--task=<task>] [--server=<url>]"
	messageSendUsage                = "usage: pua message send --to=<resource|user> [--mode=steer|enqueue|interrupt] [--subscribe-result=false] [--server=<url>] <message>"
	messageShowUsage                = "usage: pua message show --id=<message-id> [--server=<url>]"
	workspaceHistoryUsage           = "usage: pua workspace history [--cursor=<cursor>] [--limit=<n>] [--server=<url>] [--json]"
	projectHistoryUsage             = "usage: pua project history [--project=<project>] [--cursor=<cursor>] [--limit=<n>] [--server=<url>] [--json]"
	taskHistoryUsage                = "usage: pua task history [--project=<project>] [--task=<task>] [--cursor=<cursor>] [--limit=<n>] [--server=<url>] [--json]"
	historyShowUsage                = "usage: pua history turn|event show --ref=<reference> [--server=<url>] [--json]"
)

type resourceServerOptions struct {
	ID              string
	Mode            string
	ModeSet         bool
	ServerURL       string
	Text            string
	SubscribeResult *bool
}

type resourceHistoryOptions struct {
	Cursor    string
	Limit     int
	Reference string
	ServerURL string
	JSON      bool
}

type serveLockMetadata struct {
	Address       string `json:"address"`
	WorkspacePath string `json:"workspacePath"`
}

type resourceServerClient struct {
	baseURL     string
	workspaceID string
	http        *http.Client
}

func parseMessageServerArgs(args []string, command string) (resourceServerOptions, error) {
	usage := messageSendUsage
	if command == "show" {
		usage = messageShowUsage
	}
	var options resourceServerOptions
	var text []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case strings.HasPrefix(arg, "--to=") && command == "send":
			if options.ID != "" {
				return resourceServerOptions{}, errors.New(usage)
			}
			options.ID = strings.TrimSpace(strings.TrimPrefix(arg, "--to="))
		case arg == "--to" && command == "send":
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") || options.ID != "" {
				return resourceServerOptions{}, errors.New(usage)
			}
			index++
			options.ID = strings.TrimSpace(args[index])
		case strings.HasPrefix(arg, "--id=") && command == "show":
			if options.ID != "" {
				return resourceServerOptions{}, errors.New(usage)
			}
			options.ID = strings.TrimSpace(strings.TrimPrefix(arg, "--id="))
		case arg == "--id" && command == "show":
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") || options.ID != "" {
				return resourceServerOptions{}, errors.New(usage)
			}
			index++
			options.ID = strings.TrimSpace(args[index])
		case strings.HasPrefix(arg, "--mode=") && command == "send":
			if options.ModeSet {
				return resourceServerOptions{}, errors.New(usage)
			}
			options.Mode = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(arg, "--mode=")))
			options.ModeSet = true
		case arg == "--mode" && command == "send":
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") || options.ModeSet {
				return resourceServerOptions{}, errors.New(usage)
			}
			index++
			options.Mode = strings.ToLower(strings.TrimSpace(args[index]))
			options.ModeSet = true
		case strings.HasPrefix(arg, "--subscribe-result=") && command == "send":
			if options.SubscribeResult != nil {
				return resourceServerOptions{}, errors.New(usage)
			}
			value, parseErr := strconv.ParseBool(strings.TrimSpace(strings.TrimPrefix(arg, "--subscribe-result=")))
			if parseErr != nil {
				return resourceServerOptions{}, errors.New(usage)
			}
			options.SubscribeResult = &value
		case arg == "--subscribe-result" && command == "send":
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") || options.SubscribeResult != nil {
				return resourceServerOptions{}, errors.New(usage)
			}
			index++
			value, parseErr := strconv.ParseBool(strings.TrimSpace(args[index]))
			if parseErr != nil {
				return resourceServerOptions{}, errors.New(usage)
			}
			options.SubscribeResult = &value
		case strings.HasPrefix(arg, "--server="):
			if options.ServerURL != "" {
				return resourceServerOptions{}, errors.New(usage)
			}
			options.ServerURL = strings.TrimSpace(strings.TrimPrefix(arg, "--server="))
		case arg == "--server":
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") || options.ServerURL != "" {
				return resourceServerOptions{}, errors.New(usage)
			}
			index++
			options.ServerURL = strings.TrimSpace(args[index])
		case strings.HasPrefix(arg, "--"):
			return resourceServerOptions{}, errors.New(usage)
		default:
			if command != "send" {
				return resourceServerOptions{}, errors.New(usage)
			}
			text = append(text, arg)
		}
	}
	if command == "send" {
		options.Text = strings.TrimSpace(strings.Join(text, " "))
		if options.ID == "" || options.Text == "" {
			return resourceServerOptions{}, errors.New(usage)
		}
		if options.Mode == "" {
			options.Mode = "steer"
		}
		if options.Mode != "steer" && options.Mode != "enqueue" && options.Mode != "interrupt" {
			return resourceServerOptions{}, errors.New("mode must be steer, enqueue, or interrupt")
		}
	} else if command == "show" && options.ID == "" {
		return resourceServerOptions{}, errors.New(usage)
	}
	return options, nil
}

func splitServerArg(args []string, usage string) ([]string, string, error) {
	filtered := make([]string, 0, len(args))
	serverURL := ""
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case strings.HasPrefix(arg, "--server="):
			if serverURL != "" {
				return nil, "", errors.New(usage)
			}
			serverURL = strings.TrimSpace(strings.TrimPrefix(arg, "--server="))
			if serverURL == "" {
				return nil, "", errors.New(usage)
			}
		case arg == "--server":
			if serverURL != "" || index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
				return nil, "", errors.New(usage)
			}
			index++
			serverURL = strings.TrimSpace(args[index])
			if serverURL == "" {
				return nil, "", errors.New(usage)
			}
		default:
			filtered = append(filtered, arg)
		}
	}
	return filtered, serverURL, nil
}

func inferCurrentResourceID() (string, error) {
	if taskID, ok, err := inferCurrentTaskID(); err != nil {
		return "", err
	} else if ok {
		return taskID, nil
	}
	if projectID, ok, err := inferCurrentProjectID(); err != nil {
		return "", err
	} else if ok {
		return projectID, nil
	}
	workspace, err := openApplicationWorkspace()
	if err != nil {
		return "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if inside, matchErr := workspace.IsSchedulerPath(cwd); matchErr == nil && inside {
		return app.SchedulerResourceID, nil
	}
	return "workspace", nil
}

func normalizePUAServerURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("pua serve owner address is empty")
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid PUA Server URL %q", value)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported PUA Server URL scheme %q", parsed.Scheme)
	}
	host, port, splitErr := net.SplitHostPort(parsed.Host)
	if splitErr == nil && (host == "" || host == "0.0.0.0" || host == "::") {
		parsed.Host = net.JoinHostPort("127.0.0.1", port)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func newResourceServerClient(serverOverride string) (*resourceServerClient, string, error) {
	workspace, err := openApplicationWorkspace()
	if err != nil {
		return nil, "", err
	}
	root, err := filepath.Abs(workspace.Root())
	if err != nil {
		return nil, "", err
	}
	if canonical, canonicalErr := filepath.EvalSymlinks(root); canonicalErr == nil {
		root = canonical
	}
	serverURL := strings.TrimSpace(serverOverride)
	if serverURL == "" {
		controlRoot, controlErr := workspacepath.ResolveControlDir(root)
		if controlErr != nil {
			return nil, "", controlErr
		}
		lockPath := filepath.Join(controlRoot, "serve.lock")
		data, readErr := os.ReadFile(lockPath)
		if readErr != nil {
			return nil, "", fmt.Errorf("discover PUA Server owner from %s: %w; start pua serve or use --server=<url>", lockPath, readErr)
		}
		var metadata serveLockMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			return nil, "", fmt.Errorf("read PUA Server owner metadata: %w", err)
		}
		ownerPath := strings.TrimSpace(metadata.WorkspacePath)
		if canonical, canonicalErr := filepath.EvalSymlinks(ownerPath); canonicalErr == nil {
			ownerPath = canonical
		}
		if ownerPath == "" || filepath.Clean(ownerPath) != filepath.Clean(root) {
			return nil, "", fmt.Errorf("PUA Server owner metadata changed or belongs to another Workspace; expected %s, got %s", root, ownerPath)
		}
		serverURL = metadata.Address
	}
	serverURL, err = normalizePUAServerURL(serverURL)
	if err != nil {
		return nil, "", err
	}
	sum := sha1.Sum([]byte(filepath.Clean(root)))
	return &resourceServerClient{
		baseURL: serverURL, workspaceID: hex.EncodeToString(sum[:8]),
		http: &http.Client{Timeout: 60 * time.Second},
	}, root, nil
}

func (client *resourceServerClient) request(ctx context.Context, method, path string, body any, output any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, client.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.http.Do(request)
	if err != nil {
		return fmt.Errorf("contact PUA Server %s: %w; verify the owner is running or use --server=<url>", client.baseURL, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 64*1024*1024))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &failure)
		message := strings.TrimSpace(failure.Error)
		if message == "" {
			message = strings.TrimSpace(string(data))
		}
		if message == "" {
			message = response.Status
		}
		if failure.Code != "" {
			return fmt.Errorf("PUA Server %s: %s", failure.Code, message)
		}
		return fmt.Errorf("PUA Server returned %s: %s", response.Status, message)
	}
	if output == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode PUA Server response: %w", err)
	}
	return nil
}

func runResourceStatus(resourceID, serverURL string) error {
	client, _, err := newResourceServerClient(serverURL)
	if err != nil {
		return err
	}
	var response map[string]any
	path := fmt.Sprintf("/api/workspaces/%s/resources/%s/status", url.PathEscape(client.workspaceID), url.PathEscape(resourceID))
	if err := client.request(context.Background(), http.MethodGet, path, nil, &response); err != nil {
		return err
	}
	return printJSON(response)
}

func runWorkspaceStatus(args []string) error {
	remaining, serverURL, err := splitServerArg(args, workspaceStatusUsage)
	if err != nil || len(remaining) != 0 {
		if err != nil {
			return err
		}
		return errors.New(workspaceStatusUsage)
	}
	return runResourceStatus("workspace", serverURL)
}

func runProjectStatus(args []string) error {
	remaining, serverURL, err := splitServerArg(args, projectStatusUsage)
	if err != nil {
		return err
	}
	projectID, err := resolveProjectArg(remaining, "status")
	if err != nil {
		return err
	}
	return runResourceStatus(projectID, serverURL)
}

func runTaskStatus(args []string) error {
	remaining, serverURL, err := splitServerArg(args, taskStatusUsage)
	if err != nil {
		return err
	}
	taskID, err := resolveTaskArg(remaining, "status")
	if err != nil {
		return err
	}
	return runResourceStatus(taskID, serverURL)
}

func runTaskState(args []string) error {
	set := len(args) > 0 && args[0] == "set"
	if set {
		args = args[1:]
	}
	remaining, serverURL, err := splitServerArg(args, taskStateUsage)
	if err != nil {
		return err
	}
	state := ""
	note := ""
	filtered := make([]string, 0, len(remaining))
	for index := 0; index < len(remaining); index++ {
		arg := remaining[index]
		switch {
		case set && strings.HasPrefix(arg, "--note="):
			if note != "" {
				return errors.New(taskStateUsage)
			}
			note = strings.TrimSpace(strings.TrimPrefix(arg, "--note="))
		case set && arg == "--note":
			if note != "" || index+1 >= len(remaining) {
				return errors.New(taskStateUsage)
			}
			index++
			note = strings.TrimSpace(remaining[index])
		case set && !strings.HasPrefix(arg, "--") && state == "":
			state = strings.TrimSpace(arg)
		default:
			filtered = append(filtered, arg)
		}
	}
	if !set && note != "" || set && state == "" {
		return errors.New(taskStateUsage)
	}
	if set && state != "waiting" && state != "blocked" && state != "paused" && state != "completed" {
		return errors.New("task state must be waiting, blocked, paused, or completed")
	}
	taskID, err := resolveTaskArg(filtered, "state")
	if err != nil {
		return err
	}
	client, _, err := newResourceServerClient(serverURL)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/workspaces/%s/resources/%s/task-state", url.PathEscape(client.workspaceID), url.PathEscape(taskID))
	method := http.MethodGet
	var body any
	if set {
		method = http.MethodPut
		body = map[string]string{"state": state, "note": note}
	}
	var response map[string]any
	if err := client.request(context.Background(), method, path, body, &response); err != nil {
		return err
	}
	return printJSON(response)
}

func runMessage(args []string) error {
	if len(args) > 0 && isHelpCommand(args[0]) {
		printMessageHelp()
		return nil
	}
	if len(args) == 0 {
		return errors.New("message requires a subcommand")
	}
	switch args[0] {
	case "send":
		return runMessageSend(args[1:])
	case "show":
		return runMessageShow(args[1:])
	default:
		return fmt.Errorf("unknown message subcommand %q", args[0])
	}
}

func runMessageSend(args []string) error {
	options, err := parseMessageServerArgs(args, "send")
	if err != nil {
		return err
	}
	if options.Text == "-" {
		text, readErr := readMessageTextFromStdin()
		if readErr != nil {
			return readErr
		}
		options.Text = text
	}
	warnLiteralEscapeSequences(options.Text)
	senderID, senderInstanceID, err := resolveMessageSender()
	if err != nil {
		return err
	}
	client, _, err := newResourceServerClient(options.ServerURL)
	if err != nil {
		return err
	}
	if !isStableResourceTarget(options.ID) {
		return runUserMessageSend(client, options, senderID, senderInstanceID)
	}
	body := map[string]any{
		"text": options.Text, "mode": options.Mode, "role": "agent",
		"sender":                    map[string]string{"id": senderID, "name": senderID},
		"senderWorkspaceInstanceId": senderInstanceID,
	}
	if options.SubscribeResult != nil {
		body["subscribeResult"] = *options.SubscribeResult
	}
	var response map[string]any
	path := fmt.Sprintf("/api/workspaces/%s/resources/%s/messages", url.PathEscape(client.workspaceID), url.PathEscape(options.ID))
	if err := client.request(context.Background(), http.MethodPost, path, body, &response); err != nil {
		return err
	}
	return printJSON(response)
}

// readMessageTextFromStdin reads a multi-line message body from standard
// input when the positional message argument is "-".
func readMessageTextFromStdin() (string, error) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("failed to read message from stdin: %w", err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return "", errors.New("message read from stdin is empty")
	}
	return text, nil
}

// warnLiteralEscapeSequences hints when the message looks like it carries
// shell-quoted escape sequences (a literal \n) instead of real newlines,
// which would render verbatim for the receiving Agent and in the Web GUI.
// The message is still sent unchanged.
func warnLiteralEscapeSequences(text string) {
	if text == "" || strings.Contains(text, "\n") || !strings.Contains(text, `\n`) {
		return
	}
	fmt.Fprintln(os.Stderr, `pua: warning: the message contains literal \n sequences but no real newline; message text is sent verbatim. Use $'...\n...' quoting or pass - to read a multi-line message from stdin.`)
}

// isStableResourceTarget mirrors the Server's stable resource id vocabulary
// (workspace, scheduler, projectN, projectN.taskM). Any other --to value is a
// user name; reserved lookalike names are rejected at user registration, so
// the two namespaces cannot collide.
func isStableResourceTarget(value string) bool {
	value = strings.TrimSpace(value)
	if value == "workspace" || value == app.SchedulerResourceID {
		return true
	}
	parts := strings.Split(value, ".")
	if len(parts) < 1 || len(parts) > 2 || !numberedResourcePart(parts[0], "project") {
		return false
	}
	return len(parts) == 1 || numberedResourcePart(parts[1], "task")
}

func numberedResourcePart(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	digits := strings.TrimPrefix(value, prefix)
	if digits == "" {
		return false
	}
	for _, character := range digits {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

// runUserMessageSend delivers an agent-to-user message into the target user's
// durable inbox. Mode and result subscription are resource mailbox concepts
// and do not apply to user delivery.
func runUserMessageSend(client *resourceServerClient, options resourceServerOptions, senderID, senderInstanceID string) error {
	userName := options.ID
	if err := app.ValidateUserName(userName); err != nil {
		return fmt.Errorf("invalid user target %q: %w", userName, err)
	}
	if options.ModeSet {
		return errors.New("--mode applies only to resource targets")
	}
	if options.SubscribeResult != nil {
		return errors.New("--subscribe-result applies only to resource targets")
	}
	body := map[string]any{
		"text":                      options.Text,
		"sender":                    map[string]string{"id": senderID, "name": senderID},
		"senderWorkspaceInstanceId": senderInstanceID,
	}
	var response map[string]any
	path := fmt.Sprintf("/api/workspaces/%s/users/%s/messages", url.PathEscape(client.workspaceID), url.PathEscape(userName))
	if err := client.request(context.Background(), http.MethodPost, path, body, &response); err != nil {
		return err
	}
	return printJSON(response)
}

func resolveMessageSender() (string, string, error) {
	root := strings.TrimSpace(os.Getenv(puaWorkspaceRootEnvironment))
	instanceID := strings.TrimSpace(os.Getenv(puaWorkspaceInstanceEnvironment))
	resourceID := strings.TrimSpace(os.Getenv(puaResourceIDEnvironment))
	if root != "" || instanceID != "" || resourceID != "" {
		currentWorkspace, currentErr := openApplicationWorkspace()
		contextMatchesCurrent := currentErr == nil && sameWorkspacePath(root, currentWorkspace.Root())
		if !contextMatchesCurrent && currentErr == nil {
			// A parent AgentHub environment can remain in the process while a
			// user changes into another temporary Workspace. The current stable
			// resource address is the unambiguous CLI provenance in that case.
			root, instanceID, resourceID = "", "", ""
		}
	}
	if root != "" || instanceID != "" || resourceID != "" {
		if root == "" || instanceID == "" || resourceID == "" {
			return "", "", fmt.Errorf("resource message sender requires %s, %s, and %s", puaWorkspaceRootEnvironment, puaWorkspaceInstanceEnvironment, puaResourceIDEnvironment)
		}
		workspace, err := app.OpenWorkspace(root)
		if err != nil {
			return "", "", fmt.Errorf("validate message sender Workspace: %w", err)
		}
		runtimeConfig, err := workspace.RuntimeConfig()
		if err != nil {
			return "", "", fmt.Errorf("validate message sender Workspace runtime: %w", err)
		}
		if runtimeConfig.InstanceID != instanceID {
			return "", "", errors.New("message sender Workspace instance does not match its persisted PUA runtime")
		}
		if resourceID != "workspace" && resourceID != app.SchedulerResourceID {
			resource, resourceErr := workspace.ResourceValue(resourceID)
			if resourceErr != nil {
				return "", "", fmt.Errorf("validate message sender resource: %w", resourceErr)
			}
			if resource.Archived {
				return "", "", errors.New("message sender resource is archived")
			}
		}
		return resourceID, instanceID, nil
	}
	senderID, err := inferCurrentResourceID()
	if err != nil {
		return "", "", err
	}
	workspace, err := openApplicationWorkspace()
	if err != nil {
		return "", "", err
	}
	runtime, err := workspace.RuntimeConfig()
	if err != nil {
		return "", "", err
	}
	return senderID, runtime.InstanceID, nil
}

func sameWorkspacePath(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}
	if resolved, err := filepath.EvalSymlinks(leftAbs); err == nil {
		leftAbs = resolved
	}
	if resolved, err := filepath.EvalSymlinks(rightAbs); err == nil {
		rightAbs = resolved
	}
	return filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}

func runMessageShow(args []string) error {
	options, err := parseMessageServerArgs(args, "show")
	if err != nil {
		return err
	}
	client, _, err := newResourceServerClient(options.ServerURL)
	if err != nil {
		return err
	}
	var response map[string]any
	path := fmt.Sprintf("/api/workspaces/%s/messages/%s", url.PathEscape(client.workspaceID), url.PathEscape(options.ID))
	if err := client.request(context.Background(), http.MethodGet, path, nil, &response); err != nil {
		return err
	}
	return printJSON(response)
}

func parseResourceHistoryArgs(args []string, usage string) ([]string, resourceHistoryOptions, error) {
	remaining := make([]string, 0, len(args))
	var options resourceHistoryOptions
	for index := 0; index < len(args); index++ {
		arg := args[index]
		readValue := func(name string) (string, bool) {
			prefix := "--" + name + "="
			if strings.HasPrefix(arg, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(arg, prefix)), true
			}
			if arg == "--"+name && index+1 < len(args) && !strings.HasPrefix(args[index+1], "--") {
				index++
				return strings.TrimSpace(args[index]), true
			}
			return "", false
		}
		if value, ok := readValue("cursor"); ok {
			if value == "" || options.Cursor != "" {
				return nil, resourceHistoryOptions{}, errors.New(usage)
			}
			options.Cursor = value
			continue
		}
		if value, ok := readValue("limit"); ok {
			limit, err := strconv.Atoi(value)
			if err != nil || limit <= 0 || limit > 100 || options.Limit != 0 {
				return nil, resourceHistoryOptions{}, errors.New("history limit must be between 1 and 100")
			}
			options.Limit = limit
			continue
		}
		if value, ok := readValue("server"); ok {
			if value == "" || options.ServerURL != "" {
				return nil, resourceHistoryOptions{}, errors.New(usage)
			}
			options.ServerURL = value
			continue
		}
		if arg == "--json" {
			if options.JSON {
				return nil, resourceHistoryOptions{}, errors.New(usage)
			}
			options.JSON = true
			continue
		}
		if strings.HasPrefix(arg, "--cursor") || strings.HasPrefix(arg, "--limit") || strings.HasPrefix(arg, "--server") {
			return nil, resourceHistoryOptions{}, errors.New(usage)
		}
		remaining = append(remaining, arg)
	}
	return remaining, options, nil
}

func runResourceHistory(resourceID string, options resourceHistoryOptions) error {
	client, _, err := newResourceServerClient(options.ServerURL)
	if err != nil {
		return err
	}
	query := make(url.Values)
	if options.Cursor != "" {
		query.Set("cursor", options.Cursor)
	}
	if options.Limit > 0 {
		query.Set("limit", strconv.Itoa(options.Limit))
	}
	path := fmt.Sprintf("/api/workspaces/%s/resources/%s/history/turns", url.PathEscape(client.workspaceID), url.PathEscape(resourceID))
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var response map[string]any
	if err := client.request(context.Background(), http.MethodGet, path, nil, &response); err != nil {
		return err
	}
	if options.JSON {
		return printJSON(response)
	}
	return printResourceHistoryText(response)
}

func runWorkspaceHistory(args []string) error {
	remaining, options, err := parseResourceHistoryArgs(args, workspaceHistoryUsage)
	if err != nil || len(remaining) != 0 {
		if err != nil {
			return err
		}
		return errors.New(workspaceHistoryUsage)
	}
	return runResourceHistory("workspace", options)
}

func runProjectHistory(args []string) error {
	remaining, options, err := parseResourceHistoryArgs(args, projectHistoryUsage)
	if err != nil {
		return err
	}
	projectID, err := resolveProjectArg(remaining, "history")
	if err != nil {
		return err
	}
	return runResourceHistory(projectID, options)
}

func runTaskHistory(args []string) error {
	remaining, options, err := parseResourceHistoryArgs(args, taskHistoryUsage)
	if err != nil {
		return err
	}
	taskID, err := resolveTaskArg(remaining, "history")
	if err != nil {
		return err
	}
	return runResourceHistory(taskID, options)
}

func parseHistoryShowArgs(args []string) (resourceHistoryOptions, error) {
	var options resourceHistoryOptions
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case strings.HasPrefix(arg, "--ref="):
			if options.Reference != "" {
				return resourceHistoryOptions{}, errors.New(historyShowUsage)
			}
			options.Reference = strings.TrimSpace(strings.TrimPrefix(arg, "--ref="))
		case arg == "--ref":
			if options.Reference != "" || index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
				return resourceHistoryOptions{}, errors.New(historyShowUsage)
			}
			index++
			options.Reference = strings.TrimSpace(args[index])
		case strings.HasPrefix(arg, "--server="):
			if options.ServerURL != "" {
				return resourceHistoryOptions{}, errors.New(historyShowUsage)
			}
			options.ServerURL = strings.TrimSpace(strings.TrimPrefix(arg, "--server="))
		case arg == "--server":
			if options.ServerURL != "" || index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
				return resourceHistoryOptions{}, errors.New(historyShowUsage)
			}
			index++
			options.ServerURL = strings.TrimSpace(args[index])
		case arg == "--json":
			if options.JSON {
				return resourceHistoryOptions{}, errors.New(historyShowUsage)
			}
			options.JSON = true
		default:
			return resourceHistoryOptions{}, errors.New(historyShowUsage)
		}
	}
	if options.Reference == "" {
		return resourceHistoryOptions{}, errors.New(historyShowUsage)
	}
	return options, nil
}

func runHistory(args []string) error {
	if len(args) > 0 && isHelpCommand(args[0]) {
		printHistoryHelp()
		return nil
	}
	if len(args) < 2 || (args[0] != "turn" && args[0] != "event") || args[1] != "show" {
		return errors.New(historyShowUsage)
	}
	options, err := parseHistoryShowArgs(args[2:])
	if err != nil {
		return err
	}
	data, err := base64.RawURLEncoding.DecodeString(options.Reference)
	if err != nil {
		return errors.New("history reference is malformed")
	}
	var reference struct {
		ResourceID string `json:"r"`
	}
	if err := json.Unmarshal(data, &reference); err != nil || strings.TrimSpace(reference.ResourceID) == "" {
		return errors.New("history reference is malformed")
	}
	client, _, err := newResourceServerClient(options.ServerURL)
	if err != nil {
		return err
	}
	plural := "turns"
	if args[0] == "event" {
		plural = "events"
	}
	path := fmt.Sprintf("/api/workspaces/%s/resources/%s/history/%s/%s",
		url.PathEscape(client.workspaceID), url.PathEscape(reference.ResourceID), plural, url.PathEscape(options.Reference))
	var response map[string]any
	if err := client.request(context.Background(), http.MethodGet, path, nil, &response); err != nil {
		return err
	}
	if options.JSON {
		return printJSON(response)
	}
	return printHistoryDetailText(args[0], response)
}
