package serve

import (
	"context"
	"log"
	"path/filepath"
	"strings"

	"github.com/disksing/pua/internal/workspacepath"
)

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
	if s.services == nil {
		s.services = make(map[string]*ServiceManager)
	}
	for _, workspace := range cfg.Workspaces {
		root, err := filepath.Abs(strings.TrimSpace(workspace.Path))
		if err != nil {
			return err
		}
		if canonical, evalErr := filepath.EvalSymlinks(root); evalErr == nil {
			root = canonical
		}
		manager, err := NewServiceManager(root, ServiceManagerOptions{Logger: log.Printf})
		if err != nil {
			return err
		}
		s.services[root] = manager
	}
	return nil
}

func (s *server) serviceManagerForWorkspace(id string) (*ServiceManager, serveWorkspace, error) {
	workspace, err := s.workspace(id)
	if err != nil {
		return nil, serveWorkspace{}, err
	}
	root, err := filepath.Abs(workspace.Path)
	if err != nil {
		return nil, workspace, err
	}
	if canonical, evalErr := filepath.EvalSymlinks(root); evalErr == nil {
		root = canonical
	}
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()
	if s.services == nil {
		s.services = make(map[string]*ServiceManager)
	}
	manager := s.services[root]
	if manager == nil {
		manager, err = NewServiceManager(root, ServiceManagerOptions{Logger: log.Printf})
		if err != nil {
			return nil, workspace, err
		}
		s.services[root] = manager
		if s.serviceContext != nil {
			if startErr := manager.Start(s.serviceContext); startErr != nil {
				log.Printf("start workspace services in %s: %v", root, startErr)
			}
		}
	}
	return manager, workspace, nil
}

func (s *server) startServices(ctx context.Context) {
	s.serviceMu.Lock()
	managers := make([]*ServiceManager, 0, len(s.services))
	for _, manager := range s.services {
		managers = append(managers, manager)
	}
	s.serviceMu.Unlock()
	for _, manager := range managers {
		if err := manager.Start(ctx); err != nil {
			log.Printf("start workspace services in %s: %v", manager.Root(), err)
		}
	}
}

func (s *server) reconcileServices(ctx context.Context) error {
	s.serviceMu.Lock()
	managers := make([]*ServiceManager, 0, len(s.services))
	for _, manager := range s.services {
		managers = append(managers, manager)
	}
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
	managers := make([]*ServiceManager, 0, len(s.services))
	for _, manager := range s.services {
		managers = append(managers, manager)
	}
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

// serviceRootExists is intentionally tiny and used by doctor/compatibility
// callers to distinguish an old Workspace with no service directory from one
// that has an invalid service tree.
func serviceRootExists(root string) bool {
	_, err := workspacepath.ResolveControlDir(root)
	return err == nil
}
