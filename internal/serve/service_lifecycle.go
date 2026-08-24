package serve

import (
	"context"
	"errors"
	"log"
)

var (
	errWorkspaceRemovalServicesActive       = errors.New("workspace could not be removed because services remain active; resolve attention-required service state and retry")
	errWorkspaceRemovalLifecycleUnavailable = errors.New("workspace could not be removed because service lifecycle ownership is unavailable; retry after the current operation completes")
	errWorkspaceServiceRemovalInProgress    = errors.New("workspace service removal is in progress")
)

type serviceWorkspaceKey struct {
	workspaceID string
	root        string
}

type serviceManagerRemoval struct {
	workspaceID string
	key         serviceWorkspaceKey
	manager     *ServiceManager
	done        chan struct{}
	result      error
}

// serviceManagerLookup reserves one Workspace lifecycle generation while a
// manager is constructed and started without holding the global lifecycle
// mutex. Removal and addition wait for the reservation before changing that
// Workspace's durable membership or authoritative manager.
type serviceManagerLookup struct {
	done chan struct{}
}

func newServiceWorkspaceKey(workspace serveWorkspace) (serviceWorkspaceKey, error) {
	root, err := canonicalWorkspacePath(workspace.Path)
	if err != nil {
		return serviceWorkspaceKey{}, err
	}
	return serviceWorkspaceKey{workspaceID: workspace.ID, root: root}, nil
}

// registeredServiceManagerLocked resolves both canonical path aliases and the
// stable Workspace identity. The identity fallback keeps an existing manager
// reachable if a relative path can no longer be canonicalized (for example,
// after its process working directory is removed) or a symlink is retargeted.
func (s *server) registeredServiceManagerLocked(workspace serveWorkspace) (serviceWorkspaceKey, *ServiceManager, error) {
	key, keyErr := newServiceWorkspaceKey(workspace)
	return s.registeredServiceManagerForResolutionLocked(workspace, key, keyErr)
}

func (s *server) registeredServiceManagerForResolutionLocked(workspace serveWorkspace, key serviceWorkspaceKey, keyErr error) (serviceWorkspaceKey, *ServiceManager, error) {
	if keyErr == nil {
		if manager := s.services[key]; manager != nil {
			return key, manager, nil
		}
	}
	for registeredKey, manager := range s.services {
		if keyErr == nil && registeredKey.root == key.root {
			return registeredKey, manager, nil
		}
		if workspace.ID != "" && registeredKey.workspaceID == workspace.ID {
			return registeredKey, manager, nil
		}
	}
	if keyErr != nil {
		return serviceWorkspaceKey{}, nil, keyErr
	}
	return key, nil, nil
}

func (s *server) ensureServiceManagerLocked(workspace serveWorkspace) (*ServiceManager, bool, error) {
	if s.serviceRemovals[workspace.ID] != nil {
		return nil, false, errWorkspaceServiceRemovalInProgress
	}
	key, manager, err := s.registeredServiceManagerLocked(workspace)
	if err != nil || manager != nil {
		return manager, false, err
	}
	manager, err = NewServiceManager(key.root, ServiceManagerOptions{})
	if err != nil {
		return nil, false, err
	}
	if s.services == nil {
		s.services = make(map[serviceWorkspaceKey]*ServiceManager)
	}
	s.services[key] = manager
	return manager, true, nil
}

func (s *server) lockServiceLifecycleAfterLookup(id string) {
	for {
		s.serviceMu.Lock()
		lookup := s.serviceLookups[id]
		if lookup == nil {
			return
		}
		done := lookup.done
		s.serviceMu.Unlock()
		<-done
	}
}

func (s *server) finishServiceManagerLookupLocked(id string, lookup *serviceManagerLookup) {
	if lookup == nil || s.serviceLookups[id] != lookup {
		return
	}
	delete(s.serviceLookups, id)
	close(lookup.done)
}

func (s *server) configuredWorkspaceLocked(id string) (serveWorkspace, error) {
	cfg, err := s.transactConfigLocked(nil)
	if err != nil {
		return serveWorkspace{}, err
	}
	for _, workspace := range cfg.Workspaces {
		if workspace.ID != id {
			continue
		}
		if err := s.requireWorkspaceOwnership(workspace.Path); err != nil {
			return serveWorkspace{}, err
		}
		return workspace, nil
	}
	return serveWorkspace{}, errors.New("workspace not found: " + id)
}

// beginWorkspaceServiceManagerRemoval resolves the Workspace and claims its
// lifecycle ownership at the same transaction boundary used by additions and
// removal commits. Concurrent callers share the same completion instead of
// stopping, detaching, or releasing the authoritative manager more than once.
func (s *server) beginWorkspaceServiceManagerRemoval(id string) (*serviceManagerRemoval, bool, error) {
	s.lockServiceLifecycleAfterLookup(id)
	defer s.serviceMu.Unlock()
	if removal := s.serviceRemovals[id]; removal != nil {
		return removal, false, nil
	}
	cfg, _, err := readServeConfigFile(s.config)
	if err != nil {
		return nil, false, err
	}
	var workspace serveWorkspace
	for _, candidate := range cfg.Workspaces {
		if candidate.ID == id {
			workspace = candidate
			break
		}
	}
	if workspace.ID == "" {
		return nil, false, errors.New("workspace not found: " + id)
	}
	key, manager, err := s.registeredServiceManagerLocked(workspace)
	if err != nil {
		return nil, false, errWorkspaceRemovalLifecycleUnavailable
	}
	if s.serviceRemovals == nil {
		s.serviceRemovals = make(map[string]*serviceManagerRemoval)
	}
	removal := &serviceManagerRemoval{
		workspaceID: workspace.ID,
		key:         key,
		manager:     manager,
		done:        make(chan struct{}),
	}
	s.serviceRemovals[workspace.ID] = removal
	return removal, true, nil
}

func waitForServiceManagerRemoval(removal *serviceManagerRemoval) error {
	if removal == nil {
		return errWorkspaceRemovalLifecycleUnavailable
	}
	<-removal.done
	return removal.result
}

// commitWorkspaceServiceManagerRemoval patches only the claimed Workspace out
// of the latest durable config and detaches its exact manager as one serialized
// transaction. The potentially long Stop happens before this boundary, so
// unrelated Workspace supervision remains available while shutdown runs.
func (s *server) commitWorkspaceServiceManagerRemoval(removal *serviceManagerRemoval) (serveWorkspace, error) {
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()
	if removal == nil || s.serviceRemovals[removal.workspaceID] != removal {
		return serveWorkspace{}, errWorkspaceRemovalLifecycleUnavailable
	}
	if removal.manager != nil && s.services[removal.key] != removal.manager {
		return serveWorkspace{}, errWorkspaceRemovalLifecycleUnavailable
	}
	var removed serveWorkspace
	_, err := s.transactConfigLocked(func(cfg *config) (bool, error) {
		next := make([]serveWorkspace, 0, len(cfg.Workspaces))
		for _, workspace := range cfg.Workspaces {
			if workspace.ID == removal.workspaceID {
				removed = workspace
				continue
			}
			next = append(next, workspace)
		}
		if removed.ID == "" {
			return false, errWorkspaceRemovalLifecycleUnavailable
		}
		cfg.Workspaces = next
		if cfg.ActiveID == removal.workspaceID {
			cfg.ActiveID = ""
			if len(cfg.Workspaces) > 0 {
				cfg.ActiveID = cfg.Workspaces[0].ID
			}
		}
		return true, nil
	})
	if err != nil {
		return serveWorkspace{}, err
	}
	if removal.manager != nil {
		delete(s.services, removal.key)
	}
	return removed, nil
}

func (s *server) finishServiceManagerRemoval(removal *serviceManagerRemoval, result error) {
	if removal == nil {
		return
	}
	s.serviceMu.Lock()
	if s.serviceRemovals[removal.workspaceID] == removal {
		removal.result = result
		delete(s.serviceRemovals, removal.workspaceID)
		close(removal.done)
	}
	s.serviceMu.Unlock()
}

func (s *server) serviceManagersLocked() []*ServiceManager {
	managers := make([]*ServiceManager, 0, len(s.services))
	for key, manager := range s.services {
		if s.serviceLookups[key.workspaceID] != nil {
			continue
		}
		if removal := s.serviceRemovals[key.workspaceID]; removal != nil && removal.manager == manager {
			continue
		}
		managers = append(managers, manager)
	}
	return managers
}

func (s *server) serviceManagersAfterLookups() []*ServiceManager {
	for {
		s.serviceMu.Lock()
		if len(s.serviceLookups) == 0 {
			managers := s.serviceManagersLocked()
			s.serviceMu.Unlock()
			return managers
		}
		lookups := make([]<-chan struct{}, 0, len(s.serviceLookups))
		for _, lookup := range s.serviceLookups {
			lookups = append(lookups, lookup.done)
		}
		s.serviceMu.Unlock()
		for _, done := range lookups {
			<-done
		}
	}
}

func (s *server) initializeServiceManagers() error {
	if s == nil {
		return nil
	}
	cfg, err := s.loadConfig()
	if err != nil {
		return err
	}
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()
	for _, workspace := range cfg.Workspaces {
		if _, _, err := s.ensureServiceManagerLocked(workspace); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) serviceManagerForWorkspace(id string) (*ServiceManager, serveWorkspace, error) {
	return s.serviceManagerForWorkspaceAtLookupBoundary(id, nil)
}

func (s *server) serviceManagerForWorkspaceAtLookupBoundary(id string, lookupPrepared func()) (*ServiceManager, serveWorkspace, error) {
	if lookupPrepared != nil {
		lookupPrepared()
	}
	s.lockServiceLifecycleAfterLookup(id)
	workspace, err := s.configuredWorkspaceLocked(id)
	if err != nil {
		s.serviceMu.Unlock()
		return nil, serveWorkspace{}, err
	}
	if s.serviceRemovals[workspace.ID] != nil {
		s.serviceMu.Unlock()
		return nil, workspace, errWorkspaceServiceRemovalInProgress
	}
	key, manager, err := s.registeredServiceManagerLocked(workspace)
	if err != nil {
		s.serviceMu.Unlock()
		return nil, workspace, err
	}
	if manager != nil {
		s.serviceMu.Unlock()
		return manager, workspace, nil
	}
	lookup := &serviceManagerLookup{done: make(chan struct{})}
	if s.serviceLookups == nil {
		s.serviceLookups = make(map[string]*serviceManagerLookup)
	}
	s.serviceLookups[id] = lookup
	factory := s.serviceFactory
	if factory == nil {
		factory = NewServiceManager
	}
	s.serviceMu.Unlock()

	manager, err = factory(key.root, ServiceManagerOptions{})
	if err != nil {
		s.serviceMu.Lock()
		s.finishServiceManagerLookupLocked(id, lookup)
		s.serviceMu.Unlock()
		return nil, workspace, err
	}

	s.serviceMu.Lock()
	if s.serviceLookups[id] != lookup {
		s.serviceMu.Unlock()
		return nil, workspace, errors.New("workspace service lookup ownership changed")
	}
	workspace, err = s.configuredWorkspaceLocked(id)
	if err != nil {
		s.finishServiceManagerLookupLocked(id, lookup)
		s.serviceMu.Unlock()
		return nil, serveWorkspace{}, err
	}
	if s.serviceRemovals[id] != nil {
		s.finishServiceManagerLookupLocked(id, lookup)
		s.serviceMu.Unlock()
		return nil, workspace, errWorkspaceServiceRemovalInProgress
	}
	latestKey, registered, err := s.registeredServiceManagerLocked(workspace)
	if err != nil {
		s.finishServiceManagerLookupLocked(id, lookup)
		s.serviceMu.Unlock()
		return nil, workspace, err
	}
	if registered != nil {
		s.finishServiceManagerLookupLocked(id, lookup)
		s.serviceMu.Unlock()
		return registered, workspace, nil
	}
	if latestKey != key {
		s.finishServiceManagerLookupLocked(id, lookup)
		s.serviceMu.Unlock()
		return nil, workspace, errors.New("workspace changed during service manager lookup: " + id)
	}
	if s.services == nil {
		s.services = make(map[serviceWorkspaceKey]*ServiceManager)
	}
	s.services[key] = manager
	serviceContext := s.serviceContext
	starter := s.serviceStarter
	s.serviceMu.Unlock()

	if serviceContext != nil {
		if starter == nil {
			starter = func(manager *ServiceManager, ctx context.Context) error {
				return manager.Start(ctx)
			}
		}
		if startErr := starter(manager, serviceContext); startErr != nil {
			log.Printf("start workspace services in %s: %v", manager.Root(), startErr)
		}
	}
	s.serviceMu.Lock()
	s.finishServiceManagerLookupLocked(id, lookup)
	s.serviceMu.Unlock()
	return manager, workspace, nil
}

func (s *server) startServices(ctx context.Context) {
	s.serviceMu.Lock()
	managers := s.serviceManagersLocked()
	s.serviceMu.Unlock()
	for _, manager := range managers {
		if err := manager.Start(ctx); err != nil {
			log.Printf("start workspace services in %s: %v", manager.Root(), err)
		}
	}
}

func (s *server) reconcileServices(ctx context.Context) error {
	s.serviceMu.Lock()
	managers := s.serviceManagersLocked()
	s.serviceMu.Unlock()
	for _, manager := range managers {
		if err := manager.Reconcile(ctx); err != nil {
			log.Printf("reconcile workspace services in %s: %v", manager.Root(), err)
		}
	}
	return nil
}

func (s *server) stopServices(ctx context.Context) error {
	// A lookup starts its manager outside serviceMu while its Workspace
	// reservation prevents removal or replacement. Preserve the old shutdown
	// guarantee by waiting for those starts before taking the stop snapshot.
	managers := s.serviceManagersAfterLookups()
	var first error
	for _, manager := range managers {
		if err := manager.Stop(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *server) serviceEnvironment(workspace serveWorkspace) (map[string]string, map[string]string, error) {
	manager, _, err := s.serviceManagerForWorkspace(workspace.ID)
	if err != nil {
		return nil, nil, err
	}
	return manager.ResolveBindings()
}
