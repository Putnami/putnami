// Package projectsetup is the project-setup job `putnami projects create` runs
// after it renders a template of the Go extension: it adds the created project
// to go.work, aligns its go directive with the workspace's, pins the
// go.putnami.dev modules its go.mod requires at the placeholder version v0.0.0
// to the version the channel names, and runs go mod tidy.
//
// Its prepare stage (params.stage "prepare") only writes a missing go.work,
// the file the workspace lock pins Go from, and runs no go command: the core
// runs it on a host where the Go the extension locks does not resolve, pins
// and installs that Go, then runs the job without a stage.
//
// The core CLI ran these steps itself before the Go extension owned them; the
// job keeps its go arguments and its progress lines. It starts no shell: see
// package workspacejob.
package projectsetup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/go/extension/internal/jobs/depsupgrade"
	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
)

// Usage is the job's help line.
const Usage = "Usage: project-setup --putnamiContext <file>"

const (
	statusOK     = "OK"
	statusFailed = "FAILED"
)

// stagePrepare is the params.stage of a run that only writes the files the
// workspace lock pins a toolchain from.
const stagePrepare = "prepare"

// channelParam names the channel the framework version is resolved on: the
// channel of the `putnami init` that runs the create, absent on latest. The
// lookup receives it as given.
const channelParam = "putnami-channel"

// frameworkPrefix is the module path prefix of the Putnami framework.
const frameworkPrefix = "go.putnami.dev/"

// placeholderVersion is the version a template requires a framework module
// at: the job replaces it with the version the channel names.
const placeholderVersion = "v0.0.0"

// Refresh asks @putnami/cloud to write the netrc entry for a module origin
// host. Its outcome never fails the job.
type Refresh func(ctx context.Context, workspaceRoot, host string) registrycred.Outcome

// Job is the project-setup job. refresh renews the netrc credential for the
// origin of a framework module the go command reads from its origin only: the
// entry a past sign-in wrote expires within hours, and the origin answers 401
// to an expired credential.
func Job(refresh Refresh) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		j := workspacejob.New(context.Background(), emit, os.Environ(), ctx.WorkspaceRoot, "")
		defer j.Trap().Disarm()
		s := &setup{Job: j, refresh: refresh, client: newClient()}
		return s.run(ctx.Params, depsupgrade.ContextFileArg(os.Args[1:])), nil, nil
	}
}

// setup is one run of the job.
type setup struct {
	*workspacejob.Job
	refresh Refresh
	client  *http.Client
}

// run sets the project params name up and returns the result status.
func (s *setup) run(params map[string]json.RawMessage, contextFile string) string {
	path := workspacejob.ParamText(params, "path")
	if path == "" || !filepath.IsLocal(path) {
		s.Emit.Diagnostic("error", "project-setup needs the directory of the created project, relative to the workspace root (params.path)", contextFile, 0)
		return statusFailed
	}
	stage := workspacejob.ParamText(params, "stage")
	if stage != "" && stage != stagePrepare {
		s.Emit.Diagnostic("error", fmt.Sprintf("project-setup needs params.stage to be %s or absent, not %q", stagePrepare, stage), contextFile, 0)
		return statusFailed
	}
	// The setup asks module versions over its own HTTP client, which the
	// offline GOPROXY does not govern, and go mod tidy downloads modules.
	if s.OfflineDependencies() {
		s.Emit.Diagnostic("error", "project setup reaches the network and rewrites go.work, go.mod and go.sum, "+
			"so it does not run when dependencies are offline (a hosted run)", "", 0)
		return statusFailed
	}
	projectDir := filepath.Join(s.WorkspaceRoot, path)
	s.ProjectPath = projectDir
	goMod := filepath.Join(projectDir, "go.mod")
	if !workspacejob.FileExists(goMod) {
		s.Emit.Log("info", path+" has no go.mod: nothing to set up")
		return statusOK
	}
	goWork := filepath.Join(s.WorkspaceRoot, "go.work")
	goVersion := workspacejob.GoDirective(goWork)
	if goVersion == "" {
		goVersion = workspacejob.GoDirective(goMod)
	}
	if stage == stagePrepare {
		if !s.writeGoWork(goWork, path, goVersion) {
			return statusFailed
		}
		return statusOK
	}
	opts, ok := depsupgrade.ParseOptions(s.Job, nil, params, contextFile)
	if !ok {
		return statusFailed
	}
	if !s.ensureGo(goWork, path, goVersion) {
		return statusFailed
	}
	depsupgrade.SetupModuleEnv(s.Job, opts)
	pins, ok := s.resolveFramework(goMod, workspacejob.ParamText(params, channelParam))
	if !ok {
		return statusFailed
	}
	if !s.addToWorkspace(goWork, path, goVersion) {
		return statusFailed
	}
	if len(pins) > 0 {
		args := []string{"-C", projectDir, "mod", "edit"}
		for _, pin := range pins {
			args = append(args, "-require="+pin.Spec())
		}
		if out, err := s.Combined(nil, s.GoBinary, args...); err != nil {
			s.Emit.Diagnostic("error", "go mod edit: "+out, goMod, 0)
			return statusFailed
		}
	}
	// A project tidy cannot resolve does not build, so its failure fails the
	// setup and quotes what go said.
	if out, err := s.Combined(nil, s.GoBinary, "-C", projectDir, "mod", "tidy"); err != nil {
		s.Emit.Diagnostic("error", fmt.Sprintf("go mod tidy failed in %s: %v%s", path, err, workspacejob.OutputExcerpt(out)), goMod, 0)
		return statusFailed
	}
	return statusOK
}

// ensureGo selects the go command the setup runs: the one the extension's
// tasks resolve, the release the workspace lock pins, else a go on PATH
// (FindGoBinary). It installs none: the core resolves the toolchain before it
// runs the job, so a missing Go is the core's to install. Without one, it
// writes the missing go.work the lock pins Go from, so that `putnami install`
// pins and installs that Go, and fails naming that command.
func (s *setup) ensureGo(goWork, path, goVersion string) bool {
	if s.FindGoBinary() {
		return true
	}
	s.writeGoWork(goWork, path, goVersion)
	s.Emit.Diagnostic("error", "Cannot set "+path+" up: no go command is available; "+
		"run `putnami install`, which pins and installs the Go go.work declares", "", 0)
	return false
}

// writeGoWork writes a missing go.work for the project at path with the bytes
// `go work init`, `go work use` and `go work edit -go` leave: the file the
// workspace lock pins Go from. It writes nothing when go.work exists or when
// no go directive names a version.
func (s *setup) writeGoWork(goWork, path, goVersion string) bool {
	if workspacejob.FileExists(goWork) || goVersion == "" {
		return true
	}
	content := "go " + goVersion + "\n\nuse ./" + filepath.ToSlash(path) + "\n"
	if err := os.WriteFile(goWork, []byte(content), 0o644); err != nil { //nolint:gosec // G306: go.work is a workspace source file, read by every tool
		s.Emit.Diagnostic("error", "write go.work: "+err.Error(), goWork, 0)
		return false
	}
	s.Emit.Log("info", "Initialized go.work")
	return true
}

// addToWorkspace adds the project to go.work, creating it when missing, and
// gives go.work and the project's go.mod the workspace's go directive.
func (s *setup) addToWorkspace(goWork, path, goVersion string) bool {
	projectDir := filepath.Join(s.WorkspaceRoot, path)
	if !workspacejob.FileExists(goWork) {
		if out, err := s.Combined(nil, s.GoBinary, "-C", s.WorkspaceRoot, "work", "init"); err != nil {
			s.Emit.Diagnostic("error", "go work init: "+out, goWork, 0)
			return false
		}
		s.Emit.Log("info", "Initialized go.work")
	}
	if out, err := s.Combined(nil, s.GoBinary, "-C", s.WorkspaceRoot, "work", "use", "./"+filepath.ToSlash(path)); err != nil {
		s.Emit.Diagnostic("error", "go work use: "+out, goWork, 0)
		return false
	}
	if goVersion != "" {
		if out, err := s.Combined(nil, s.GoBinary, "-C", s.WorkspaceRoot, "work", "edit", "-go="+goVersion); err != nil {
			s.Emit.Diagnostic("error", "go work edit: "+out, goWork, 0)
			return false
		}
		if out, err := s.Combined(nil, s.GoBinary, "-C", projectDir, "mod", "edit", "-go="+goVersion); err != nil {
			s.Emit.Diagnostic("error", "go mod edit: "+out, filepath.Join(projectDir, "go.mod"), 0)
			return false
		}
	}
	s.Emit.Log("info", "Added "+path+" to go.work")
	return true
}

// resolveFramework returns the version channel names for each framework
// module goMod requires at the placeholder version, in go.mod order. A go.mod
// without a placeholder asks nothing.
func (s *setup) resolveFramework(goMod, channel string) ([]workspacejob.Module, bool) {
	mod, err := s.ReadModFile(goMod)
	if err != nil {
		s.Emit.Diagnostic("error", "go mod edit -json: "+err.Error(), goMod, 0)
		return nil, false
	}
	var modules []string
	for _, require := range mod.Require {
		if strings.HasPrefix(require.Path, frameworkPrefix) && require.Version == placeholderVersion {
			modules = append(modules, require.Path)
		}
	}
	if len(modules) == 0 {
		return nil, true
	}
	l, ok := s.newLookup()
	if !ok {
		return nil, false
	}
	refreshed := map[string]bool{}
	pins := make([]workspacejob.Module, 0, len(modules))
	for _, module := range modules {
		if host, _, _ := strings.Cut(module, "/"); l.private(module) && !refreshed[host] {
			refreshed[host] = true
			s.report(host, s.refresh(s.Ctx, s.WorkspaceRoot, host))
		}
		version, source, err := l.resolve(module, channel)
		if err != nil {
			// A missing credential matters only when a request is refused: the
			// warning held for the host is logged with a 401 or a 403 the
			// lookup reports, and never with any other failure.
			registrycred.ReportRefusal(err.Error(), s.Emit.Log)
			s.Emit.Diagnostic("error", err.Error(), goMod, 0)
			return nil, false
		}
		s.Emit.Log("debug", "Resolved "+module+" "+version+" from "+source)
		pins = append(pins, workspacejob.Module{Path: module, Version: version})
	}
	return pins, true
}

// report logs the outcome of the credential refresh for host. A warning is
// held (registrycred.HoldWarning) until the origin refuses a request, since
// most framework lookups are public and need no credential; any other outcome
// is logged at once.
func (s *setup) report(host string, outcome registrycred.Outcome) {
	switch {
	case outcome.Message == "":
	case outcome.Level == "warn":
		registrycred.HoldWarning(host, outcome.Message)
	default:
		s.Emit.Log(outcome.Level, outcome.Message)
	}
}

// newLookup is the lookup of the job's go command: the Go settings it reads,
// and the credentials it would send.
func (s *setup) newLookup() (*lookup, bool) {
	out, err := s.Output(nil, s.GoBinary, "env", "-json", "GOPROXY", "GONOPROXY", "GOAUTH")
	var settings goSettings
	if err == nil {
		err = json.Unmarshal([]byte(out), &settings)
	}
	if err != nil {
		s.Emit.Diagnostic("error", "go env: "+err.Error(), "", 0)
		return nil, false
	}
	return &lookup{
		ctx:       s.Ctx,
		client:    s.client,
		userAgent: s.UserAgent(),
		settings:  settings,
		credential: func(goAuth string, target *url.URL) (string, string, bool) {
			return depsupgrade.NetrcCredential(s.Env, goAuth, target)
		},
	}, true
}
