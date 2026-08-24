package serve

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
)

// resourceController serializes operations for one stable resource address.
// Jobs are FIFO within a key, while different keys have independent workers.
// A job may perform a bounded or slow AgentHub operation; that only delays
// later jobs for the same resource.
type resourceController struct {
	mu      sync.Mutex
	jobs    []resourceControllerJob
	running bool
}

type resourceControllerJob struct {
	ctx  context.Context
	fn   func() error
	done chan error
}

func (c *resourceController) enqueue(ctx context.Context, fn func() error) <-chan error {
	return c.enqueueWithStart(ctx, fn, func(run func()) { go run() })
}

func (c *resourceController) enqueueWithStart(ctx context.Context, fn func() error, startWorker func(func())) <-chan error {
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan error, 1)
	c.mu.Lock()
	c.jobs = append(c.jobs, resourceControllerJob{ctx: ctx, fn: fn, done: done})
	start := !c.running
	if start {
		c.running = true
	}
	c.mu.Unlock()
	if start {
		startWorker(c.run)
	}
	return done
}

func (c *resourceController) run() {
	for {
		c.mu.Lock()
		if len(c.jobs) == 0 {
			c.running = false
			c.mu.Unlock()
			return
		}
		job := c.jobs[0]
		c.jobs[0] = resourceControllerJob{}
		c.jobs = c.jobs[1:]
		c.mu.Unlock()

		var err error
		if job.ctx != nil && job.ctx.Err() != nil {
			err = job.ctx.Err()
		} else if job.fn != nil {
			err = job.fn()
		}
		job.done <- err
	}
}

func (c *resourceController) do(ctx context.Context, fn func() error) error {
	return c.doWithStart(ctx, fn, func(run func()) { go run() })
}

func (c *resourceController) doWithStart(ctx context.Context, fn func() error, startWorker func(func())) error {
	done := c.enqueueWithStart(ctx, fn, startWorker)
	if ctx == nil {
		return <-done
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type resourceControllerKey struct {
	workspaceInstanceID string
	staleWorkspacePath  string
	resourceID          string
}

func resourceControllerKeyForInstanceID(instanceID, resourceID string) (resourceControllerKey, error) {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return resourceControllerKey{}, errors.New("Workspace runtime instance id is empty")
	}
	return resourceControllerKey{workspaceInstanceID: instanceID, resourceID: normalizedResourceID(resourceID)}, nil
}

// resourceControllerKeyForStaleWorkspacePath addresses removal of a legacy
// serve-config entry whose Workspace identity can no longer be read. The
// distinct key field is a separate namespace, not a fabricated instance ID,
// so it cannot alias a healthy Workspace controller.
func resourceControllerKeyForStaleWorkspacePath(workspacePath, resourceID string) (resourceControllerKey, error) {
	canonical, err := canonicalWorkspacePath(workspacePath)
	if err != nil {
		return resourceControllerKey{}, err
	}
	return resourceControllerKey{staleWorkspacePath: canonical, resourceID: normalizedResourceID(resourceID)}, nil
}

func (m *agentManager) resourceControllerKey(workspace serveWorkspace, resourceID string) (resourceControllerKey, error) {
	instanceID, err := workspaceInstanceID(workspace.Path)
	if err != nil {
		return resourceControllerKey{}, err
	}
	return resourceControllerKeyForInstanceID(instanceID, resourceID)
}

func (m *agentManager) controllerForResourceKey(key resourceControllerKey) *resourceController {
	m.resourceControllersMu.Lock()
	defer m.resourceControllersMu.Unlock()
	controller := m.resourceControllers[key]
	if controller == nil {
		controller = &resourceController{}
		m.resourceControllers[key] = controller
	}
	return controller
}

func (m *agentManager) controllerForStaleWorkspacePath(workspacePath, resourceID string) (*resourceController, error) {
	key, err := resourceControllerKeyForStaleWorkspacePath(workspacePath, resourceID)
	if err != nil {
		return nil, err
	}
	return m.controllerForResourceKey(key), nil
}

func (m *agentManager) controllerForResourceInstanceID(instanceID, resourceID string) (*resourceController, error) {
	key, err := resourceControllerKeyForInstanceID(instanceID, resourceID)
	if err != nil {
		return nil, err
	}
	return m.controllerForResourceKey(key), nil
}

func (m *agentManager) controllerForResource(workspace serveWorkspace, resourceID string) (*resourceController, error) {
	key, err := m.resourceControllerKey(workspace, resourceID)
	if err != nil {
		return nil, err
	}
	return m.controllerForResourceKey(key), nil
}

func (m *agentManager) withResourceControllerInstanceID(ctx context.Context, instanceID, resourceID string, fn func() error) error {
	controller, err := m.controllerForResourceInstanceID(instanceID, resourceID)
	if err != nil {
		return err
	}
	return controller.doWithStart(ctx, fn, m.runBackground)
}

func (m *agentManager) withStaleWorkspacePathController(ctx context.Context, workspacePath, resourceID string, fn func() error) error {
	controller, err := m.controllerForStaleWorkspacePath(workspacePath, resourceID)
	if err != nil {
		return err
	}
	return controller.doWithStart(ctx, fn, m.runBackground)
}

func (m *agentManager) withResourceController(ctx context.Context, workspace serveWorkspace, resourceID string, fn func() error) error {
	controller, err := m.controllerForResource(workspace, resourceID)
	if err != nil {
		return err
	}
	// Track even synchronously requested workers. They can enqueue asynchronous
	// follow-up jobs before the caller returns; keeping the worker registered
	// until its FIFO is empty prevents shutdown from racing those follow-ups.
	return controller.doWithStart(ctx, fn, m.runBackground)
}

// withResourceControllers serializes one operation with every listed stable
// resource address. Controllers are always acquired by normalized resource ID
// so overlapping multi-resource operations cannot invert their lock order.
// Callers already running under a resource controller must not include it and
// must ensure no path holding an inner controller can acquire their outer one.
func (m *agentManager) withResourceControllers(ctx context.Context, workspace serveWorkspace, resourceIDs []string, fn func() error) error {
	ids := make([]string, 0, len(resourceIDs))
	seen := make(map[string]bool, len(resourceIDs))
	for _, resourceID := range resourceIDs {
		resourceID = normalizedResourceID(resourceID)
		if resourceID == "" || seen[resourceID] {
			continue
		}
		seen[resourceID] = true
		ids = append(ids, resourceID)
	}
	sort.Strings(ids)
	var acquire func(int) error
	acquire = func(index int) error {
		if index == len(ids) {
			return fn()
		}
		return m.withResourceController(ctx, workspace, ids[index], func() error {
			return acquire(index + 1)
		})
	}
	return acquire(0)
}

func (m *agentManager) enqueueResourceController(workspace serveWorkspace, resourceID string, fn func() error) error {
	controller, err := m.controllerForResource(workspace, resourceID)
	if err != nil {
		return err
	}
	// Register a newly started controller worker before it can run. Besides
	// making fire-and-forget work observable during shutdown, this ordering
	// lets a tracked worker enqueue follow-up work without racing the final
	// background wait.
	controller.enqueueWithStart(context.Background(), fn, m.runBackground)
	return nil
}

func (m *agentManager) enqueueRuntimeOperation(rt *agentRuntime, fn func()) error {
	if rt == nil {
		return errors.New("resource runtime is nil")
	}
	record := rt.snapshotGeneration()
	return m.enqueueResourceController(rt.workspace, record.ResourceID, func() error {
		fn()
		return nil
	})
}
