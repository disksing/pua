package serve

import (
	"context"
	"errors"
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
		go c.run()
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
	done := c.enqueue(ctx, fn)
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
	resourceID          string
}

func (key resourceControllerKey) string() string {
	return key.workspaceInstanceID + "\x00" + key.resourceID
}

func (m *agentManager) resourceControllerKey(workspace serveWorkspace, resourceID string) (resourceControllerKey, error) {
	instanceID, err := workspaceInstanceID(workspace.Path)
	if err != nil {
		return resourceControllerKey{}, err
	}
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return resourceControllerKey{}, errors.New("Workspace runtime instance id is empty")
	}
	return resourceControllerKey{workspaceInstanceID: instanceID, resourceID: normalizedResourceID(resourceID)}, nil
}

func (m *agentManager) controllerForResource(workspace serveWorkspace, resourceID string) (*resourceController, error) {
	key, err := m.resourceControllerKey(workspace, resourceID)
	if err != nil {
		return nil, err
	}
	keyString := key.string()
	m.resourceControllersMu.Lock()
	defer m.resourceControllersMu.Unlock()
	controller := m.resourceControllers[keyString]
	if controller == nil {
		controller = &resourceController{}
		m.resourceControllers[keyString] = controller
	}
	return controller, nil
}

func (m *agentManager) withResourceController(ctx context.Context, workspace serveWorkspace, resourceID string, fn func() error) error {
	controller, err := m.controllerForResource(workspace, resourceID)
	if err != nil {
		return err
	}
	return controller.do(ctx, fn)
}

func (m *agentManager) enqueueResourceController(workspace serveWorkspace, resourceID string, fn func() error) error {
	controller, err := m.controllerForResource(workspace, resourceID)
	if err != nil {
		return err
	}
	finished, ok := m.beginBackground()
	if !ok {
		return errors.New("resource manager is shutting down")
	}
	done := controller.enqueue(context.Background(), fn)
	// Keep fire-and-forget controller work observable by orderly shutdown and
	// tests. The waiter does not occupy the resource controller, so jobs remain
	// free to enqueue follow-up work for the same resource.
	go func() {
		defer finished()
		<-done
	}()
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
