package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func openTestPUAWorkspace(t *testing.T, path, language string) *app.Workspace {
	t.Helper()
	workspace, err := app.Initialize(path, language)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.RegisterUser(app.LegacyDefaultUserName); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func rewriteTestGenerationRecords(workspacePath string, records []generationRecord) error {
	byResource := make(map[string][]generationRecord)
	for _, record := range records {
		byResource[normalizedResourceID(record.ResourceID)] = append(byResource[normalizedResourceID(record.ResourceID)], record)
	}
	for _, resourceRecords := range byResource {
		sort.SliceStable(resourceRecords, func(i, j int) bool {
			if resourceRecords[i].Generation != resourceRecords[j].Generation {
				return resourceRecords[i].Generation < resourceRecords[j].Generation
			}
			return resourceRecords[i].ID < resourceRecords[j].ID
		})
		for _, record := range resourceRecords[:len(resourceRecords)-1] {
			storeRecord, err := toStoreRecord(record)
			if err != nil {
				return err
			}
			store, err := openGenerationStore(workspacePath, record.SourceInstanceID)
			if err != nil {
				return err
			}
			if err := store.SaveRetired(storeRecord, "test_fixture"); err != nil {
				return err
			}
		}
		if err := saveGenerationRecord(workspacePath, resourceRecords[len(resourceRecords)-1]); err != nil {
			return err
		}
	}
	return nil
}

func saveRetiredGenerationForTest(t *testing.T, workspacePath string, record generationRecord, reason string) {
	t.Helper()
	store, err := openGenerationStore(workspacePath, record.SourceInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	storeRecord, err := toStoreRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRetired(storeRecord, reason); err != nil {
		t.Fatal(err)
	}
}

// newResourceMessage and enqueueResourceMessage reconstruct the retired
// runtime enqueue helper for tests that exercise mailbox mutation,
// serialization, and delivery retry without the removed run lifecycle handlers.
func newResourceMessage(text, userName string) resourceMailboxMessage {
	role, sender := agentHubMessageProvenance(userName)
	return resourceMailboxMessage{
		ID: "msg-" + newGenerationRecordID(), Text: strings.TrimSpace(text), Role: role,
		Sender: sender, RequestedMode: resourceMessageModeSteer, ActualMode: resourceMessageModeSteer,
		AcceptedAt: time.Now().Format(time.RFC3339Nano),
	}
}

func (rt *agentRuntime) enqueueResourceMessage(message resourceMailboxMessage) error {
	record := rt.snapshotGeneration()
	_, err := mutateResourceMailbox(rt.workspace.Path, func(mailbox *resourceMailbox) error {
		for _, existing := range mailbox.Messages {
			if existing.ID == message.ID {
				return nil
			}
		}
		mailbox.NextSequence++
		requested := strings.TrimSpace(message.RequestedMode)
		if requested == "" {
			requested = resourceMessageModeSteer
		}
		actual := strings.TrimSpace(message.ActualMode)
		if actual == "" {
			actual = requested
		}
		acceptedAt := strings.TrimSpace(message.AcceptedAt)
		if acceptedAt == "" {
			acceptedAt = time.Now().Format(time.RFC3339Nano)
		}
		mailbox.Messages = append(mailbox.Messages, resourceMailboxMessage{
			ID: message.ID, Sequence: mailbox.NextSequence, ResourceID: normalizedResourceID(record.ResourceID),
			Text: message.Text, Role: message.Role, Sender: message.Sender,
			RequestedMode: requested, ActualMode: actual, ModeFrozen: message.ModeFrozen, Status: resourceMessageQueued,
			AcceptedAt: acceptedAt, UpdatedAt: acceptedAt,
		})
		return nil
	})
	return err
}

// closeRuntimeTestGeneration stops one generation's AgentHub session and marks the
// generation stopped on disk so cleanup assertions can converge without the removed
// run lifecycle handlers.
func closeRuntimeTestGeneration(t *testing.T, manager *agentManager, workspace serveWorkspace, recordID string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	record, err := loadGenerationRecord(workspace.Path, recordID)
	if err != nil {
		writeError(response, err, http.StatusNotFound)
		return response
	}
	if sessionID := strings.TrimSpace(record.AgentHubSessionID); sessionID != "" {
		_, client, cfgErr := manager.agentHubRuntimeConfig()
		if cfgErr != nil {
			writeError(response, cfgErr, http.StatusServiceUnavailable)
			return response
		}
		if _, err := client.Stop(context.Background(), sessionID); err != nil {
			writeError(response, err, http.StatusBadGateway)
			return response
		}
	}
	record.Status = "stopped"
	record.AgentHubStoppedObserved = true
	record.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		writeError(response, err, http.StatusInternalServerError)
		return response
	}
	writeJSON(response, map[string]any{"status": "stopped"})
	return response
}
