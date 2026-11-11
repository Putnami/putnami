package stackstest

import (
	"encoding/json"
	"os"
	"testing"
)

// Conventions of the command surface every stack extension shares (ADR 0015
// of tooling/stacks) that an extension may leave out, with its reason.
const (
	// ConventionTestFilter is the test command's test flag, short -t.
	ConventionTestFilter = "test-filter"
	// ConventionServeArgs is the serve command's args flag.
	ConventionServeArgs = "serve-args"
)

// Exemption names a convention a stack extension leaves out, and why.
type Exemption struct {
	Convention string
	Reason     string
}

// The flags each command of the shared surface declares.
var surfaceFlags = map[string][]string{
	"lint":    {"docs-links"},
	"test":    {"test"},
	"serve":   {"port", "args"},
	"run":     {"args"},
	"package": {"docker", "docker-base-image", "docker-tag", "docker-load", "dry-run", "platform", "port"},
	"publish": {"docker", "docker-registry", "dry-run"},
}

// The defaults the surface fixes for a flag of a command. A nil default means
// the manifest declares none: package --port is shared with the Go and
// TypeScript extensions, whose default differs, so each stack applies 8080 in
// code (stacks.ImagePort) and the manifests stay compatible.
var surfaceDefaults = map[[2]string]any{
	{"lint", "docs-links"}: true,
	{"serve", "port"}:      float64(3000),
	{"package", "port"}:    nil,
}

type surfaceFlag struct {
	Type    string `json:"type"`
	Short   string `json:"short"`
	Default any    `json:"default"`
}

type surfaceManifest struct {
	Workspace struct {
		InstallInputs []string `json:"installInputs"`
	} `json:"workspace"`
	Commands map[string]struct {
		Flags map[string]surfaceFlag `json:"flags"`
		Run   []commandStep          `json:"run"`
	} `json:"commands"`
}

// CheckSurface checks the manifest at path against the command surface every
// stack extension shares: the flags of lint, test, serve, run, package and
// publish when it declares them, the documentation link check of lint, and the
// install inputs of an extension with a workspace-install. An exemption leaves
// one convention out; it must state its reason.
func CheckSurface(t testing.TB, path string, exemptions ...Exemption) {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the extension's own committed manifest
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc surfaceManifest
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	exempt := map[string]bool{}
	for _, e := range exemptions {
		if e.Reason == "" {
			t.Errorf("the exemption from %s states no reason", e.Convention)
		}
		exempt[e.Convention] = true
	}
	for command, flags := range surfaceFlags {
		c, declared := doc.Commands[command]
		if !declared {
			continue
		}
		for _, name := range flags {
			if command == "test" && name == "test" && exempt[ConventionTestFilter] || command == "serve" && name == "args" && exempt[ConventionServeArgs] {
				continue
			}
			flag, ok := c.Flags[name]
			if !ok {
				t.Errorf("command %s declares no %s flag", command, name)
				continue
			}
			if want, fixed := surfaceDefaults[[2]string{command, name}]; fixed && flag.Default != want {
				t.Errorf("command %s flag %s defaults to %v, want %v", command, name, flag.Default, want)
			}
			if command == "test" && name == "test" && flag.Short != "-t" {
				t.Errorf("the test flag's short form is %q, want -t", flag.Short)
			}
		}
	}
	if lint, ok := doc.Commands["lint"]; ok && !runsTask(lint.Run, "lint-docs") {
		t.Errorf("lint runs no lint-docs task")
	}
	if _, ok := doc.Commands["workspace-install"]; ok && len(doc.Workspace.InstallInputs) == 0 {
		t.Errorf("an extension with a workspace-install declares no workspace.installInputs")
	}
	if exempt[ConventionTestFilter] {
		if _, ok := doc.Commands["test"].Flags["test"]; ok {
			t.Errorf("the test filter is exempted but declared")
		}
	}
}

func runsTask(steps []commandStep, task string) bool {
	for _, s := range steps {
		if s.Task == task {
			return true
		}
	}
	return false
}
