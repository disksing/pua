package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	osuser "os/user"
	"path/filepath"
	"strings"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/workspacepath"
)

const workspaceUserHeader = "X-PUA-User"

func (s *server) workspaceUserName(r *http.Request, workspacePath string) (string, error) {
	name := strings.TrimSpace(r.Header.Get(workspaceUserHeader))
	if name == "" {
		return "", &resourceAPIError{Code: "user_required", Message: "select a Workspace user before accessing personal data"}
	}
	if err := app.ValidateUserName(name); err != nil {
		return "", &resourceAPIError{Code: "invalid_request", Message: err.Error()}
	}
	workspace, err := app.OpenWorkspace(workspacePath)
	if err != nil {
		return "", err
	}
	if _, err := workspace.User(name); err != nil {
		return "", &resourceAPIError{Code: "user_not_found", Message: fmt.Sprintf("Workspace user not found: %s", name)}
	}
	return name, nil
}

func suggestedSystemUserName() string {
	current, err := osuser.Current()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(current.Username)
	if app.ValidateUserName(name) != nil {
		return ""
	}
	return name
}

func (s *server) handleUsers(w http.ResponseWriter, r *http.Request, workspaceID string, parts []string) {
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
	if len(parts) >= 2 && parts[0] != "" && parts[1] == "messages" {
		writeError(w, errors.New("Inbox was removed; use resource chat"), http.StatusGone)
		return
	}
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			users, err := puaWorkspace.Users()
			if err != nil {
				writeError(w, err, http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"users": users})
		case http.MethodPost:
			var body struct {
				Name string `json:"name"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil {
				writeError(w, err, http.StatusBadRequest)
				return
			}
			profile, err := puaWorkspace.RegisterUser(body.Name)
			if err != nil {
				writeError(w, err, http.StatusBadRequest)
				return
			}
			if err := s.ensureUserUIStateBaseline(workspace.Path, profile.Name); err != nil {
				writeError(w, err, http.StatusInternalServerError)
				return
			}
			writeJSON(w, profile)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	if len(parts) != 1 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	name := parts[0]
	if err := app.ValidateUserName(name); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Preference string `json:"preference"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		profile, err := puaWorkspace.UpdateUserPreference(name, body.Preference)
		if err != nil {
			writeError(w, err, http.StatusNotFound)
			return
		}
		writeJSON(w, profile)
	case http.MethodDelete:
		telegram := s.telegramFor(workspace)
		telegram.mu.Lock()
		defer telegram.mu.Unlock()
		chatMu.Lock()
		users, listErr := puaWorkspace.Users()
		found := false
		for _, user := range users {
			found = found || user.Name == name
		}
		if listErr != nil || !found || len(users) <= 1 {
			chatMu.Unlock()
			if listErr != nil {
				writeError(w, listErr, http.StatusInternalServerError)
			} else if !found {
				writeError(w, errors.New("user not found"), http.StatusNotFound)
			} else {
				writeError(w, &resourceAPIError{Code: "last_user", Message: app.ErrLastUser.Error()}, http.StatusConflict)
			}
			return
		}
		if err := revokeChatUserLocked(workspace.Path, name); err != nil {
			chatMu.Unlock()
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		if err := telegram.revokeUser(name); err != nil {
			chatMu.Unlock()
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		if err := puaWorkspace.DeleteUser(name); err != nil {
			chatMu.Unlock()
			if errors.Is(err, app.ErrLastUser) {
				writeError(w, &resourceAPIError{Code: "last_user", Message: app.ErrLastUser.Error()}, http.StatusConflict)
				return
			}
			writeError(w, err, http.StatusNotFound)
			return
		}
		chatMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *server) ensureWorkspaceUIState(workspacePath string) error {
	workspace, err := app.OpenWorkspace(workspacePath)
	if err != nil {
		return err
	}

	s.uiStateMu.Lock()
	defer s.uiStateMu.Unlock()
	shared, err := loadResourceStateFile(resourceStatePath(workspacePath))
	if err != nil {
		return err
	}
	sharedChanged := false
	records, err := loadGenerationRecords(workspacePath)
	if err != nil {
		return err
	}
	for resourceID, record := range latestTurnGenerationByResource(records) {
		if record.TurnNumber > shared.TurnNumbers[resourceID] {
			shared.TurnNumbers[resourceID] = record.TurnNumber
			sharedChanged = true
		}
	}
	if sharedChanged {
		if err := saveResourceStateFile(resourceStatePath(workspacePath), shared); err != nil {
			return err
		}
	}
	users, err := workspace.Users()
	if err != nil {
		return err
	}
	for _, user := range users {
		if err := ensureUserUIStateFile(workspacePath, user.Name); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) ensureUserUIStateBaseline(workspacePath, userName string) error {
	s.uiStateMu.Lock()
	defer s.uiStateMu.Unlock()
	return ensureUserUIStateFile(workspacePath, userName)
}

func ensureUserUIStateFile(workspacePath, userName string) error {
	statePath := userUIStatePath(workspacePath, userName)
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		return saveUIStateFile(statePath, uiState{ExpandedProjects: []string{}})
	} else if err != nil {
		return err
	}
	_, err := loadUIStateFile(statePath)
	return err
}

func userUIStatePath(workspacePath, userName string) string {
	return filepath.Join(workspacepath.ControlDir(workspacePath), "users", userName, "ui-state.json")
}

func resourceStatePath(workspacePath string) string {
	return filepath.Join(workspacepath.ControlDir(workspacePath), "resource-state.json")
}
