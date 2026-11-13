package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/disksing/pua/internal/workspacepath"
)

type turnOutputKey struct {
	root     string
	resource string
}

type turnOutputTarget struct {
	workspace serveWorkspace
	pending   bool
	inFlight  bool
	requested bool
	nextAt    time.Time
	since     time.Time
}

func logReconcileDuration(stage string, started time.Time) {
	if elapsed := time.Since(started); elapsed >= time.Second {
		log.Printf("reconcile timing stage=%s started_at=%s duration_ms=%d",
			stage, started.UTC().Format(time.RFC3339Nano), elapsed.Milliseconds())
	}
}

func (m *agentManager) wakeTurnOutputs() {
	select {
	case m.turnOutputWake <- struct{}{}:
	default:
	}
}

func (m *agentManager) updateTurnOutputTarget(workspace serveWorkspace, resource string, file turnWorkFile, requested bool) {
	pending := false
	var since time.Time
	for _, work := range file.Works {
		if work.OutputDone {
			continue
		}
		pending = true
		if created := generationTime(work.CreatedAt); !created.IsZero() && (since.IsZero() || created.Before(since)) {
			since = created
		}
	}
	key := turnOutputKey{root: workspace.Path, resource: resource}
	m.turnOutputMu.Lock()
	target := m.turnOutputTargets[key]
	wake := false
	if pending && target == nil {
		if since.IsZero() {
			since = time.Now()
		}
		target = &turnOutputTarget{workspace: workspace, since: since}
		m.turnOutputTargets[key] = target
		requested = true
	}
	if target != nil {
		target.pending = pending
		if pending && !since.IsZero() {
			target.since = since
		}
		if pending && requested {
			target.requested = true
			target.nextAt = time.Time{}
			wake = true
		}
		if !pending && !target.inFlight {
			delete(m.turnOutputTargets, key)
		}
	}
	m.turnOutputMu.Unlock()
	if wake {
		m.wakeTurnOutputs()
	}
}

func (m *agentManager) requestTurnOutput(workspace serveWorkspace, resource string) {
	m.refreshTurnOutputTarget(workspace, resource, true)
}

func (m *agentManager) refreshTurnOutputTarget(workspace serveWorkspace, resource string, requested bool) {
	file, err := loadTurnWork(workspace.Path, resource)
	if err == nil {
		m.updateTurnOutputTarget(workspace, resource, file, requested)
	}
}

func (m *agentManager) recoverTurnOutputTargets(ctx context.Context) error {
	cfg, _, err := m.agentHubRuntimeConfig()
	if err != nil {
		return err
	}
	var failures []error
	for _, workspace := range cfg.Workspaces {
		if !m.server.ownsWorkspace(workspace.Path) {
			continue
		}
		paths, err := filepath.Glob(filepath.Join(workspacepath.ControlDir(workspace.Path), "turn-work", "*.json"))
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, path := range paths {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(failures, err)...)
			}
			data, err := os.ReadFile(path)
			var file turnWorkFile
			if err == nil {
				err = json.Unmarshal(data, &file)
			}
			if err == nil && (file.Version != 1 || file.ResourceID == "") {
				err = errors.New("invalid Turn work record")
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", path, err))
				continue
			}
			for _, work := range file.Works {
				if !work.OutputDone {
					m.updateTurnOutputTarget(workspace, file.ResourceID, file, true)
					break
				}
			}
		}
	}
	return errors.Join(failures...)
}

func (m *agentManager) runTurnOutputLoop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	lastDiagnostic := time.Now()
	for {
		if ctx.Err() != nil {
			return
		}
		now := time.Now()
		next := now.Add(durationOr(m.activePollInterval, 2*time.Second))
		var ready []turnOutputKey
		var oldest time.Time
		pending, inFlight := 0, 0
		m.turnOutputMu.Lock()
		for key, target := range m.turnOutputTargets {
			if target.inFlight {
				inFlight++
			}
			if !target.pending {
				continue
			}
			pending++
			if oldest.IsZero() || target.since.Before(oldest) {
				oldest = target.since
			}
			if target.inFlight {
				continue
			}
			if !target.nextAt.After(now) {
				target.inFlight, target.requested = true, false
				ready = append(ready, key)
			} else if target.nextAt.Before(next) {
				next = target.nextAt
			}
		}
		m.turnOutputMu.Unlock()
		for _, key := range ready {
			m.enqueueTurnOutput(ctx, key)
		}
		if now.Sub(lastDiagnostic) >= 30*time.Second {
			age := time.Duration(0)
			if !oldest.IsZero() {
				age = now.Sub(oldest)
			}
			log.Printf("Turn output pending resources=%d in_flight=%d oldest_work_ms=%d", pending, inFlight, age.Milliseconds())
			lastDiagnostic = now
		}
		resetReconcileTimer(timer, next)
		select {
		case <-ctx.Done():
			return
		case <-m.turnOutputWake:
		case <-timer.C:
		}
	}
}

func (m *agentManager) enqueueTurnOutput(ctx context.Context, key turnOutputKey) {
	m.turnOutputMu.Lock()
	target := m.turnOutputTargets[key]
	workspace := target.workspace
	m.turnOutputMu.Unlock()
	queued := time.Now()
	finished, ok := m.beginBackground()
	if !ok {
		m.finishTurnOutput(key)
		return
	}
	controller, err := m.controllerForResource(workspace, key.resource)
	if err != nil {
		finished()
		m.finishTurnOutput(key)
		log.Printf("Turn output controller resource=%s error=%v", key.resource, err)
		return
	}
	done := controller.enqueue(ctx, func() error {
		started := time.Now()
		if !m.server.ownsWorkspace(workspace.Path) {
			return errors.New("Turn output Workspace is not owned by this server")
		}
		queryContext, cancel := context.WithTimeout(ctx, agentHubRequestTimeout)
		defer cancel()
		_, client, err := m.agentHubRuntimeConfig()
		if err == nil {
			err = m.reconcileTurnWorkLocked(queryContext, workspace, key.resource, client)
		}
		if elapsed := time.Since(started); elapsed >= time.Second || started.Sub(queued) >= time.Second || err != nil {
			log.Printf("Turn output reconcile workspace=%s resource=%s queued_at=%s queue_ms=%d duration_ms=%d error=%v",
				workspace.ID, key.resource, queued.UTC().Format(time.RFC3339Nano), started.Sub(queued).Milliseconds(), elapsed.Milliseconds(), err)
		}
		return err
	})
	go func() {
		defer finished()
		<-done
		m.finishTurnOutput(key)
	}()
}

func (m *agentManager) finishTurnOutput(key turnOutputKey) {
	m.turnOutputMu.Lock()
	if target := m.turnOutputTargets[key]; target != nil {
		target.inFlight = false
		target.nextAt = time.Now().Add(durationOr(m.activePollInterval, 2*time.Second))
		if !target.pending {
			delete(m.turnOutputTargets, key)
		} else if target.requested {
			target.nextAt = time.Time{}
		}
	}
	m.turnOutputMu.Unlock()
	m.wakeTurnOutputs()
}
