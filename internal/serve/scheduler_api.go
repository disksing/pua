package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/disksing/pua/internal/app"
)

type schedulerChangeRequest struct {
	Operation        string               `json:"operation"`
	ID               string               `json:"id,omitempty"`
	ExpectedRevision uint64               `json:"expectedRevision,omitempty"`
	Description      *string              `json:"description,omitempty"`
	Condition        *string              `json:"condition,omitempty"`
	Guard            *string              `json:"guard,omitempty"`
	Target           *string              `json:"target,omitempty"`
	Trigger          *app.ScheduleTrigger `json:"trigger,omitempty"`
}

func (s *server) handleScheduler(w http.ResponseWriter, r *http.Request, workspaceID string, parts []string) {
	workspace, err := s.workspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	if s.agents == nil {
		writeError(w, errors.New("Scheduler owner Server is unavailable"), http.StatusServiceUnavailable)
		return
	}
	native := newNativeScheduler(s.agents, workspace)

	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			var snapshot app.SchedulerSnapshot
			readErr := s.agents.withResourceController(r.Context(), workspace, app.SchedulerResourceID, func() error {
				var err error
				snapshot, err = native.Snapshot(s.agents.now())
				return err
			})
			if readErr != nil {
				writeError(w, readErr, http.StatusBadRequest)
				return
			}
			writeJSON(w, snapshot)
		case http.MethodPost:
			s.handleNaturalLanguageScheduleRequest(w, r, workspace, "create", "")
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	if len(parts) == 1 && parts[0] == "changes" {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body schedulerChangeRequest
		if err := decodeSchedulerBody(r, &body); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		change, err := schedulerNativeChange(body)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		var changed app.Schedule
		changeErr := s.agents.withResourceController(r.Context(), workspace, app.SchedulerResourceID, func() error {
			var err error
			changed, err = native.Change(r.Context(), change)
			return err
		})
		if changeErr != nil {
			writeSchedulerChangeError(w, changeErr)
			return
		}
		s.agents.requestReconcile(reconcileScheduler)
		writeJSON(w, changed)
		return
	}

	id := strings.TrimSpace(parts[0])
	if id == "" || len(parts) > 2 {
		writeError(w, errors.New("schedule id is required"), http.StatusBadRequest)
		return
	}
	if len(parts) == 2 {
		operation := parts[1]
		if r.Method != http.MethodPost || (operation != "pause" && operation != "resume") {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.applyDirectScheduleChange(w, r, workspace, NativeSchedulerChange{Operation: operation, ID: id})
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.handleNaturalLanguageScheduleRequest(w, r, workspace, "update", id)
	case http.MethodDelete:
		s.applyDirectScheduleChange(w, r, workspace, NativeSchedulerChange{Operation: "remove", ID: id})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func decodeSchedulerBody(r *http.Request, output any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	return ensureJSONRequestEOF(decoder)
}

func ensureJSONRequestEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return errors.New("unexpected data after JSON document")
}

func schedulerNativeChange(body schedulerChangeRequest) (NativeSchedulerChange, error) {
	operation := strings.TrimSpace(body.Operation)
	change := NativeSchedulerChange{Operation: operation, ID: strings.TrimSpace(body.ID)}
	switch operation {
	case "create":
		if body.Description == nil || body.Condition == nil || body.Target == nil || body.Trigger == nil {
			return change, errors.New("create requires description, condition, target, and trigger")
		}
		guard := ""
		if body.Guard != nil {
			guard = *body.Guard
		}
		change.Create = app.CreateScheduleInput{Description: *body.Description, Condition: *body.Condition, Guard: guard, Target: *body.Target, Trigger: body.Trigger}
	case "update":
		if change.ID == "" || body.ExpectedRevision == 0 {
			return change, errors.New("update requires id and expectedRevision")
		}
		if body.Description == nil && body.Condition == nil && body.Guard == nil && body.Target == nil && body.Trigger == nil {
			return change, errors.New("update requires at least one changed field")
		}
		change.Update = app.UpdateScheduleInput{ID: change.ID, ExpectedRevision: body.ExpectedRevision, Description: body.Description, Condition: body.Condition, Guard: body.Guard, Target: body.Target, Trigger: body.Trigger}
	case "pause", "resume", "remove":
		if change.ID == "" {
			return change, errors.New(operation + " requires id")
		}
	default:
		return change, fmt.Errorf("unsupported Scheduler change %q", operation)
	}
	return change, nil
}

func (s *server) applyDirectScheduleChange(w http.ResponseWriter, r *http.Request, workspace serveWorkspace, change NativeSchedulerChange) {
	native := newNativeScheduler(s.agents, workspace)
	var changed app.Schedule
	err := s.agents.withResourceController(r.Context(), workspace, app.SchedulerResourceID, func() error {
		var err error
		changed, err = native.Change(r.Context(), change)
		return err
	})
	if err != nil {
		writeSchedulerChangeError(w, err)
		return
	}
	s.agents.requestReconcile(reconcileScheduler)
	writeJSON(w, changed)
}

func writeSchedulerChangeError(w http.ResponseWriter, err error) {
	var conflict *app.ScheduleRevisionConflictError
	if errors.As(err, &conflict) {
		writeError(w, &resourceAPIError{Code: "schedule_revision_conflict", Message: conflict.Error()}, http.StatusConflict)
		return
	}
	writeError(w, err, http.StatusBadRequest)
}

func (s *server) handleNaturalLanguageScheduleRequest(w http.ResponseWriter, r *http.Request, workspace serveWorkspace, operation, id string) {
	var body struct {
		Description string `json:"description"`
		Condition   string `json:"condition"`
		Target      string `json:"target"`
	}
	if err := decodeSchedulerBody(r, &body); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Description) == "" || strings.TrimSpace(body.Condition) == "" || strings.TrimSpace(body.Target) == "" {
		writeError(w, errors.New("description, condition, and target are required"), http.StatusBadRequest)
		return
	}
	text := fmt.Sprintf("Please %s a native schedule", operation)
	if id != "" {
		text += " for " + id
	}
	text += fmt.Sprintf(".\n\nDescription: %s\nCondition: %s\nTarget: %s\n\nCompile this request into a structured trigger with the Scheduler CLI. If the timing, recurrence, or IANA timezone is ambiguous, ask me in this Turn and do not modify the existing definition.", strings.TrimSpace(body.Description), strings.TrimSpace(body.Condition), strings.TrimSpace(body.Target))
	message, err := s.agents.acceptResourceMessage(r.Context(), workspace, app.SchedulerResourceID, resourceMessageRequest{Text: text, Mode: resourceMessageModeEnqueue, Role: "user"})
	if err != nil {
		writeError(w, err, resourceErrorStatus(err))
		return
	}
	response := mailboxMessageResponse(message)
	response.Reference = fmt.Sprintf("/api/workspaces/%s/messages/%s", workspace.ID, message.ID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(response)
}
