package nx

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/sdk/extension/tscscript"
	detect "go.putnami.dev/tooling/observe/detect/nx"
)

// nxJsTsc is the Nx executor that compiles a library with tsc.
const nxJsTsc = "@nx/js:tsc"

// tscBuild reports whether @putnami/typescript compiles a build target with
// tsc: the target runs the package.json build script, and that script runs
// tsc alone, which the compiler auto reads. For a tsc target that does not
// map, reason says why; it is empty for a target that runs no tsc.
func tscBuild(root, dir string, target detect.Target) (bool, string) {
	command := strings.TrimSpace(target.Command)
	runsTsc := strings.HasPrefix(command+" ", "tsc ")
	switch target.Executor {
	case "", "nx:run-script":
		if !runsTsc {
			return false, ""
		}
		if !tscscript.Runs(command) {
			return false, fmt.Sprintf("@putnami/typescript compiles a build script that runs tsc alone, not %q: build keeps running through nx run", command)
		}
		if script, _ := buildScript(filepath.Join(root, filepath.FromSlash(dir))); script != target.Command {
			return false, fmt.Sprintf("@putnami/typescript compiles the tsc of the package.json build script, and %q is another script: build keeps running through nx run", command)
		}
		return true, ""
	case "nx:run-commands":
		if runsTsc {
			return false, fmt.Sprintf("@putnami/typescript compiles the tsc of the package.json build script, not the nx:run-commands command %q: build keeps running through nx run", command)
		}
	case nxJsTsc:
		return false, "@nx/js:tsc compiles with rootDir at the project root into its outputPath and writes a package.json there, which @putnami/typescript does not reproduce: build keeps running through nx run"
	}
	return false, ""
}

// buildScript returns the build script of the project's package.json.
func buildScript(abs string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(abs, "package.json")) //nolint:gosec // a manifest of the project
	if err != nil {
		return "", false
	}
	var pkg struct {
		Scripts map[string]any `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return "", false
	}
	script, ok := pkg.Scripts["build"].(string)
	return script, ok
}
