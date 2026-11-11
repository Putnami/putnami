package nx

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
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
// tsc, and whether the project needs the tsc compiler written for that: a
// target that runs the package.json build script needs none, since the
// compiler auto reads that script, and an @nx/js:tsc target needs it. For a
// tsc target that does not map, reason says why; it is empty for a target
// that runs no tsc.
func tscBuild(root, dir string, target detect.Target) (covered, compiler bool, reason string) {
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
		if script, _ := buildScript(filepath.Join(root, filepath.FromSlash(dir))); script != target.Command {
			return false, false, fmt.Sprintf("@putnami/typescript compiles the tsc of the package.json build script, and %q is another script: build keeps running through nx run", command)
		}
		return true, false, ""
	case "nx:run-commands":
		if runsTsc {
			return false, false, fmt.Sprintf("@putnami/typescript compiles the tsc of the package.json build script, not the nx:run-commands command %q: build keeps running through nx run", command)
		}
	case nxJsTsc:
		if compiler := projectCompiler(root, dir); compiler != "" && compiler != "tsc" {
			return false, false, fmt.Sprintf("@nx/js:tsc compiles with tsc, and the project's putnami.json sets the compiler %s: build keeps running through nx run", compiler)
		}
		if reason := nxJsTscDiffers(root, dir, target.Options); reason != "" {
			return false, false, "@nx/js:tsc " + reason + ", which @putnami/typescript does not reproduce: build keeps running through nx run"
		}
		return true, true, ""
	}
	return false, false, ""
}

// nxJsTscOptions are the @nx/js:tsc options the tsc build reproduces once the
// executor copies no assets, runs no transformers and writes no package.json:
// the ones that decide what it compiles and where, and the ones it reads only
// to write the package.json, to clean or to watch. external and
// externalBuildTargets are not among them: set, they make the executor inline
// dependencies into its outputPath and rewrite their imports.
var nxJsTscOptions = []string{
	"additionalEntryPoints", "assets", "buildableProjectDepsInPackageJsonType",
	"clean", "generateExportsField",
	"generatePackageJson", "main", "outputPath", "rootDir", "transformers",
	"tsConfig", "updateBuildableProjectDepsInPackageJson", "watch",
}

// nxJsTscDiffers returns how the emit of an @nx/js:tsc target, with its
// effective options, differs from what @putnami/typescript compiles with tsc
// (ADR 0003), or "" when it is the same: the executor sets the config's
// outDir to its outputPath and its rootDir to its rootDir option, else the
// project root, and the tsc build compiles the config as written.
func nxJsTscDiffers(root, dir string, options map[string]any) string {
	keys := make([]string, 0, len(options))
	for key := range options {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if !slices.Contains(nxJsTscOptions, key) {
			return "sets " + key
		}
	}
	for _, key := range []string{"assets", "transformers"} {
		if list, ok := options[key].([]any); ok && len(list) > 0 {
			return "sets " + key
		}
	}
	if generate, ok := options["generatePackageJson"].(bool); !ok || generate {
		return "writes a package.json into its outputPath unless generatePackageJson is false"
	}

	abs := filepath.Join(root, filepath.FromSlash(dir))
	config, buildMode, reason := tscConfig(abs)
	if reason != "" {
		return reason
	}
	configRel := slashRel(root, config)
	tsConfig, _ := options["tsConfig"].(string)
	if tsConfig == "" || nxPath(tsConfig, dir) != configRel {
		return fmt.Sprintf("compiles %s, and @putnami/typescript compiles %s", orNone(tsConfig, dir), configRel)
	}

	chain := tscscript.ReadChain(config)
	configDir := filepath.Dir(config)
	if chain.True("noEmit") {
		return fmt.Sprintf("compiles %s, which sets noEmit", configRel)
	}
	if _, ok := chain.Path("outFile", configDir); ok {
		return fmt.Sprintf("compiles %s, which sets outFile", configRel)
	}
	outDir, ok := chain.Path("outDir", configDir)
	if !ok {
		return fmt.Sprintf("compiles %s, which sets no outDir", configRel)
	}
	outputPath, _ := options["outputPath"].(string)
	if outputPath == "" || nxPath(outputPath, dir) != slashRel(root, outDir) {
		return fmt.Sprintf("writes into the outputPath %s, and %s names the outDir %s", orNone(outputPath, dir), configRel, slashRel(root, outDir))
	}
	if !inside(abs, outDir) {
		return fmt.Sprintf("writes into the outputPath %s, outside the project, where the tsc build does not write", slashRel(root, outDir))
	}
	executorRoot := dir
	if value, _ := options["rootDir"].(string); value != "" {
		executorRoot = nxPath(value, dir)
	}
	rootDir, ok := chain.Path("rootDir", configDir)
	if !ok {
		return fmt.Sprintf("compiles with the rootDir %s, and %s declares none, so tsc computes it from the sources", executorRoot, configRel)
	}
	if slashRel(root, rootDir) != path.Clean(executorRoot) {
		return fmt.Sprintf("compiles with the rootDir %s, and %s declares the rootDir %s", executorRoot, configRel, slashRel(root, rootDir))
	}
	if writesBuildInfo := buildMode || chain.True("incremental") || chain.True("composite"); writesBuildInfo {
		if _, ok := chain.Path("tsBuildInfoFile", configDir); !ok {
			return fmt.Sprintf("places the .tsbuildinfo of %s, which sets no tsBuildInfoFile, by its own rule, which the tsc build may not follow", configRel)
		}
	}
	return ""
}

// tscConfig returns the config the tsc build compiles for the project at abs,
// as @putnami/typescript plans it: the one its package.json build script
// names, which must run tsc alone, else tsconfig.lib.json, else tsconfig.json;
// and whether it compiles with tsc -b. reason says why it compiles none.
func tscConfig(abs string) (config string, buildMode bool, reason string) {
	rel := tscscript.DefaultConfig(abs)
	if script, ok := buildScript(abs); ok {
		cmd, runs := tscscript.Parse(script)
		if !runs {
			return "", false, fmt.Sprintf("is the build, and the package.json build script %q, which the tsc build would compile, does not run tsc alone", script)
		}
		rel, buildMode = cmd.Config, cmd.Mode == tscscript.ModeBuild
		if rel == "" {
			rel = "tsconfig.json"
		}
	}
	config = filepath.Join(abs, filepath.FromSlash(rel))
	if info, err := os.Stat(config); err == nil && info.IsDir() {
		config = filepath.Join(config, "tsconfig.json")
	}
	if info, err := os.Stat(config); err != nil || info.IsDir() {
		return "", false, fmt.Sprintf("is the build, and the config %s the tsc build would compile does not exist", filepath.ToSlash(rel))
	}
	return config, buildMode, ""
}

// projectCompiler returns the compiler the project's putnami.json sets in
// options["@putnami/typescript"].compiler, or "" when it sets none.
func projectCompiler(root, dir string) string {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), "putnami.json")) //nolint:gosec // a manifest of the project
	if err != nil {
		return ""
	}
	var doc struct {
		Options map[string]map[string]any `json:"options"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return ""
	}
	compiler, _ := doc.Options[TypeScriptExtension]["compiler"].(string)
	return compiler
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

// orNone names an option's path for a reason, or "none" when it is unset.
func orNone(value, dir string) string {
	if value == "" {
		return "none"
	}
	return nxPath(value, dir)
}

// slashRel is path relative to base, as a slash path.
func slashRel(base, p string) string {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

// inside reports whether p lies strictly inside dir.
func inside(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
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
