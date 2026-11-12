package app

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	block := []byte(fmt.Sprintf("<!-- pua:merged-chat:%s -->\n%s\n<!-- pua:merged-chat:end -->", markdownContentHash(legacy), legacy))
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

func ensureWorkspaceDocument(root, language string) error {
	initial := localize.MustRender(language, workspaceMarkdownFile, nil) + "\n" + followupMarkdown(language)
	return ensureResourceDocument(root, workspaceMarkdownFile, initial)
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
			case ".git", ".pua", ".forge", reposDir, "worktree", "artifacts", "wiki", "templates":
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

func resourceInitialMarkdown(resource Resource, language string) string {
	return withFollowupMarkdown(defaultTaskMD(resource, language), language)
}

func withFollowupMarkdown(markdown, language string) string {
	for _, heading := range []string{"## Chat follow-ups\n", "## 对话事项\n"} {
		if strings.Contains("\n"+markdown+"\n", "\n"+heading) {
			return markdown
		}
	}
	separator := "\n\n"
	if strings.HasSuffix(markdown, "\n") {
		separator = "\n"
	}
	return markdown + separator + followupMarkdown(language)
}
