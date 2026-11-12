package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/disksing/pua/agenthub/app"
	"github.com/disksing/pua/agenthub/internal/client"
	"github.com/disksing/pua/agenthub/internal/session"
	"github.com/disksing/pua/internal/buildinfo"
)

func version() string {
	return buildinfo.Current("agenthub").Version
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "agenthub:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(helpOutput, rootHelp)
		return nil
	}
	if isHelpFlag(args[0]) {
		fmt.Fprint(helpOutput, rootHelp)
		return nil
	}
	switch args[0] {
	case "help":
		return runHelp(args[1:])
	case "serve":
		return runServe(args[1:])
	case "status":
		return runStatus(args[1:])
	case "agents":
		return runAgents(args[1:])
	case "run":
		return runOneShot(args[1:])
	case "chat":
		return runChat(args[1:])
	case "session":
		return runSession(args[1:])
	case "version":
		if hasHelpFlag(args[1:]) {
			printTopic("version")
			return nil
		}
		if len(args) == 2 && args[1] == "--json" {
			data, err := buildinfo.JSON("agenthub")
			if err != nil {
				return err
			}
			fmt.Printf("%s\n", data)
			return nil
		}
		if len(args) != 1 {
			return errors.New("usage: agenthub version [--json]")
		}
		fmt.Print(buildinfo.Text("agenthub"))
		return nil
	case "--version", "-version":
		fmt.Print(buildinfo.Text("agenthub"))
		return nil
	case "--version-json":
		data, err := buildinfo.JSON("agenthub")
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", data)
		return nil
	default:
		return fmt.Errorf("unknown command %q\nRun 'agenthub help' for usage.", args[0])
	}
}

// stringListFlag collects the values of a repeatable string flag.
type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ", ") }

func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	address := flags.String("addr", app.DefaultListenAddress, "listen address as host:port; default "+app.DefaultListenAddress+" (loopback only); IPv6 needs brackets, e.g. [::1]:4646")
	webDir := flags.String("web-dir", "", "built Web UI directory (overrides the embedded UI)")
	var allowedOrigins stringListFlag
	flags.Var(&allowedOrigins, "allow-origin", "trusted browser origin (scheme://host[:port]) for mutating requests through a reverse proxy; repeatable")
	if err := flags.Parse(args); err != nil {
		return flagParseError(err, "serve")
	}
	if flags.NArg() != 0 {
		return usageError("agenthub serve [--addr host:port] [--web-dir path] [--allow-origin origin]...", "serve")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.Serve(ctx, app.Options{
		Address: *address, Version: version(), WebDir: *webDir, AllowedOrigins: allowedOrigins,
	})
}

func runStatus(args []string) error {
	if hasHelpFlag(args) {
		printTopic("status")
		return nil
	}
	if len(args) != 0 {
		return usageError("agenthub status", "status")
	}
	apiClient, err := client.Discover()
	if err != nil {
		return err
	}
	status, err := apiClient.Status()
	if err != nil {
		return err
	}
	return printJSON(status)
}

func runAgents(args []string) error {
	if hasHelpFlag(args) {
		printTopic("agents")
		return nil
	}
	if len(args) != 0 {
		return usageError("agenthub agents", "agents")
	}
	apiClient, err := client.Discover()
	if err != nil {
		return err
	}
	value, err := apiClient.Agents()
	if err != nil {
		return err
	}
	return printJSON(value)
}

func runOneShot(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	cwd := flags.String("cwd", ".", "working directory")
	title := flags.String("title", "", "session title")
	agentName := flags.String("agent", "", "agent name from the configuration (required)")
	if err := flags.Parse(args); err != nil {
		return flagParseError(err, "run")
	}
	message := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if message == "" {
		return usageError("agenthub run [--cwd dir] [--title title] --agent name <message>", "run")
	}
	if strings.TrimSpace(*agentName) == "" {
		return errors.New("--agent is required: sessions always run with an explicit agent")
	}
	absolute, err := filepath.Abs(*cwd)
	if err != nil {
		return err
	}
	apiClient, err := client.Discover()
	if err != nil {
		return err
	}
	value, err := apiClient.CreateSessionWithMessage(*title, absolute, *agentName, message)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "session %s (%s)\n", value.ID, value.AgentName)
	return printUntilTurnEnds(apiClient, value.ID, 0)
}

func runChat(args []string) error {
	flags := flag.NewFlagSet("chat", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	cwd := flags.String("cwd", ".", "working directory")
	title := flags.String("title", "", "session title")
	agentName := flags.String("agent", "", "agent name from the configuration (required when creating a session)")
	sessionID := flags.String("session", "", "attach existing session")
	if err := flags.Parse(args); err != nil {
		return flagParseError(err, "chat")
	}
	if flags.NArg() != 0 {
		return usageError("agenthub chat [--session id | --cwd dir --title title --agent name]", "chat")
	}
	// Validate usage before touching the daemon so argument errors fail
	// fast, even with no daemon running.
	if strings.TrimSpace(*sessionID) == "" && strings.TrimSpace(*agentName) == "" {
		return errors.New("--agent is required when creating a session")
	}
	apiClient, err := client.Discover()
	if err != nil {
		return err
	}
	id := *sessionID
	if id == "" {
		absolute, err := filepath.Abs(*cwd)
		if err != nil {
			return err
		}
		value, err := apiClient.CreateSession(*title, absolute, *agentName)
		if err != nil {
			return err
		}
		id = value.ID
	}
	fmt.Fprintf(os.Stderr, "attached %s; /quit exits, /stop stops provider, /interrupt cancels turn\n", id)
	reader := bufio.NewScanner(os.Stdin)
	attached, err := apiClient.GetSession(id)
	if err != nil {
		return err
	}
	cursor := attached.LastEventID
	for {
		fmt.Fprint(os.Stderr, "> ")
		if !reader.Scan() {
			return reader.Err()
		}
		text := strings.TrimSpace(reader.Text())
		switch text {
		case "":
			continue
		case "/quit", "/exit":
			return nil
		case "/stop":
			_, err := apiClient.SessionAction(id, "stop")
			return err
		case "/interrupt":
			_, err := apiClient.SessionAction(id, "interrupt")
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
			continue
		}
		if _, err := apiClient.SendMessage(id, text, false); err != nil {
			fmt.Fprintln(os.Stderr, err)
			continue
		}
		cursor, err = printTurn(apiClient, id, cursor)
		if err != nil {
			return err
		}
	}
}

func printUntilTurnEnds(apiClient *client.Client, id string, cursor int64) error {
	_, err := printTurn(apiClient, id, cursor)
	return err
}

func printTurn(apiClient *client.Client, id string, cursor int64) (int64, error) {
	for {
		frames, err := apiClient.EventsAfter(id, cursor)
		if err != nil {
			return cursor, err
		}
		for _, frame := range frames {
			cursor = frame.Cursor
			for _, event := range frame.Events {
				switch event.Type {
				case "message.assistant.delta":
					var data struct {
						Text string `json:"text"`
					}
					encoded, _ := json.Marshal(event.Data)
					_ = json.Unmarshal(encoded, &data)
					fmt.Print(data.Text)
				case "approval.requested":
					fmt.Fprintln(os.Stderr, "\napproval required; use the Web UI or approval API")
				case "provider.error":
					var data map[string]any
					encoded, _ := json.Marshal(event.Data)
					_ = json.Unmarshal(encoded, &data)
					if message, _ := data["message"].(string); message != "" {
						fmt.Fprintln(os.Stderr, "\nprovider:", message)
					}
				case "turn.completed":
					fmt.Println()
					return cursor, nil
				case "turn.failed", "turn.cancelled":
					return cursor, fmt.Errorf("turn ended with %s", event.Type)
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func runSession(args []string) error {
	if len(args) == 0 || isHelpFlag(args[0]) {
		printTopic("session")
		return nil
	}
	if args[0] == "help" {
		if len(args) == 1 {
			printTopic("session")
			return nil
		}
		return runHelp(append([]string{"session"}, args[1:]...))
	}
	if len(args) == 2 && isHelpFlag(args[1]) {
		if _, ok := helpTopics["session "+args[0]]; ok {
			printTopic("session " + args[0])
			return nil
		}
	}
	switch args[0] {
	case "create":
		return runSessionCreate(args[1:])
	case "list":
		return runSessionList(args[1:])
	case "show":
		return runSessionShow(args[1:])
	case "attach":
		if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
			return usageError("agenthub session attach <session-id>", "session attach")
		}
		return runChat([]string{"--session", args[1]})
	case "events":
		if len(args) != 2 {
			return usageError("agenthub session events <session-id>", "session events")
		}
		apiClient, err := client.Discover()
		if err != nil {
			return err
		}
		frames, err := apiClient.EventsAfter(args[1], 0)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"schema": "agenthub.semantic-events.v1", "frames": frames})
	case "approve":
		return runSessionApprove(args[1:])
	case "archive":
		if len(args) != 2 {
			return usageError("agenthub session archive <session-id>", "session archive")
		}
		apiClient, err := client.Discover()
		if err != nil {
			return err
		}
		value, err := apiClient.ArchiveSession(args[1])
		if err != nil {
			return err
		}
		return printJSON(value)
	case "resume", "stop", "interrupt":
		if len(args) != 2 {
			return usageError(fmt.Sprintf("agenthub session %s <session-id>", args[0]), "session "+args[0])
		}
		apiClient, err := client.Discover()
		if err != nil {
			return err
		}
		value, err := apiClient.SessionAction(args[1], args[0])
		if err != nil {
			return err
		}
		return printJSON(value)
	default:
		return fmt.Errorf("unknown session command %q\nRun 'agenthub help session' for usage.", args[0])
	}
}

func runSessionApprove(args []string) error {
	flags := flag.NewFlagSet("session approve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	decision := flags.String("decision", "accept", "accept, acceptForSession, decline, or cancel")
	if err := flags.Parse(args); err != nil {
		return flagParseError(err, "session approve")
	}
	if flags.NArg() != 2 {
		return usageError("agenthub session approve [--decision decision] <session-id> <approval-id>", "session approve")
	}
	switch *decision {
	case "accept", "acceptForSession", "decline", "cancel":
	default:
		return fmt.Errorf("invalid decision %q", *decision)
	}
	apiClient, err := client.Discover()
	if err != nil {
		return err
	}
	value, err := apiClient.ResolveApproval(flags.Arg(0), flags.Arg(1), *decision)
	if err != nil {
		return err
	}
	return printJSON(value)
}

func runSessionCreate(args []string) error {
	flags := flag.NewFlagSet("session create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	title := flags.String("title", "", "session title")
	cwd := flags.String("cwd", ".", "working directory")
	agentName := flags.String("agent", "", "agent name from the configuration (required)")
	if err := flags.Parse(args); err != nil {
		return flagParseError(err, "session create")
	}
	if flags.NArg() != 0 {
		return usageError("agenthub session create [--cwd dir] [--title title] --agent name", "session create")
	}
	if strings.TrimSpace(*agentName) == "" {
		return errors.New("--agent is required: sessions always run with an explicit agent")
	}
	absoluteCwd, err := filepath.Abs(*cwd)
	if err != nil {
		return err
	}
	apiClient, err := client.Discover()
	if err != nil {
		return err
	}
	value, err := apiClient.CreateSession(*title, absoluteCwd, *agentName)
	if err != nil {
		return err
	}
	return printJSON(value)
}

func runSessionList(args []string) error {
	flags := flag.NewFlagSet("session list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	includeArchived := flags.Bool("all", false, "include archived sessions")
	archivedOnly := flags.Bool("archived", false, "list only archived sessions")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return flagParseError(err, "session list")
	}
	if flags.NArg() != 0 {
		return usageError("agenthub session list [--all] [--archived] [--json]", "session list")
	}
	if *includeArchived && *archivedOnly {
		return fmt.Errorf("--all and --archived cannot be combined\nRun 'agenthub help session list' for usage.")
	}
	apiClient, err := client.Discover()
	if err != nil {
		return err
	}
	var values []session.Session
	if *archivedOnly {
		values, err = apiClient.ListArchivedSessions()
	} else {
		values, err = apiClient.ListSessions(*includeArchived)
	}
	if err != nil {
		return err
	}
	if *jsonOutput {
		return printJSON(map[string]any{"sessions": values})
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tSTATE\tSTOP REASON\tAGENT\tTITLE\tUPDATED")
	for _, value := range values {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
			value.ID,
			value.State,
			value.StopReason,
			value.AgentName,
			value.Title,
			value.UpdatedAt.Local().Format(time.RFC3339),
		)
	}
	return writer.Flush()
}

func runSessionShow(args []string) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return usageError("agenthub session show <session-id>", "session show")
	}
	apiClient, err := client.Discover()
	if err != nil {
		return err
	}
	value, err := apiClient.GetSession(args[0])
	if err != nil {
		return err
	}
	return printJSON(value)
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
