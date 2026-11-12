//go:build darwin

package main

import (
	"context"
	"embed"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/disksing/pua/internal/buildinfo"
	"github.com/disksing/pua/internal/desktop"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed assets/*
var assets embed.FS

type DesktopService struct {
	manager   *desktop.Manager
	onChanged func()
}

func (service *DesktopService) Status() desktop.Status {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return service.manager.Status(ctx)
}

func (service *DesktopService) Start() error {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := service.manager.Start(ctx); err != nil {
		return err
	}
	service.onChanged()
	return nil
}

func (service *DesktopService) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := service.manager.Stop(ctx); err != nil {
		return err
	}
	service.onChanged()
	return nil
}

func (service *DesktopService) Restart() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := service.manager.Restart(ctx); err != nil {
		return err
	}
	service.onChanged()
	return nil
}

func (service *DesktopService) SaveConfig(config desktop.Config) (bool, error) {
	return service.manager.SaveConfig(config)
}

func (service *DesktopService) OpenFullDiskAccessSettings() error {
	return desktop.OpenFullDiskAccessSettings()
}

func (service *DesktopService) CheckUpdates() (desktop.UpdateCheck, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return service.manager.CheckUpdates(ctx)
}

func (service *DesktopService) InstallUpdates(components []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := service.manager.InstallUpdates(ctx, components); err != nil {
		return err
	}
	service.onChanged()
	return nil
}

type windowSet struct {
	mu      sync.Mutex
	app     *application.App
	manager *desktop.Manager
	windows map[string]*application.WebviewWindow
}

func (set *windowSet) show(name string) {
	set.mu.Lock()
	defer set.mu.Unlock()
	if window := set.windows[name]; window != nil {
		if name != "services" {
			set.updateRemoteURL(name, window)
		}
		window.UnMinimise()
		window.Show()
		window.Focus()
		return
	}
	options := application.WebviewWindowOptions{Name: name, Width: 1240, Height: 820, MinWidth: 840, MinHeight: 580}
	switch name {
	case "services":
		options.Title, options.URL, options.Width, options.Height = "PUA Service Manager", "/", 960, 760
	case "agenthub":
		options.Title = "AgentHub"
	case "beeper":
		options.Title, options.Width, options.Height = "AgentHub Beeper", 760, 680
	default:
		options.Name, options.Title, name = "main", "PUA", "main"
	}
	if name != "services" {
		options.URL = set.currentWindowURL(name)
	}
	window := set.app.Window.NewWithOptions(options)
	set.windows[name] = window
	window.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		event.Cancel()
		window.Hide()
	})
}

// windowURL resolves the remote URL a service window should show. PUA windows
// load the PUA server; AgentHub and Beeper windows load the standalone
// AgentHub endpoint directly, because a managed PUA server runs with
// --agenthub-mode=external and does not serve /agenthub/ itself.
func windowURL(status desktop.Status, name string) string {
	base := strings.TrimRight(status.PUA.Endpoint, "/")
	suffix := "/"
	if name == "agenthub" || name == "beeper" {
		base = strings.TrimRight(status.AgentHub.Endpoint, "/")
		if name == "beeper" {
			suffix = "/beeper"
		}
	}
	if base == "" {
		return ""
	}
	return base + suffix
}

func (set *windowSet) currentWindowURL(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return windowURL(set.manager.Status(ctx), name)
}

func (set *windowSet) updateRemoteURL(name string, window *application.WebviewWindow) {
	if url := set.currentWindowURL(name); url != "" {
		window.SetURL(url)
	}
}

func (set *windowSet) refreshRemoteWindows() {
	set.mu.Lock()
	defer set.mu.Unlock()
	for name, window := range set.windows {
		if name != "services" {
			set.updateRemoteURL(name, window)
		}
	}
}

func main() {
	options, err := desktop.DefaultOptions()
	if err != nil {
		fatal(err)
	}
	manager, err := desktop.NewManager(options)
	if err != nil {
		fatal(err)
	}
	var quitInProgress atomic.Bool
	var quitApproved atomic.Bool
	var app *application.App
	windows := &windowSet{manager: manager, windows: map[string]*application.WebviewWindow{}}
	service := &DesktopService{manager: manager, onChanged: windows.refreshRemoteWindows}
	appName, uniqueID := "PUA", "com.disksing.pua.desktop"
	if buildinfo.IsDevelopment(buildinfo.Current("macapp")) {
		appName, uniqueID = "PUA Dev", "com.disksing.pua.desktop.dev"
	}
	app = application.New(application.Options{
		Name:        appName,
		Description: "PUA desktop application and service manager",
		Assets: application.AssetOptions{
			Handler:        application.BundledAssetFileServer(assets),
			DisableLogging: true,
		},
		Services: []application.Service{application.NewService(service)},
		ShouldQuit: func() bool {
			if quitApproved.Load() {
				return true
			}
			if quitInProgress.CompareAndSwap(false, true) {
				go prepareQuit(app, manager, &quitInProgress, &quitApproved)
			}
			return false
		},
		Mac: application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: false},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: uniqueID,
			OnSecondInstanceLaunch: func(data application.SecondInstanceData) {
				if hasArgument(data.Args, "--service-manager") {
					windows.show("services")
				} else {
					windows.show("main")
				}
			},
			ExitCode: 0,
		},
	})
	windows.app = app
	startSparkle()
	installMenu(app, windows)
	ctx, cancel := context.WithTimeout(context.Background(), options.StartupTimeout+5*time.Second)
	_, backendErr := manager.Ensure(ctx)
	cancel()
	if backendErr != nil || hasArgument(os.Args, "--service-manager") {
		windows.show("services")
	} else {
		windows.show("main")
	}
	go automaticUpdateChecks(app, manager, windows)
	if err := app.Run(); err != nil {
		fatal(err)
	}
}

func hasArgument(arguments []string, target string) bool {
	for _, argument := range arguments {
		if argument == target {
			return true
		}
	}
	return false
}

func installMenu(app *application.App, windows *windowSet) {
	menu := app.NewMenu()
	menu.AddRole(application.AppMenu)
	navigate := menu.AddSubmenu("Navigate")
	navigate.Add("PUA").SetAccelerator("CmdOrCtrl+1").OnClick(func(*application.Context) { windows.show("main") })
	navigate.Add("AgentHub").SetAccelerator("CmdOrCtrl+2").OnClick(func(*application.Context) { windows.show("agenthub") })
	navigate.Add("Beeper").SetAccelerator("CmdOrCtrl+3").OnClick(func(*application.Context) { windows.show("beeper") })
	navigate.AddSeparator()
	navigate.Add("Service Manager…").SetAccelerator("CmdOrCtrl+,").OnClick(func(*application.Context) { windows.show("services") })
	navigate.AddSeparator()
	navigate.Add("Check for PUA.app Updates…").OnClick(func(*application.Context) { checkSparkle() })
	menu.AddRole(application.EditMenu)
	menu.AddRole(application.ViewMenu)
	menu.AddRole(application.WindowMenu)
	menu.AddRole(application.HelpMenu)
	app.Menu.SetApplicationMenu(menu)
}

func automaticUpdateChecks(app *application.App, manager *desktop.Manager, windows *windowSet) {
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			status := manager.Status(ctx)
			if status.Config.AutoCheck && manager.AutomaticUpdateDue(time.Now()) {
				check, err := manager.CheckUpdates(ctx)
				if err == nil && (check.Plan.PUA != nil || check.Plan.AgentHub != nil || check.Plan.AppUpdateRequired) {
					app.Dialog.Info().SetTitle("PUA updates are available").SetMessage("Open Service Manager to review and install component updates. PUA.app updates are offered separately.").Show()
					windows.show("services")
				}
			}
			cancel()
			timer.Reset(time.Hour)
		}
	}
}

func prepareQuit(app *application.App, manager *desktop.Manager, inProgress, approved *atomic.Bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	status := manager.Status(ctx)
	activeTurns, activeErr := manager.ActiveTurns(ctx)
	cancel()
	if !status.PUA.Managed && !status.AgentHub.Managed {
		approved.Store(true)
		app.Quit()
		return
	}
	if activeErr == nil && activeTurns == 0 {
		stopAndQuit(app, manager, inProgress, approved)
		return
	}
	message := "PUA could not confirm whether AgentHub has active turns. Quitting will stop the managed services and may interrupt work."
	if activeErr == nil {
		message = fmt.Sprintf("AgentHub has %d active turn(s). Quitting will stop the managed services and interrupt that work.", activeTurns)
	}
	dialog := app.Dialog.Question().SetTitle("Stop services and quit PUA?").SetMessage(message)
	dialog.AddButton("Cancel").SetAsDefault().SetAsCancel().OnClick(func() { inProgress.Store(false) })
	dialog.AddButton("Stop and Quit").OnClick(func() { go stopAndQuit(app, manager, inProgress, approved) })
	dialog.Show()
}

func stopAndQuit(app *application.App, manager *desktop.Manager, inProgress, approved *atomic.Bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := manager.Stop(ctx)
	cancel()
	if err != nil {
		inProgress.Store(false)
		app.Dialog.Error().SetTitle("PUA could not stop its services").SetMessage(err.Error()).Show()
		return
	}
	approved.Store(true)
	app.Quit()
}

func fatal(err error) {
	_, _ = fmt.Fprintf(os.Stderr, "pua-desktop: %v\n", err)
	os.Exit(1)
}
