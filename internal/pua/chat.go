package pua

import (
	"errors"
	"strings"
)

const chatUsage = "usage: pua chat show --work=<opener-work-id>"

func runChat(args []string) error {
	if len(args) == 2 && args[0] == "show" && strings.HasPrefix(args[1], "--work=") {
		return runTurn([]string{"chat", args[1]})
	}
	return errors.New(chatUsage)
}

// Retired dialogue processes may only inspect historical resources.
func dialogueCommandAllowed(args []string) bool {
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "help", "--help", "-h", "--version", "--version-json", "history":
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
