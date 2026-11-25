package pua

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const cardHelp = `Workspace cards (run from the publishing resource):
  pua card list --user NAME
  pua card create --user NAME --kind list --source running --title "Running resources"
  echo '{"items":[{"label":"Review report","resource":"project1.task1"}]}' | pua card create --user NAME --kind list --source agent --title "Review"
  echo '{"items":[]}' | pua card update --user NAME --id CARD_ID --version CURRENT_VERSION

System sources: stat/trend/list: running, running-tasks, tasks; gauge: task-ratio; pie: task-states; quota: magpie-quota.
Agent content: stat {"value":4}; gauge {"value":4,"max":10}; pie {"items":[{"label":"Open","value":4}]};
trend {"points":[{"time":"2026-01-01T00:00:00Z","value":4}]}; list {"items":[{"label":"Report","url":"https://example.org"}]};
html {"html":"<h3>Milestones</h3><ul><li>Ready</li></ul>"} (close tags; optional color/background-color styles with six-digit hex colors).
Create returns the card ID. Use list to read the current version before updating. System cards refresh automatically.
The CLI keeps the private publishing key for this resource locally. Stop if the card was removed.`

func runCard(args []string) error {
	if len(args) == 0 || isHelpCommand(args[0]) || len(args) > 1 && isHelpCommand(args[1]) {
		fmt.Println(cardHelp)
		return nil
	}
	action := args[0]
	flags := flag.NewFlagSet("card "+action, flag.ContinueOnError)
	user := flags.String("user", "", "Workspace user")
	serverURL := flags.String("server", "", "PUA Server")
	id := flags.String("id", "", "card ID")
	version := flags.Int("version", 0, "current card version")
	kind := flags.String("kind", "", "card style")
	source := flags.String("source", "", "data source")
	title := flags.String("title", "", "card title")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *user == "" || flags.NArg() != 0 {
		return errors.New(cardHelp)
	}
	client, root, err := newResourceServerClient(*serverURL)
	if err != nil {
		return err
	}
	client.userName = *user
	path := "/api/workspaces/" + client.workspaceID + "/cards"
	ctx := context.Background()
	var output json.RawMessage
	if action == "list" {
		if err := client.request(ctx, "GET", path, nil, &output); err != nil {
			return err
		}
		return printJSON(output)
	}
	body := map[string]any{"action": action}
	if action == "create" {
		body["kind"], body["source"], body["title"] = *kind, *source, *title
	} else if action == "update" {
		decoded, err := hex.DecodeString(*id)
		if err != nil || len(decoded) != sha256.Size || *version < 1 {
			return errors.New("update requires an agent card ID and its current version")
		}
		body["id"], body["version"] = *id, *version
	} else {
		return errors.New(cardHelp)
	}
	if action == "update" || *source == "agent" {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 128<<10+1))
		if err != nil {
			return err
		}
		if len(data) > 128<<10 || !json.Valid(data) {
			return errors.New("provide one JSON content object on stdin (128 KiB maximum)")
		}
		body["content"] = json.RawMessage(data)
		resource, err := inferCurrentResourceID()
		if err != nil {
			return err
		}
		configDir, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		namespace := sha256.Sum256([]byte(root + "\x00" + resource + "\x00" + *user))
		dir := filepath.Join(configDir, "pua", "card-publishers", hex.EncodeToString(namespace[:]))
		if action == "create" {
			if err := os.MkdirAll(dir, 0700); err != nil {
				return err
			}
			key := rand.Text()
			sum := sha256.Sum256([]byte(key))
			if err := os.WriteFile(filepath.Join(dir, hex.EncodeToString(sum[:])), []byte(key), 0600); err != nil {
				return err
			}
			body["key"] = key
		} else {
			key, err := os.ReadFile(filepath.Join(dir, *id))
			if err != nil {
				return errors.New("no publisher key for this resource, user and card")
			}
			body["key"] = string(key)
		}
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := client.request(ctx, "POST", path, body, &result); err != nil {
		return err
	}
	return printJSON(result)
}
