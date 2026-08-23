package serve

import (
	"context"
	"log"
)

type serviceWorkspaceKey struct {
	workspaceID string
	root        string
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

func (s *server) removeServiceManagerLocked(workspace serveWorkspace) (*ServiceManager, error) {
	key, keyErr := newServiceWorkspaceKey(workspace)
	return s.removeServiceManagerForResolutionLocked(workspace, key, keyErr)
}

func (s *server) removeServiceManagerForResolutionLocked(workspace serveWorkspace, key serviceWorkspaceKey, keyErr error) (*ServiceManager, error) {
	key, manager, err := s.registeredServiceManagerForResolutionLocked(workspace, key, keyErr)
	if err != nil || manager == nil {
		return manager, err
	}
	delete(s.services, key)
	return manager, nil
}

func (s *server) serviceManagersLocked() []*ServiceManager {
	managers := make([]*ServiceManager, 0, len(s.services))
	for _, manager := range s.services {
		managers = append(managers, manager)
	}
	return managers
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
	workspace, err := s.workspace(id)
	if err != nil {
		return nil, serveWorkspace{}, err
	}
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()
	manager, created, err := s.ensureServiceManagerLocked(workspace)
	if err != nil {
		return nil, workspace, err
	}
	if created {
		if s.serviceContext != nil {
			if startErr := manager.Start(s.serviceContext); startErr != nil {
				log.Printf("start workspace services in %s: %v", manager.Root(), startErr)
			}
		}
	}
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
	s.serviceMu.Lock()
	managers := s.serviceManagersLocked()
	s.serviceMu.Unlock()
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
