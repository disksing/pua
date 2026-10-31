package pua

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const chatUsage = "usage: pua chat <show|notes|publish> --reply-to=<capability> [--id=<stable-id>] [text]"

func runChat(args []string) error {
	if len(args) == 0 {
		return errors.New(chatUsage)
	}
	if os.Getenv("PUA_CHANNEL") == "dialogue" || os.Getenv("PUA_RESOURCE_ID") == "" {
		return errors.New("chat commands require the resource execution Agent and an explicit --reply-to capability")
	}
	var replyTo string
	operation := args[0]
	body := map[string]any{}
	var words []string
	for _, arg := range args[1:] {
		switch {
		case strings.HasPrefix(arg, "--id="):
			body["id"] = strings.TrimPrefix(arg, "--id=")
		case strings.HasPrefix(arg, "--reply-to="):
			replyTo = strings.TrimPrefix(arg, "--reply-to=")
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
	case "publish":
		if body["id"] == nil || strings.TrimSpace(text) == "" {
			return errors.New(chatUsage)
		}
		body["text"] = text

	default:
		return errors.New(chatUsage)
	}
	client, _, err := newResourceServerClient("")
	if err != nil {
		return err
	}
	capability := strings.SplitN(replyTo, ".", 2)
	if len(capability) == 1 && capability[0] != "" {
		client.chatToken = capability[0]
	} else if len(capability) == 2 && capability[0] != "" && capability[1] != "" {
		client.chatID, client.chatToken = capability[0], capability[1]
	} else {
		return errors.New(chatUsage)
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

// Retired dialogue processes may only inspect historical resources. Chat
// publication rejects their old launch context in runChat and on the server.
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
