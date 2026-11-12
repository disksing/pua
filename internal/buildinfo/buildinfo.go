package buildinfo

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"strings"
)

// These values are intended to be set by release/build scripts via -ldflags.
// Keep semantic versions separate from the source identity: update selection
// compares Version, while SHA identifies the exact source tree.
var (
	Version                   = "0.1.0-dev"
	Channel                   = "dev"
	Branch                    = "unknown"
	SHA                       = "unknown"
	BuildTime                 = "unknown"
	EmbeddedAgentHubVersion   = "0.1.0-dev"
	MinAgentHubVersion        = "0.1.0"
	MinDesktopManagerProtocol = "1"
	ComponentManifestURL      = ""
	ComponentUpdatePublicKey  = ""
)

type Info struct {
	Component                 string `json:"component"`
	Version                   string `json:"version"`
	Channel                   string `json:"channel"`
	Branch                    string `json:"branch"`
	SHA                       string `json:"commit"`
	BuildTime                 string `json:"buildTime"`
	EmbeddedAgentHubVersion   string `json:"embeddedAgentHubVersion,omitempty"`
	MinAgentHubVersion        string `json:"minAgentHubVersion,omitempty"`
	MinDesktopManagerProtocol int    `json:"minDesktopManagerProtocol"`
}

func Current(component ...string) Info {
	name := "unknown"
	if len(component) > 0 && clean(component[0]) != "unknown" {
		name = clean(component[0])
	}
	info := Info{
		Component:                 name,
		Version:                   clean(Version),
		Channel:                   clean(Channel),
		Branch:                    clean(Branch),
		SHA:                       clean(SHA),
		BuildTime:                 clean(BuildTime),
		MinDesktopManagerProtocol: parsePositiveInt(MinDesktopManagerProtocol),
	}
	if name == "pua" {
		info.EmbeddedAgentHubVersion = clean(EmbeddedAgentHubVersion)
		info.MinAgentHubVersion = clean(MinAgentHubVersion)
	}
	if info.SHA == "unknown" {
		if revision := vcsRevision(); revision != "" {
			info.SHA = revision
		}
	}
	return info
}

func Text(program string) string {
	info := Current(program)
	return fmt.Sprintf("%s version=%s channel=%s branch=%s sha=%s\n", program, info.Version, info.Channel, info.Branch, info.SHA)
}

func JSON(program string) ([]byte, error) {
	return json.MarshalIndent(Current(program), "", "  ")
}

func IsDevelopment(info Info) bool {
	return info.Channel == "dev" || strings.Contains(info.Version, "-dev") || strings.Contains(info.Version, ".dirty")
}

func clean(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

func parsePositiveInt(value string) int {
	value = strings.TrimSpace(value)
	result := 0
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0
		}
		result = result*10 + int(char-'0')
	}
	return result
}

func vcsRevision() string {
	build, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range build.Settings {
		if setting.Key == "vcs.revision" {
			return strings.TrimSpace(setting.Value)
		}
	}
	return ""
}
