// Package template provides types and loading logic for putnami template
// manifests (putnami.template.json).
package template

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ManifestFilename is the name of the template manifest file.
const ManifestFilename = "putnami.template.json"

// IsTemplateRootPackagingExclusion reports whether a top-level template entry
// stays out of what the template ships: a name that starts with a dot, local
// installed state, or a declaration that describes the template project in its
// source workspace rather than content for the rendered project (its project
// configuration, its feature, its specs and the baseline that protects them).
// The archive leaves these entries out, and a render from the source directory
// skips them, so both produce the same project. Nested entries are ordinary
// template content.
func IsTemplateRootPackagingExclusion(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "node_modules", "putnami.json", "putnami.features.json", "specs", "specs.baseline.json":
		return true
	default:
		return false
	}
}

// Manifest is the root of putnami.template.json.
type Manifest struct {
	Schema        string            `json:"$schema,omitempty"`
	Name          string            `json:"name"`
	Version       string            `json:"version,omitempty"`
	Description   string            `json:"description"`
	Extension     string            `json:"extension,omitempty"`
	TestVariables map[string]string `json:"testVariables,omitempty"`
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
