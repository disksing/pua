package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResourceDocumentsInitializeAndRefresh(t *testing.T) {
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
			for _, id := range []string{task.ID, project.ID} {
				if _, err := ws.ArchiveResource(id); err != nil {
					t.Fatal(err)
				}
			}

			projectDetail, err := ws.Resource(project.ID)
			if err != nil {
				t.Fatal(err)
			}
			taskDetail, err := ws.Resource(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			directories := map[string]string{
				root:                                    workspaceMarkdownFile,
				filepath.Join(root, projectDetail.Path): projectMDFile,
				filepath.Join(root, taskDetail.Path):    taskMDFile,
			}
			const originalMemory = "Keep Memory and its source markers exactly.\n"
			const legacyBody = "Unimported legacy contents.\n"
			for directory, legacyName := range directories {
				specPath := filepath.Join(directory, SpecMarkdownFile)
				memoryPath := filepath.Join(directory, MemoryMarkdownFile)
				memory, err := os.ReadFile(memoryPath)
				if err != nil || string(memory) != followupMarkdown(language) {
					t.Fatalf("missing initial Memory in %s: %s (%v)", directory, memory, err)
				}
				if err := os.Remove(specPath); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(memoryPath, []byte(originalMemory), 0o640); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{legacyName, legacyChatMarkdownFile} {
					path := filepath.Join(directory, name)
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatalf("resource creation wrote %s: %v", name, err)
					}
					if err := os.WriteFile(path, []byte(legacyBody), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}

			for range 2 {
				if err := ws.Migrate(language); err != nil {
					t.Fatal(err)
				}
			}
			for directory, legacyName := range directories {
				spec, err := os.ReadFile(filepath.Join(directory, SpecMarkdownFile))
				if err != nil || len(spec) == 0 || strings.Contains(string(spec), legacyBody) {
					t.Fatalf("invalid initialized Spec in %s: %s (%v)", directory, spec, err)
				}
				for name, want := range map[string]string{
					MemoryMarkdownFile: originalMemory, legacyName: legacyBody, legacyChatMarkdownFile: legacyBody,
				} {
					data, err := os.ReadFile(filepath.Join(directory, name))
					if err != nil || string(data) != want {
						t.Fatalf("refresh changed %s in %s: %q (%v)", name, directory, data, err)
					}
				}
			}
			for _, id := range []string{project.ID, task.ID} {
				detail, err := ws.Resource(id)
				if err != nil || len(detail.Files) != 3 || detail.Files[0].Name != SpecMarkdownFile ||
					detail.Files[1].Name != MemoryMarkdownFile || detail.Files[2].Name != "AGENTS.md" {
					t.Fatalf("unexpected resource files: %#v %v", detail, err)
				}
			}
		})
	}
}

func TestCreateDocumentIfMissingRejectsUnsafePaths(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		if err := createDocumentIfMissing(t.TempDir(), "Initial"); err == nil {
			t.Fatal("accepted a directory as a document")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		link := filepath.Join(dir, SpecMarkdownFile)
		if err := os.Symlink(filepath.Join(dir, "missing"), link); err != nil {
			t.Fatal(err)
		}
		if err := createDocumentIfMissing(link, "Initial"); err == nil {
			t.Fatal("accepted a symlink as a document")
		}
	})
}

func TestRefreshGuidanceSkipsRepositoriesAndArtifacts(t *testing.T) {
	ws, err := Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{reposDir, "worktree", "artifacts", "wiki", "templates", ".pua", ".git", legacyDocumentStage} {
		path := filepath.Join(ws.Root(), directory, "fixture")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, projectJSONFile), []byte("invalid metadata"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := ws.Migrate("en"); err != nil {
		t.Fatal(err)
	}
}
