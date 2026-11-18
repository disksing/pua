package app_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

const v2Template = `---
schema-version: 2
title: Request or bug
description: Capture a concrete change.
task-title: "{{ summary }}"
fields:
  - name: summary
    type: text
    label: Summary
    required: true
  - name: behavior
    type: textarea
    label: Expected behavior
    required: true
  - name: priority
    type: select
    label: Priority
    default: medium
    options: [low, medium, high]
  - name: verified
    type: boolean
    label: Already reproduced
    default: false
---
# {{ summary }}

{{ behavior }}

Priority: {{ priority }}
Verified: {{ verified }}
`

func templateWorkspace(t *testing.T) (*app.Workspace, string, app.Project) {
	t.Helper()
	root := t.TempDir()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("Template project", "templates")
	if err != nil {
		t.Fatal(err)
	}
	return workspace, root, project
}

func writeTemplate(t *testing.T, root string, project app.Project, name, content string) string {
	t.Helper()
	projects, err := filepath.Glob(filepath.Join(root, "project1*"))
	if err != nil || len(projects) != 1 {
		t.Fatalf("resolve project path: %v %#v", err, projects)
	}
	path := filepath.Join(projects[0], "templates", name+".md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = project
	return path
}

func TestTemplateV2RenderIsTypedDeterministicAndSinglePass(t *testing.T) {
	workspace, root, project := templateWorkspace(t)
	writeTemplate(t, root, project, "request", v2Template)

	templates, err := workspace.Templates(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 1 || !templates[0].Valid || templates[0].SchemaVersion != 2 || len(templates[0].Fields) != 4 {
		t.Fatalf("unexpected template: %#v", templates)
	}
	input := app.TemplateRenderInput{ProjectID: project.ID, Name: "request", Fields: map[string]any{
		"summary": "Keep {{ priority }} literal", "behavior": "First line\nSecond line", "verified": true,
	}}
	first, err := workspace.RenderTemplate(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := workspace.RenderTemplate(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest || first.Markdown != second.Markdown || first.Title != second.Title {
		t.Fatalf("render is not deterministic: %#v %#v", first, second)
	}
	if first.Title != "Keep {{ priority }} literal" || !strings.Contains(first.Markdown, "# Keep {{ priority }} literal") {
		t.Fatalf("field values were unexpectedly reparsed: %#v", first)
	}
	if !strings.Contains(first.Markdown, "Priority: medium") || !strings.Contains(first.Markdown, "Verified: true") || !strings.HasSuffix(first.Markdown, "\n") {
		t.Fatalf("defaults or normalized output missing: %q", first.Markdown)
	}
}

func TestTemplateValidationReportsInvalidEntriesAndFieldPaths(t *testing.T) {
	workspace, root, project := templateWorkspace(t)
	writeTemplate(t, root, project, "a-invalid", `---
schema-version: 2
title: Invalid
autorun: true
fields:
  - name: choice
    type: select
    label: Choice
    options: []
---
{{ missing }}
`)
	writeTemplate(t, root, project, "z-valid", v2Template)

	templates, err := workspace.Templates(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 2 || templates[0].Name != "a-invalid" || templates[1].Name != "z-valid" {
		t.Fatalf("templates are not stably sorted and complete: %#v", templates)
	}
	if templates[0].Valid || len(templates[0].Errors) < 2 {
		t.Fatalf("invalid template diagnostics missing: %#v", templates[0])
	}
	codes := map[string]bool{}
	for _, issue := range templates[0].Errors {
		codes[issue.Code] = true
		if issue.Severity != "error" {
			t.Fatalf("unexpected issue severity: %#v", issue)
		}
	}
	for _, code := range []string{"unknown_property", "invalid_options"} {
		if !codes[code] {
			t.Fatalf("missing %s in %#v", code, templates[0].Errors)
		}
	}
}

func TestTemplateValidationRejectsDuplicateProperties(t *testing.T) {
	workspace, root, project := templateWorkspace(t)
	writeTemplate(t, root, project, "duplicate", `---
schema-version: 2
title: First
title: Second
fields:
  - name: summary
    name: repeated
    type: text
    label: Summary
---
Body
`)
	template, err := workspace.Template(project.ID, "duplicate")
	if err != nil {
		t.Fatal(err)
	}
	if template.Valid {
		t.Fatal("duplicate YAML properties should be rejected")
	}
	var paths []string
	for _, issue := range template.Errors {
		if issue.Code == "duplicate_property" {
			paths = append(paths, issue.Path)
		}
	}
	if strings.Join(paths, ",") != "title,fields[0].name" {
		t.Fatalf("unexpected duplicate property diagnostics: %#v", template.Errors)
	}
}

func TestTemplateRenderRejectsMissingUnknownAndTypedFields(t *testing.T) {
	workspace, root, project := templateWorkspace(t)
	writeTemplate(t, root, project, "request", v2Template)
	_, err := workspace.RenderTemplate(app.TemplateRenderInput{ProjectID: project.ID, Name: "request", Fields: map[string]any{
		"summary": " ", "behavior": true, "extra": "typo",
	}})
	if err == nil {
		t.Fatal("expected typed field validation failure")
	}
	var apiErr *app.APIError
	var validation *app.TemplateValidationError
	if !errors.As(err, &apiErr) || !errors.As(err, &validation) {
		t.Fatalf("expected structured errors, got %T %v", err, err)
	}
	codes := map[string]bool{}
	for _, issue := range validation.Issues {
		codes[issue.Code] = true
	}
	for _, code := range []string{"required_field", "field_type", "unknown_field"} {
		if !codes[code] {
			t.Fatalf("missing %s in %#v", code, validation.Issues)
		}
	}
}

func TestCreateTaskFromTemplateRecordsSource(t *testing.T) {
	workspace, root, project := templateWorkspace(t)
	writeTemplate(t, root, project, "request", v2Template)
	fields := map[string]any{"summary": "Structured task", "behavior": "Create it safely"}
	preview, err := workspace.PreviewTask(app.CreateTaskInput{ProjectID: project.ID, TemplateName: "request", TemplateFields: fields})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Template == nil {
		t.Fatalf("template source missing from preview: %#v", preview)
	}
	task, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, TemplateName: "request", TemplateFields: fields, ExpectedTemplateDigest: preview.Template.Digest})
	if err != nil {
		t.Fatal(err)
	}
	if task.Template == nil || task.Template.Name != "request" || task.Template.SchemaVersion != 2 || task.Template.Digest != preview.Template.Digest {
		t.Fatalf("unexpected template source: %#v", task)
	}
	detail, err := workspace.Resource(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Template == nil || detail.Template.Digest != preview.Template.Digest || task.Path == "" {
		t.Fatalf("template source or create response path missing: %#v %#v", detail.Template, task)
	}
	markdown, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(detail.Path), app.SpecMarkdownFile))
	if err != nil || string(markdown) != preview.Markdown {
		t.Fatalf("preview and created markdown differ: %v %q %q", err, markdown, preview.Markdown)
	}
	metadata, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(detail.Path), "task.json"))
	if err != nil || strings.Contains(string(metadata), `"path"`) || !strings.Contains(string(metadata), `"template"`) {
		t.Fatalf("task source persistence is incorrect: %v %s", err, metadata)
	}
}

func TestTemplateDigestConflictAndValidationAreAtomic(t *testing.T) {
	workspace, root, project := templateWorkspace(t)
	path := writeTemplate(t, root, project, "request", v2Template)
	preview, err := workspace.PreviewTask(app.CreateTaskInput{ProjectID: project.ID, TemplateName: "request", TemplateFields: map[string]any{"summary": "One", "behavior": "Two"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(v2Template, "Capture a concrete change.", "Changed after preview.", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = workspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, TemplateName: "request", TemplateFields: map[string]any{"summary": "One", "behavior": "Two"}, ExpectedTemplateDigest: preview.Template.Digest})
	if err == nil || !app.IsKind(err, "template_conflict") {
		t.Fatalf("expected digest conflict, got %v", err)
	}
	projects, _ := filepath.Glob(filepath.Join(root, "project1*"))
	entries, err := os.ReadDir(projects[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "task") || strings.HasPrefix(entry.Name(), ".pua-create-") {
			t.Fatalf("failed template creation left side effects: %s", entry.Name())
		}
	}
}

func TestTemplateWithoutSchemaVersionIsRejected(t *testing.T) {
	workspace, root, project := templateWorkspace(t)
	legacy := "---\ntitle: Legacy\n---\n# Keep {{ literal }} unchanged\n"
	writeTemplate(t, root, project, "legacy", legacy)
	templates, err := workspace.Templates(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if templates[0].Valid {
		t.Fatal("template without schema-version should be rejected")
	}
	var missing bool
	for _, issue := range templates[0].Errors {
		missing = missing || issue.Code == "required_property" && issue.Path == "schema-version"
	}
	if !missing {
		t.Fatalf("expected a required schema-version error: %#v", templates[0].Errors)
	}
	if _, err := workspace.RenderTemplate(app.TemplateRenderInput{ProjectID: project.ID, Name: "legacy"}); err == nil {
		t.Fatal("template without schema-version should not render")
	}
}

func TestTemplateSymlinkAndTraversalAreRejected(t *testing.T) {
	workspace, root, project := templateWorkspace(t)
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte(v2Template), 0o644); err != nil {
		t.Fatal(err)
	}
	projects, _ := filepath.Glob(filepath.Join(root, "project1*"))
	if err := os.Symlink(outside, filepath.Join(projects[0], "templates", "linked.md")); err != nil {
		t.Fatal(err)
	}
	templates, err := workspace.Templates(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 1 || templates[0].Valid || templates[0].Errors[0].Code != "template_path_unsafe" {
		t.Fatalf("symlink was not reported as invalid: %#v", templates)
	}
	if _, err := workspace.Template(project.ID, "../outside"); err == nil {
		t.Fatal("expected traversal template name rejection")
	}
}
