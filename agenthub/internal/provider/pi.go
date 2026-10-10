package provider

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/disksing/pua/agenthub/internal/semantic"
)

type piSession struct {
	options Options
	command string
	cmd     *exec.Cmd
	pgid    int
	stdin   io.WriteCloser
	mu      sync.Mutex
	writeMu sync.Mutex
	nextID  int64
	waiting map[string]chan piResponse
	// messageStreams records whether a content block emitted a real delta;
	// snapshot-only providers need one normalized fallback at *_end, while a
	// normal delta stream must not append its full end snapshot again.
	messageStreams map[piMessageStreamKey]bool
	// The read loop owns this state. Pi can emit several failed assistant
	// messages during recovery; only agent_settled closes the public Turn.
	assistantError       string
	closed               bool
	nativeSessionMissing bool
	done                 chan struct{}
	prompts              asyncOperations
}

type piMessageStreamKey struct {
	kind         string
	contentIndex int
}

type piAssistantMessageUpdate struct {
	Type         string          `json:"type"`
	ContentIndex int             `json:"contentIndex"`
	Delta        string          `json:"delta"`
	Content      string          `json:"content"`
	ToolCall     json.RawMessage `json:"toolCall"`
}

type piResponse struct {
	Type    string          `json:"type"`
	ID      json.RawMessage `json:"id"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

func newPi(command string, options Options) *piSession {
	value := &piSession{
		command: command, options: options, nextID: 1,
		waiting: make(map[string]chan piResponse), messageStreams: make(map[piMessageStreamKey]bool),
		done: make(chan struct{}),
	}
	processEnd := value.options.Hooks.ProcessEnd
	value.options.Hooks.ProcessEnd = func(err error) {
		value.prompts.stopAndWait()
		if processEnd != nil {
			processEnd(err)
		}
	}
	return value
}

func (p *piSession) Start(resumeID string) error {
	args := []string{"--mode", "rpc"}
	if model := strings.TrimSpace(p.options.Agent.Options["model"]); model != "" {
		args = append(args, "--model", model)
	}
	if resumeID != "" {
		args = append(args, "--session", resumeID)
	}
	if p.options.Title != "" {
		args = append(args, "--name", p.options.Title)
	}
	cmd := exec.Command(p.command, args...)
	cmd.Dir = p.options.Cwd
	cmd.Env = processEnvironment(p.options.Environment)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = processTerminateGrace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, stdoutWriter := io.Pipe()
	stderr, stderrWriter := io.Pipe()
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		_ = stderr.Close()
		_ = stderrWriter.Close()
		return err
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = stdoutWriter.Close()
		_ = stderrWriter.Close()
		return fmt.Errorf("get provider process group: %w", err)
	}
	p.cmd, p.pgid, p.stdin = cmd, pgid, stdin
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		defer stdout.Close()
		p.readLoop(stdout)
	}()
	go func() {
		defer close(stderrDone)
		defer stderr.Close()
		p.stderrLoop(stderr)
	}()
	go func() {
		// Wait owns the copy into these writers and therefore cannot publish
		// process completion before the final protocol output is delivered.
		err := cmd.Wait()
		_ = stdoutWriter.Close()
		_ = stderrWriter.Close()
		<-stdoutDone
		<-stderrDone
		p.finish(err)
	}()
	if p.options.Hooks.ProcessStart != nil {
		if err := p.options.Hooks.ProcessStart(ProcessInfo{PID: cmd.Process.Pid, ProcessGroupID: pgid}); err != nil {
			_ = p.Close()
			return fmt.Errorf("persist provider process: %w", err)
		}
	}
	response, err := p.startRequest("get_state", nil)
	if err != nil {
		// A broken stdin can return before the stderr reader sees why Pi
		// exited. Confirm cleanup and drain output before classifying failure.
		_ = p.Close()
		p.mu.Lock()
		missing := p.nativeSessionMissing
		p.mu.Unlock()
		if missing && resumeID != "" {
			return fmt.Errorf("%w: Pi session %s: %v", ErrNativeSessionMissing, resumeID, err)
		}
		return err
	}
	native := lookup(response.Data, "sessionId")
	if native == "" {
		return errors.New("Pi RPC returned no session id")
	}
	if p.options.Hooks.NativeID != nil {
		p.options.Hooks.NativeID(native)
	}
	return nil
}

func (p *piSession) Prompt(text string, steer bool) error {
	return p.prompt(text, steer)
}

func (p *piSession) prompt(text string, steer bool) error {
	command := "prompt"
	if steer {
		command = "steer"
	}
	if !p.prompts.begin() {
		return errors.New("Pi provider process is stopping")
	}
	go func() {
		defer p.prompts.done()
		if _, err := p.requestLongRunning(command, map[string]any{"message": text}); err != nil && p.options.Hooks.Event != nil {
			p.options.Hooks.Event(Event{Type: "provider.error", Data: map[string]any{"message": err.Error()}, TurnDone: true, TurnFailed: true})
		}
	}()
	return nil
}

func (p *piSession) Interrupt() error {
	_, err := p.request("abort", nil)
	return err
}
func (p *piSession) Approve(string, ApprovalResolution) error {
	return errors.New("Pi RPC does not expose approvals")
}
func (p *piSession) Close() error {
	p.mu.Lock()
	if p.closed {
		cmd, pgid, stdin, done := p.cmd, p.pgid, p.stdin, p.done
		p.mu.Unlock()
		if cmd != nil {
			return terminateChildProcess(cmd, pgid, stdin, done)
		}
		return nil
	}
	p.closed = true
	cmd, pgid, stdin := p.cmd, p.pgid, p.stdin
	p.mu.Unlock()
	return terminateChildProcess(cmd, pgid, stdin, p.done)
}

// startRequest issues a handshake request bounded by the startup timeout,
// so a stuck Pi process fails session creation fast instead of hanging it.
func (p *piSession) startRequest(command string, fields map[string]any) (piResponse, error) {
	response, err := p.requestWithTimeout(command, fields, startupRequestTimeout)
	if err != nil {
		return response, startRequestError("Pi Coding Agent", command, err)
	}
	return response, nil
}

func (p *piSession) request(command string, fields map[string]any) (piResponse, error) {
	return p.requestWithTimeout(command, fields, controlRequestTimeout)
}

// requestLongRunning waits for a prompt/steer response until the provider
// settles the turn or session shutdown closes the waiter.
func (p *piSession) requestLongRunning(command string, fields map[string]any) (piResponse, error) {
	return p.requestWithTimeout(command, fields, 0)
}

func (p *piSession) requestWithTimeout(command string, fields map[string]any, timeout time.Duration) (piResponse, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return piResponse{}, errors.New("Pi process is closed")
	}
	id := p.nextID
	p.nextID++
	key := fmt.Sprint(id)
	ch := make(chan piResponse, 1)
	p.waiting[key] = ch
	p.mu.Unlock()
	value := map[string]any{"id": id, "type": command}
	for key, field := range fields {
		value[key] = field
	}
	if err := p.write(value); err != nil {
		p.mu.Lock()
		delete(p.waiting, key)
		p.mu.Unlock()
		return piResponse{}, err
	}
	var response piResponse
	if timeout <= 0 {
		value, ok := <-ch
		if !ok {
			return piResponse{}, errors.New("Pi exited before responding")
		}
		response = value
		if !response.Success {
			return response, fmt.Errorf("Pi %s: %s", command, response.Error)
		}
		return response, nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case value, ok := <-ch:
		if !ok {
			return piResponse{}, errors.New("Pi exited before responding")
		}
		response = value
	case <-timer.C:
		p.mu.Lock()
		delete(p.waiting, key)
		p.mu.Unlock()
		return piResponse{}, &RequestTimeoutError{Method: command, Timeout: timeout}
	}
	if !response.Success {
		return response, fmt.Errorf("Pi %s: %s", command, response.Error)
	}
	return response, nil
}

func (p *piSession) write(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err = p.stdin.Write(append(data, '\n'))
	return err
}

func (p *piSession) readLoop(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		raw := append(json.RawMessage(nil), scanner.Bytes()...)
		var envelope struct {
			Type string          `json:"type"`
			ID   json.RawMessage `json:"id"`
		}
		if json.Unmarshal(raw, &envelope) != nil {
			continue
		}
		if envelope.Type == "response" {
			var response piResponse
			_ = json.Unmarshal(raw, &response)
			key := strings.Trim(string(response.ID), `"`)
			p.mu.Lock()
			ch := p.waiting[key]
			delete(p.waiting, key)
			p.mu.Unlock()
			if ch != nil {
				ch <- response
				close(ch)
			}
			continue
		}
		p.event(envelope.Type, raw)
	}
}

func (p *piSession) event(kind string, raw json.RawMessage) {
	event := Event{Type: "provider.event", Data: map[string]any{"method": kind, "raw": raw}}
	var value struct {
		AssistantMessageEvent piAssistantMessageUpdate `json:"assistantMessageEvent"`
		Message               struct {
			Role         string `json:"role"`
			StopReason   string `json:"stopReason"`
			ErrorMessage string `json:"errorMessage"`
		} `json:"message"`
	}
	_ = json.Unmarshal(raw, &value)
	switch kind {
	case "message_start":
		clear(p.messageStreams)
	case "message_end":
		if value.Message.Role == "assistant" {
			p.assistantError = ""
			if value.Message.StopReason == "error" {
				p.assistantError = strings.TrimSpace(value.Message.ErrorMessage)
				if p.assistantError == "" {
					p.assistantError = "Pi assistant response failed"
				}
			}
		}
	case "message_update":
		var emit bool
		event, emit = p.normalizeMessageUpdate(event, value.AssistantMessageEvent, raw)
		if !emit {
			return
		}
	case "tool_execution_start", "tool_execution_end":
		event.Type = "tool.call"
		event.Data = semantic.ToolCallData(kind, raw)
	case "agent_settled":
		clear(p.messageStreams)
		event.Type, event.TurnDone = "provider.turn.completed", true
		if p.assistantError != "" {
			event.Type, event.TurnFailed = "provider.error", true
			event.Data.(map[string]any)["message"] = p.assistantError
		}
		p.assistantError = ""
	case "extension_error":
		clear(p.messageStreams)
		p.assistantError = ""
		event.Type, event.TurnDone, event.TurnFailed = "provider.error", true, true
	}
	if p.options.Hooks.Event != nil {
		p.options.Hooks.Event(event)
	}
}

// normalizeMessageUpdate removes Pi's cumulative message/partial snapshots
// from known streaming updates. The returned bool reports whether the compact
// event should reach the durable Session event stream.
func (p *piSession) normalizeMessageUpdate(fallback Event, update piAssistantMessageUpdate, raw json.RawMessage) (Event, bool) {
	if p.messageStreams == nil {
		p.messageStreams = make(map[piMessageStreamKey]bool)
	}
	kind := strings.TrimSpace(update.Type)
	switch kind {
	case "text_start", "thinking_start":
		p.messageStreams[piMessageStreamKey{kind: strings.TrimSuffix(kind, "_start"), contentIndex: update.ContentIndex}] = false
		return Event{}, false
	case "text_delta", "thinking_delta":
		messageKind := strings.TrimSuffix(kind, "_delta")
		if update.Delta == "" {
			return Event{}, false
		}
		p.messageStreams[piMessageStreamKey{kind: messageKind, contentIndex: update.ContentIndex}] = true
		return piMessageDelta(messageKind, update.Delta), true
	case "text_end", "thinking_end":
		messageKind := strings.TrimSuffix(kind, "_end")
		key := piMessageStreamKey{kind: messageKind, contentIndex: update.ContentIndex}
		hadDelta := p.messageStreams[key]
		delete(p.messageStreams, key)
		if hadDelta {
			return Event{}, false
		}
		text := update.Content
		if text == "" {
			text = piPartialMessageText(raw, update.ContentIndex, messageKind)
		}
		if text == "" {
			return Event{}, false
		}
		return piMessageDelta(messageKind, text), true
	case "toolcall_delta":
		// The execution lifecycle is emitted separately as tool_execution_*
		// events. Persisting this model-side JSON assembly stream would retain
		// a full cumulative partial/message snapshot for every tiny fragment.
		return Event{}, false
	case "toolcall_start", "toolcall_end":
		data := map[string]any{"method": "message_update", "kind": kind, "contentIndex": update.ContentIndex}
		if id, name := piToolCallIdentity(update, raw); id != "" || name != "" {
			if id != "" {
				data["toolCallId"] = id
			}
			if name != "" {
				data["toolName"] = name
			}
		}
		return Event{Type: "provider.metadata", Data: data}, true
	default:
		return fallback, true
	}
}

func piMessageDelta(kind, text string) Event {
	eventType := "message.assistant.delta"
	if kind == "thinking" {
		eventType = "message.reasoning.delta"
	}
	return Event{Type: eventType, Data: map[string]any{"text": text, "method": "message_update"}}
}

func piPartialMessageText(raw json.RawMessage, index int, kind string) string {
	content := piPartialMessageContent(raw)
	if index < 0 || index >= len(content) {
		return ""
	}
	var block struct {
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	}
	if json.Unmarshal(content[index], &block) != nil {
		return ""
	}
	if kind == "thinking" {
		return block.Thinking
	}
	return block.Text
}

func piToolCallIdentity(update piAssistantMessageUpdate, eventRaw json.RawMessage) (string, string) {
	toolCallRaw := update.ToolCall
	if len(toolCallRaw) == 0 {
		content := piPartialMessageContent(eventRaw)
		if update.ContentIndex >= 0 && update.ContentIndex < len(content) {
			toolCallRaw = content[update.ContentIndex]
		}
	}
	var toolCall struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	_ = json.Unmarshal(toolCallRaw, &toolCall)
	return toolCall.ID, toolCall.Name
}

func piPartialMessageContent(raw json.RawMessage) []json.RawMessage {
	var value struct {
		AssistantMessageEvent struct {
			Partial struct {
				Content []json.RawMessage `json:"content"`
			} `json:"partial"`
		} `json:"assistantMessageEvent"`
	}
	_ = json.Unmarshal(raw, &value)
	return value.AssistantMessageEvent.Partial.Content
}

func (p *piSession) stderrLoop(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "No session found matching '") {
			p.mu.Lock()
			p.nativeSessionMissing = true
			p.mu.Unlock()
		}
		if text := strings.TrimSpace(scanner.Text()); text != "" && p.options.Hooks.Event != nil {
			p.options.Hooks.Event(Event{Type: "provider.stderr", Data: map[string]any{"text": text}})
		}
	}
}

func (p *piSession) finish(err error) {
	p.mu.Lock()
	p.closed = true
	for key, ch := range p.waiting {
		delete(p.waiting, key)
		close(ch)
	}
	p.mu.Unlock()
	close(p.done)
	if p.options.Hooks.ProcessEnd != nil {
		p.options.Hooks.ProcessEnd(err)
	}
}
