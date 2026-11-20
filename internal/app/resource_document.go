package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/disksing/pua/internal/localize"
)

const workspaceMarkdownFile = "workspace.md"
const legacyChatMarkdownFile = "chat.md"
const legacyDocumentStage = ".pua-legacy-documents"
const SpecMarkdownFile = "spec.md"
const MemoryMarkdownFile = "memory.md"

func (w *Workspace) WithResourceDocumentMutation(update func() error) error {
	return withWorkspaceMutationLock(w.root, update)
}

func followupMarkdown(language string) string {
	return localize.MustRender(language, "followups.md", nil)
}

func readRegularDocument(path string) ([]byte, os.FileMode, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("resource document must be a regular file: %s", path)
	}
	content, err := os.ReadFile(path)
	return content, info.Mode().Perm(), err
}

func createDocumentIfMissing(path, initial string) error {
	if _, _, err := readRegularDocument(path); !os.IsNotExist(err) {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".pua-document-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if err := temp.Chmod(0o644); err != nil {
		return err
	}
	if _, err := temp.WriteString(initial); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		if err != nil {
			return err
		}
		return fmt.Errorf("resource document appeared during initialization: %s", path)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func ensureWorkspaceDocument(root, language string) error {
	return ensureSplitDocuments(root, localize.MustRender(language, workspaceMarkdownFile, nil), language)
}

func ensureSplitDocuments(dir, spec, language string) error {
	if err := createDocumentIfMissing(filepath.Join(dir, SpecMarkdownFile), spec); err != nil {
		return err
	}
	return createDocumentIfMissing(filepath.Join(dir, MemoryMarkdownFile), followupMarkdown(language))
}

func LegacyResourceDocument(root, path string) bool {
	dir, name := filepath.Split(path)
	dir = filepath.Clean(dir)
	if dir == filepath.Clean(root) {
		return name == workspaceMarkdownFile
	}
	if name != projectMDFile && name != taskMDFile {
		return false
	}
	resource, err := readResourceAtDir(dir)
	return err == nil && resourceDirNameMatches(filepath.Base(dir), resource) && name == markdownFileName(resource)
}

func refreshResourceGuidance(root, language string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		if path != root {
			switch entry.Name() {
			case ".git", ".pua", ".forge", reposDir, "worktree", "artifacts", "wiki", "templates", legacyDocumentStage:
				return filepath.SkipDir
			}
		}
		if path == filepath.Join(root, schedulerDir) {
			return filepath.SkipDir
		}
		if !pathExists(filepath.Join(path, projectJSONFile)) && !pathExists(filepath.Join(path, taskJSONFile)) {
			return nil
		}
		resource, err := readResourceAtDir(path)
		if err != nil {
			return fmt.Errorf("read resource for guidance refresh: %s: %w", path, err)
		}
		return updateTaskAgentsMD(root, path, resource, language)
	})
}
