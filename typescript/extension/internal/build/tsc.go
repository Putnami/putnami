package build

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/tscscript"
	"go.putnami.dev/typescript/extension/internal/parse"
	"go.putnami.dev/typescript/extension/internal/project"
)

// The compiler parameter's values.
const (
	CompilerAuto = "auto"
	CompilerBun  = "bun"
	CompilerTsc  = "tsc"
)

// Diagnostic codes of a tsc build.
const (
	CodeTscCompiler       = "typescript-tsc-compiler"
	CodeTscScript         = "typescript-tsc-unsupported-script"
	CodeTscConfig         = "typescript-tsc-config"
	CodeTscOutDir         = "typescript-tsc-outdir"
	CodeTscOutputIgnored  = "typescript-tsc-output-not-ignored"
	CodeTscOutputTracked  = "typescript-tsc-tracked-output"
	CodeTscMissing        = "typescript-tsc-missing"
	CodeTscEmittedNothing = "typescript-tsc-emitted-nothing"
	CodeTscPackageRefusal = "typescript-tsc-package-unsupported"
)

// TscError is a refusal of a tsc build, with the diagnostic code that names
// it and the file it is about, relative to the project.
type TscError struct {
	Code    string
	File    string
	Message string
}

func (e *TscError) Error() string { return e.Message }

func tscErrorf(code, file, format string, args ...any) *TscError {
	return &TscError{Code: code, File: file, Message: fmt.Sprintf(format, args...)}
}

// BuildScript returns the build script of the project's package.json, and
// whether it declares one.
func BuildScript(projectPath string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(projectPath, "package.json"))
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

// ResolveCompiler decides the compiler of a project from the compiler
// parameter: bun or tsc as given, and under auto, or when it is unset, tsc
// when the package.json build script runs tsc alone and bun otherwise.
func ResolveCompiler(param, projectPath string) (string, error) {
	switch param {
	case CompilerBun, CompilerTsc:
		return param, nil
	case "", CompilerAuto:
		if script, ok := BuildScript(projectPath); ok && tscscript.Runs(script) {
			return CompilerTsc, nil
		}
		return CompilerBun, nil
	}
	return "", tscErrorf(CodeTscCompiler, "", "compiler %q is none of auto, bun and tsc", param)
}

// TscPlan is how build-tsc compiles one project: the tsc it starts, with
// which arguments, and the outputs the compile writes. Paths are absolute;
// DeclarationDir and BuildInfo are empty when they lie inside OutDir or the
// compile writes none.
type TscPlan struct {
	Script         string
	Config         string
	Args           []string
	Tsc            string
	OutDir         string
	DeclarationDir string
	BuildInfo      string
}

// Outputs lists the plan's outputs, the outDir first.
func (p *TscPlan) Outputs() []string {
	outputs := []string{p.OutDir}
	for _, path := range []string{p.DeclarationDir, p.BuildInfo} {
		if path != "" {
			outputs = append(outputs, path)
		}
	}
	return outputs
}

// PlanTsc reads how a project compiles with tsc: its build script, or tsc -p
// on tsconfig.lib.json, else tsconfig.json, when it has none; the config that
// command compiles; and the outputs that config names. tsc -b on a config
// that declares references runs as tsc -p, because Putnami builds or restores
// each reference first and tsc -b would rebuild a restored one in place.
func PlanTsc(projectPath, workspaceRoot string) (*TscPlan, error) {
	script, hasScript := BuildScript(projectPath)
	var cmd tscscript.Command
	if hasScript {
		parsed, ok := tscscript.Parse(script)
		if !ok {
			return nil, tscErrorf(CodeTscScript, "package.json",
				"the build script %q does not run tsc alone (tsc, tsc -b or tsc -p with at most one config, plus --pretty or --verbose); set compiler to bun, or make the script run tsc alone", script)
		}
		cmd = parsed
	} else {
		cmd = tscscript.Command{Mode: tscscript.ModeProject, Config: tscscript.DefaultConfig(projectPath)}
	}

	config, err := tscConfigFile(projectPath, cmd.Config)
	if err != nil {
		return nil, err
	}
	relConfig := slashRel(projectPath, config)
	args := cmd.Args()
	buildMode := cmd.Mode == tscscript.ModeBuild
	if buildMode && declaresReferences(config) {
		args = []string{"-p", relConfig}
		for _, flag := range cmd.Flags {
			// --verbose is a flag of build mode only.
			if flag != "--verbose" {
				args = append(args, flag)
			}
		}
		buildMode = false
	}

	plan := &TscPlan{Script: script, Config: config, Args: args}
	if err := plan.resolveOutputs(projectPath, project.ReadTsConfigChain(config), buildMode); err != nil {
		return nil, err
	}
	tsc, ok := installedTsc(projectPath, workspaceRoot)
	if !ok {
		return nil, tscErrorf(CodeTscMissing, "package.json",
			"no node_modules/typescript/bin/tsc in the project directory or above it up to the workspace root: add typescript to the devDependencies and install")
	}
	plan.Tsc = tsc
	return plan, nil
}

// tscConfigFile resolves the config a command names the way tsc does: a
// directory stands for its tsconfig.json, and no config for the project's.
func tscConfigFile(projectPath, config string) (string, error) {
	if config == "" {
		config = "tsconfig.json"
	}
	path := filepath.Join(projectPath, filepath.FromSlash(config))
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, "tsconfig.json")
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "", tscErrorf(CodeTscConfig, filepath.ToSlash(config), "the config %s the build compiles does not exist", slashRel(projectPath, path))
	}
	return path, nil
}

// declaresReferences reports whether a config lists project references. A
// config does not inherit references through extends, so only its own count.
func declaresReferences(config string) bool {
	data, err := os.ReadFile(config)
	if err != nil {
		return false
	}
	var cfg struct {
		References []json.RawMessage `json:"references"`
	}
	if json.Unmarshal(project.StripJSONC(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})), &cfg) != nil {
		return false
	}
	return len(cfg.References) > 0
}

// resolveOutputs reads the outDir, declarationDir and .tsbuildinfo the
// config names, and refuses a config whose emit Putnami cannot own: no
// outDir, an outFile, noEmit, or an output outside the project.
func (p *TscPlan) resolveOutputs(projectPath string, chain project.TsConfigChain, buildMode bool) error {
	configDir := filepath.Dir(p.Config)
	rel := slashRel(projectPath, p.Config)
	if chain.True("noEmit") {
		return tscErrorf(CodeTscOutDir, rel, "%s sets noEmit, so tsc builds nothing: set compiler to bun, or remove noEmit", rel)
	}
	if _, ok := chain.Path("outFile", configDir); ok {
		return tscErrorf(CodeTscOutDir, rel, "%s sets outFile; Putnami compiles with tsc into an outDir only", rel)
	}
	outDir, ok := chain.Path("outDir", configDir)
	if !ok {
		return tscErrorf(CodeTscOutDir, rel, "%s sets no outDir, so tsc would write next to the sources: set compilerOptions.outDir", rel)
	}
	if err := insideProject(projectPath, outDir, "outDir", rel); err != nil {
		return err
	}
	p.OutDir = outDir

	if dir, ok := chain.Path("declarationDir", configDir); ok && !within(outDir, dir) {
		if err := insideProject(projectPath, dir, "declarationDir", rel); err != nil {
			return err
		}
		p.DeclarationDir = dir
	}

	if info, ok := buildInfoPath(chain, p.Config, outDir, buildMode); ok && !within(outDir, info) {
		if err := insideProject(projectPath, info, "the .tsbuildinfo", rel); err != nil {
			return err
		}
		p.BuildInfo = info
	}
	return nil
}

// buildInfoPath is where tsc writes the .tsbuildinfo of a config, by tsc's
// own rule: tsBuildInfoFile when set; otherwise, for an incremental or
// composite program or any tsc -b build, the config's name with the
// .tsbuildinfo extension, under outDir and as deep relative to rootDir as the
// config lies, so a config beside a rootDir of src writes it next to itself.
func buildInfoPath(chain project.TsConfigChain, config, outDir string, buildMode bool) (string, bool) {
	if !buildMode && !chain.True("incremental") && !chain.True("composite") {
		return "", false
	}
	configDir := filepath.Dir(config)
	if path, ok := chain.Path("tsBuildInfoFile", configDir); ok {
		return path, true
	}
	extensionLess := strings.TrimSuffix(config, filepath.Ext(config))
	if rootDir, ok := chain.Path("rootDir", configDir); ok {
		rel, err := filepath.Rel(rootDir, extensionLess)
		if err != nil {
			return "", false
		}
		return filepath.Join(outDir, rel) + ".tsbuildinfo", true
	}
	return filepath.Join(outDir, filepath.Base(extensionLess)) + ".tsbuildinfo", true
}

// insideProject refuses an output that is the project itself or lies outside
// it: Putnami owns, empties and restores only paths of the project.
func insideProject(projectPath, path, setting, config string) error {
	rel, err := filepath.Rel(projectPath, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return tscErrorf(CodeTscOutDir, config, "%s names %s for %s, which is not a path inside the project: Putnami empties and restores only a directory of the project it builds", config, path, setting)
	}
	return nil
}

func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// installedTsc finds the tsc the workspace installed: the first
// node_modules/typescript/bin/tsc from the project directory up to the
// workspace root. It names the package's script, not node_modules/.bin/tsc,
// which pnpm writes as a shell wrapper that starts node.
func installedTsc(projectPath, workspaceRoot string) (string, bool) {
	for dir := projectPath; ; dir = filepath.Dir(dir) {
		path := filepath.Join(dir, "node_modules", "typescript", "bin", "tsc")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, true
		}
		if dir == workspaceRoot || !within(workspaceRoot, dir) || filepath.Dir(dir) == dir {
			return "", false
		}
	}
}

// CheckTscOutputs refuses outputs Putnami cannot own in a Git work tree: one
// Git does not ignore, which would enter the build's own key, and one that
// holds a file Git tracks, which emptying it would delete. Outside a work
// tree it checks nothing.
func CheckTscOutputs(projectPath string, plan *TscPlan) error {
	if err := osexec.Command("git", "-C", projectPath, "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		return nil
	}
	rel := slashRel(projectPath, plan.Config)
	for i, output := range plan.Outputs() {
		path := slashRel(projectPath, output)
		tracked, err := osexec.Command("git", "-C", projectPath, "ls-files", "-z", "--", path).Output()
		if err != nil {
			return fmt.Errorf("list the files Git tracks under %s: %w", path, err)
		}
		if len(tracked) > 0 {
			first := strings.SplitN(string(tracked), "\x00", 2)[0]
			return tscErrorf(CodeTscOutputTracked, rel, "%s is an output of tsc, and Git tracks %s in it: building empties it, so move the committed files out of it or stop tracking them", path, first)
		}
		probe := path
		if i == 0 || output == plan.DeclarationDir {
			// A directory pattern such as dist/ matches a path under the
			// directory even before the directory exists.
			probe = path + "/.putnami-ignore-probe"
		}
		err = osexec.Command("git", "-C", projectPath, "check-ignore", "-q", "--no-index", "--", probe).Run()
		var exit *osexec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exit) && exit.ExitCode() == 1:
			return tscErrorf(CodeTscOutputIgnored, rel, "%s is an output of tsc that Git does not ignore: add it to .gitignore, so the files tsc emits do not key the build that emits them", path)
		default:
			return fmt.Errorf("ask Git whether it ignores %s: %w", path, err)
		}
	}
	return nil
}

// CleanTscOutputs removes the plan's outputs, so a file tsc stops emitting is
// gone after the run. The engine empties no output whose path the task
// reports, so the task does it.
func CleanTscOutputs(plan *TscPlan) error {
	for _, output := range plan.Outputs() {
		if err := os.RemoveAll(output); err != nil {
			return fmt.Errorf("empty the tsc output %s: %w", output, err)
		}
	}
	return nil
}

// TscResult is what a tsc compile reported.
type TscResult struct {
	Success     bool
	Diagnostics []parse.TscDiagnostic
	RawOutput   string
}

// RunTsc runs the plan's tsc with bun in the project directory. Bun runs the
// compiler's script itself, so no node program is needed.
func RunTsc(bunBin, projectPath string, plan *TscPlan) (*TscResult, error) {
	args := append([]string{plan.Tsc}, plan.Args...)
	result, err := execRunFunc(bunBin, args, exec.Dir(projectPath), exec.Timeout(10*time.Minute))
	if err != nil {
		return nil, err
	}
	raw := result.Stdout + result.Stderr
	return &TscResult{Success: result.Success, Diagnostics: parse.ParseTscOutput(raw), RawOutput: raw}, nil
}

func slashRel(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}
