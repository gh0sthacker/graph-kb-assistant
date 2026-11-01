// Package buildinfo exposes reproducible Go build metadata without relying on
// deployment-specific environment variables.
package buildinfo

import (
	"fmt"
	"runtime/debug"
)

const version = "0.1.0-go-dev"

// Info describes the version and VCS state embedded by the Go toolchain.
type Info struct {
	Version  string
	Revision string
	Modified bool
}

// Current returns build metadata from the binary. Revision and Modified are
// empty/false when the binary was not built from a VCS-aware checkout.
func Current() Info {
	result := Info{Version: version}
	build, ok := debug.ReadBuildInfo()
	if !ok {
		return result
	}
	for _, setting := range build.Settings {
		switch setting.Key {
		case "vcs.revision":
			result.Revision = setting.Value
		case "vcs.modified":
			result.Modified = setting.Value == "true"
		}
	}
	return result
}

// String returns a compact version string suitable for CLI and MCP clients.
// It keeps the development version when revision metadata is unavailable.
func String() string {
	info := Current()
	if info.Revision == "" {
		return info.Version
	}
	revision := info.Revision
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if info.Modified {
		return fmt.Sprintf("%s+%s.dirty", info.Version, revision)
	}
	return fmt.Sprintf("%s+%s", info.Version, revision)
}
