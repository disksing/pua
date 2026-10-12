package pua

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const chatUsage = "usage: pua chat show|notes [text]|publish --id=<stable-id> <text>|dispatch --id=<stable-id> [--to=<resource>] <text>|timer <seconds>"

func runChat(args []string) error {
	if len(args) == 0 {
		return errors.New(chatUsage)
	}
	if os.Getenv("PUA_CHANNEL") != "dialogue" || os.Getenv("PUA_CHAT_ID") == "" {
		return errors.New("chat commands require a dialogue Session")
	}
	operation := args[0]
	body := map[string]any{}
	var words []string
	for _, arg := range args[1:] {
		switch {
		case strings.HasPrefix(arg, "--id="):
			body["id"] = strings.TrimPrefix(arg, "--id=")
		case strings.HasPrefix(arg, "--to="):
			body["target"] = strings.TrimPrefix(arg, "--to=")
		default:
			words = append(words, arg)
		}
	}
	text := strings.Join(words, " ")
	if text == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		text = string(data)
	}
	method := http.MethodPost
	switch operation {
	case "show":
		operation = ""
		method = http.MethodGet
	case "notes":
		if len(words) == 0 {
			method = http.MethodGet
		} else {
			body["text"] = text
		}
	case "publish", "dispatch":
		if body["id"] == nil || strings.TrimSpace(text) == "" {
			return errors.New(chatUsage)
		}
		body["text"] = text
	case "timer":
		n, err := strconv.Atoi(text)
		if err != nil {
			return errors.New(chatUsage)
		}
		body["seconds"] = n
	default:
		return errors.New(chatUsage)
	}
	client, _, err := newResourceServerClient("")
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/workspaces/%s/resources/%s/chat", url.PathEscape(client.workspaceID), url.PathEscape(os.Getenv("PUA_RESOURCE_ID")))
	if operation != "" {
		path += "/" + operation
	}
	var response any
	var payload any = body
	if method == http.MethodGet {
		payload = nil
	}
	if err = client.request(context.Background(), method, path, payload, &response); err != nil {
		return err
	}
	return printJSON(response)
}

// These local CLI commands are observational. Dialogue side effects cross
// the fenced chat API instead of mutating resource files directly.
func dialogueCommandAllowed(args []string) bool {
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "chat", "help", "--help", "-h", "--version", "--version-json", "history":
		return true
	case "workspace", "project", "task", "user", "scheduler", "template", "repo", "message":
		if len(args) < 2 {
			return true
		}
		switch args[1] {
		case "list", "show", "status", "history", "resource", "tree", "help", "--help":
			return true
		}
	}
	return false
}
