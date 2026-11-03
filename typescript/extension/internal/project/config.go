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

// ConfigPath returns the path to the project config file in dir, preferring
// putnami.json and falling back to .putnamirc.json.
func ConfigPath(dir string) string {
	p := filepath.Join(dir, "putnami.json")
	if FileExists(p) {
		return p
	}
	legacy := filepath.Join(dir, ".putnamirc.json")
	if FileExists(legacy) {
		return legacy
	}
	return p
}

// CommitsCapabilityManifest reports whether the project config at rcPath sets
// options.generate.capabilities to true. A missing file, a missing block and a
// block that does not decode answer false; a file that cannot be read or
// parsed is an error, because the answer decides whether a build removes a
// tracked file.
func CommitsCapabilityManifest(rcPath string) (bool, error) {
	rc, err := readPutnamiRC(rcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	raw, ok := rc.Options["generate"]
	if !ok {
		return false, nil
	}
	var opts struct {
		Capabilities *bool `json:"capabilities"`
	}
	if json.Unmarshal(raw, &opts) != nil || opts.Capabilities == nil {
		return false, nil
	}
	return *opts.Capabilities, nil
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
