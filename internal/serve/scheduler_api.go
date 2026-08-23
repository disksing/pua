package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/localize"
	"github.com/disksing/pua/internal/schedulerapi"
)

type schedulerChangeRequest struct {
	Operation        string                `json:"operation"`
	ID               string                `json:"id,omitempty"`
	ExpectedRevision schedulerapi.Revision `json:"expectedRevision,omitempty"`
	Description      *string               `json:"description,omitempty"`
	Condition        *string               `json:"condition,omitempty"`
	Guard            *string               `json:"guard,omitempty"`
	Target           *string               `json:"target,omitempty"`
	Trigger          *app.ScheduleTrigger  `json:"trigger,omitempty"`
}

type schedulerResourceDetail struct {
	app.ResourceDetailView
	Scheduler *schedulerapi.Snapshot `json:"scheduler,omitempty"`
}

func schedulerResourceDetailAPIResponse(detail app.ResourceDetailView) schedulerResourceDetail {
	response := schedulerResourceDetail{ResourceDetailView: detail}
	if detail.Scheduler != nil {
		snapshot := schedulerapi.FromSnapshot(*detail.Scheduler)
		response.Scheduler = &snapshot
	}
	return response
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
			writeJSON(w, schedulerapi.FromSnapshot(snapshot))
		case http.MethodPost:
			s.handleNaturalLanguageScheduleRequest(w, r, workspace, app.ScheduleChangeCreate, "")
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
		writeJSON(w, schedulerapi.FromSchedule(changed))
		return
	}

	id := strings.TrimSpace(parts[0])
	if id == "" || len(parts) > 2 {
		writeError(w, errors.New("schedule id is required"), http.StatusBadRequest)
		return
	}
	if len(parts) == 2 {
		operation, err := app.ParseScheduleChangeOperation(parts[1])
		if r.Method != http.MethodPost || err != nil || (operation != app.ScheduleChangePause && operation != app.ScheduleChangeResume) {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.applyDirectScheduleChange(w, r, workspace, NativeSchedulerChange{Operation: operation, ID: id})
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.handleNaturalLanguageScheduleRequest(w, r, workspace, app.ScheduleChangeUpdate, id)
	case http.MethodDelete:
		s.applyDirectScheduleChange(w, r, workspace, NativeSchedulerChange{Operation: app.ScheduleChangeRemove, ID: id})
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
	return ensureJSONEOF(decoder)
}

func schedulerNativeChange(body schedulerChangeRequest) (NativeSchedulerChange, error) {
	operation, err := app.ParseScheduleChangeOperation(body.Operation)
	if err != nil {
		return NativeSchedulerChange{}, err
	}
	expectedRevision := uint64(0)
	if body.ExpectedRevision != "" {
		expectedRevision, err = body.ExpectedRevision.Uint64()
		if err != nil {
			return NativeSchedulerChange{}, err
		}
	}
	if operation == app.ScheduleChangeUpdate && expectedRevision == 0 {
		return NativeSchedulerChange{}, errors.New("expectedRevision is required for schedule updates")
	}
	if operation != app.ScheduleChangeUpdate && expectedRevision != 0 {
		return NativeSchedulerChange{}, errors.New("expectedRevision is only valid for schedule updates")
	}
	return NativeSchedulerChange{
		Operation:        operation,
		ID:               strings.TrimSpace(body.ID),
		ExpectedRevision: expectedRevision,
		Description:      body.Description,
		Condition:        body.Condition,
		Guard:            body.Guard,
		Target:           body.Target,
		Trigger:          body.Trigger,
	}, nil
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
	writeJSON(w, schedulerapi.FromSchedule(changed))
}

func writeSchedulerChangeError(w http.ResponseWriter, err error) {
	if errors.Is(err, app.ErrScheduleNotFound) {
		writeError(w, &resourceAPIError{Code: "schedule_not_found", Message: err.Error()}, http.StatusNotFound)
		return
	}
	var conflict *app.ScheduleRevisionConflictError
	if errors.As(err, &conflict) {
		writeError(w, &resourceAPIError{Code: "schedule_revision_conflict", Message: conflict.Error()}, http.StatusConflict)
		return
	}
	if errors.Is(err, app.ErrScheduleRevisionExhausted) {
		writeError(w, &resourceAPIError{Code: "schedule_revision_exhausted", Message: err.Error()}, http.StatusConflict)
		return
	}
	if errors.Is(err, app.ErrScheduleOccurrenceOutOfRange) {
		writeError(w, &resourceAPIError{Code: "schedule_occurrence_out_of_range", Message: err.Error()}, http.StatusBadRequest)
		return
	}
	if errors.Is(err, app.ErrScheduleTargetScheduler) {
		writeSchedulerTargetError(w)
		return
	}
	if errors.Is(err, errNativeSchedulerUpdateTriggerRequired) {
		writeError(w, &resourceAPIError{Code: "schedule_trigger_required", Message: err.Error()}, http.StatusBadRequest)
		return
	}
	if errors.Is(err, errNativeSchedulerPauseCompleted) {
		writeError(w, &resourceAPIError{Code: "schedule_state_conflict", Message: err.Error()}, http.StatusConflict)
		return
	}
	writeError(w, err, http.StatusBadRequest)
}

func writeSchedulerTargetError(w http.ResponseWriter) {
	writeError(w, &resourceAPIError{
		Code: "schedule_target_invalid", Message: app.ErrScheduleTargetScheduler.Error(),
	}, http.StatusBadRequest)
}

func (s *server) handleNaturalLanguageScheduleRequest(w http.ResponseWriter, r *http.Request, workspace serveWorkspace, operation app.ScheduleChangeOperation, id string) {
	var body struct {
		ExpectedRevision *schedulerapi.Revision `json:"expectedRevision,omitempty"`
		Description      string                 `json:"description"`
		Condition        string                 `json:"condition"`
		Target           string                 `json:"target"`
	}
	if err := decodeSchedulerBody(r, &body); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	description, condition, target := strings.TrimSpace(body.Description), strings.TrimSpace(body.Condition), strings.TrimSpace(body.Target)
	if description == "" || condition == "" || target == "" {
		writeError(w, errors.New("description, condition, and target are required"), http.StatusBadRequest)
		return
	}
	if operation == app.ScheduleChangeCreate && body.ExpectedRevision != nil {
		writeError(w, errors.New("expectedRevision is only valid for schedule updates"), http.StatusBadRequest)
		return
	}
	if operation == app.ScheduleChangeUpdate && body.ExpectedRevision == nil {
		writeError(w, errors.New("expectedRevision is required for schedule updates"), http.StatusBadRequest)
		return
	}
	expectedRevision := uint64(0)
	if body.ExpectedRevision != nil {
		parsedRevision, parseErr := body.ExpectedRevision.Uint64()
		if parseErr != nil {
			writeError(w, parseErr, http.StatusBadRequest)
			return
		}
		expectedRevision = parsedRevision
	}
	if err := app.ValidateScheduleTarget(target); err != nil {
		writeSchedulerTargetError(w)
		return
	}
	userName, err := s.workspaceUserName(r, workspace.Path)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	language, err := workspaceContentLanguage(workspace.Path)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	text := strings.TrimSuffix(localize.MustRender(language, "scheduler-compilation.md", map[string]any{
		"Operation":           string(operation),
		"HasID":               id != "",
		"ID":                  id,
		"HasExpectedRevision": operation == app.ScheduleChangeUpdate,
		"ExpectedRevision":    strconv.FormatUint(expectedRevision, 10),
		"Description":         description,
		"Condition":           condition,
		"Target":              target,
	}), "\n")
	role, sender := agentHubMessageProvenance(userName)
	var message resourceMailboxMessage
	acceptErr := s.agents.withResourceController(r.Context(), workspace, app.SchedulerResourceID, func() error {
		var err error
		message, err = s.agents.acceptResourceMessageDurable(r.Context(), workspace, app.SchedulerResourceID, resourceMessageRequest{
			Text: text, Mode: resourceMessageModeEnqueue, Role: role, Sender: sender,
		})
		return err
	})
	if acceptErr != nil {
		writeError(w, acceptErr, resourceErrorStatus(acceptErr))
		return
	}
	s.markResourceReadOnUserMessage(workspace.Path, app.SchedulerResourceID, userName)
	if wakeErr := s.agents.enqueueResourceController(workspace, app.SchedulerResourceID, func() error {
		if err := s.agents.reconcileResourceMailboxLocked(context.Background(), workspace, app.SchedulerResourceID); err != nil {
			recordMailboxFailure(workspace.Path, message.ID, err)
		}
		s.agents.requestReconcile(reconcileNotifications)
		return nil
	}); wakeErr != nil {
		recordMailboxFailure(workspace.Path, message.ID, wakeErr)
		s.agents.requestReconcile(reconcileNotifications)
	}
	response := mailboxMessageResponse(message)
	response.Reference = fmt.Sprintf("/api/workspaces/%s/messages/%s", workspace.ID, message.ID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(response)
}
