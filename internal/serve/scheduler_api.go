package serve

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/disksing/pua/internal/app"
)

func (s *server) handleScheduler(w http.ResponseWriter, r *http.Request, workspaceID string, parts []string) {
	workspace, err := s.workspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			config, readErr := puaWorkspace.Scheduler()
			if readErr != nil {
				writeError(w, readErr, http.StatusBadRequest)
				return
			}
			writeJSON(w, config)
		case http.MethodPost:
			var body struct {
				Description string                     `json:"description"`
				Condition   string                     `json:"condition"`
				Target      string                     `json:"target"`
				ExactTime   *app.ScheduleExactTimeSpec `json:"exactTime"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if decodeErr := decoder.Decode(&body); decodeErr != nil {
				writeError(w, decodeErr, http.StatusBadRequest)
				return
			}
			created, createErr := puaWorkspace.AddSchedule(app.CreateScheduleInput{Description: body.Description, Condition: body.Condition, Target: body.Target, ExactTime: body.ExactTime})
			if createErr != nil {
				writeError(w, createErr, http.StatusBadRequest)
				return
			}
			if s.agents != nil {
				s.agents.requestReconcile(reconcileScheduler)
			}
			writeJSON(w, created)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	if len(parts) == 1 && parts[0] == "settings" {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body app.SchedulerSettingsInput
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decodeErr := decoder.Decode(&body); decodeErr != nil {
			writeError(w, decodeErr, http.StatusBadRequest)
			return
		}
		updated, updateErr := puaWorkspace.SetSchedulerSettings(body)
		if updateErr != nil {
			writeError(w, updateErr, http.StatusBadRequest)
			return
		}
		if s.agents != nil {
			s.agents.requestReconcile(reconcileScheduler)
		}
		writeJSON(w, updated)
		return
	}
	if len(parts) != 1 || strings.TrimSpace(parts[0]) == "" {
		writeError(w, errors.New("schedule id is required"), http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(parts[0])
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Description *string                    `json:"description"`
			Condition   *string                    `json:"condition"`
			Target      *string                    `json:"target"`
			ExactTime   *app.ScheduleExactTimeSpec `json:"exactTime"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decodeErr := decoder.Decode(&body); decodeErr != nil {
			writeError(w, decodeErr, http.StatusBadRequest)
			return
		}
		updated, updateErr := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{ID: id, Description: body.Description, Condition: body.Condition, Target: body.Target, ExactTime: body.ExactTime})
		if updateErr != nil {
			writeError(w, updateErr, http.StatusBadRequest)
			return
		}
		if s.agents != nil {
			s.agents.requestReconcile(reconcileScheduler)
		}
		writeJSON(w, updated)
	case http.MethodDelete:
		removed, removeErr := puaWorkspace.RemoveSchedule(id)
		if removeErr != nil {
			writeError(w, removeErr, http.StatusBadRequest)
			return
		}
		if s.agents != nil {
			s.agents.requestReconcile(reconcileScheduler)
		}
		writeJSON(w, removed)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
