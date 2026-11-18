package serve

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/disksing/pua/internal/app"
)

// deleteResourceArtifact handles DELETE
// /api/workspaces/{workspace}/resources/{resource}/artifacts?path=... and
// removes a single regular file inside the resource's artifacts/ directory.
func (s *server) deleteResourceArtifact(w http.ResponseWriter, r *http.Request, workspaceID, resourceID string) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
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
	resource, err := puaWorkspace.ResourceValue(resourceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	if resource.Archived {
		writeError(w, errors.New("archived resources cannot be edited"), http.StatusBadRequest)
		return
	}

	relPath := filepath.ToSlash(filepath.Clean(strings.TrimSpace(r.URL.Query().Get("path"))))
	if relPath == "." || relPath == "" {
		writeError(w, errors.New("path is required"), http.StatusBadRequest)
		return
	}
	abs, err := deletableResourceArtifactPath(workspace.Path, resource, relPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, errors.New("artifact file does not exist"), http.StatusNotFound)
			return
		}
		writeError(w, err, http.StatusBadRequest)
		return
	}

	if err := os.Remove(abs); err != nil {
		if os.IsNotExist(err) {
			writeError(w, errors.New("artifact file does not exist"), http.StatusNotFound)
			return
		}
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, struct {
		Path string `json:"path"`
	}{Path: relPath})
}

func deletableResourceArtifactPath(workspaceRoot string, resource app.ResourceResult, relPath string) (string, error) {
	resourceRel := filepath.ToSlash(filepath.Clean(resource.Path))
	pathRel, err := filepath.Rel(filepath.FromSlash(resourceRel), filepath.FromSlash(relPath))
	if err != nil || pathRel == ".." || strings.HasPrefix(pathRel, ".."+string(filepath.Separator)) {
		return "", errors.New("file does not belong to the selected resource")
	}
	pathSlash := filepath.ToSlash(pathRel)
	if !strings.HasPrefix(pathSlash, "artifacts/") || strings.TrimPrefix(pathSlash, "artifacts/") == "" {
		return "", errors.New("only files inside the resource artifacts/ directory can be deleted")
	}

	abs, err := safeWorkspacePath(workspaceRoot, filepath.FromSlash(relPath))
	if err != nil {
		return "", err
	}
	resourceAbs, err := safeWorkspacePath(workspaceRoot, filepath.FromSlash(resourceRel))
	if err != nil {
		return "", err
	}
	resourceEval, err := filepath.EvalSymlinks(resourceAbs)
	if err != nil {
		return "", err
	}
	targetEval, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if targetEval != filepath.Join(resourceEval, filepath.FromSlash(pathSlash)) {
		return "", errors.New("deletable artifact path must not resolve through symbolic links")
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("deletable artifact path must be a regular file, not a symbolic link")
	}
	return abs, nil
}
