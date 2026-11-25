package serve

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/workspacepath"
)

type chatInputStage string

const (
	chatInputTransfer chatInputStage = "transfer"
	chatInputMailbox  chatInputStage = "mailbox"
)

type chatInputKey struct {
	root     string
	resource string
	stage    chatInputStage
}

type chatInputTarget struct {
	workspace serveWorkspace
	inFlight  bool
	requested bool
	queuedAt  time.Time
	nextAt    time.Time
}

func (m *agentManager) wakeChatInputs() {
	select {
	case m.chatInputWake <- struct{}{}:
	default:
	}
}

func (m *agentManager) requestChatInput(workspace serveWorkspace, resource string, stage chatInputStage) {
	m.requestChatInputAt(workspace, resource, stage, time.Now())
}

func (m *agentManager) requestChatInputAt(workspace serveWorkspace, resource string, stage chatInputStage, queuedAt time.Time) {
	key := chatInputKey{root: workspace.Path, resource: normalizedResourceID(resource), stage: stage}
	m.chatInputMu.Lock()
	target := m.chatInputTargets[key]
	if target == nil {
		target = &chatInputTarget{workspace: workspace}
		m.chatInputTargets[key] = target
	}
	if !target.requested {
		target.queuedAt = queuedAt
	}
	target.requested, target.nextAt = true, time.Time{}
	m.chatInputMu.Unlock()
	m.wakeChatInputs()
}

func (m *agentManager) recoverChatInputs(ctx context.Context) error {
	cfg, _, err := m.agentHubRuntimeConfig()
	if err != nil {
		return err
	}
	var failures []error
	for _, workspace := range cfg.Workspaces {
		if !m.server.ownsWorkspace(workspace.Path) {
			continue
		}
		paths, err := filepath.Glob(filepath.Join(workspacepath.ControlDir(workspace.Path), "chats", "*.json"))
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, path := range paths {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(failures, err)...)
			}
			chatMu.Lock()
			chat, err := loadChatLocked(workspace.Path, strings.TrimSuffix(filepath.Base(path), ".json"))
			chatMu.Unlock()
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", path, err))
				continue
			}
			if chat.Version < 4 {
				continue
			}
			destinations := map[string]bool{chat.ResourceID: true}
			for _, delivery := range chat.Outbox {
				if !delivery.Accepted {
					m.requestChatInput(workspace, chat.ResourceID, chatInputTransfer)
				} else {
					destinations[delivery.Target] = true
				}
			}
			for resource := range destinations {
				pending, err := mailboxPendingForResource(workspace.Path, resource)
				if err != nil {
					failures = append(failures, fmt.Errorf("%s mailbox %s: %w", path, resource, err))
					continue
				}
				if pending {
					m.requestChatInput(workspace, resource, chatInputMailbox)
				}
			}
		}
	}
	return errors.Join(failures...)
}

func (m *agentManager) runChatInputLoop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		now := time.Now()
		next := now.Add(durationOr(m.activePollInterval, 2*time.Second))
		type readyTarget struct {
			key       chatInputKey
			workspace serveWorkspace
			queuedAt  time.Time
		}
		var ready []readyTarget
		m.chatInputMu.Lock()
		for key, target := range m.chatInputTargets {
			if target.inFlight {
				continue
			}
			if target.nextAt.After(now) {
				if target.nextAt.Before(next) {
					next = target.nextAt
				}
				continue
			}
			target.inFlight, target.requested = true, false
			ready = append(ready, readyTarget{key: key, workspace: target.workspace, queuedAt: target.queuedAt})
		}
		m.chatInputMu.Unlock()
		for _, target := range ready {
			m.enqueueChatInput(ctx, target.key, target.workspace, target.queuedAt)
		}
		resetReconcileTimer(timer, next)
		select {
		case <-ctx.Done():
			return
		case <-m.chatInputWake:
		case <-timer.C:
		}
	}
}

func (m *agentManager) enqueueChatInput(ctx context.Context, key chatInputKey, workspace serveWorkspace, queuedAt time.Time) {
	finished, ok := m.beginBackground()
	if !ok {
		m.finishChatInput(ctx, key, context.Canceled)
		return
	}
	attempt := ctx
	cancel := func() {}
	if key.stage == chatInputMailbox {
		attempt, cancel = context.WithDeadline(ctx, queuedAt.Add(agentHubRequestTimeout))
	}
	execute := func() error {
		started := time.Now()
		var err error
		defer func() {
			log.Printf("Chat input reconcile workspace=%s resource=%s stage=%s queued_at=%s started_at=%s queue_ms=%d duration_ms=%d error_code=%s",
				workspace.ID, key.resource, key.stage, queuedAt.UTC().Format(time.RFC3339Nano),
				started.UTC().Format(time.RFC3339Nano), started.Sub(queuedAt).Milliseconds(),
				time.Since(started).Milliseconds(), reconcileErrorCode(err))
		}()
		if err = attempt.Err(); err != nil {
			return err
		}
		if !m.server.ownsWorkspace(workspace.Path) {
			return nil
		}
		if key.stage == chatInputTransfer {
			var ws *app.Workspace
			ws, err = app.OpenWorkspace(workspace.Path)
			if err != nil {
				return err
			}
			users, userErr := ws.Users()
			if userErr != nil {
				err = userErr
				return err
			}
			registered := make(map[string]bool, len(users))
			for _, user := range users {
				registered[user.Name] = true
			}
			_, err = m.reconcileChatOutbox(ctx, workspace, sharedChatID(key.resource), registered)
			return err
		}
		err = m.reconcileResourceMailboxLocked(attempt, workspace, key.resource)
		return err
	}
	if key.stage == chatInputTransfer {
		go func() {
			defer finished()
			defer cancel()
			m.finishChatInput(ctx, key, execute())
		}()
		return
	}
	controller, err := m.controllerForResource(workspace, key.resource)
	if err != nil {
		cancel()
		finished()
		m.finishChatInput(ctx, key, err)
		return
	}
	done := controller.enqueue(attempt, execute)
	go func() {
		defer finished()
		defer cancel()
		deadline, _ := attempt.Deadline()
		waiting := time.AfterFunc(time.Until(queuedAt.Add(2*time.Second)), func() {
			log.Printf("Chat input pending workspace=%s resource=%s stage=%s elapsed_ms=%d deadline_at=%s",
				workspace.ID, key.resource, key.stage, time.Since(queuedAt).Milliseconds(), deadline.UTC().Format(time.RFC3339Nano))
		})
		defer waiting.Stop()
		select {
		case err := <-done:
			m.finishChatInput(ctx, key, err)
		case <-attempt.Done():
			if errors.Is(attempt.Err(), context.DeadlineExceeded) {
				queued := controller.cancelQueued(done, attempt.Err())
				log.Printf("Chat input deadline workspace=%s resource=%s stage=%s controller_queued=%t elapsed_ms=%d error_code=deadline_exceeded",
					workspace.ID, key.resource, key.stage, queued, time.Since(queuedAt).Milliseconds())
			}
			m.finishChatInput(ctx, key, <-done)
		}
	}()
}

func reconcileErrorCode(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	var hubErr *agentHubAPIError
	if errors.As(err, &hubErr) && strings.TrimSpace(hubErr.Code) != "" {
		return hubErr.Code
	}
	return resourceDeliveryErrorCode(err)
}

func (m *agentManager) finishChatInput(ctx context.Context, key chatInputKey, err error) {
	m.chatInputMu.Lock()
	if target := m.chatInputTargets[key]; target != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || (err == nil && !target.requested) {
			delete(m.chatInputTargets, key)
		} else {
			target.inFlight = false
			if !target.requested {
				target.queuedAt = time.Now()
				target.nextAt = target.queuedAt.Add(durationOr(m.activePollInterval, 2*time.Second))
			}
		}
	}
	m.chatInputMu.Unlock()
	m.wakeChatInputs()
}
