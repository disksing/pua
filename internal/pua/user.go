package pua

import (
	"errors"
	"fmt"

	"github.com/disksing/pua/internal/app"
)

const userListUsage = "usage: pua user list [--json]"

func runUser(args []string) error {
	if len(args) > 0 && isHelpCommand(args[0]) {
		printUserHelp()
		return nil
	}
	if len(args) == 0 {
		return errors.New("user requires a subcommand")
	}
	if args[0] != "list" {
		return fmt.Errorf("unknown user subcommand %q", args[0])
	}
	jsonOutput := false
	for _, arg := range args[1:] {
		if arg != "--json" || jsonOutput {
			return errors.New(userListUsage)
		}
		jsonOutput = true
	}
	workspace, err := openApplicationWorkspace()
	if err != nil {
		return err
	}
	users, err := workspace.Users()
	if err != nil {
		return err
	}
	if jsonOutput {
		return printJSON(map[string][]app.UserProfile{"users": users})
	}
	for _, user := range users {
		fmt.Printf("%s\t%s\n", user.Name, user.Preference)
	}
	return nil
}

func printUserHelp() {
	fmt.Print(`Usage:
  pua user list [--json]

Commands:
  pua user list [--json]
    List users and their preferences in the current Workspace.
`)
}
