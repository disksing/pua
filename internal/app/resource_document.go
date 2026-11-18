package app

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/disksing/pua/internal/localize"
)

const workspaceMarkdownFile = "workspace.md"
const legacyChatMarkdownFile = "chat.md"

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

func writeDocumentAtomic(path string, content []byte, mode os.FileMode) error {
	return writeDocumentAtomicChecked(path, content, mode, nil)
}

func writeDocumentAtomicChecked(path string, content []byte, mode os.FileMode, check func() error) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".pua-document-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if err := temp.Chmod(mode); err != nil {
		return err
	}
	if _, err := temp.Write(content); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func ensureResourceDocument(dir, name, initial string) error {
	target := filepath.Join(dir, name)
	content, mode, err := readRegularDocument(target)
	missing := os.IsNotExist(err)
	if err != nil && !missing {
		return err
	}
	if missing {
		content, mode = []byte(initial), 0o644
	}
	source := filepath.Join(dir, legacyChatMarkdownFile)
	legacy, _, err := readRegularDocument(source)
	if os.IsNotExist(err) {
		if missing {
			return writeDocumentAtomic(target, content, mode)
		}
		return nil
	}
	if err != nil {
		return err
	}
	block := mergedChatBlock(legacy)
	if !bytes.Contains(content, block) {
		content = append(content, []byte("\n\n")...)
		content = append(content, block...)
		content = append(content, '\n')
		if err := writeDocumentAtomic(target, content, mode); err != nil {
			return err
		}
	}
	persisted, _, err := readRegularDocument(target)
	if err != nil {
		return err
	}
	if !bytes.Equal(persisted, content) {
		return fmt.Errorf("resource document changed during migration: %s", target)
	}
	latest, _, err := readRegularDocument(source)
	if err != nil {
		return err
	}
	if !bytes.Equal(latest, legacy) {
		return fmt.Errorf("legacy chat document changed during migration: %s", source)
	}
	if err := os.Remove(source); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func mergedChatBlock(legacy []byte) []byte {
	return []byte(fmt.Sprintf("<!-- pua:merged-chat:%s -->\n%s\n<!-- pua:merged-chat:end -->", markdownContentHash(legacy), legacy))
}

func ensureWorkspaceDocument(root, language string) error {
	return ensureSplitDocuments(root, workspaceMarkdownFile, localize.MustRender(language, workspaceMarkdownFile, nil), language)
}

func migrateResourceDocuments(root, language string) error {
	if err := ensureWorkspaceDocument(root, language); err != nil {
		return err
	}
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
			return fmt.Errorf("read resource for document migration: %s: %w", path, err)
		}
		if err := updateTaskAgentsMD(root, path, resource, language); err != nil {
			return err
		}
		return nil
	})
}
