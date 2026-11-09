// Package moon is @putnami/moon, the integration of a moon repository: it
// runs the projects of a moon workspace as Putnami projects, with the stack
// core of go.putnami.dev/tooling/stacks and the moon detector of putnami
// observe, in the mode the workspace adopted it in. In delegate mode every
// task runs moon on one project, and moon keeps its own cache: the manifest
// disables Putnami's. In import mode a project whose language has a Putnami
// extension runs that extension instead, and every other project stays
// delegated to moon.
package moon

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	detect "go.putnami.dev/tooling/observe/detect/moon"
	"go.putnami.dev/tooling/stacks"
)

// ExtensionName is the extension's name.
const ExtensionName = "@putnami/moon"

// profileID is the id of the moon profile: the tool, the tag the sync writes
// and the prefix of the diagnostic codes.
const profileID = "moon"

// Modes of adoption, the value of the workspace's
// options["@putnami/moon"].mode.
const (
	// ModeImport runs each project whose language has a Putnami extension
	// through that extension, and delegates the others to moon.
	ModeImport = "import"
	// ModeDelegate runs every project through moon.
	ModeDelegate = "delegate"
)

// Commands are the Putnami commands a moon task maps to, by its name or a
// tag.
var Commands = []string{"build", "lint", "test"}

// LanguageExtensions maps a moon language to the Putnami extension a project
// of that language is imported into.
var LanguageExtensions = map[string]string{
	"go":         "@putnami/go",
	"php":        "@putnami/php",
	"python":     "@putnami/python",
	"ruby":       "@putnami/ruby",
	"rust":       "@putnami/rust",
	"typescript": "@putnami/typescript",
}

//go:embed profile.json
var profileJSON []byte

// Stack returns the moon stack: the embedded profile and the moon detector.
func Stack() (stacks.Stack, error) {
	profile, err := stacks.Decode(profileJSON)
	if err != nil {
		return stacks.Stack{}, fmt.Errorf("profile.json: %w", err)
	}
	return stacks.Stack{Extension: ExtensionName, Profile: profile, Detect: Detect}, nil
}

// Mode reads the adoption mode of the workspace at root, from its
// options["@putnami/moon"].mode; a workspace that states none, or a value
// that is no mode, delegates.
func Mode(root string) string {
	data, err := os.ReadFile(filepath.Join(root, wsproto.WorkspaceConfigFilename)) //nolint:gosec // the workspace config under the root
	if err != nil {
		return ModeDelegate
	}
	var cfg struct {
		Options map[string]struct {
			Mode string `json:"mode"`
		} `json:"options"`
	}
	if json.Unmarshal(data, &cfg) != nil || cfg.Options[ExtensionName].Mode != ModeImport {
		return ModeDelegate
	}
	return ModeImport
}

// ExtensionFor is the extension a project runs in the mode: its language's in
// import mode when the language has one, else @putnami/moon.
func ExtensionFor(mode string, p detect.Project) string {
	if mode == ModeImport {
		if ext, ok := LanguageExtensions[p.Language]; ok {
			return ext
		}
	}
	return ExtensionName
}

// Detect names the moon projects among the candidate paths that run
// @putnami/moon in the workspace's mode: each named by its moon project id,
// with the workspace root for stack root and moon's edges. A project the mode
// imports into a language extension is left to that extension, and a
// candidate that is no moon project is not answered.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	if !detect.IsWorkspace(root) {
		return nil, nil
	}
	detection := detect.Detect(root, detect.Options{})
	mode := Mode(root)
	projects := make(map[string]detect.Project, len(detection.Projects))
	for _, p := range detection.Projects {
		projects[p.Dir] = p
	}
	var out []stacks.Component
	for _, dir := range request.Paths {
		p, ok := projects[dir]
		if !ok || ExtensionFor(mode, p) != ExtensionName {
			continue
		}
		var deps []string
		for _, d := range p.DependsOn {
			if d != wsproto.ProbeRootPath {
				deps = append(deps, d)
			}
		}
		out = append(out, stacks.Component{
			Path:         p.Dir,
			Name:         p.ID,
			SourceFile:   p.SourceFile,
			Root:         wsproto.ProbeRootPath,
			Dependencies: deps,
		})
	}
	return out, detection.Diagnostics
}
