package app

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/disksing/pua/internal/localize"
)

const SpecMarkdownFile = "spec.md"
const MemoryMarkdownFile = "memory.md"
const legacyDocumentStage = ".pua-legacy-documents"

func (w *Workspace) WithResourceDocumentMutation(update func() error) error {
	return withWorkspaceMutationLock(w.root, update)
}

func ensureSplitDocuments(dir, legacyName, spec, language string) error {
	stage := filepath.Join(dir, legacyDocumentStage)
	stageInfo, err := os.Lstat(stage)
	if err == nil && !stageInfo.IsDir() {
		return fmt.Errorf("resource document staging path must be a directory: %s", stage)
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	names := []string{legacyName, legacyChatMarkdownFile}
	memoryInitial := followupMarkdown(language)
	legacySource := ""
	for _, sourceDir := range []string{dir, stage} {
		for _, name := range names {
			content, _, err := readRegularDocument(filepath.Join(sourceDir, name))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if legacySource == "" || name == legacyName {
				legacySource = name
			}
			for _, line := range bytes.Split(content, []byte("\n")) {
				switch string(bytes.TrimSuffix(line, []byte("\r"))) {
				case "## Chat follow-ups", "## 对话事项":
					memoryInitial = ""
				}
			}
		}
	}
	if legacySource != "" {
		spec = localize.MustRender(language, "migrated-spec.md", map[string]string{"Source": legacySource})
	}
	if err := createDocumentIfMissing(filepath.Join(dir, SpecMarkdownFile), spec); err != nil {
		return err
	}
	memory := filepath.Join(dir, MemoryMarkdownFile)
	if err := createDocumentIfMissing(memory, memoryInitial); err != nil {
		return err
	}
	for _, name := range names {
		if err := retireLegacyDocument(dir, stage, name, memory); err != nil {
			return err
		}
	}
	if err := os.Remove(stage); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func createDocumentIfMissing(path, initial string) error {
	if _, _, err := readRegularDocument(path); !os.IsNotExist(err) {
		return err
	}
	return writeDocumentAtomicChecked(path, []byte(initial), 0o644, func() error {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			if err != nil {
				return err
			}
			return fmt.Errorf("resource document appeared during initialization: %s", path)
		}
		return nil
	})
}

func retireLegacyDocument(dir, stage, name, memory string) error {
	source := filepath.Join(dir, name)
	pending := filepath.Join(stage, name)
	if err := importStagedDocument(pending, name, memory); err != nil {
		return err
	}
	if _, _, err := readRegularDocument(source); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.Mkdir(stage, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	if err := os.Rename(source, pending); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return err
	}
	if err := syncDirectory(stage); err != nil {
		return err
	}
	return importStagedDocument(pending, name, memory)
}

func importStagedDocument(pending, name, memory string) error {
	legacy, _, err := readRegularDocument(pending)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	current, mode, err := readRegularDocument(memory)
	if err != nil {
		return err
	}
	next, err := mergeLegacyDocument(current, name, legacy)
	if err != nil {
		return fmt.Errorf("merge staged legacy document %s: %w", pending, err)
	}
	if !bytes.Equal(current, next) {
		if err := writeDocumentAtomicChecked(memory, next, mode, func() error {
			return verifyDocumentContent(memory, current)
		}); err != nil {
			return err
		}
	}
	if err := verifyDocumentContent(memory, next); err != nil {
		return err
	}
	if err := verifyDocumentContent(pending, legacy); err != nil {
		return err
	}
	if err := os.Remove(pending); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(pending))
}

func verifyDocumentContent(path string, expected []byte) error {
	persisted, _, err := readRegularDocument(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(persisted, expected) {
		return fmt.Errorf("resource document changed during migration: %s", path)
	}
	return nil
}

func mergeLegacyDocument(memory []byte, name string, legacy []byte) ([]byte, error) {
	hash := markdownContentHash(legacy)
	prefix := []byte("<!-- pua:legacy-document:" + name + ":")
	startMarker := func(digest string) []byte {
		return []byte(string(prefix) + digest + " -->\n")
	}
	endMarker := func(digest string) []byte {
		return []byte("\n<!-- pua:legacy-document-end:" + name + ":" + digest + " -->")
	}
	block := bytes.Join([][]byte{startMarker(hash), legacy, endMarker(hash)}, nil)
	start := bytes.Index(memory, prefix)
	if start < 0 {
		if name == legacyChatMarkdownFile && bytes.Contains(memory, mergedChatBlock(legacy)) {
			return memory, nil
		}
		return bytes.Join([][]byte{memory, []byte("\n\n"), block, []byte("\n")}, nil), nil
	}
	digestStart := start + len(prefix)
	digestEnd := digestStart + 64
	if digestEnd > len(memory) {
		return nil, fmt.Errorf("incomplete migration marker in %s", MemoryMarkdownFile)
	}
	previous := string(memory[digestStart:digestEnd])
	header, footer := startMarker(previous), endMarker(previous)
	if !bytes.HasPrefix(memory[start:], header) {
		return nil, fmt.Errorf("invalid migration marker in %s", MemoryMarkdownFile)
	}
	bodyStart := start + len(header)
	bodyLength := bytes.Index(memory[bodyStart:], footer)
	if bodyLength < 0 {
		return nil, fmt.Errorf("missing migration end marker in %s", MemoryMarkdownFile)
	}
	bodyEnd := bodyStart + bodyLength
	if previous == hash {
		return memory, nil
	}
	body := memory[bodyStart:bodyEnd]
	if !bytes.Equal(body, legacy) && markdownContentHash(body) != previous {
		return nil, fmt.Errorf("both the legacy source and its imported block in %s changed", MemoryMarkdownFile)
	}
	return bytes.Join([][]byte{memory[:start], block, memory[bodyEnd+len(footer):]}, nil), nil
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
