package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (s *server) handleWorkspaceServices(w http.ResponseWriter, r *http.Request, workspaceID string, parts []string) {
	manager, workspace, err := s.serviceManagerForWorkspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	if ownershipErr := s.requireWorkspaceOwnership(workspace.Path); ownershipErr != nil {
		writeError(w, ownershipErr, http.StatusConflict)
		return
	}
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]any{"services": manager.List()})
		case http.MethodPut:
			var body struct {
				Services []ServiceConfig `json:"services"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil {
				writeError(w, err, http.StatusBadRequest)
				return
			}
			for _, cfg := range body.Services {
				if err := manager.Apply(cfg); err != nil {
					writeError(w, err, http.StatusBadRequest)
					return
				}
			}
			writeJSON(w, map[string]any{"services": manager.List()})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	id := strings.TrimSpace(parts[0])
	if id == "" {
		writeError(w, errors.New("service id is required"), http.StatusBadRequest)
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			status, err := manager.Show(id)
			if err != nil {
				writeError(w, err, http.StatusNotFound)
				return
			}
			writeJSON(w, status)
		case http.MethodPut:
			var cfg ServiceConfig
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&cfg); err != nil {
				writeError(w, err, http.StatusBadRequest)
				return
			}
			if cfg.ID == "" {
				cfg.ID = id
			}
			if cfg.ID != id {
				writeError(w, errors.New("service id does not match URL"), http.StatusBadRequest)
				return
			}
			if err := manager.Apply(cfg); err != nil {
				writeError(w, err, http.StatusBadRequest)
				return
			}
			status, _ := manager.Show(id)
			writeJSON(w, status)
		case http.MethodDelete:
			if err := manager.Remove(r.Context(), id); err != nil {
				writeError(w, err, http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	action := parts[1]
	switch action {
	case "start", "stop", "restart":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var operationErr error
		switch action {
		case "start":
			operationErr = manager.StartService(r.Context(), id)
		case "stop":
			operationErr = manager.StopService(r.Context(), id)
		case "restart":
			operationErr = manager.RestartService(r.Context(), id)
		}
		if operationErr != nil {
			writeError(w, operationErr, statusForServiceError(operationErr))
			return
		}
		status, _ := manager.Show(id)
		writeJSON(w, status)
	case "enable", "disable":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var operationErr error
		if action == "enable" {
			operationErr = manager.Enable(id)
		} else {
			operationErr = manager.Disable(r.Context(), id)
		}
		if operationErr != nil {
			writeError(w, operationErr, statusForServiceError(operationErr))
			return
		}
		status, _ := manager.Show(id)
		writeJSON(w, status)
	case "logs":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		followValue, followPresent := r.URL.Query()["follow"]
		follow := followPresent && (len(followValue) == 0 || followValue[0] == "" || followValue[0] == "1" || strings.EqualFold(followValue[0], "true"))
		stream := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("stream")))
		if stream == "" {
			stream = "stdout"
		}
		if stream != "stdout" && stream != "stderr" {
			writeError(w, errors.New("stream must be stdout or stderr"), http.StatusBadRequest)
			return
		}
		reader, err := manager.LogsContext(r.Context(), id, stream, follow)
		if err != nil {
			writeError(w, err, http.StatusNotFound)
			return
		}
		defer reader.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if follow {
			w.Header().Set("Cache-Control", "no-cache")
		}
		_, _ = io.Copy(w, reader)
	case "exports":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		exports, err := manager.Exports(id)
		if err != nil {
			writeError(w, err, http.StatusNotFound)
			return
		}
		writeJSON(w, exports)
	default:
		http.NotFound(w, r)
	}
}

func statusForServiceError(err error) int {
	if errors.Is(err, os.ErrNotExist) {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

func (s *server) handleServiceBindings(w http.ResponseWriter, r *http.Request, workspaceID string) {
	manager, workspace, err := s.serviceManagerForWorkspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	_ = manager
	if ownershipErr := s.requireWorkspaceOwnership(workspace.Path); ownershipErr != nil {
		writeError(w, ownershipErr, http.StatusConflict)
		return
	}
	path := serviceBindingsPath(workspace.Path)
	if !pathWithinResolved(filepath.Join(workspace.Path, ".pua"), path) {
		writeError(w, errors.New("service bindings path escapes the workspace control directory"), http.StatusConflict)
		return
	}
	switch r.Method {
	case http.MethodGet:
		bindings := ServiceBindings{SchemaVersion: serviceSchemaVersion, Variables: map[string]string{}, Secrets: map[string]string{}}
		data, err := os.ReadFile(path)
		if err == nil {
			decoder := json.NewDecoder(strings.NewReader(string(data)))
			if decodeErr := decoder.Decode(&bindings); decodeErr != nil {
				writeError(w, decodeErr, http.StatusInternalServerError)
				return
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, bindings)
	case http.MethodPut:
		var bindings ServiceBindings
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&bindings); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		manager.mu.Lock()
		validationErr := validateServiceBindingsLocked(bindings, manager)
		if validationErr == nil {
			bindings.SchemaVersion = serviceSchemaVersion
			validationErr = writeServiceJSON(path, bindings, 0o600)
		}
		manager.mu.Unlock()
		if validationErr != nil {
			writeError(w, validationErr, http.StatusBadRequest)
			return
		}
		writeJSON(w, bindings)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func validateServiceBindings(bindings ServiceBindings, manager *ServiceManager) error {
	if manager == nil {
		return validateServiceBindingsLocked(bindings, nil)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return validateServiceBindingsLocked(bindings, manager)
}

func validateServiceBindingsLocked(bindings ServiceBindings, manager *ServiceManager) error {
	if bindings.SchemaVersion != 0 && bindings.SchemaVersion != serviceSchemaVersion {
		return fmt.Errorf("unsupported schema version %d", bindings.SchemaVersion)
	}
	for name, value := range bindings.Variables {
		if !environmentNamePattern.MatchString(name) {
			return fmt.Errorf("invalid variable name %q", name)
		}
		if reservedServiceBindingName(name) {
			return fmt.Errorf("variable name %q is reserved for PUA provenance", name)
		}
		if secretTemplatePattern.MatchString(value) {
			return fmt.Errorf("secret references must not appear in variable mapping %q", name)
		}
		if strings.Contains(value, "${") {
			if manager == nil {
				return fmt.Errorf("variable mapping %q references an unavailable service", name)
			}
			if err := validateEnvironmentTemplate(value, "", manager.configs); err != nil {
				return fmt.Errorf("variable mapping %q: %w", name, err)
			}
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("variable %q contains NUL", name)
		}
		if manager != nil {
			if match := serviceTemplatePattern.FindStringSubmatch(value); len(match) == 3 && match[0] == value {
				if rt := manager.runtimes[match[1]]; rt != nil {
					if _, secret := rt.exports.Secrets[match[2]]; secret {
						return fmt.Errorf("secret service export %q must be mapped under secrets", match[2])
					}
				}
			}
		}
	}
	for name, value := range bindings.Secrets {
		if !environmentNamePattern.MatchString(name) {
			return fmt.Errorf("invalid secret variable name %q", name)
		}
		if reservedServiceBindingName(name) {
			return fmt.Errorf("secret variable name %q is reserved for PUA provenance", name)
		}
		if match := secretTemplatePattern.FindStringSubmatch(value); len(match) == 2 && match[0] == value && validSecretName(match[1]) {
			continue
		}
		if match := serviceTemplatePattern.FindStringSubmatch(value); len(match) == 3 && match[0] == value {
			if manager == nil {
				return fmt.Errorf("secret mapping %q references unavailable service %q", name, match[1])
			}
			rt := manager.runtimes[match[1]]
			if rt == nil {
				return fmt.Errorf("secret mapping %q references unknown service %q", name, match[1])
			}
			continue
		}
		return fmt.Errorf("secret mapping %q must be a complete secret or service export reference", name)
	}
	return nil
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
