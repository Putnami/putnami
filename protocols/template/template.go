// Package template provides types and loading logic for putnami template
// manifests (putnami.template.json).
package template

import (
	"encoding/json"
	"fmt"
	"os"
)

// ManifestFilename is the name of the template manifest file.
const ManifestFilename = "putnami.template.json"

// IsTemplateRootPackagingExclusion reports whether a top-level template entry
// describes the template project in its source workspace, or is local installed
// state, rather than content for the rendered project. Nested entries are
// ordinary template content.
func IsTemplateRootPackagingExclusion(name string) bool {
	switch name {
	case "node_modules", "putnami.json", "putnami.features.json", "specs":
		return true
	default:
		return false
	}
}

// Manifest is the root of putnami.template.json.
type Manifest struct {
	Schema                   string            `json:"$schema,omitempty"`
	Name                     string            `json:"name"`
	Version                  string            `json:"version,omitempty"`
	Description              string            `json:"description"`
	Extension                string            `json:"extension,omitempty"`
	WorkspaceDevDependencies map[string]string `json:"workspaceDevDependencies,omitempty"`
	TestVariables            map[string]string `json:"testVariables,omitempty"`
}

// LoadManifest reads and parses a putnami.template.json file.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read template manifest %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse template manifest %s: %w", path, err)
	}
	return &m, nil
}
