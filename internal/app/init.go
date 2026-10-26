package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ensureWorkspaceWiki(root, language string) error {
	dir := filepath.Join(root, wikiDir)
	info, err := os.Lstat(dir)
	switch {
	case os.IsNotExist(err):
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
	case err != nil:
		return err
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("workspace wiki path must not be a symbolic link: %s", dir)
	case !info.IsDir():
		return fmt.Errorf("workspace wiki path is not a directory: %s", dir)
	}

	indexPath := filepath.Join(dir, "index.md")
	if _, err := os.Lstat(indexPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(indexPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	if _, err := file.WriteString(defaultWikiIndexForLanguage(language)); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func updateAgentsMD(path, language string) error {
	if err := ensureChatDocument(filepath.Dir(path), language); err != nil {
		return err
	}
	return updateAgentsMDWithBlock(path, puaPromptBlock(language))
}

func updateAgentsMDWithBlock(path, block string) error {
	content := ""
	if data, err := os.ReadFile(path); err == nil {
		content = string(data)
	} else if !os.IsNotExist(err) {
		return err
	}

	updated, err := upsertManagedBlock(content, block)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(updated), 0o644)
}

func upsertManagedBlock(content, block string) (string, error) {
	start, end, found, err := managedBlockBounds(content)
	if err != nil {
		return "", err
	}
	if found {
		return content[:start] + block + content[end:], nil
	}

	content = strings.TrimRight(content, " \t\r\n")
	if content == "" {
		return block + "\n", nil
	}
	return content + "\n\n" + block + "\n", nil
}

func managedBlockBounds(content string) (int, int, bool, error) {
	startCount := strings.Count(content, puaPromptStart)
	endCount := strings.Count(content, puaPromptEnd)
	if startCount == 0 && endCount == 0 {
		return -1, -1, false, nil
	}
	if startCount != 1 || endCount != 1 {
		return 0, 0, false, fmt.Errorf("AGENTS.md managed markers are duplicated or incomplete")
	}
	startIndex := strings.Index(content, puaPromptStart)
	endMarkerIndex := strings.Index(content, puaPromptEnd)
	if endMarkerIndex < startIndex {
		return 0, 0, false, fmt.Errorf("AGENTS.md managed end marker appears before start marker")
	}
	return startIndex, endMarkerIndex + len(puaPromptEnd), true, nil
}

func puaPromptBlock(language string) string {
	return puaPromptStart + "\n" + workspaceAgentsPromptForLanguage(language) + puaPromptEnd
}

func findEnclosingWorkspaceRoot(start string) (string, error) {
	cwd, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if hasWorkspaceConfig(cwd) || isDir(filepath.Join(cwd, reposDir)) {
			return cwd, nil
		}
		parent := filepath.Dir(cwd)
		if parent == cwd {
			return "", nil
		}
		cwd = parent
	}
}

const (
	puaPromptStart = "<!-- managed by pua cli -->"
	puaPromptEnd   = "<!-- end of pua cli prompt -->"
)
