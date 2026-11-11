package nx

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go.putnami.dev/sdk/extension/tscscript"
	detect "go.putnami.dev/tooling/observe/detect/nx"
)

// nxJsTsc is the Nx executor that compiles a library with tsc.
const nxJsTsc = "@nx/js:tsc"

// compilerRecord is the member of options["@putnami/nx"] in which the sync
// records the compiler it wrote in options["@putnami/typescript"], so it
// removes only a value it wrote.
const compilerRecord = "typescriptCompiler"

// tscBuild reports whether @putnami/typescript compiles a build target with
// tsc, and whether the project needs the tsc compiler written for that:
// a target that runs the package.json build script needs none, since the
// compiler auto reads that script, and an @nx/js:tsc target needs it. For a
// tsc target that does not map, reason says why; it is empty for a target
// that runs no tsc.
func tscBuild(root, dir string, target detect.Target) (covered, compiler bool, reason string) {
	abs := filepath.Join(root, filepath.FromSlash(dir))
	command := strings.TrimSpace(target.Command)
	runsTsc := strings.HasPrefix(command+" ", "tsc ")
	switch target.Executor {
	case "", "nx:run-script":
		if !runsTsc {
			return false, false, ""
		}
		if !tscscript.Runs(command) {
			return false, false, fmt.Sprintf("@putnami/typescript compiles a build script that runs tsc alone, not %q: build keeps running through nx run", command)
		}
		if script, _ := buildScript(abs); script != target.Command {
			return false, false, fmt.Sprintf("@putnami/typescript compiles the tsc of the package.json build script, and %q is another script: build keeps running through nx run", command)
		}
		return true, false, ""
	case "nx:run-commands":
		if runsTsc {
			return false, false, fmt.Sprintf("@putnami/typescript compiles the tsc of the package.json build script, not the nx:run-commands command %q: build keeps running through nx run", command)
		}
		return false, false, ""
	case nxJsTsc:
		return nxJsTscBuild(root, dir)
	}
	return false, false, ""
}

// nxJsTscBuild maps an @nx/js:tsc build target whose emit @putnami/typescript
// reproduces: it copies no assets, and it sets neither outputPath nor rootDir,
// with which the executor overrides where the config emits, so the config
// alone decides. It compiles the config tsc -p compiles by default, and the
// project has no build script other than one that runs tsc alone, which the
// tsc compiler would refuse.
func nxJsTscBuild(root, dir string) (bool, bool, string) {
	abs := filepath.Join(root, filepath.FromSlash(dir))
	options := buildOptions(abs)
	if assets, ok := options["assets"].([]any); ok && len(assets) > 0 {
		return false, false, "@nx/js:tsc copies assets, which @putnami/typescript does not reproduce: build keeps running through nx run"
	}
	for _, option := range []string{"outputPath", "rootDir"} {
		if value, _ := options[option].(string); value != "" {
			return false, false, fmt.Sprintf("@nx/js:tsc sets %s %s, which overrides where the tsconfig emits, and @putnami/typescript compiles the tsconfig as written: build keeps running through nx run", option, nxPath(value, dir))
		}
	}
	config := path.Join(dir, tscscript.DefaultConfig(abs))
	if tsConfig, _ := options["tsConfig"].(string); tsConfig != "" && nxPath(tsConfig, dir) != config {
		return false, false, fmt.Sprintf("@nx/js:tsc compiles %s, and @putnami/typescript compiles %s: build keeps running through nx run", nxPath(tsConfig, dir), config)
	}
	if script, ok := buildScript(abs); ok && !tscscript.Runs(script) {
		return false, false, fmt.Sprintf("the package.json build script %q does not run tsc alone, and @putnami/typescript would compile it: build keeps running through nx run", script)
	}
	return true, true, ""
}

// buildOptions returns the options of the project's build target, from its
// project.json, else from the nx member of its package.json.
func buildOptions(abs string) map[string]any {
	var doc struct {
		Targets map[string]struct {
			Options map[string]any `json:"options"`
		} `json:"targets"`
		Nx *struct {
			Targets map[string]struct {
				Options map[string]any `json:"options"`
			} `json:"targets"`
		} `json:"nx"`
	}
	for _, name := range []string{"project.json", "package.json"} {
		data, err := os.ReadFile(filepath.Join(abs, name)) //nolint:gosec // a manifest of the project
		if err != nil || json.Unmarshal(data, &doc) != nil {
			continue
		}
		if target, ok := doc.Targets["build"]; ok && name == "project.json" {
			return target.Options
		}
		if doc.Nx != nil {
			if target, ok := doc.Nx.Targets["build"]; ok {
				return target.Options
			}
		}
	}
	return nil
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

// nxPath turns a path of an Nx option, relative to the workspace root or
// written with {projectRoot} or {workspaceRoot}, into a clean
// workspace-relative slash path.
func nxPath(value, dir string) string {
	value = strings.ReplaceAll(value, "{projectRoot}", dir)
	value = strings.TrimPrefix(strings.ReplaceAll(value, "{workspaceRoot}", ""), "/")
	return path.Clean(strings.TrimPrefix(filepath.ToSlash(value), "./"))
}

// setCompiler writes options["@putnami/typescript"].compiler = "tsc" for a
// project whose @nx/js:tsc target maps, when the project sets no compiler,
// and records in options["@putnami/nx"] that it wrote it. Without want, it
// removes only a compiler it recorded, and only while the value is still the
// one it wrote; a compiler the project wrote stays.
func setCompiler(doc map[string]any, want bool) {
	options, _ := doc["options"].(map[string]any)
	if options == nil {
		options = map[string]any{}
	}
	ts, _ := options[TypeScriptExtension].(map[string]any)
	if ts == nil {
		ts = map[string]any{}
	}
	nx, _ := options[ExtensionName].(map[string]any)
	if nx == nil {
		nx = map[string]any{}
	}
	wrote := nx[compilerRecord] == "tsc"
	current, set := ts["compiler"]
	switch {
	case want && !set:
		ts["compiler"] = "tsc"
		nx[compilerRecord] = "tsc"
	case want && wrote && current != "tsc":
		// The project changed the value: it is the project's now.
		delete(nx, compilerRecord)
	case !want && wrote:
		if current == "tsc" {
			delete(ts, "compiler")
		}
		delete(nx, compilerRecord)
	}
	for name, layer := range map[string]map[string]any{TypeScriptExtension: ts, ExtensionName: nx} {
		if len(layer) == 0 {
			delete(options, name)
		} else {
			options[name] = layer
		}
	}
	if len(options) == 0 {
		delete(doc, "options")
	} else {
		doc["options"] = options
	}
}
