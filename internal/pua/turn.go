package pua

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const turnUsage = "usage: pua turn status | pua turn <yield|notify|finish|cancel-resume> --work=<opener-work-id> [--message=<queued-id>] [--checkpoint=<text>] [--text=<intent>] [--reason=completed|cancelled]"

func runTurn(args []string) error {
	if len(args) == 0 || os.Getenv("PUA_CHANNEL") == "dialogue" {
		return errors.New(turnUsage)
	}
	resource := strings.TrimSpace(os.Getenv(puaResourceIDEnvironment))
	if resource == "" {
		return errors.New("turn commands require a resource execution context")
	}
	body := map[string]string{"action": args[0]}
	for _, arg := range args[1:] {
		key, value, ok := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if !ok || !strings.HasPrefix(arg, "--") {
			return errors.New(turnUsage)
		}
		switch key {
		case "work":
			key = "workId"
		case "message":
			key = "messageId"
		case "checkpoint", "text", "reason":
		default:
			return errors.New(turnUsage)
		}
		if _, exists := body[key]; exists {
			return errors.New(turnUsage)
		}
		body[key] = value
	}
	method := http.MethodPost
	switch args[0] {
	case "status", "chat":
		method = http.MethodGet
	case "yield", "notify", "finish", "cancel-resume":
		if body["workId"] == "" {
			return errors.New(turnUsage)
		}
	default:
		return errors.New(turnUsage)
	}
	client, _, err := newResourceServerClient("")
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/workspaces/%s/resources/%s/turn-work", url.PathEscape(client.workspaceID), url.PathEscape(resource))
	if args[0] == "chat" {
		if body["workId"] == "" {
			return errors.New("chat history requires --work=<opener-work-id>")
		}
		path += "?chat=true&work=" + url.QueryEscape(body["workId"])
	}
	var response any
	var payload any = body
	if method == http.MethodGet {
		payload = nil
	}
	if err := client.request(context.Background(), method, path, payload, &response); err != nil {
		return err
	}
	return printJSON(response)
}
