package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/disksing/pua/internal/localize"
)

const chatMarkdownFile = "chat.md"

// ensureChatDocument creates a missing document without replacing Agent content.
func ensureChatDocument(dir, language string) error {
	path := filepath.Join(dir, chatMarkdownFile)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("chat document must be a regular file: %s", path)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return ensureChatDocument(dir, language)
		}
		return err
	}
	if _, err := file.WriteString(localize.MustRender(language, "chat.md", nil)); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
