// Package app composes AgentHub's persistent store, runtime, HTTP API, and
// Web UI into a reusable application. Both the standalone agenthub binary and
// PUA's embedded service use this package.
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/disksing/pua/agenthub/internal/api"
	"github.com/disksing/pua/agenthub/internal/config"
	"github.com/disksing/pua/agenthub/internal/daemon"
	"github.com/disksing/pua/agenthub/internal/paths"
	"github.com/disksing/pua/agenthub/internal/provider"
	"github.com/disksing/pua/agenthub/internal/runtime"
	"github.com/disksing/pua/agenthub/internal/session"
	agentweb "github.com/disksing/pua/agenthub/web"
)

const (
	// BasePath is AgentHub's canonical public mount path in both standalone
	// and PUA-embedded deployments.
	BasePath = "/agenthub"

	DefaultListenAddress = api.DefaultListenAddress
)

type Options struct {
	Address        string
	Version        string
	WebDir         string
	WebFS          fs.FS
	AllowedOrigins []string
}

type Service struct {
	paths         paths.Paths
	lock          *daemon.Lock
	manager       *runtime.Manager
	listenAddress *api.ListenAddress
	handler       http.Handler
	startedAt     time.Time
	version       string
	closing       chan struct{}
	closeOnce     sync.Once
	activeMu      sync.Mutex
	active        bool
	closeErr      error
}

func New(options Options) (*Service, error) {
	address := strings.TrimSpace(options.Address)
	if address == "" {
		address = DefaultListenAddress
	}
	listenAddress, err := api.ResolveListenAddress(address)
	if err != nil {
		return nil, err
	}
	normalizedOrigins := make([]string, 0, len(options.AllowedOrigins))
	for _, origin := range options.AllowedOrigins {
		normalized, err := api.NormalizeOrigin(origin)
		if err != nil {
			return nil, err
		}
		normalizedOrigins = append(normalizedOrigins, normalized)
	}
	resolved, err := paths.Resolve()
	if err != nil {
		return nil, err
	}
	if err := resolved.Ensure(); err != nil {
		return nil, err
	}
	lock, err := daemon.AcquireLock(resolved.LockFile)
	if err != nil {
		return nil, err
	}
	cleanupLock := true
	defer func() {
		if cleanupLock {
			_ = lock.Release()
		}
	}()
	store, err := session.Open(resolved.SessionsDir)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(resolved.ConfigFile)
	if err != nil {
		return nil, err
	}
	manager := runtime.New(store, cfg)
	webFS := options.WebFS
	if strings.TrimSpace(options.WebDir) == "" && webFS == nil {
		webFS, err = fs.Sub(agentweb.Assets, "static")
		if err != nil {
			manager.Close()
			return nil, fmt.Errorf("open embedded AgentHub Web assets: %w", err)
		}
	}
	startedAt := time.Now().UTC()
	closing := make(chan struct{})
	apiHandler := api.New(store, options.Version, startedAt, api.Dependencies{
		Runtime: manager, ConfigPath: resolved.ConfigFile, WebDir: options.WebDir, WebFS: webFS,
		Listen: listenAddress, Models: provider.NewModelCache(), LogsDir: resolved.LogsDir,
		Closing: closing, AllowedOrigins: normalizedOrigins, PublicBasePath: BasePath,
		EphemeralEnvironment: true,
	}).Handler()
	mux := http.NewServeMux()
	mux.Handle(BasePath+"/", http.StripPrefix(BasePath, apiHandler))
	mux.HandleFunc("GET "+BasePath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, BasePath+"/", http.StatusTemporaryRedirect)
	})
	cleanupLock = false
	return &Service{
		paths: resolved, lock: lock, manager: manager, listenAddress: listenAddress,
		handler: mux, startedAt: startedAt, version: options.Version, closing: closing,
	}, nil
}

func (s *Service) Handler() http.Handler { return s.handler }

func (s *Service) StandaloneHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			http.Redirect(w, r, BasePath+"/", http.StatusTemporaryRedirect)
			return
		}
		s.handler.ServeHTTP(w, r)
	})
}

func (s *Service) BindAddress() string { return s.listenAddress.BindAddress() }

func (s *Service) Endpoint() string { return s.listenAddress.Endpoint() + BasePath }

func (s *Service) Exposed() bool { return s.listenAddress.Exposed() }

// Activate publishes endpoint discovery after the owning listener has bound.
func (s *Service) Activate() error {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if s.active {
		return nil
	}
	if err := daemon.WriteState(s.paths.ServerFile, daemon.State{
		PID: os.Getpid(), Endpoint: s.Endpoint(), StartedAt: s.startedAt,
	}); err != nil {
		return err
	}
	s.active = true
	return nil
}

func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		close(s.closing)
		s.manager.Close()
		s.activeMu.Lock()
		if s.active {
			if err := os.Remove(s.paths.ServerFile); err != nil && !errors.Is(err, os.ErrNotExist) {
				s.closeErr = err
			}
			s.active = false
		}
		s.activeMu.Unlock()
		if err := s.lock.Release(); err != nil && s.closeErr == nil {
			s.closeErr = err
		}
	})
	return s.closeErr
}

// Serve runs a standalone AgentHub server until ctx is cancelled or the HTTP
// server fails. Signal handling remains the caller's responsibility.
func Serve(ctx context.Context, options Options) error {
	service, err := New(options)
	if err != nil {
		return err
	}
	defer service.Close()
	listener, err := net.Listen("tcp", service.BindAddress())
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w", service.BindAddress(), err)
	}
	defer listener.Close()
	if err := service.Activate(); err != nil {
		return err
	}
	fmt.Printf("AgentHub %s listening on %s\n", service.version, service.BindAddress())
	fmt.Printf("local endpoint: %s\n", service.Endpoint())
	if service.Exposed() {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintf(os.Stderr, "WARNING: AgentHub is listening on %s and is reachable from other machines.\n", service.BindAddress())
		fmt.Fprintln(os.Stderr, "AgentHub has NO authentication: anyone who can reach this address can run")
		fmt.Fprintln(os.Stderr, "agents, modify sessions and change the configuration. Only use this on")
		fmt.Fprintln(os.Stderr, "trusted networks. Do NOT expose the daemon to the public internet.")
		fmt.Fprintln(os.Stderr)
	}
	httpServer := &http.Server{
		Handler: service.StandaloneHandler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 75 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serverErrors <- err
	}()
	select {
	case err := <-serverErrors:
		return err
	case <-ctx.Done():
		_ = service.Close()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownContext); err != nil {
			_ = httpServer.Close()
			return nil
		}
		return <-serverErrors
	}
}
