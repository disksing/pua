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

const turnUsage = `usage: pua turn switch --message=<queued-id>
       pua turn switch --to=chat --context="<what to tell the user>"
       pua turn status
       pua turn finish --reason=completed|cancelled
       pua turn cancel-resume --reason="<reason>"
After switch is accepted, end the current Turn. The server resumes the original work
after the selected Turn completes. Add --checkpoint="<next step>" if needed.
Control commands optionally accept --work=<opener-work-id>. Legacy yield and notify remain available.`

func runTurn(args []string) error {
	if (len(args) == 1 && isHelpCommand(args[0])) ||
		(len(args) == 2 && args[0] == "switch" && isHelpCommand(args[1])) {
		fmt.Println(turnUsage)
		return nil
	}
	if len(args) == 0 || os.Getenv("PUA_CHANNEL") == "dialogue" {
		return errors.New(turnUsage)
	}
	resource := strings.TrimSpace(os.Getenv(puaResourceIDEnvironment))
	if resource == "" {
		return errors.New("turn commands require a resource execution context")
	}
	body := map[string]string{"action": args[0]}
	switching := args[0] == "switch"
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
		case "checkpoint":
		case "text", "reason":
			if switching {
				return errors.New(turnUsage)
			}
		case "context", "to":
			if !switching || strings.TrimSpace(value) == "" {
				return errors.New(turnUsage)
			}
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
	case "switch":
		if message, specified := body["messageId"]; specified {
			if strings.TrimSpace(message) == "" || body["to"] != "" || body["context"] != "" {
				return errors.New(turnUsage)
			}
		} else {
			if body["to"] != "chat" || body["context"] == "" {
				return errors.New(turnUsage)
			}
			body["text"] = body["context"]
			delete(body, "to")
			delete(body, "context")
		}
	case "yield", "notify", "finish", "cancel-resume":
		if args[0] == "yield" && (body["messageId"] == "" || strings.TrimSpace(body["checkpoint"]) == "") {
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
	if method == http.MethodPost && body["workId"] == "" {
		body["workId"], err = inferCurrentWorkID(client, resource)
		if err != nil {
			return err
		}
	}
	if args[0] == "chat" {
		if body["workId"] == "" {
			return errors.New("chat history requires --work=<opener-work-id>")
		}
		path += "?chat=true&work=" + url.QueryEscape(body["workId"])
	}
	if switching {
		var receipt struct {
			Next string `json:"next"`
		}
		if err := client.request(context.Background(), method, path, body, &receipt); err != nil {
			return err
		}
		fmt.Println(receipt.Next)
		return nil
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
