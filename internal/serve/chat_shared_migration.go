package serve

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/workspacepath"
)

// Original per-user records stay in place and are also copied to a protected
// backup before a shared record is committed. A restart can repeat migration.
func backupLegacyChat(root, id string) error {
	data, err := os.ReadFile(chatPath(root, id))
	if err != nil {
		return err
	}
	dir := filepath.Join(workspacepath.ControlDir(root), "chats-legacy-backup")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, id+".json")
	existing, err := os.ReadFile(path)
	if err == nil {
		if !bytes.Equal(existing, data) {
			return fmt.Errorf("legacy chat backup differs for %s", id)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	file, err := os.CreateTemp(dir, ".chat-backup-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func legacyMessageTime(message chatMessage) time.Time {
	result, _ := time.Parse(time.RFC3339Nano, message.CreatedAt)
	return result
}

func migrateSharedChatLocked(root, resourceID string) (resourceChat, error) {
	resourceID = normalizedResourceID(resourceID)
	shared := resourceChat{
		Version: 4, ID: sharedChatID(resourceID), ResourceID: resourceID,
		Messages: []chatMessage{}, ReadCursors: map[string]int{}, LegacyUnread: map[string][]string{},
		LegacySources: map[string]legacyChatSource{},
		Binding:       app.AgentBinding{Kind: "profile", Name: "default"},
	}
	paths, err := filepath.Glob(filepath.Join(workspacepath.ControlDir(root), "chats", "*.json"))
	if err != nil {
		return shared, err
	}
	sort.Strings(paths)
	var old []resourceChat
	for _, path := range paths {
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		if id == shared.ID {
			continue
		}
		c, loadErr := loadChatLocked(root, id)
		if loadErr != nil {
			return shared, loadErr
		}
		if c.Version >= 4 || normalizedResourceID(c.ResourceID) != resourceID {
			continue
		}
		if err = backupLegacyChat(root, id); err != nil {
			return shared, err
		}
		if err = migrateChatDataLocked(root, &c); err != nil {
			return shared, err
		}
		old = append(old, c)
	}
	ws, err := app.OpenWorkspace(root)
	if err != nil {
		return shared, err
	}
	var maxEpoch uint64
	for _, c := range old {
		_, userErr := ws.User(c.User)
		active := c.Open && userErr == nil
		source := legacyChatSource{User: c.User, Epoch: c.Epoch, Open: active, MessageIDs: make([]string, len(c.Messages))}
		if active {
			source.Token = c.Token
			shared.Open = true
		}
		shared.LegacySources[c.ID] = source
		maxEpoch = max(maxEpoch, c.Epoch)
	}

	// Merge by timestamp while retaining each original Chat's exact order.
	offsets := make([]int, len(old))
	used := map[string]bool{}
	for {
		next := -1
		for i := range old {
			if offsets[i] >= len(old[i].Messages) {
				continue
			}
			if next < 0 || legacyMessageTime(old[i].Messages[offsets[i]]).Before(legacyMessageTime(old[next].Messages[offsets[next]])) {
				next = i
			}
		}
		if next < 0 {
			break
		}
		c, index := old[next], offsets[next]
		message := c.Messages[index]
		message.LegacyChatID, message.LegacyID, message.LegacyIndex = c.ID, message.ID, index
		if message.Role == "user" {
			message.Sender, message.ClientID = c.User, message.ID
		}
		if used[message.ID] {
			base := "legacy-" + chatID(c.ID, fmt.Sprintf("%d:%s", index, message.ID))
			message.ID = base
			for suffix := 1; used[message.ID]; suffix++ {
				message.ID = fmt.Sprintf("%s-%d", base, suffix)
			}
		}
		used[message.ID] = true
		source := shared.LegacySources[c.ID]
		source.MessageIDs[index] = message.ID
		shared.LegacySources[c.ID] = source
		if index >= c.ReadThrough && message.Role == "agent" {
			shared.LegacyUnread[c.User] = append(shared.LegacyUnread[c.User], message.ID)
		}
		shared.Messages = append(shared.Messages, message)
		offsets[next]++
	}
	shared.MigrationEnd = len(shared.Messages)
	for _, c := range old {
		shared.ReadCursors[c.User] = shared.MigrationEnd
	}
	shared.Epoch = maxEpoch
	if shared.Open {
		shared.Epoch++
		start := shared.MigrationEnd
		shared.EpochStart = &start
		shared.Token = newGenerationRecordID()
	}
	for _, c := range old {
		source := shared.LegacySources[c.ID]
		for _, delivery := range c.Outbox {
			if delivery.Accepted || delivery.Target == c.address() {
				continue
			}
			if delivery.userInput() && (!source.Open || delivery.Epoch != c.Epoch) {
				continue
			}
			delivery.SourceChatID = c.ID
			delivery.Epoch = shared.Epoch
			shared.Outbox = append(shared.Outbox, delivery)
		}
	}
	if err = saveChatLocked(root, shared); err != nil {
		return shared, err
	}
	return shared, nil
}

// Migrate only resources already represented by legacy files; listing does not
// manufacture chats for unrelated resources.
func migrateExistingChatsLocked(root string) error {
	paths, err := filepath.Glob(filepath.Join(workspacepath.ControlDir(root), "chats", "*.json"))
	if err != nil {
		return err
	}
	resources := map[string]bool{}
	for _, path := range paths {
		c, loadErr := loadChatLocked(root, strings.TrimSuffix(filepath.Base(path), ".json"))
		if loadErr != nil {
			return loadErr
		}
		if c.Version < 4 {
			resources[c.ResourceID] = true
		}
	}
	for resourceID := range resources {
		if _, err := ensureChatLocked(root, resourceID, ""); err != nil {
			return err
		}
	}
	return nil
}

// Revoke old per-user capabilities before deleting the profile. The current
// shared capability is unaffected because it belongs to the resource epoch.
func revokeLegacyChatUserLocked(root, user string) error {
	if err := migrateExistingChatsLocked(root); err != nil {
		return err
	}
	paths, err := filepath.Glob(filepath.Join(workspacepath.ControlDir(root), "chats", "*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		c, err := loadChatLocked(root, strings.TrimSuffix(filepath.Base(path), ".json"))
		if err != nil {
			return err
		}
		if c.Version < 4 {
			continue
		}
		changed := false
		for id, source := range c.LegacySources {
			if source.User == user && (source.Open || source.Token != "") {
				source.Open, source.Token = false, ""
				c.LegacySources[id] = source
				changed = true
			}
		}
		if _, found := c.ReadCursors[user]; found {
			delete(c.ReadCursors, user)
			changed = true
		}
		if _, found := c.LegacyUnread[user]; found {
			delete(c.LegacyUnread, user)
			changed = true
		}
		if changed {
			if err := saveChatLocked(root, c); err != nil {
				return err
			}
		}
	}
	return nil
}
