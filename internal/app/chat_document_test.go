package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChatDocumentsCreateAndMigrateWithoutOverwriting(t *testing.T) {
	for _, language := range []string{"en", "zh-CN"} {
		t.Run(language, func(t *testing.T) {
			root := t.TempDir()
			ws, err := Initialize(root, language)
			if err != nil {
				t.Fatal(err)
			}
			project, err := ws.CreateProject("Project", "")
			if err != nil {
				t.Fatal(err)
			}
			task, err := ws.CreateTask(CreateTaskInput{ProjectID: project.ID, Title: "Task"})
			if err != nil {
				t.Fatal(err)
			}
			pd, _ := ws.Resource(project.ID)
			td, _ := ws.Resource(task.ID)
			paths := []string{root, filepath.Join(root, pd.Path), filepath.Join(root, td.Path)}
			for _, dir := range paths {
				data, err := os.ReadFile(filepath.Join(dir, chatMarkdownFile))
				if err != nil || !strings.Contains(string(data), "History") || !strings.Contains(string(data), "publish") {
					t.Fatalf("missing document: %s %v", data, err)
				}
				if language == "en" && !strings.Contains(string(data), "# Chat follow-ups") {
					t.Fatal("wrong language")
				}
			}
			if err := os.WriteFile(filepath.Join(root, chatMarkdownFile), []byte("Keep my pending reply"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(paths[1], chatMarkdownFile)); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(paths[2], chatMarkdownFile)); err != nil {
				t.Fatal(err)
			}
			if _, err := ws.ArchiveResource(task.ID); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := ws.Migrate(language); err != nil {
					t.Fatal(err)
				}
			}
			data, _ := os.ReadFile(filepath.Join(root, chatMarkdownFile))
			if string(data) != "Keep my pending reply" {
				t.Fatal("overwrote existing document")
			}
			pd, _ = ws.Resource(project.ID)
			found := false
			for _, file := range pd.Files {
				if file.Name == chatMarkdownFile {
					found = true
				}
			}
			if !found {
				t.Fatal("missing migrated document in resource detail")
			}
			td, _ = ws.Resource(task.ID)
			if _, err := os.Stat(filepath.Join(root, td.Path, chatMarkdownFile)); !os.IsNotExist(err) {
				t.Fatalf("wrote archived resource: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "scheduler", chatMarkdownFile)); !os.IsNotExist(err) {
				t.Fatal("created scheduler document")
			}
		})
	}
}

func TestChatDocumentRejectsNonRegularExistingPath(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		dir := t.TempDir()
		path := filepath.Join(dir, chatMarkdownFile)
		if symlink {
			if err := os.Symlink(filepath.Join(dir, "missing"), path); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := ensureChatDocument(dir, "en"); err == nil {
			t.Fatal("accepted non-regular document")
		}
	}
}
