package serve

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func TestResourceMailboxLocatorsOnlyUpdateChangedMessages(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	oldReceiptCount, oldReceiptWindow := resourceMailboxReceiptRetentionCount, resourceMailboxReceiptRetentionWindow
	oldExpiredCount, oldExpiredWindow := resourceMailboxExpiredRetentionCount, resourceMailboxExpiredRetentionWindow
	defer func() {
		resourceMailboxReceiptRetentionCount, resourceMailboxReceiptRetentionWindow = oldReceiptCount, oldReceiptWindow
		resourceMailboxExpiredRetentionCount, resourceMailboxExpiredRetentionWindow = oldExpiredCount, oldExpiredWindow
	}()
	resourceMailboxReceiptRetentionCount, resourceMailboxReceiptRetentionWindow = 2, 0
	resourceMailboxExpiredRetentionCount, resourceMailboxExpiredRetentionWindow = 1, 24*time.Hour

	now := time.Now()
	_, err := mutateResourceMailboxForResource(root, "workspace", func(mailbox *resourceMailbox) error {
		for i := 0; i < 3; i++ {
			stamp := now.Add(time.Duration(i-3) * time.Minute).Format(time.RFC3339Nano)
			mailbox.NextSequence++
			mailbox.Messages = append(mailbox.Messages, resourceMailboxMessage{
				ID: fmt.Sprintf("completed-%d", i), Sequence: mailbox.NextSequence, ResourceID: "workspace",
				Role: "user", Status: resourceMessageDelivered, AcceptedAt: stamp,
				UpdatedAt: stamp, DeliveredAt: stamp, TerminalAt: stamp,
			})
		}
		mailbox.NextSequence++
		mailbox.Messages = append(mailbox.Messages, resourceMailboxMessage{
			ID: "pending", Sequence: mailbox.NextSequence, ResourceID: "workspace",
			Role: "user", Status: resourceMessageQueued, AcceptedAt: now.Format(time.RFC3339Nano),
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	oldTime := time.Unix(946684800, 0)
	for _, id := range []string{"completed-0", "completed-1", "completed-2", "pending"} {
		if err := os.Chtimes(resourceMailboxLocatorPath(root, id), oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	_, err = mutateResourceMailboxForResource(root, "workspace", func(mailbox *resourceMailbox) error {
		mailbox.NextSequence++
		mailbox.Messages = append(mailbox.Messages, resourceMailboxMessage{
			ID: "new-pending", Sequence: mailbox.NextSequence, ResourceID: "workspace",
			Role: "user", Status: resourceMessageQueued, AcceptedAt: now.Format(time.RFC3339Nano),
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"completed-0", "completed-1", "completed-2", "pending"} {
		info, err := os.Stat(resourceMailboxLocatorPath(root, id))
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(oldTime) {
			t.Errorf("unchanged locator %s was rewritten at %s", id, info.ModTime())
		}
	}
	locator, found, err := readResourceMailboxLocator(root, "new-pending")
	if err != nil || !found || locator.State != "hot" {
		t.Fatalf("new locator = %#v, found=%v err=%v", locator, found, err)
	}

	_, err = mutateResourceMailboxForResource(root, "workspace", func(mailbox *resourceMailbox) error {
		for i := range mailbox.Messages {
			if mailbox.Messages[i].ID == "pending" {
				stamp := now.Format(time.RFC3339Nano)
				mailbox.Messages[i].Status = resourceMessageDelivered
				mailbox.Messages[i].UpdatedAt = stamp
				mailbox.Messages[i].DeliveredAt = stamp
				mailbox.Messages[i].TerminalAt = stamp
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		"completed-1": resourceMailboxExpiredState,
		"completed-2": "receipt",
		"pending":     "receipt",
		"new-pending": "hot",
	} {
		locator, found, err := readResourceMailboxLocator(root, id)
		if err != nil || !found || locator.State != want {
			t.Errorf("locator %s = %#v, found=%v err=%v; want state %s", id, locator, found, err, want)
		}
	}
	if _, err := os.Stat(resourceMailboxLocatorPath(root, "completed-0")); !os.IsNotExist(err) {
		t.Errorf("evicted locator still exists: %v", err)
	}
	info, err := os.Stat(resourceMailboxLocatorPath(root, "completed-2"))
	if err != nil || !info.ModTime().Equal(oldTime) {
		t.Errorf("unchanged receipt locator was rewritten: %v, %v", info, err)
	}
	_, found, err = mailboxMessageByID(root, "completed-1")
	var apiErr *resourceAPIError
	if found || !errors.As(err, &apiErr) || apiErr.Code != "message_receipt_expired" {
		t.Errorf("expired receipt lookup = found=%v err=%v", found, err)
	}
}

func TestResourceMailboxLookupValidatesStaleExpiredLocator(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	_, err := mutateResourceMailboxForResource(root, "workspace", func(mailbox *resourceMailbox) error {
		mailbox.NextSequence++
		mailbox.Messages = append(mailbox.Messages, resourceMailboxMessage{
			ID: "live-message", Sequence: mailbox.NextSequence, ResourceID: "workspace",
			Role: "user", Status: resourceMessageQueued, AcceptedAt: time.Now().Format(time.RFC3339Nano),
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"live-message", "forgotten-message"} {
		locator := resourceMailboxLocator{
			Version: resourceMailboxStoreVersion, MessageID: id, ResourceID: "workspace",
			ResourceKey: resourceMailboxKey(mailboxInstanceID(root), "workspace"),
			State:       resourceMailboxExpiredState,
		}
		if err := writeResourceMailboxIndexJSON(resourceMailboxLocatorPath(root, id), locator); err != nil {
			t.Fatal(err)
		}
	}
	message, found, err := mailboxMessageByID(root, "live-message")
	if err != nil || !found || message.ID != "live-message" {
		t.Fatalf("live message behind stale expired locator = %#v, found=%v err=%v", message, found, err)
	}
	_, found, err = mailboxMessageByID(root, "forgotten-message")
	if err != nil || found {
		t.Fatalf("forgotten message behind stale expired locator = found=%v err=%v", found, err)
	}
}

func TestResourceMailboxLocatorFailureDoesNotSkipOtherChanges(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	blockedPath := resourceMailboxLocatorPath(root, "blocked")
	if err := os.MkdirAll(blockedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := mutateResourceMailboxForResource(root, "workspace", func(mailbox *resourceMailbox) error {
		for _, id := range []string{"blocked", "unblocked"} {
			mailbox.NextSequence++
			mailbox.Messages = append(mailbox.Messages, resourceMailboxMessage{
				ID: id, Sequence: mailbox.NextSequence, ResourceID: "workspace",
				Role: "user", Status: resourceMessageQueued, AcceptedAt: time.Now().Format(time.RFC3339Nano),
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("locator failure affected authoritative mailbox commit: %v", err)
	}
	if _, found, err := readResourceMailboxLocator(root, "unblocked"); err != nil || !found {
		t.Fatalf("unblocked locator = found=%v err=%v", found, err)
	}
	if err := os.Remove(blockedPath); err != nil {
		t.Fatal(err)
	}
	message, found, err := mailboxMessageByID(root, "blocked")
	if err != nil || !found || message.ID != "blocked" {
		t.Fatalf("fallback for missing locator = %#v, found=%v err=%v", message, found, err)
	}
	if _, found, err := readResourceMailboxLocator(root, "blocked"); err != nil || !found {
		t.Fatalf("fallback did not rebuild locator: found=%v err=%v", found, err)
	}
}
