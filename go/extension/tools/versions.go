// Package tools exposes the Go extension's pinned development-tool contract.
package tools

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// Manifest is the machine-readable toolchain contract shipped with the Go
// extension. CI images can read versions.json directly before baking tools.
type Manifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	GoVersion     string          `json:"goVersion"`
	Tools         map[string]Tool `json:"tools"`
}

// Tool describes one pinned development tool.
type Tool struct {
	Install string `json:"install"`
	Version string `json:"version"`
}

//go:embed versions.json
var versionsJSON []byte

var manifest Manifest

func init() {
	if err := json.Unmarshal(versionsJSON, &manifest); err != nil {
		panic(fmt.Sprintf("invalid embedded Go tool versions manifest: %v", err))
	}
	if manifest.SchemaVersion != 1 || manifest.GoVersion == "" || len(manifest.Tools) == 0 {
		panic("invalid embedded Go tool versions manifest")
	}
	for name, tool := range manifest.Tools {
		if tool.Install == "" || tool.Version == "" {
			panic(fmt.Sprintf("invalid embedded Go tool versions manifest entry for %s", name))
		}
	}
}

// ManifestContract returns the extension's immutable toolchain contract.
func ManifestContract() Manifest {
	entries := make(map[string]Tool, len(manifest.Tools))
	for name, tool := range manifest.Tools {
		entries[name] = tool
	}
	return Manifest{SchemaVersion: manifest.SchemaVersion, GoVersion: manifest.GoVersion, Tools: entries}
}

// Lookup returns the pinned contract for name.
func Lookup(name string) (Tool, bool) {
	tool, ok := manifest.Tools[name]
	return tool, ok
}
