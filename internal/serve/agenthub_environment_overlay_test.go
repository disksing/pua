package serve

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func resumeOverlayTestPlan(record generationRecord) GenerationLifecyclePlan {
	return GenerationLifecyclePlan{
		Operation:    GenerationOperationResumeSession,
		OperationID:  "resume-overlay-preflight",
		GenerationID: record.GenerationID,
		SessionID:    record.AgentHubSessionID,
		Guard: GenerationLifecycleGuard{
			ResourceID:   record.ResourceID,
			GenerationID: record.GenerationID,
			SessionID:    record.AgentHubSessionID,
		},
	}
}

func assertWorkspaceSecretAbsent(t *testing.T, workspacePath, secret string) {
	t.Helper()
	if secret == "" {
		return
	}
	err := filepath.WalkDir(filepath.Join(workspacePath, ".pua"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("secret persisted in Workspace file %s", filepath.Base(path))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func unsetResumeOverlayTestEnvironment(t *testing.T, key string) {
	t.Helper()
	previous, present := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(key, previous)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestResumeEnvironmentOverlayPreflightPreservesStoppedBoundary(t *testing.T) {
	tests := []struct {
		name      string
		handler   func(*runtimeFakeAgentHub) http.Handler
		bindings  func(*testing.T, serveWorkspace) string
		wantError string
	}{
		{
			name: "binding resolution",
			handler: func(fake *runtimeFakeAgentHub) http.Handler {
				capabilities := append(append([]string(nil), requiredAgentHubCapabilities...), agentHubEphemeralEnvironmentCapability)
				return runtimeFakeAgentHubWithCapabilities(fake, capabilities)
			},
			bindings: func(t *testing.T, workspace serveWorkspace) string {
				const secret = "resume-binding-preflight-secret"
				t.Setenv("PUA_SECRET_ROUND3_RESUME_KNOWN", secret)
				unsetResumeOverlayTestEnvironment(t, "round3-resume-binding-absent")
				unsetResumeOverlayTestEnvironment(t, "PUA_SECRET_ROUND3_RESUME_BINDING_ABSENT")
				if err := writeServiceJSON(serviceBindingsPath(workspace.Path), ServiceBindings{
					SchemaVersion: serviceSchemaVersion,
					Secrets: map[string]string{
						"KNOWN_TOKEN":   "${secret.round3-resume-known}",
						"MISSING_TOKEN": "${secret.round3-resume-binding-absent}",
					},
				}, 0o600); err != nil {
					t.Fatal(err)
				}
				return secret
			},
			wantError: "is unavailable",
		},
		{
			name:    "status absent",
			handler: func(fake *runtimeFakeAgentHub) http.Handler { return fake },
			bindings: func(t *testing.T, workspace serveWorkspace) string {
				const secret = "resume-status-preflight-secret"
				writeRuntimeServiceBindings(t, workspace, secret)
				return secret
			},
		},
		{
			name: "capability absent",
			handler: func(fake *runtimeFakeAgentHub) http.Handler {
				return runtimeFakeAgentHubWithCapabilities(fake, append([]string(nil), requiredAgentHubCapabilities...))
			},
			bindings: func(t *testing.T, workspace serveWorkspace) string {
				const secret = "resume-capability-preflight-secret"
				writeRuntimeServiceBindings(t, workspace, secret)
				return secret
			},
			wantError: "does not support ephemeral service secrets",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newRuntimeFakeAgentHub()
			hub := httptest.NewServer(test.handler(fake))
			defer hub.Close()
			manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
			secret := test.bindings(t, workspace)
			record := idleTestGeneration(workspace, "project1.task1", "gen-overlay-preflight", "ses-overlay-preflight", manager.resourceNow())
			record.Status = "stopped"
			record.IdleSinceAt = ""
			record.IdleDeadlineAt = ""
			record.AgentHubStoppedObserved = true
			previousReceipt := GenerationLifecycleReceipt{
				Operation: GenerationOperationStopSession, State: GenerationReceiptSucceeded,
				OperationID: "stop-overlay-preflight", GenerationID: record.GenerationID, SessionID: record.AgentHubSessionID,
			}
			record.LifecycleReceipt = &previousReceipt
			seedIdleTestGeneration(t, fake, workspace, record, "stopped")
			_, client, err := manager.agentHubRuntimeConfig()
			if err != nil {
				t.Fatal(err)
			}
			rt := manager.ensureRuntime(workspace, record, client)

			resumed, terminal, resumeErr := manager.resumeStoppedGenerationLocked(
				context.Background(), workspace, record, rt, client, resumeOverlayTestPlan(record),
			)
			if resumeErr == nil || resumed || terminal {
				t.Fatalf("Resume preflight result = resumed %v, terminal %v, error %v", resumed, terminal, resumeErr)
			}
			if test.wantError != "" && !strings.Contains(resumeErr.Error(), test.wantError) {
				t.Fatalf("Resume preflight error = %v, want %q", resumeErr, test.wantError)
			}
			current, err := loadGenerationRecord(workspace.Path, record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status != "stopped" || !current.AgentHubStoppedObserved ||
				!reflect.DeepEqual(current.LifecycleReceipt, &previousReceipt) {
				t.Fatalf("failed preflight changed stopped evidence: %#v", current)
			}
			if current.ResumeFailureCount != 0 || current.ResumeRetryAt != "" || current.ResumeLastError != "" {
				t.Fatalf("failed preflight recorded a remote Resume failure: %#v", current)
			}
			fake.mu.Lock()
			resumeAttempts := len(fake.resumeEnvironments)
			fake.mu.Unlock()
			if resumeAttempts != 0 {
				t.Fatalf("failed preflight issued %d Resume requests", resumeAttempts)
			}
			assertWorkspaceSecretAbsent(t, workspace.Path, secret)
		})
	}
}

func TestResumeEnvironmentOverlayMatchesCreate(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	capabilities := append(append([]string(nil), requiredAgentHubCapabilities...), agentHubEphemeralEnvironmentCapability)
	hub := httptest.NewServer(runtimeFakeAgentHubWithCapabilities(fake, capabilities))
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	const secret = "resume-overlay-parity-secret"
	writeRuntimeServiceBindings(t, workspace, secret)
	cfg, client, err := manager.agentHubRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := manager.resolveResourceAgent(workspace, "project1.task1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.createResourceGeneration(context.Background(), workspace, "project1.task1", workspace.Path, cfg, client, resolved)
	if err != nil {
		t.Fatal(err)
	}
	rt := manager.runtimeByID(record.ID)
	if rt == nil {
		t.Fatal("created generation has no runtime")
	}
	stopReceipt := GenerationLifecycleReceipt{
		Operation: GenerationOperationStopSession, State: GenerationReceiptSucceeded,
		OperationID: "stop-overlay-parity", GenerationID: record.GenerationID, SessionID: record.AgentHubSessionID,
	}
	record, err = rt.mutateGeneration(func(current *generationRecord) {
		current.Status = "stopped"
		current.AgentHubStoppedObserved = true
		current.LifecycleReceipt = &stopReceipt
	})
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	session := fake.sessions[record.AgentHubSessionID]
	session.State = "stopped"
	fake.sessions[session.ID] = session
	fake.mu.Unlock()

	resumed, terminal, err := manager.resumeStoppedGenerationLocked(
		context.Background(), workspace, record, rt, client, resumeOverlayTestPlan(record),
	)
	if err != nil || !resumed || terminal {
		t.Fatalf("Resume result = resumed %v, terminal %v, error %v", resumed, terminal, err)
	}
	fake.mu.Lock()
	createRequests := append([]agentHubCreateSessionRequest(nil), fake.createRequests...)
	resumeEnvironments := append([]map[string]string(nil), fake.resumeEnvironments...)
	resumeEphemeralEnvironments := append([]map[string]string(nil), fake.resumeSecrets...)
	resumedSession := fake.sessions[record.AgentHubSessionID]
	fake.mu.Unlock()
	if len(createRequests) != 1 || len(resumeEnvironments) != 1 || len(resumeEphemeralEnvironments) != 1 {
		t.Fatalf("overlay requests = create %d, resume %d/%d", len(createRequests), len(resumeEnvironments), len(resumeEphemeralEnvironments))
	}
	created := createRequests[0]
	if created.LaunchEnvironment["PUBLIC_ENDPOINT"] != "http://service.test" ||
		resumeEnvironments[0]["PUBLIC_ENDPOINT"] != created.LaunchEnvironment["PUBLIC_ENDPOINT"] {
		t.Fatalf("durable service overlay diverged: create=%#v resume=%#v", created.LaunchEnvironment, resumeEnvironments[0])
	}
	if created.EphemeralEnvironment["SERVICE_TOKEN"] != secret ||
		resumeEphemeralEnvironments[0]["SERVICE_TOKEN"] != created.EphemeralEnvironment["SERVICE_TOKEN"] {
		t.Fatalf("ephemeral service overlay diverged: create=%#v resume=%#v", created.EphemeralEnvironment, resumeEphemeralEnvironments[0])
	}
	for key, want := range map[string]string{
		"PUA_WORKSPACE_ROOT":        workspace.Path,
		"PUA_WORKSPACE_INSTANCE_ID": record.SourceInstanceID,
		"PUA_RESOURCE_ID":           record.ResourceID,
	} {
		if created.LaunchEnvironment[key] != want || resumedSession.LaunchEnvironment[key] != want {
			t.Fatalf("provenance %s was not preserved: create=%#v resumed=%#v", key, created.LaunchEnvironment, resumedSession.LaunchEnvironment)
		}
	}
	assertWorkspaceSecretAbsent(t, workspace.Path, secret)
}
