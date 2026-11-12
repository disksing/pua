package update

import (
	"fmt"

	productversion "github.com/disksing/pua/internal/version"
)

type Installed struct {
	PUAVersion      string
	AgentHubVersion string
	ManagerProtocol int
	OS              string
	Arch            string
}

type Selection struct {
	Release ComponentRelease `json:"release"`
	Asset   Asset            `json:"asset"`
}

type Plan struct {
	PUA                *Selection `json:"pua,omitempty"`
	AgentHub           *Selection `json:"agentHub,omitempty"`
	AppUpdateRequired  bool       `json:"appUpdateRequired"`
	RequiredManager    int        `json:"requiredManager,omitempty"`
	CompatibilityError string     `json:"compatibilityError,omitempty"`
}

func Resolve(manifest Manifest, installed Installed) (Plan, error) {
	if err := manifest.Validate(); err != nil {
		return Plan{}, err
	}
	currentPUA, err := productversion.Parse(installed.PUAVersion)
	if err != nil {
		return Plan{}, fmt.Errorf("installed PUA version is invalid: %w", err)
	}
	currentAgentHub, err := productversion.Parse(installed.AgentHubVersion)
	if err != nil {
		return Plan{}, fmt.Errorf("installed AgentHub version is invalid: %w", err)
	}
	latestPUA := productversion.MustParse(manifest.PUA.Version)
	latestAgentHub := productversion.MustParse(manifest.AgentHub.Version)
	plan := Plan{}
	if productversion.Compare(latestPUA, currentPUA) > 0 {
		selection, err := selectRelease(manifest.PUA, installed)
		if err != nil {
			return Plan{}, err
		}
		plan.PUA = &selection
	}
	if productversion.Compare(latestAgentHub, currentAgentHub) > 0 {
		selection, err := selectRelease(manifest.AgentHub, installed)
		if err != nil {
			return Plan{}, err
		}
		plan.AgentHub = &selection
	}

	targetAgentHubVersion := installed.AgentHubVersion
	if plan.AgentHub != nil {
		targetAgentHubVersion = plan.AgentHub.Release.Version
	}
	if plan.PUA != nil {
		minimum := productversion.MustParse(plan.PUA.Release.MinAgentHubVersion)
		target := productversion.MustParse(targetAgentHubVersion)
		if productversion.Compare(target, minimum) < 0 {
			plan.CompatibilityError = fmt.Sprintf("PUA %s requires AgentHub %s or newer, but channel offers %s",
				plan.PUA.Release.Version, plan.PUA.Release.MinAgentHubVersion, targetAgentHubVersion)
		}
	}
	selected := make([]ComponentRelease, 0, 2)
	if plan.PUA != nil {
		selected = append(selected, plan.PUA.Release)
	}
	if plan.AgentHub != nil {
		selected = append(selected, plan.AgentHub.Release)
	}
	for _, release := range selected {
		if release.MinDesktopManagerProtocol > installed.ManagerProtocol {
			plan.AppUpdateRequired = true
			if release.MinDesktopManagerProtocol > plan.RequiredManager {
				plan.RequiredManager = release.MinDesktopManagerProtocol
			}
		}
	}
	return plan, nil
}

func selectRelease(release ComponentRelease, installed Installed) (Selection, error) {
	asset, err := release.AssetFor(installed.OS, installed.Arch)
	if err != nil {
		return Selection{}, err
	}
	return Selection{Release: release, Asset: asset}, nil
}
