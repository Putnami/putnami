package project

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// putnamiRC represents putnami.json project configuration.
type putnamiRC struct {
	Name    string                     `json:"name"`
	Type    string                     `json:"type"`
	Tags    []string                   `json:"tags"`
	Options map[string]json.RawMessage `json:"options"`
}

// readPutnamiRC reads and parses a putnami.json file.
func readPutnamiRC(path string) (*putnamiRC, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rc putnamiRC
	if err := json.Unmarshal(data, &rc); err != nil {
		return nil, err
	}
	return &rc, nil
}

// GenerateAsset represents a {from, to} entry in options.generate.assets.
type GenerateAsset struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// GetGenerateAssets reads options.generate.assets from a project config file.
func GetGenerateAssets(rcPath string) []GenerateAsset {
	rc, err := readPutnamiRC(rcPath)
	if err != nil {
		return nil
	}

	genRaw, ok := rc.Options["generate"]
	if !ok {
		return nil
	}

	var genOpts struct {
		Assets []GenerateAsset `json:"assets"`
	}
	if json.Unmarshal(genRaw, &genOpts) != nil {
		return nil
	}
	return genOpts.Assets
}

// ResolveTsConfig finds the appropriate tsconfig.json for a project.
// Checks: tsconfig.app.json → tsconfig.lib.json → tsconfig.json → workspace tsconfig.json
func ResolveTsConfig(projectRoot, workspaceRoot string) string {
	candidates := []string{
		filepath.Join(projectRoot, "tsconfig.app.json"),
		filepath.Join(projectRoot, "tsconfig.lib.json"),
		filepath.Join(projectRoot, "tsconfig.json"),
		filepath.Join(workspaceRoot, "tsconfig.json"),
	}
	for _, c := range candidates {
		if FileExists(c) {
			return c
		}
	}
	return ""
}
