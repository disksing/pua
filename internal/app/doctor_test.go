package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func healthyDoctorCatalog() *DoctorBindingCatalog {
	return &DoctorBindingCatalog{
		Profiles: []DoctorProfile{
			{Key: "default", AgentName: "test-agent"},
			{Key: "fast", AgentName: "test-agent"},
			{Key: "reasoning", AgentName: "test-agent"},
		},
		Agents: []DoctorAgent{{Name: "test-agent", Available: true}},
	}
}

func doctorTestWorkspace(t *testing.T) (*Workspace, string, Project, Task) {
	t.Helper()
	root := t.TempDir()
	workspace, err := Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("Doctor project", "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := workspace.CreateTask(CreateTaskInput{ProjectID: project.ID, Title: "Doctor task"})
	if err != nil {
		t.Fatal(err)
	}
	return workspace, root, project, task
}

func issueCodes(report DoctorReport) map[string]int {
	result := make(map[string]int)
	for _, issue := range report.Issues {
		result[issue.Code]++
	}
	return result
}

func TestDoctorAcceptsHealthyOpenWorkspace(t *testing.T) {
	_, root, _, _ := doctorTestWorkspace(t)
	report, err := CheckWorkspace(root, DoctorOptions{BindingCatalog: healthyDoctorCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || len(report.Issues) != 0 || report.Summary != (DoctorSummary{}) {
		t.Fatalf("healthy report = %#v", report)
	}
}

func TestDoctorLocalizesHumanReadableIssuesWithoutChangingCodes(t *testing.T) {
	root := t.TempDir()
	if _, err := Initialize(root, "zh-CN"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	report, err := CheckWorkspace(root, DoctorOptions{BindingCatalog: healthyDoctorCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	if report.Language != "zh-CN" {
		t.Fatalf("report language = %q", report.Language)
	}
	for _, issue := range report.Issues {
		if issue.Code != "agents_file_missing" {
			continue
		}
		if issue.Message != "AGENTS.md 缺失" || !strings.Contains(issue.Suggestion, "恢复") {
			t.Fatalf("localized issue = %#v", issue)
		}
		return
	}
	t.Fatalf("agents_file_missing issue not found: %#v", report.Issues)
}

func TestDoctorDetectsModifiedManagedInstructions(t *testing.T) {
	_, root, project, task := doctorTestWorkspace(t)
	path, _, err := loadResource(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	agentsPath := filepath.Join(path, "AGENTS.md")
	data, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatal(err)
	}
	modified := strings.Replace(string(data), project.ID, "project999", 1)
	if modified == string(data) {
		t.Fatal("test did not modify the managed section")
	}
	if err := os.WriteFile(agentsPath, []byte(modified), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := CheckWorkspace(root, DoctorOptions{BindingCatalog: healthyDoctorCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	if issueCodes(report)["agents_managed_section_modified"] != 1 {
		t.Fatalf("managed instruction issue missing: %#v", report.Issues)
	}
}

func TestDoctorDetectsMissingManagedInstructions(t *testing.T) {
	_, root, _, task := doctorTestWorkspace(t)
	path, _, err := loadResource(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	agentsPath := filepath.Join(path, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte("# Local instructions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := CheckWorkspace(root, DoctorOptions{BindingCatalog: healthyDoctorCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	if issueCodes(report)["agents_managed_section_missing"] != 1 {
		t.Fatalf("missing managed instruction issue: %#v", report.Issues)
	}
}

func TestDoctorFindsMissingOpenMetadataAndSkipsArchive(t *testing.T) {
	_, root, project, task := doctorTestWorkspace(t)
	path, _, err := loadResource(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(path, taskJSONFile)); err != nil {
		t.Fatal(err)
	}
	archivedBroken := filepath.Join(root, project.ID, archiveDir, "task99-broken")
	if err := os.MkdirAll(archivedBroken, 0o755); err != nil {
		t.Fatal(err)
	}
	archivedProject := filepath.Join(root, archiveDir, "project99-broken", "task99-broken")
	if err := os.MkdirAll(archivedProject, 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := CheckWorkspace(root, DoctorOptions{BindingCatalog: healthyDoctorCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	if issueCodes(report)["task_metadata_missing"] != 1 {
		t.Fatalf("expected exactly one open missing metadata issue: %#v", report.Issues)
	}
	for _, issue := range report.Issues {
		if strings.Contains(issue.Path, "project99-broken") || strings.Contains(issue.Path, "task99-broken") {
			t.Fatalf("archive content was inspected: %#v", issue)
		}
	}
}

func TestDoctorReportsMissingDirectAgentAndIncompleteCatalog(t *testing.T) {
	workspace, root, _, task := doctorTestWorkspace(t)
	if _, err := workspace.SetResourceAgentBinding(task.ID, AgentBinding{Kind: "agent", Name: "deleted-agent"}); err != nil {
		t.Fatal(err)
	}
	report, err := CheckWorkspace(root, DoctorOptions{BindingCatalog: healthyDoctorCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	if issueCodes(report)["agent_binding_target_missing"] != 1 || report.Summary.Errors != 1 {
		t.Fatalf("missing Agent report = %#v", report)
	}
	incomplete, err := CheckWorkspace(root, DoctorOptions{BindingError: "catalog offline"})
	if err != nil {
		t.Fatal(err)
	}
	if incomplete.Complete || issueCodes(incomplete)["agent_catalog_unavailable"] != 1 {
		t.Fatalf("incomplete report = %#v", incomplete)
	}
}
