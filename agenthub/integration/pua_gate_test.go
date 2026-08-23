//go:build integration && !windows

package integration_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/disksing/pua/agenthub/internal/session"
)

var (
	repositoryRoot string
	agenthubBinary string
	providerBinary string
	buildRoot      string
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	repositoryRoot = filepath.Dir(filepath.Dir(file))
	var err error
	buildRoot, err = os.MkdirTemp("", "agenthub-pua-gate-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	agenthubBinary = filepath.Join(buildRoot, "agenthub")
	providerBinary = filepath.Join(buildRoot, "fakeprovider")
	for _, build := range []struct {
		output   string
		pkg      string
		withRace bool
	}{
		{agenthubBinary, "./cmd/agenthub", true},
		{providerBinary, "./internal/integrationtest/fakeprovider", false},
	} {
		args := []string{"build"}
		if build.withRace {
			args = append(args, "-race")
		}
		args = append(args, "-o", build.output, build.pkg)
		command := exec.Command("go", args...)
		command.Dir = repositoryRoot
		command.Stdout = os.Stderr
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n", build.pkg, err)
			_ = os.RemoveAll(buildRoot)
			os.Exit(1)
		}
	}
	status := m.Run()
	if err := os.RemoveAll(buildRoot); err != nil {
		fmt.Fprintf(os.Stderr, "remove integration build root: %v\n", err)
		if status == 0 {
			status = 1
		}
	}
	os.Exit(status)
}

type daemon struct {
	t        *testing.T
	root     string
	cwd      string
	addr     string
	endpoint string
	cmd      *exec.Cmd
	done     chan error
	stdout   *os.File
	stderr   *os.File
}

type sessionValue struct {
	ID                           string            `json:"id"`
	State                        string            `json:"state"`
	StopReason                   string            `json:"stopReason"`
	CurrentTurnID                string            `json:"currentTurnId"`
	PendingApprovalIDs           []string          `json:"pendingApprovalIds"`
	ProviderSessionID            string            `json:"providerSessionId"`
	LastEventID                  int64             `json:"lastEventId"`
	LaunchEnvironment            map[string]string `json:"launchEnvironment"`
	EphemeralEnvironmentRequired bool              `json:"ephemeralEnvironmentRequired"`
	Source                       *sourceValue      `json:"source"`
}

type sourceValue struct {
	App        string `json:"app"`
	InstanceID string `json:"instanceId"`
	ExternalID string `json:"externalId"`
}

type eventValue struct {
	ID            string          `json:"id"`
	SourceEventID int64           `json:"sourceEventId"`
	Type          string          `json:"type"`
	Data          json.RawMessage `json:"data"`
}

type frameValue struct {
	Cursor int64 `json:"cursor"`
	Source struct {
		Type string `json:"type"`
	} `json:"source"`
	Events []eventValue `json:"events"`
}

func newGate(t *testing.T) *daemon {
	t.Helper()
	root := t.TempDir()
	cwd := t.TempDir()
	writeConfig(t, root)
	return startGate(t, root, cwd)
}

func startGate(t *testing.T, root, cwd string) *daemon {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	stdout, err := os.Create(filepath.Join(root, fmt.Sprintf("daemon-%d.stdout", time.Now().UnixNano())))
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(filepath.Join(root, fmt.Sprintf("daemon-%d.stderr", time.Now().UnixNano())))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(agenthubBinary, "serve", "--addr", addr)
	command.Dir = repositoryRoot
	command.Env = append(filteredEnvironment(),
		"AGENTHUB_HOME="+root,
		"HOME="+filepath.Join(root, "fake-home"),
		"AGENTHUB_CODEX_CLI=missing-codex",
		"AGENTHUB_KIMI_CLI="+providerBinary,
		"AGENTHUB_PI_CLI=missing-pi",
		"AGENTHUB_OPENCODE_CLI=missing-opencode",
	)
	command.Stdout = stdout
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	gate := &daemon{
		t: t, root: root, cwd: cwd, addr: addr, endpoint: "http://" + addr + "/agenthub",
		cmd: command, done: make(chan error, 1), stdout: stdout, stderr: stderr,
	}
	go func() { gate.done <- command.Wait() }()
	t.Cleanup(func() {
		gate.killIfRunning()
		_ = stdout.Close()
		_ = stderr.Close()
	})
	gate.waitReady()
	return gate
}

func filteredEnvironment() []string {
	result := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		switch key {
		case "AGENTHUB_HOME", "HOME", "AGENTHUB_CODEX_CLI", "AGENTHUB_KIMI_CLI",
			"AGENTHUB_PI_CLI", "AGENTHUB_OPENCODE_CLI":
			continue
		}
		result = append(result, value)
	}
	return result
}

func writeConfig(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "config", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"version": 1,
		"agentProviders": []map[string]any{{
			"id": "fake", "name": "PUA gate fake ACP", "type": "kimi",
			"enabled": true, "command": providerBinary,
		}},
		"agents": []map[string]any{{"name": "Fake ACP", "providerId": "fake"}},
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (d *daemon) waitReady() {
	d.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(d.endpoint + "/v1/status")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case err := <-d.done:
			d.t.Fatalf("daemon exited during startup: %v\n%s", err, d.logs())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.t.Fatalf("daemon did not become ready\n%s", d.logs())
}

func (d *daemon) stop() {
	d.t.Helper()
	if d.cmd == nil || d.cmd.Process == nil {
		return
	}
	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		d.t.Fatal(err)
	}
	select {
	case err := <-d.done:
		if err != nil {
			d.t.Fatalf("daemon SIGTERM exit: %v\n%s", err, d.logs())
		}
	case <-time.After(10 * time.Second):
		d.t.Fatalf("daemon did not stop\n%s", d.logs())
	}
	d.cmd = nil
}

func (d *daemon) kill() {
	d.t.Helper()
	if d.cmd == nil || d.cmd.Process == nil {
		return
	}
	if err := d.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		d.t.Fatal(err)
	}
	select {
	case <-d.done:
	case <-time.After(5 * time.Second):
		d.t.Fatal("daemon SIGKILL did not complete")
	}
	d.cmd = nil
}

func (d *daemon) killIfRunning() {
	if d.cmd == nil || d.cmd.Process == nil {
		return
	}
	_ = d.cmd.Process.Kill()
	select {
	case <-d.done:
	case <-time.After(2 * time.Second):
	}
	d.cmd = nil
}

func (d *daemon) logs() string {
	_ = d.stdout.Sync()
	_ = d.stderr.Sync()
	var result strings.Builder
	for _, path := range []string{d.stdout.Name(), d.stderr.Name()} {
		data, _ := os.ReadFile(path)
		result.Write(data)
	}
	return result.String()
}

func (d *daemon) request(method, path string, body any) (int, map[string]any) {
	d.t.Helper()
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			d.t.Fatal(err)
		}
		input = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, d.endpoint+path, input)
	if err != nil {
		d.t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		d.t.Fatal(err)
	}
	defer response.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		d.t.Fatalf("%s %s: decode %s: %v", method, path, response.Status, err)
	}
	return response.StatusCode, decoded
}

func (d *daemon) create(environment map[string]string, source *sourceValue) (int, map[string]any) {
	body := map[string]any{
		"title": "PUA contract gate", "cwd": d.cwd, "agentName": "Fake ACP",
		"launchEnvironment": environment,
	}
	if source != nil {
		body["source"] = source
	}
	return d.request(http.MethodPost, "/v1/sessions", body)
}

func decodeSession(t *testing.T, body map[string]any) sessionValue {
	t.Helper()
	data, _ := json.Marshal(body["session"])
	var value sessionValue
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func errorCode(body map[string]any) string {
	value, _ := body["error"].(map[string]any)
	code, _ := value["code"].(string)
	return code
}

func (d *daemon) session(id string) sessionValue {
	d.t.Helper()
	status, body := d.request(http.MethodGet, "/v1/sessions/"+id, nil)
	if status != http.StatusOK {
		d.t.Fatalf("get session: status=%d body=%v", status, body)
	}
	return decodeSession(d.t, body)
}

func (d *daemon) waitSession(id string, predicate func(sessionValue) bool) sessionValue {
	d.t.Helper()
	return d.waitSessionFor(id, 20*time.Second, predicate)
}

func (d *daemon) waitSessionFor(id string, timeout time.Duration, predicate func(sessionValue) bool) sessionValue {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	var value sessionValue
	for time.Now().Before(deadline) {
		value = d.session(id)
		if predicate(value) {
			return value
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.t.Fatalf("session %s did not converge: %+v\n%s", id, value, d.logs())
	return value
}

func (d *daemon) frames(id string) []frameValue {
	d.t.Helper()
	status, body := d.request(http.MethodGet, "/v1/sessions/"+id+"/events?limit=1000", nil)
	if status != http.StatusOK {
		d.t.Fatalf("events: status=%d body=%v", status, body)
	}
	if body["schema"] != "agenthub.semantic-events.v1" {
		d.t.Fatalf("events schema=%v", body["schema"])
	}
	data, _ := json.Marshal(body["frames"])
	var frames []frameValue
	if err := json.Unmarshal(data, &frames); err != nil {
		d.t.Fatal(err)
	}
	return frames
}

func (d *daemon) events(id string) []eventValue {
	d.t.Helper()
	var events []eventValue
	for _, frame := range d.frames(id) {
		events = append(events, frame.Events...)
	}
	return events
}

func eventTypes(events []eventValue) []string {
	result := make([]string, 0, len(events))
	for _, event := range events {
		result = append(result, event.Type)
	}
	return result
}

func countType(events []eventValue, target string) int {
	count := 0
	for _, event := range events {
		if event.Type == target {
			count++
		}
	}
	return count
}

func countFrameSourceType(frames []frameValue, target string) int {
	count := 0
	for _, frame := range frames {
		if frame.Source.Type == target {
			count++
		}
	}
	return count
}

func assistantText(events []eventValue) string {
	var result strings.Builder
	for _, event := range events {
		if event.Type != "message.assistant.delta" {
			continue
		}
		var data struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(event.Data, &data)
		result.WriteString(data.Text)
	}
	return result.String()
}

func assistantTextAfter(events []eventValue, sourceEventID int64) string {
	filtered := make([]eventValue, 0, len(events))
	for _, event := range events {
		if event.SourceEventID > sourceEventID {
			filtered = append(filtered, event)
		}
	}
	return assistantText(filtered)
}

func (d *daemon) promptAndWait(id, text string) string {
	d.t.Helper()
	after := d.session(id).LastEventID
	code, body := d.request(http.MethodPost, "/v1/sessions/"+id+"/messages", map[string]any{"text": text})
	if code != http.StatusAccepted {
		d.t.Fatalf("message: status=%d body=%v", code, body)
	}
	d.waitSession(id, func(current sessionValue) bool {
		return current.CurrentTurnID == "" && current.LastEventID > after
	})
	return assistantTextAfter(d.events(id), after)
}

func (d *daemon) requireSessionSecretsAbsent(id string, secrets ...string) {
	d.t.Helper()
	responses := []struct {
		label string
		body  map[string]any
	}{
		{label: "session", body: mustRequest(d, http.MethodGet, "/v1/sessions/"+id)},
		{label: "session list", body: mustRequest(d, http.MethodGet, "/v1/sessions?limit=100")},
		{label: "event history", body: mustRequest(d, http.MethodGet, "/v1/sessions/"+id+"/events?limit=1000")},
	}
	for _, frame := range d.frames(id) {
		responses = append(responses, struct {
			label string
			body  map[string]any
		}{
			label: fmt.Sprintf("raw event %d", frame.Cursor),
			body:  mustRequest(d, http.MethodGet, fmt.Sprintf("/v1/sessions/%s/event/%d", id, frame.Cursor)),
		})
	}
	for _, response := range responses {
		data, err := json.Marshal(response.body)
		if err != nil {
			d.t.Fatal(err)
		}
		requireSecretsAbsent(d.t, response.label, data, secrets...)
	}

	directory := filepath.Join(d.root, "data", "sessions", id)
	if err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		requireSecretsAbsent(d.t, path, data, secrets...)
		return nil
	}); err != nil {
		d.t.Fatal(err)
	}
}

func mustRequest(d *daemon, method, path string) map[string]any {
	d.t.Helper()
	status, body := d.request(method, path, nil)
	if status != http.StatusOK {
		d.t.Fatalf("%s %s: status=%d body=%v", method, path, status, body)
	}
	return body
}

func requireSecretsAbsent(t *testing.T, label string, data []byte, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("%s contains ephemeral secret %q: %s", label, secret, data)
		}
	}
}

func requireOrdered(t *testing.T, types []string, wanted ...string) {
	t.Helper()
	position := 0
	for _, value := range types {
		if position < len(wanted) && value == wanted[position] {
			position++
		}
	}
	if position != len(wanted) {
		t.Fatalf("events %v do not contain ordered sequence %v", types, wanted)
	}
}

func TestPUAGateSourceEnvironmentResumeCapabilitiesAndErrors(t *testing.T) {
	gate := newGate(t)
	status, body := gate.request(http.MethodGet, "/v1/status", nil)
	if status != http.StatusOK || body["apiVersion"] != "1" {
		t.Fatalf("status contract: status=%d body=%v", status, body)
	}
	capabilities := map[string]bool{}
	for _, raw := range body["capabilities"].([]any) {
		capabilities[raw.(string)] = true
	}
	for _, required := range []string{
		"session.source", "session.launch-environment", "session.launch-environment-update", "session.strict-stopped",
		"messages.opaque-payload-v2",
		"events.lossless-replay", "events.canonical-turn-terminals", "recovery.closed-turns",
	} {
		if !capabilities[required] {
			t.Errorf("missing capability %q: %v", required, capabilities)
		}
	}

	type created struct {
		session sessionValue
		err     error
	}
	results := make(chan created, 2)
	for index, id := range []string{"pua-one", "pua-two"} {
		index, id := index, id
		go func() {
			source := &sourceValue{App: "pua", InstanceID: "gate", ExternalID: "task-" + strconv.Itoa(index)}
			code, response := gate.create(map[string]string{
				"SESSION_CONTEXT_ID": id,
				"FAKE_INSTANCE":      strconv.Itoa(index),
				"FAKE_NATIVE_ID":     "native-" + strconv.Itoa(index),
			}, source)
			if code != http.StatusCreated {
				results <- created{err: fmt.Errorf("create status=%d body=%v", code, response)}
				return
			}
			results <- created{session: decodeSession(t, response)}
		}()
	}
	var sessions []sessionValue
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		sessions = append(sessions, result.session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Source.ExternalID < sessions[j].Source.ExternalID
	})

	var wait sync.WaitGroup
	for _, value := range sessions {
		value := value
		wait.Add(1)
		go func() {
			defer wait.Done()
			code, response := gate.request(http.MethodPost, "/v1/sessions/"+value.ID+"/messages", map[string]any{
				"schemaVersion": 2, "text": "first", "payload": map[string]any{"schema": "pua.test.v1", "resource": value.Source.ExternalID},
			})
			if code != http.StatusAccepted {
				t.Errorf("message status=%d body=%v", code, response)
			}
		}()
	}
	wait.Wait()
	for index, value := range sessions {
		gate.waitSession(value.ID, func(current sessionValue) bool { return current.CurrentTurnID == "" })
		events := gate.events(value.ID)
		text := assistantText(events)
		var input struct {
			SchemaVersion int             `json:"schemaVersion"`
			Text          string          `json:"text"`
			Payload       json.RawMessage `json:"payload"`
		}
		for _, event := range events {
			if event.Type == "message.input" {
				_ = json.Unmarshal(event.Data, &input)
				break
			}
		}
		if input.SchemaVersion != 2 || input.Text != "first" || !bytes.Contains(input.Payload, []byte(`"schema":"pua.test.v1"`)) {
			t.Errorf("opaque input was not persisted unchanged: %+v payload=%s", input, input.Payload)
		}
		for _, want := range []string{
			"context=pua-" + []string{"one", "two"}[index],
			"instance=" + strconv.Itoa(index),
			"resumed=false",
			"native=native-" + strconv.Itoa(index),
		} {
			if !strings.Contains(text, want) {
				t.Errorf("session %d output %q missing %q", index, text, want)
			}
		}
	}

	query := url.Values{"sourceApp": {"pua"}, "sourceInstanceId": {"gate"}, "sourceExternalId": {"task-1"}}
	code, filtered := gate.request(http.MethodGet, "/v1/sessions?"+query.Encode(), nil)
	if code != http.StatusOK || len(filtered["sessions"].([]any)) != 1 {
		t.Fatalf("combined source filter: status=%d body=%v", code, filtered)
	}

	root, cwd := gate.root, gate.cwd
	gate.stop()
	restarted := startGate(t, root, cwd)
	for index, value := range sessions {
		code, response := restarted.request(http.MethodPost, "/v1/sessions/"+value.ID+"/resume", map[string]any{})
		if code != http.StatusOK {
			t.Fatalf("resume %d: status=%d body=%v", index, code, response)
		}
		code, response = restarted.request(http.MethodPost, "/v1/sessions/"+value.ID+"/messages", map[string]any{"text": "after restart"})
		if code != http.StatusAccepted {
			t.Fatalf("message after restart %d: status=%d body=%v", index, code, response)
		}
		restarted.waitSession(value.ID, func(current sessionValue) bool { return current.CurrentTurnID == "" })
		current := restarted.session(value.ID)
		if current.Source == nil || current.Source.ExternalID != "task-"+strconv.Itoa(index) ||
			current.LaunchEnvironment["SESSION_CONTEXT_ID"] != "pua-"+[]string{"one", "two"}[index] {
			t.Errorf("session metadata did not survive restart: %+v", current)
		}
		text := assistantText(restarted.events(value.ID))
		if !strings.Contains(text, "resumed=true") || !strings.Contains(text, "native=native-"+strconv.Itoa(index)) {
			t.Errorf("provider resume was not observed: %q", text)
		}
	}

	code, invalid := restarted.request(http.MethodPost, "/v1/sessions", map[string]any{
		"cwd": restarted.cwd, "agentName": "missing",
	})
	if code != http.StatusUnprocessableEntity || errorCode(invalid) != "invalid_agent" {
		t.Fatalf("structured error contract: status=%d body=%v", code, invalid)
	}
	errorBody := invalid["error"].(map[string]any)
	for _, key := range []string{"code", "message", "retryable", "details", "requestId"} {
		if _, ok := errorBody[key]; !ok {
			t.Errorf("error envelope missing %q: %v", key, errorBody)
		}
	}
}

func TestPUAGateEphemeralEnvironmentIsOneShotRequiredAndSecret(t *testing.T) {
	gate := newGate(t)
	const ephemeralKey = "FAKE_EPHEMERAL_SECRET"
	const createSecret = "pua-create-ephemeral-7bcf27d2"
	const resumeSecret = "pua-resume-ephemeral-fd352e91"

	status, statusBody := gate.request(http.MethodGet, "/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status: status=%d body=%v", status, statusBody)
	}
	hasEphemeralEnvironment := false
	for _, raw := range statusBody["capabilities"].([]any) {
		if raw == "session.ephemeral-environment" {
			hasEphemeralEnvironment = true
			break
		}
	}
	if !hasEphemeralEnvironment {
		t.Fatal("refusing to send an ephemeral environment without the advertised capability")
	}

	code, createBody := gate.request(http.MethodPost, "/v1/sessions", map[string]any{
		"title":     "PUA ephemeral environment contract",
		"cwd":       gate.cwd,
		"agentName": "Fake ACP",
		"launchEnvironment": map[string]string{
			"FAKE_REPORT_EPHEMERAL": "1",
		},
		"ephemeralEnvironment": map[string]string{
			ephemeralKey: createSecret,
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%v", code, createBody)
	}
	requireSecretsAbsent(t, "create response", mustJSONBytes(t, createBody), ephemeralKey, createSecret)
	value := decodeSession(t, createBody)
	if !value.EphemeralEnvironmentRequired {
		t.Fatalf("create response lacks ephemeral requirement marker: %+v", value)
	}
	var requirementEvents int
	for _, event := range gate.events(value.ID) {
		if event.Type != session.EventEphemeralEnvironmentRequired {
			continue
		}
		requirementEvents++
		var data map[string]any
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		if len(data) != 1 || data["required"] != true {
			t.Fatalf("ephemeral requirement event data = %#v", data)
		}
	}
	if requirementEvents != 1 {
		t.Fatalf("ephemeral requirement events = %d, want 1", requirementEvents)
	}
	if text := gate.promptAndWait(value.ID, "observe create overlay"); !strings.Contains(text, "ephemeral=<redacted>") {
		t.Fatalf("create process did not receive a redacted ephemeral value: %q", text)
	}
	// The create overlay reaches the Provider above, but the session store's
	// projection, event facts, snapshots, and API views must remain secret-free.
	gate.requireSessionSecretsAbsent(value.ID, ephemeralKey, createSecret)

	root, cwd := gate.root, gate.cwd
	gate.stop()
	restarted := startGate(t, root, cwd)
	restarted.requireSessionSecretsAbsent(value.ID, ephemeralKey, createSecret)
	recovered := restarted.session(value.ID)
	if !recovered.EphemeralEnvironmentRequired {
		t.Fatalf("daemon replay lost ephemeral requirement marker: %+v", recovered)
	}

	blockedEventID := recovered.LastEventID
	for name, requestBody := range map[string]map[string]any{
		"omitted overlay": {},
		"empty overlay":   {"ephemeralEnvironment": map[string]string{}},
	} {
		code, resumeBody := restarted.request(http.MethodPost, "/v1/sessions/"+value.ID+"/resume", requestBody)
		if code != http.StatusConflict || errorCode(resumeBody) != "runtime_operation_failed" {
			t.Fatalf("%s: status=%d body=%v", name, code, resumeBody)
		}
		message := resumeBody["error"].(map[string]any)["message"].(string)
		if !strings.Contains(message, "non-empty ephemeral environment") || strings.Contains(message, ephemeralKey) || strings.Contains(message, createSecret) {
			t.Fatalf("%s returned unsafe/unclear error: %q", name, message)
		}
		blocked := restarted.session(value.ID)
		if blocked.State != "stopped" || blocked.LastEventID != blockedEventID {
			t.Fatalf("%s changed stopped boundary: %+v", name, blocked)
		}
	}

	code, resumeBody := restarted.request(http.MethodPost, "/v1/sessions/"+value.ID+"/resume", map[string]any{
		"ephemeralEnvironment": map[string]string{
			ephemeralKey: resumeSecret,
		},
	})
	if code != http.StatusOK {
		t.Fatalf("resume with overlay: status=%d body=%v", code, resumeBody)
	}
	requireSecretsAbsent(t, "resume response", mustJSONBytes(t, resumeBody), ephemeralKey, createSecret, resumeSecret)
	if text := restarted.promptAndWait(value.ID, "observe resume overlay"); !strings.Contains(text, "ephemeral=<redacted>") {
		t.Fatalf("resumed process did not receive a redacted ephemeral value: %q", text)
	}

	code, stopBody := restarted.request(http.MethodPost, "/v1/sessions/"+value.ID+"/stop", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("stop after resume overlay: status=%d body=%v", code, stopBody)
	}
	restarted.waitSession(value.ID, func(current sessionValue) bool { return current.State == "stopped" })
	beforeSecondBlockedResume := restarted.session(value.ID)
	code, resumeBody = restarted.request(http.MethodPost, "/v1/sessions/"+value.ID+"/resume", map[string]any{})
	if code != http.StatusConflict || errorCode(resumeBody) != "runtime_operation_failed" {
		t.Fatalf("second resume without overlay: status=%d body=%v", code, resumeBody)
	}
	afterSecondBlockedResume := restarted.session(value.ID)
	if afterSecondBlockedResume.State != "stopped" || afterSecondBlockedResume.LastEventID != beforeSecondBlockedResume.LastEventID {
		t.Fatalf("second blocked resume changed stopped boundary: before=%+v after=%+v", beforeSecondBlockedResume, afterSecondBlockedResume)
	}
	// The resume overlay is equally one-shot: it reaches only this Provider
	// process and never becomes part of a durable session fact.
	restarted.requireSessionSecretsAbsent(value.ID, ephemeralKey, createSecret, resumeSecret)
}

func TestPUAGateOrphanReadyRecoveryRequiresOverlayResume(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	writeConfig(t, root)
	store, err := session.Open(filepath.Join(root, "data", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(session.CreateInput{
		Title: "PUA orphan ready recovery", Cwd: cwd, AgentName: "Fake ACP",
		IdempotencyKey: "pua-orphan-ready-recovery",
		Source: &session.Source{
			App: "pua", InstanceID: "gate", ExternalID: "project1.task1/1",
		},
		LaunchEnvironment: map[string]string{"FAKE_REPORT_EPHEMERAL": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.State != "ready" {
		t.Fatalf("seeded crash-boundary Session = %+v", created)
	}

	gate := startGate(t, root, cwd)
	recovered := gate.waitSession(created.ID, func(current sessionValue) bool { return current.State == "stopped" })
	if recovered.StopReason != "daemon_recovery" || recovered.ProviderSessionID != "" {
		t.Fatalf("orphan ready recovery projection = %+v", recovered)
	}
	types := eventTypes(gate.events(created.ID))
	requireOrdered(t, types, "session.state", "provider.error", "session.state")
	if countFrameSourceType(gate.frames(created.ID), "provider.process.started") != 0 {
		t.Fatalf("orphan ready recovery started a Provider: %v", types)
	}

	// A second daemon reconstruction must retain the same strict stopped
	// boundary without adding another recovery sequence.
	lastRecoveryEventID := recovered.LastEventID
	gate.stop()
	restarted := startGate(t, root, cwd)
	recovered = restarted.session(created.ID)
	if recovered.State != "stopped" || recovered.StopReason != "daemon_recovery" || recovered.LastEventID != lastRecoveryEventID {
		t.Fatalf("idempotent orphan recovery projection = %+v, lastEventId want %d", recovered, lastRecoveryEventID)
	}

	const ephemeralKey = "FAKE_EPHEMERAL_SECRET"
	const secret = "pua-orphan-resume-secret-18a94d"
	code, resumeBody := restarted.request(http.MethodPost, "/v1/sessions/"+created.ID+"/resume", map[string]any{
		"ephemeralEnvironment": map[string]string{ephemeralKey: secret},
	})
	if code != http.StatusOK {
		t.Fatalf("overlay Resume: status=%d body=%v", code, resumeBody)
	}
	requireSecretsAbsent(t, "orphan Resume response", mustJSONBytes(t, resumeBody), ephemeralKey, secret)
	if text := restarted.promptAndWait(created.ID, "observe recovered overlay"); !strings.Contains(text, "ephemeral=<redacted>") {
		t.Fatalf("first recovered Provider start did not receive overlay: %q", text)
	}
	if countFrameSourceType(restarted.frames(created.ID), "provider.process.started") != 1 {
		t.Fatalf("recovered Session did not start exactly one Provider: %v", eventTypes(restarted.events(created.ID)))
	}
	code, stopBody := restarted.request(http.MethodPost, "/v1/sessions/"+created.ID+"/stop", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("stop recovered Provider: status=%d body=%v", code, stopBody)
	}
	restarted.waitSession(created.ID, func(current sessionValue) bool { return current.State == "stopped" })
	restarted.requireSessionSecretsAbsent(created.ID, ephemeralKey, secret)
}

func mustJSONBytes(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPUAGateStrictStoppedFaultMatrix(t *testing.T) {
	t.Run("startup failure", func(t *testing.T) {
		gate := newGate(t)
		code, body := gate.create(map[string]string{"FAKE_MODE": "startup-crash"}, nil)
		if code != http.StatusBadGateway || errorCode(body) != "provider_start_failed" {
			t.Fatalf("startup failure: status=%d body=%v", code, body)
		}
		details := body["error"].(map[string]any)["details"].(map[string]any)
		value := gate.session(details["sessionId"].(string))
		if value.State != "stopped" || value.StopReason != "startup_error" {
			t.Fatalf("startup failure projection: %+v", value)
		}
		requireOrdered(t, eventTypes(gate.events(value.ID)), "session.created", "session.state", "provider.error", "session.state")
	})

	t.Run("normal exit", func(t *testing.T) {
		gate := newGate(t)
		code, body := gate.create(map[string]string{"FAKE_MODE": "complete-exit"}, nil)
		if code != http.StatusCreated {
			t.Fatalf("create: status=%d body=%v", code, body)
		}
		value := decodeSession(t, body)
		code, body = gate.request(http.MethodPost, "/v1/sessions/"+value.ID+"/messages", map[string]any{"text": "finish"})
		if code != http.StatusAccepted {
			t.Fatalf("message: status=%d body=%v", code, body)
		}
		value = gate.waitSession(value.ID, func(current sessionValue) bool { return current.State == "stopped" })
		if value.StopReason != "completed" {
			t.Fatalf("normal exit projection: %+v", value)
		}
		requireOrdered(t, eventTypes(gate.events(value.ID)), "turn.started", "turn.completed", "session.state")
	})

	t.Run("provider crash closes approval and turn", func(t *testing.T) {
		gate := newGate(t)
		trigger := filepath.Join(gate.root, "crash-provider")
		code, body := gate.create(map[string]string{
			"FAKE_MODE":          "crash",
			"FAKE_CRASH_TRIGGER": trigger,
		}, nil)
		if code != http.StatusCreated {
			t.Fatalf("create: status=%d body=%v", code, body)
		}
		value := decodeSession(t, body)
		code, body = gate.request(http.MethodPost, "/v1/sessions/"+value.ID+"/messages", map[string]any{"text": "crash"})
		if code != http.StatusAccepted {
			t.Fatalf("message: status=%d body=%v", code, body)
		}
		gate.waitSession(value.ID, func(current sessionValue) bool {
			return current.State == "waiting_approval" && len(current.PendingApprovalIDs) == 1
		})
		if err := os.WriteFile(trigger, []byte("crash\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		value = gate.waitSession(value.ID, func(current sessionValue) bool { return current.State == "stopped" })
		if value.StopReason != "provider_error" || value.CurrentTurnID != "" || len(value.PendingApprovalIDs) != 0 {
			t.Fatalf("crash projection: %+v", value)
		}
		events := gate.events(value.ID)
		requireOrdered(t, eventTypes(events), "turn.started", "approval.requested", "provider.error", "approval.resolved", "turn.failed", "session.state")
		if countType(events, "turn.failed") != 1 {
			t.Fatalf("crash terminal count: %v", eventTypes(events))
		}
	})

	t.Run("stop exit and resume stop races then archive", func(t *testing.T) {
		gate := newGate(t)
		code, body := gate.create(map[string]string{"FAKE_MODE": "hold"}, nil)
		if code != http.StatusCreated {
			t.Fatalf("create: status=%d body=%v", code, body)
		}
		value := decodeSession(t, body)
		code, body = gate.request(http.MethodPost, "/v1/sessions/"+value.ID+"/messages", map[string]any{"text": "hold"})
		if code != http.StatusAccepted {
			t.Fatalf("message: status=%d body=%v", code, body)
		}
		gate.waitSession(value.ID, func(current sessionValue) bool { return current.CurrentTurnID != "" })
		responses := make(chan int, 2)
		for range 2 {
			go func() {
				status, _ := gate.request(http.MethodPost, "/v1/sessions/"+value.ID+"/stop", map[string]any{})
				responses <- status
			}()
		}
		for range 2 {
			if status := <-responses; status != http.StatusOK {
				t.Errorf("concurrent stop status=%d", status)
			}
		}
		value = gate.waitSession(value.ID, func(current sessionValue) bool { return current.State == "stopped" })
		if value.StopReason != "requested" {
			t.Fatalf("stop race projection: %+v", value)
		}

		var race sync.WaitGroup
		race.Add(2)
		for _, operation := range []string{"resume", "stop"} {
			operation := operation
			go func() {
				defer race.Done()
				_, _ = gate.request(http.MethodPost, "/v1/sessions/"+value.ID+"/"+operation, map[string]any{})
			}()
		}
		race.Wait()
		_, _ = gate.request(http.MethodPost, "/v1/sessions/"+value.ID+"/stop", map[string]any{})
		value = gate.waitSession(value.ID, func(current sessionValue) bool { return current.State == "stopped" })
		if value.CurrentTurnID != "" || len(value.PendingApprovalIDs) != 0 {
			t.Fatalf("resume/stop race left active work: %+v", value)
		}
		code, body = gate.request(http.MethodDelete, "/v1/sessions/"+value.ID, map[string]any{})
		if code != http.StatusOK || decodeSession(t, body).State != "archived" {
			t.Fatalf("archive: status=%d body=%v", code, body)
		}
		code, body = gate.request(http.MethodPost, "/v1/sessions/"+value.ID+"/resume", map[string]any{})
		if code != http.StatusConflict || errorCode(body) != "session_archived" {
			t.Fatalf("archived resume: status=%d body=%v", code, body)
		}
	})
}

func TestPUAGateDaemonKillRecoveryClosesOpenWork(t *testing.T) {
	gate := newGate(t)
	code, body := gate.create(map[string]string{"FAKE_MODE": "approval-hold"}, nil)
	if code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%v", code, body)
	}
	value := decodeSession(t, body)
	code, body = gate.request(http.MethodPost, "/v1/sessions/"+value.ID+"/messages", map[string]any{"text": "wait for approval"})
	if code != http.StatusAccepted {
		t.Fatalf("message: status=%d body=%v", code, body)
	}
	value = gate.waitSession(value.ID, func(current sessionValue) bool {
		return current.State == "waiting_approval" && current.CurrentTurnID != "" && len(current.PendingApprovalIDs) == 1
	})
	var providerPID int
	for _, frame := range gate.frames(value.ID) {
		if frame.Source.Type != "provider.process.started" {
			continue
		}
		status, detail := gate.request(http.MethodGet, fmt.Sprintf("/v1/sessions/%s/event/%d", value.ID, frame.Cursor), nil)
		if status != http.StatusOK {
			t.Fatalf("event detail: status=%d body=%v", status, detail)
		}
		source := detail["sourceEvent"].(map[string]any)
		data := source["data"].(map[string]any)
		providerPID = int(data["pid"].(float64))
	}
	if providerPID <= 1 {
		t.Fatal("provider process identity was not persisted")
	}

	root, cwd := gate.root, gate.cwd
	gate.kill()
	restarted := startGate(t, root, cwd)
	value = restarted.waitSession(value.ID, func(current sessionValue) bool { return current.State == "stopped" })
	if value.StopReason != "daemon_recovery" || value.CurrentTurnID != "" || len(value.PendingApprovalIDs) != 0 {
		t.Fatalf("daemon recovery projection: %+v", value)
	}
	if err := syscall.Kill(providerPID, 0); err == nil || err == syscall.EPERM {
		t.Fatalf("orphan provider pid %d survived daemon recovery", providerPID)
	}
	types := eventTypes(restarted.events(value.ID))
	requireOrdered(t, types, "approval.requested", "session.state", "provider.error", "approval.resolved", "turn.cancelled", "session.state")
	if countType(restarted.events(value.ID), "turn.cancelled") != 1 {
		t.Fatalf("daemon recovery terminal count: %v", types)
	}
}

func TestPUAGateLosslessReplayBacklogDisconnectOverflowAndCatchup(t *testing.T) {
	t.Run("backlog disconnect REST catch-up and cursor gap", func(t *testing.T) {
		gate := newGate(t)
		code, body := gate.create(nil, nil)
		if code != http.StatusCreated {
			t.Fatalf("create: status=%d body=%v", code, body)
		}
		value := decodeSession(t, body)
		_, _ = gate.request(http.MethodPost, "/v1/sessions/"+value.ID+"/stop", map[string]any{})
		value = gate.waitSession(value.ID, func(current sessionValue) bool { return current.State == "stopped" })
		root, cwd := gate.root, gate.cwd
		gate.stop()

		appendBacklog(t, root, value.ID, value.LastEventID, 5201)
		restarted := startGate(t, root, cwd)
		value = restarted.session(value.ID)
		if value.LastEventID < 5201 {
			t.Fatalf("backlog head=%d, want at least 5201", value.LastEventID)
		}
		first := readSSE(t, restarted.endpoint+"/v1/sessions/"+value.ID+"/events?stream=true", 0, 137)
		if len(first) != 137 || first[0] != 1 {
			t.Fatalf("first SSE segment: len=%d first=%v", len(first), first)
		}
		// The second segment starts at the last received id; the replay
		// re-sends that cursor event once before continuing with newer ids.
		second := readSSE(t, restarted.endpoint+"/v1/sessions/"+value.ID+"/events?stream=true", first[len(first)-1], int(value.LastEventID)-len(first)+1)
		if second[0] != first[len(first)-1] {
			t.Fatalf("second SSE segment must start with the cursor event: %v", second[:min(3, len(second))])
		}
		assertContiguous(t, append(first, second[1:]...), 1, value.LastEventID)

		status, page := restarted.request(http.MethodGet, "/v1/sessions/"+value.ID+"/events?after=1000&limit=1000", nil)
		if status != http.StatusOK || int(page["latestCursor"].(float64)) != int(value.LastEventID) {
			t.Fatalf("REST catch-up: status=%d body=%v", status, page)
		}
		status, gap := restarted.request(http.MethodGet, fmt.Sprintf("/v1/sessions/%s/events?after=%d", value.ID, value.LastEventID+1), nil)
		if status != http.StatusConflict || errorCode(gap) != "event_cursor_ahead" {
			t.Fatalf("cursor gap: status=%d body=%v", status, gap)
		}
	})

	t.Run("slow subscriber overflow preserves durable REST catch-up", func(t *testing.T) {
		gate := newGate(t)
		code, body := gate.create(map[string]string{
			"FAKE_MODE":        "burst",
			"FAKE_BURST_COUNT": "270",
			"FAKE_BURST_BYTES": "65536",
		}, nil)
		if code != http.StatusCreated {
			t.Fatalf("create: status=%d body=%v", code, body)
		}
		value := decodeSession(t, body)
		connection := openSlowSSE(t, gate.addr, value.ID, value.LastEventID)
		defer connection.Close()

		code, body = gate.request(http.MethodPost, "/v1/sessions/"+value.ID+"/messages", map[string]any{"text": "burst"})
		if code != http.StatusAccepted {
			t.Fatalf("burst message: status=%d body=%v", code, body)
		}
		// This case deliberately persists and fsyncs more than 17 MiB across
		// enough distinct events to overflow the 256-event live queue. APFS
		// can require substantially longer than the ordinary 20-second
		// convergence budget for that durability workload.
		value = gate.waitSessionFor(value.ID, 2*time.Minute, func(current sessionValue) bool {
			return current.CurrentTurnID == ""
		})
		if value.LastEventID < 270 {
			t.Fatalf("burst durable head=%d", value.LastEventID)
		}
		// The deliberately tiny receive window and 17+ MiB burst force the
		// live subscriber queue past its 256-event capacity. Close the slow
		// client as a real SSE interruption; exact immediate-handler
		// termination is covered deterministically by the API/store tests,
		// while this process test proves the durable recovery path.
		if err := connection.Close(); err != nil {
			t.Fatal(err)
		}
		after := value.LastEventID - 25
		status, catchup := gate.request(http.MethodGet, fmt.Sprintf("/v1/sessions/%s/events?after=%d&limit=100", value.ID, after), nil)
		if status != http.StatusOK || len(catchup["frames"].([]any)) != 25 {
			t.Fatalf("overflow REST recovery: status=%d body=%v", status, catchup)
		}
	})
}

func appendBacklog(t *testing.T, root, id string, after int64, count int) {
	t.Helper()
	path := filepath.Join(root, "data", "sessions", id, "events.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writer := bufio.NewWriterSize(file, 1024*1024)
	now := time.Now().UTC()
	for offset := 1; offset <= count; offset++ {
		event := map[string]any{
			"id": after + int64(offset), "time": now, "type": "provider.backlog",
			"sessionId": id, "data": map[string]any{"sequence": offset},
		}
		data, _ := json.Marshal(event)
		if _, err := writer.Write(append(data, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func readSSE(t *testing.T, endpoint string, after int64, count int) []int64 {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, endpoint, nil)
	request.Header.Set("Accept", "text/event-stream")
	if after > 0 {
		request.Header.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	result := make([]int64, 0, count)
	for len(result) < count {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("SSE ended at %d/%d: %v", len(result), count, err)
		}
		if !strings.HasPrefix(line, "id: ") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "id: ")), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, id)
	}
	return result
}

func assertContiguous(t *testing.T, ids []int64, first, last int64) {
	t.Helper()
	if len(ids) != int(last-first+1) {
		t.Fatalf("cursor count=%d, want %d", len(ids), last-first+1)
	}
	for index, id := range ids {
		if want := first + int64(index); id != want {
			t.Fatalf("cursor[%d]=%d, want %d", index, id, want)
		}
	}
}

func openSlowSSE(t *testing.T, addr, id string, after int64) *net.TCPConn {
	t.Helper()
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	connection := raw.(*net.TCPConn)
	if err := connection.SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	request := fmt.Sprintf(
		"GET /v1/sessions/%s/events?stream=true&after=%d HTTP/1.1\r\nHost: %s\r\nAccept: text/event-stream\r\nConnection: close\r\n\r\n",
		id, after, addr,
	)
	if _, err := io.WriteString(connection, request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	return connection
}
