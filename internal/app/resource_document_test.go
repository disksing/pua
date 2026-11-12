package app

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResourceDocumentsCreateAndMigrate(t *testing.T) {
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
			for _, document := range []string{workspaceMarkdownFile, filepath.Join(pd.Path, projectMDFile), filepath.Join(td.Path, taskMDFile)} {
				data, err := os.ReadFile(filepath.Join(root, document))
				if err != nil || !bytes.Contains(data, []byte(followupMarkdown(language))) {
					t.Fatalf("missing follow-ups in %s: %v", document, err)
				}
				if _, err := os.Lstat(filepath.Join(root, filepath.Dir(document), legacyChatMarkdownFile)); !os.IsNotExist(err) {
					t.Fatalf("new resource created a legacy document: %s: %v", document, err)
				}
			}
			if _, err := ws.ArchiveResource(task.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := ws.ArchiveResource(project.ID); err != nil {
				t.Fatal(err)
			}
			pd, _ = ws.Resource(project.ID)
			td, _ = ws.Resource(task.ID)
			documents := []string{workspaceMarkdownFile, filepath.Join(pd.Path, projectMDFile), filepath.Join(td.Path, taskMDFile), filepath.Join(schedulerDir, schedulerMarkdownFile)}
			for _, document := range documents {
				path := filepath.Join(root, document)
				if err := os.WriteFile(path, []byte("Keep background exactly.\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), legacyChatMarkdownFile), []byte("Pending reply\nHistory: source-42\nPublished: no\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := ws.Migrate(language); err != nil {
					t.Fatal(err)
				}
			}
			for _, document := range documents {
				path := filepath.Join(root, document)
				data, err := os.ReadFile(path)
				if err != nil || !bytes.HasPrefix(data, []byte("Keep background exactly.\n")) ||
					bytes.Count(data, []byte("Pending reply\nHistory: source-42\nPublished: no\n")) != 1 {
					t.Fatalf("lost or duplicated content in %s: %q %v", path, data, err)
				}
				if _, err := os.Lstat(filepath.Join(filepath.Dir(path), legacyChatMarkdownFile)); !os.IsNotExist(err) {
					t.Fatalf("legacy document remains: %s: %v", path, err)
				}
				agents, err := os.ReadFile(filepath.Join(filepath.Dir(path), "AGENTS.md"))
				if err != nil || !bytes.Contains(agents, []byte(filepath.Base(document))) {
					t.Fatalf("guidance not migrated: %s: %v", document, err)
				}
			}
			for _, id := range []string{project.ID, task.ID} {
				detail, err := ws.Resource(id)
				if err != nil || len(detail.Files) != 2 || detail.Files[1].Name != "AGENTS.md" {
					t.Fatalf("unexpected resource files: %#v %v", detail, err)
				}
			}
			guidance, _ := os.ReadFile(filepath.Join(root, "AGENTS.md"))
			if !bytes.Contains(guidance, []byte("400")) || !bytes.Contains(guidance, []byte("AGENTS.md")) {
				t.Fatal("lost collaboration conciseness guidance")
			}
		})
	}
}

func TestResourceDocumentMigrationCasesAndRecovery(t *testing.T) {
	for _, target := range []string{"missing", "", "Background\n", "<!-- pua:merged-chat:incomplete -->"} {
		for _, source := range []string{"missing", "", "Pending\n\nHistory ref\nNo trailing newline", "Pending\r\n"} {
			t.Run(target+"/"+source, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, taskMDFile)
				legacy := filepath.Join(dir, legacyChatMarkdownFile)
				if target != "missing" {
					if err := os.WriteFile(path, []byte(target), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if source != "missing" {
					if err := os.WriteFile(legacy, []byte(source), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				if err := ensureResourceDocument(dir, taskMDFile, "Initial\n"); err != nil {
					t.Fatal(err)
				}
				first, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				expectedPrefix := target
				if target == "missing" {
					expectedPrefix = "Initial\n"
				}
				if !bytes.HasPrefix(first, []byte(expectedPrefix)) || source != "missing" && !bytes.Contains(first, []byte(source)) {
					t.Fatalf("content not preserved: %q", first)
				}
				if source != "missing" {
					if err := os.WriteFile(legacy, []byte(source), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				if err := ensureResourceDocument(dir, taskMDFile, "Replacement"); err != nil {
					t.Fatal(err)
				}
				second, _ := os.ReadFile(path)
				if !bytes.Equal(first, second) {
					t.Fatalf("interrupted removal duplicated merge: %q", second)
				}
				if _, err := os.Lstat(legacy); !os.IsNotExist(err) {
					t.Fatalf("source not retired: %v", err)
				}
				if info, err := os.Stat(path); err != nil || target != "missing" && info.Mode().Perm() != 0o600 {
					t.Fatalf("lost target permissions: %v %v", info, err)
				}
			})
		}
	}
}

func TestResourceDocumentMigrationRejectsUnsafePathsWithoutLosingSource(t *testing.T) {
	for _, name := range []string{taskMDFile, legacyChatMarkdownFile} {
		for _, symlink := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/symlink=%v", name, symlink), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, name)
				if symlink {
					if err := os.Symlink(filepath.Join(dir, "missing"), path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
				source := filepath.Join(dir, legacyChatMarkdownFile)
				if name != legacyChatMarkdownFile {
					if err := os.WriteFile(source, []byte("Keep pending"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				if err := ensureResourceDocument(dir, taskMDFile, "Initial"); err == nil {
					t.Fatal("accepted unsafe document")
				}
				if _, err := os.Lstat(source); err != nil {
					t.Fatal("removed legacy source on failed migration")
				}
			})
		}
	}
}

func TestRefreshGuidanceMigratesDocumentsAndNeverRecreatesChat(t *testing.T) {
	ws, err := Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	project, _ := ws.CreateProject("Project", "")
	task, _ := ws.CreateTask(CreateTaskInput{ProjectID: project.ID, Title: "Task"})
	detail, _ := ws.Resource(task.ID)
	path := filepath.Join(ws.Root(), detail.Path)
	source := filepath.Join(path, legacyChatMarkdownFile)
	if err := os.WriteFile(source, []byte("Late pending"), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		for _, id := range []string{"workspace", project.ID, task.ID, SchedulerResourceID} {
			if err := ws.RefreshTurnGuidance(id); err != nil {
				t.Fatal(err)
			}
		}
	}
	data, _ := os.ReadFile(filepath.Join(path, taskMDFile))
	if strings.Count(string(data), "Late pending") != 1 {
		t.Fatal("late legacy write was not migrated")
	}
	if err := filepath.WalkDir(ws.Root(), func(path string, entry os.DirEntry, err error) error {
		if entry != nil && entry.Name() == legacyChatMarkdownFile {
			t.Fatalf("recreated chat.md: %s", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentMigrationSkipsRepositoriesAndArtifacts(t *testing.T) {
	ws, err := Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{reposDir, "worktree", "artifacts", "wiki", "templates", ".pua", ".git"} {
		path := filepath.Join(ws.Root(), directory, "fixture")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, projectJSONFile), []byte("invalid metadata"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, legacyChatMarkdownFile), []byte("Keep external source"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := ws.Migrate("en"); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{reposDir, "worktree", "artifacts", "wiki", "templates", ".pua", ".git"} {
		content, err := os.ReadFile(filepath.Join(ws.Root(), directory, "fixture", legacyChatMarkdownFile))
		if err != nil || string(content) != "Keep external source" {
			t.Fatalf("migration touched unrelated source: %s: %v", directory, err)
		}
	}
}
