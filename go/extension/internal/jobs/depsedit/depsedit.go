// Package depsedit is the deps-edit job `putnami deps add`, `deps remove`
// and `deps prune` run for a Go project: it edits the requirements of one or
// more Go modules with `go mod edit`, then tidies each module as the workspace
// resolves it. A failure puts every go.mod and go.sum the job edited back as
// it was.
//
// The core CLI ran these steps itself before the Go extension owned them; the
// job keeps its go arguments, its failure messages and its progress lines. It
// starts no shell: see package workspacejob.
package depsedit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"

	"go.putnami.dev/go/extension/internal/jobs/depsupgrade"
	"go.putnami.dev/go/extension/internal/workspacejob"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Usage is the job's help line.
const Usage = "Usage: deps-edit --putnamiContext <file>"

const (
	statusOK     = "OK"
	statusFailed = "FAILED"
)

// The actions the job takes. add requires the versions `go list -m` resolves;
// remove and prune drop requirements. prune names each module to drop by the
// directory of the workspace project that declares it.
const (
	actionAdd    = "add"
	actionRemove = "remove"
	actionPrune  = "prune"
)

// edit is the change to the requirements of one Go module.
type edit struct {
	// Project is the name of the workspace project, for the messages.
	Project string `json:"project"`
	// Path is the module directory, relative to the workspace root.
	Path string `json:"path"`
	// Modules are module specs (add), module paths (remove) or the
	// workspace-relative directories of the projects whose module is dropped
	// (prune).
	Modules []string `json:"modules"`
}

// Run is the deps-edit job.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	j := workspacejob.New(context.Background(), emit, os.Environ(), ctx.WorkspaceRoot, "")
	defer j.Trap().Disarm()
	return run(j, ctx.Params, depsupgrade.ContextFileArg(os.Args[1:])), nil, nil
}

// run reads the action and the edits from params, edits every module as one
// transaction, and returns the result status.
func run(j *workspacejob.Job, params map[string]json.RawMessage, contextFile string) string {
	action, edits, problem := parseParams(params)
	if problem != "" {
		j.Emit.Diagnostic("error", problem, contextFile, 0)
		return statusFailed
	}
	if action == actionAdd {
		if problem := refuseDropOnAdd(edits); problem != "" {
			j.Emit.Diagnostic("error", problem, "", 0)
			return statusFailed
		}
	}
	// An edit resolves module versions and tidy downloads modules.
	if j.OfflineDependencies() {
		j.Emit.Diagnostic("error", "deps "+action+" reaches the network and rewrites go.mod and go.sum, "+
			"so it does not run when dependencies are offline (a hosted run)", "", 0)
		return statusFailed
	}
	opts, ok := depsupgrade.ParseOptions(j, nil, params, contextFile)
	if !ok {
		return statusFailed
	}
	for _, e := range edits {
		if goMod := filepath.Join(j.WorkspaceRoot, e.Path, "go.mod"); !workspacejob.FileExists(goMod) {
			j.Emit.Diagnostic("error", fmt.Sprintf("%s is not a Go module: %s has no go.mod", e.Project, e.Path), "", 0)
			return statusFailed
		}
	}
	// The go the workspace asks for: the go directive of go.work, else the one
	// of the first module. The job installs none: the core resolves the
	// toolchain before it runs the job.
	j.ProjectPath = filepath.Join(j.WorkspaceRoot, edits[0].Path)
	if !j.FindGoBinary() {
		j.Emit.Diagnostic("error", "deps "+action+": no go command is available; run `putnami install`", "", 0)
		return statusFailed
	}
	depsupgrade.SetupModuleEnv(j, opts)

	var snapshot workspacejob.Snapshot
	for _, e := range edits {
		dir := filepath.Join(j.WorkspaceRoot, e.Path)
		for _, file := range []string{"go.mod", "go.sum"} {
			if err := snapshot.Add(filepath.Join(dir, file)); err != nil {
				j.Emit.Diagnostic("error", err.Error(), "", 0)
				return statusFailed
			}
		}
	}
	restore := func() error { return snapshot.Restore(nil, nil) }
	// A SIGINT or a SIGTERM puts the files back too, then ends the job.
	j.Trap().Arm(func() { _ = restore() })
	for _, e := range edits {
		if err := apply(j, action, e); err != nil {
			message := err.Error() + "; every file the command edited is back as it was"
			if restoreErr := restore(); restoreErr != nil {
				message = err.Error() + "\n" + restoreErr.Error()
			}
			j.Trap().Disarm()
			j.Emit.Diagnostic("error", message, "", 0)
			return statusFailed
		}
	}
	j.Trap().Disarm()
	return statusOK
}

// parseParams reads the action and the edits, or names what is wrong with
// them.
func parseParams(params map[string]json.RawMessage) (string, []edit, string) {
	action := workspacejob.ParamText(params, "action")
	if action != actionAdd && action != actionRemove && action != actionPrune {
		return "", nil, fmt.Sprintf("deps-edit needs params.action to be add, remove or prune, not %q", action)
	}
	raw, _ := workspacejob.Param(params, "edits")
	var edits []edit
	if err := json.Unmarshal(raw, &edits); err != nil || len(edits) == 0 {
		return "", nil, "deps-edit needs params.edits: a non-empty array of {project, path, modules}"
	}
	for _, e := range edits {
		if e.Path == "" || !filepath.IsLocal(e.Path) {
			return "", nil, fmt.Sprintf("deps-edit needs the directory of %q relative to the workspace root, not %q", e.Project, e.Path)
		}
		if len(e.Modules) == 0 {
			return "", nil, fmt.Sprintf("deps %s requires at least one module (e.g. `putnami deps %s golang.org/x/text@latest`)", action, action)
		}
	}
	return action, edits, ""
}

// refuseDropOnAdd names the specs of an add whose version is none, which `go
// get` read as "drop this module", and the deps remove that drops them.
func refuseDropOnAdd(edits []edit) string {
	var dropped []string
	for _, e := range edits {
		for _, spec := range e.Modules {
			if path, version, ok := strings.Cut(spec, "@"); ok && version == "none" {
				dropped = append(dropped, path)
			}
		}
	}
	if len(dropped) == 0 {
		return ""
	}
	return fmt.Sprintf("deps add does not drop a module: %s names the version none; deps remove drops it: putnami deps remove %s",
		strings.Join(dropped, ", "), strings.Join(dropped, " "))
}

// apply edits the requirements of one module, then tidies it (tidyModule).
// add resolves the module specs with `go list -m` (resolveModules) and
// requires what that returns with `go mod edit -require`; remove and prune run
// `go mod edit -droprequire=<module>`.
//
// None of them runs `go get`. `go get` loads the module graph, which resolves
// every version go.mod requires through the module proxy, whatever the edit
// is. A requirement no proxy serves, such as `require example.com/lib v0.0.0`,
// the placeholder a workspace module keeps after its local `replace` is gone or
// that only go.work satisfies, then fails the very add or remove that would
// fix it.
func apply(j *workspacejob.Job, action string, e edit) error {
	dir := filepath.Join(j.WorkspaceRoot, e.Path)
	modules := e.Modules
	if action == actionPrune {
		var err error
		if modules, err = prunedModules(j.WorkspaceRoot, e); err != nil {
			return err
		}
	}
	verb := map[string]string{actionAdd: "Adding", actionRemove: "Removing", actionPrune: "Removing"}[action]
	j.Emit.Log("info", fmt.Sprintf("%s %s in %s...", verb, strings.Join(modules, " "), e.Project))

	args := []string{"-C", dir, "mod", "edit"}
	if action == actionAdd {
		resolved, err := resolveModules(j, dir, modules)
		if err != nil {
			return err
		}
		for _, m := range resolved {
			args = append(args, "-require="+m.Spec())
		}
	} else {
		for _, m := range modules {
			// A version the caller gave is not part of the requirement to drop.
			path, _, _ := strings.Cut(m, "@")
			args = append(args, "-droprequire="+path)
		}
	}
	if out, err := j.Combined(goWorkEnv(dir), j.GoBinary, args...); err != nil {
		return fmt.Errorf("go mod edit: %w%s", err, workspacejob.OutputExcerpt(out))
	}
	if err := tidyModule(j, dir); err != nil {
		return fmt.Errorf("go mod tidy: %w", err)
	}
	j.Emit.Log("info", "✓ updated "+filepath.ToSlash(filepath.Join(e.Path, "go.mod")))
	return nil
}

// prunedModules are the module paths the go.mod of each pruned project
// declares, sorted. A project's name is not its module path: a putnami.json
// name replaces both.
func prunedModules(workspaceRoot string, e edit) ([]string, error) {
	modules := make([]string, 0, len(e.Modules))
	for _, target := range e.Modules {
		module := ""
		if filepath.IsLocal(target) {
			module = modulePath(filepath.Join(workspaceRoot, target))
		}
		if module == "" {
			return nil, fmt.Errorf("cannot name the module of %s to remove it from %s", target, e.Project)
		}
		modules = append(modules, module)
	}
	slices.Sort(modules)
	return modules, nil
}

// modulePath is the module path the go.mod in dir declares, or "" when dir
// holds no readable go.mod.
func modulePath(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	return modfile.ModulePath(data)
}

// resolveModules returns the module version each spec names, from one `go
// list -m` in dir. A spec without a version asks for what `go get` asks for,
// @upgrade: @latest, unless go.mod already requires the module at a newer
// version, which is kept (keepNewerRequired).
//
// @upgrade is not queried itself: it reads the version go.mod requires, so it
// loads the module graph. @latest and an exact version load no graph, so they
// resolve a spec in a module that requires a version no proxy serves.
//
// GOWORK=off keeps the query out of the workspace build list, which resolves
// that version and fails; a version query reads no replace directive, so
// go.work changes no answer. -mod=readonly keeps go list from writing go.mod
// or go.sum, and in a module with a vendor directory it lets the query reach
// the proxy.
func resolveModules(j *workspacejob.Job, dir string, specs []string) ([]workspacejob.Module, error) {
	args := make([]string, 0, 6+len(specs))
	args = append(args, "-C", dir, "list", "-m", "-json", "-mod=readonly")
	bare := make([]bool, len(specs))
	for i, spec := range specs {
		if !strings.Contains(spec, "@") {
			bare[i] = true
			spec += "@latest"
		}
		args = append(args, spec)
	}
	out, errOut, err := j.Split([]string{"GOWORK=off"}, j.GoBinary, args...)
	if err != nil {
		return nil, fmt.Errorf("go list -m: %w (a spec names a module path, not a package path)%s", err, workspacejob.OutputExcerpt(errOut))
	}
	var resolved []workspacejob.Module
	for decoder := json.NewDecoder(strings.NewReader(out)); decoder.More(); {
		var listed workspacejob.Module
		if err := decoder.Decode(&listed); err != nil {
			return nil, fmt.Errorf("go list -m: read the go list output: %w", err)
		}
		if listed.Path == "" || listed.Version == "" {
			return nil, fmt.Errorf("go list -m: go list gave no version for %q", listed.Path)
		}
		resolved = append(resolved, listed)
	}
	if len(resolved) != len(specs) {
		return nil, fmt.Errorf("go list -m: go list resolved %d of %d modules", len(resolved), len(specs))
	}
	return keepNewerRequired(resolved, bare, requiredModules(dir)), nil
}

// keepNewerRequired returns resolved with, for each module whose spec named no
// version (bare), the version go.mod requires when that is semver-greater than
// the latest one: @upgrade never downgrades a module.
func keepNewerRequired(resolved []workspacejob.Module, bare []bool, required map[string]string) []workspacejob.Module {
	for i, m := range resolved {
		if have, ok := required[m.Path]; ok && bare[i] && semver.Compare(have, m.Version) > 0 {
			resolved[i].Version = have
		}
	}
	return resolved
}

// requiredModules returns the version of each module the go.mod in dir
// requires, by module path. A go.mod that is missing or does not parse
// requires none: go reports it.
func requiredModules(dir string) map[string]string {
	required := map[string]string{}
	goModPath := filepath.Join(dir, "go.mod")
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return required
	}
	file, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return required
	}
	for _, require := range file.Require {
		required[require.Mod.Path] = require.Mod.Version
	}
	return required
}
