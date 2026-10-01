// Package workspaceinstall is the workspace-install job `putnami install` runs:
// it resolves or installs Go, syncs go.work with the workspace's Go projects,
// fills the module cache with the complete build list, and installs the pinned
// development tools. A workspace that declares no Go yet and has no go command
// gets none of it: its first Go project pins the Go release and installs it.
//
// It is the Go port of the former bin/workspace-install script, and it starts
// no shell: see package workspacejob.
package workspaceinstall

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/go/extension/internal/workspacejob"
	registry "go.putnami.dev/protocol/registry"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
)

// Usage is the job's help line.
const Usage = "Usage: workspace-install --putnamiContext <file> [--force]"

// FetchUsage is the help line of workspace-fetch.
const FetchUsage = "Usage: workspace-fetch --putnamiContext <file> [--force]"

const (
	statusOK     = "OK"
	statusFailed = "FAILED"
)

// mode is what one run of the job may reach.
type mode int

const (
	// modeInstall is workspace-install without the offline signal: it
	// resolves or installs Go, syncs go.work, and downloads the build list
	// and the pinned tools with the machine's own credentials. It is the only
	// mode that writes go.work or a go.mod.
	modeInstall mode = iota
	// modeFetch is workspace-fetch: modeInstall with module downloads allowed
	// whatever the offline signal says, and the read credential the engine
	// handed the job given to the go commands that download, and to nothing
	// else. On a hosted run it runs to completion before any
	// repository-controlled process starts.
	modeFetch
	// modeOffline is workspace-install on a hosted run, after workspace-fetch
	// downloaded everything: it downloads nothing. It selects a Go without
	// installing one, keeps go.work as committed, skips the module warm-up, and resolves
	// the pinned tools from what is already on the machine.
	modeOffline
)

// buildListTemplate resolves each module of the build list to the coordinate
// actually built: a replacement with a version is downloaded at the
// replacement, and a directory replacement and the main modules print nothing.
const buildListTemplate = `{{if not .Main}}{{if .Replace}}{{if .Replace.Version}}{{.Replace.Path}}@{{.Replace.Version}}{{end}}{{else if .Version}}{{.Path}}@{{.Version}}{{end}}{{end}}`

// downloadBatch bounds the coordinates one `go mod download` receives, so a
// large graph cannot overflow the argument list.
const downloadBatch = 100

// Run is the workspace-install job.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	j := workspacejob.New(context.Background(), emit, os.Environ(), ctx.WorkspaceRoot, ctx.Project.FullPath)
	defer j.Trap().Disarm()
	origin := workspacejob.ParamText(ctx.Params, "registries", "go", "origin")
	return run(j, parseForce(args), origin, workspacejob.MemberPaths(ctx.WorkspaceProjects)), nil, nil
}

// RunFetch is the workspace-fetch job: it downloads what workspace-install
// needs, and runs none of it.
//
// The read credential the engine hands this job on an inherited descriptor is
// read, and the descriptor closed, first: before the job starts any process,
// so no process inherits the descriptor. A descriptor that cannot be read
// fails the job before anything runs.
func RunFetch(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	credential, err := registrycred.ReadJobCredential()
	j := workspacejob.New(context.Background(), emit, os.Environ(), ctx.WorkspaceRoot, ctx.Project.FullPath)
	defer j.Trap().Disarm()
	if err != nil {
		j.Emit.Diagnostic("error", "Cannot read the registry credential the engine handed workspace-fetch: "+err.Error(), "", 0)
		return statusFailed, nil, nil
	}
	origin := workspacejob.ParamText(ctx.Params, "registries", "go", "origin")
	return fetch(j, parseForce(args), origin, workspacejob.MemberPaths(ctx.WorkspaceProjects), credential), nil, nil
}

// parseForce reads --force and --no-force; the last one wins.
func parseForce(args []string) bool {
	force := false
	for _, arg := range args {
		switch arg {
		case "--force":
			force = true
		case "--no-force":
			force = false
		}
	}
	return force
}

type install struct {
	*workspacejob.Job
	goWork string
	// projects are the Go projects the job syncs and warms
	// (workspacejob.WorkspaceGoProjects), sorted and without duplicates.
	projects []string
	// legacy reports that projects come from the configuration's legacy
	// "projects" member: go.work is rewritten to use exactly them. Otherwise
	// they are Go members of the resolved membership, and go.work gains those
	// it lacks.
	legacy bool
	// mode is what the run may reach.
	mode mode
	// credential is the read credential the engine handed workspace-fetch, or
	// nil. withCredential gives it to the go commands that download.
	credential *registry.Credential
}

// run executes the workspace-install phases and returns the result status.
// members are the project paths of the workspace membership. With the
// offline signal (workspacejob.Job.OfflineDependencies) the run downloads
// nothing.
func run(j *workspacejob.Job, force bool, contextOrigin string, members []string) string {
	m := modeInstall
	if j.OfflineDependencies() {
		m = modeOffline
	}
	return execute(j, force, contextOrigin, members, m, nil)
}

// fetch executes the workspace-fetch phases and returns the result status.
// credential is the read credential the engine handed the job, or nil.
func fetch(j *workspacejob.Job, force bool, contextOrigin string, members []string, credential *registry.Credential) string {
	return execute(j, force, contextOrigin, members, modeFetch, credential)
}

func execute(j *workspacejob.Job, force bool, contextOrigin string, members []string, m mode, credential *registry.Credential) string {
	projects, legacy := workspacejob.WorkspaceGoProjects(j.WorkspaceRoot, members)
	w := &install{
		Job:        j,
		goWork:     filepath.Join(j.WorkspaceRoot, "go.work"),
		projects:   sortedUnique(projects),
		legacy:     legacy,
		mode:       m,
		credential: credential,
	}
	if m == modeOffline {
		// Before a go command is selected: selecting one asks a go on PATH for
		// the toolchain the workspace requests, which it would download.
		j.ForbidModuleDownloads()
	}
	if m == modeFetch {
		// Before a go command is selected: selecting one runs each candidate.
		j.OnlyProgramsOutsideTheWorkspace()
	}

	j.Emit.PhaseStart("go")
	if !w.declaresGo() && !j.FindGoBinary() {
		// No Go release is pinned or requested, so there is no verified Go to
		// install, and nothing needs one: the Go tasks run for Go projects,
		// and the first one pins the release and runs this job again.
		j.Emit.Log("info", "The workspace declares no Go module or Go toolchain yet; its first Go project installs Go")
		j.Emit.PhaseEnd("go", "skipped")
		return statusOK
	}
	if j.GoBinary == "" && !w.selectGo() {
		j.Emit.PhaseEnd("go", "failed")
		return statusFailed
	}
	origin := contextOrigin
	if registry := j.Env.Get("GO_REGISTRY_URL"); registry != "" {
		origin = registry
	}
	if m == modeFetch {
		j.SetupFetchGoEnv(workspacejob.OriginHost(origin))
	} else {
		j.SetupGoEnv(workspacejob.OriginHost(origin))
	}
	goVersion := j.GoVersion()
	goWorkVersion := goVersion
	if version := workspacejob.GoDirective(w.goWork); version != "" {
		goWorkVersion = version
	}
	j.Emit.Log("info", "Go "+goVersion+" ready at "+j.GoBinary)
	j.Emit.PhaseEnd("go", "success")

	j.Emit.PhaseStart("sync")
	var synced bool
	if m != modeInstall {
		// A hosted run writes no file the repository commits: workspace-fetch
		// and the offline install read go.work and every go.mod as committed.
		j.Emit.Log("info", "A hosted run uses go.work as committed")
		j.Emit.PhaseEnd("sync", "skipped")
		synced = true
	} else if w.legacy {
		synced = w.syncGoWork(goWorkVersion)
	} else {
		synced = w.useMembers()
	}
	if !synced {
		j.Emit.PhaseEnd("sync", "failed")
		return statusFailed
	}
	if m == modeInstall {
		j.Emit.PhaseEnd("sync", "success")
	}

	if m == modeOffline {
		// workspace-fetch downloaded the build list before any repository
		// process started. A module it missed fails the task that needs it,
		// with the go command's own GOPROXY=off error naming the module.
		j.Emit.PhaseStart("modules")
		j.Emit.Log("info", "Module downloads are off on this run; workspace-fetch filled the module cache")
		j.Emit.PhaseEnd("modules", "skipped")
		// --force would delete the tools workspace-fetch just installed, and
		// this run cannot download them again.
		force = false
	} else if !w.warmModules() {
		return statusFailed
	}
	return w.installTools(force)
}

// selectGo records the go command the run uses: the one ResolveGoBinary
// selects, which it installs when needed, or offline the one FindGoBinary
// finds, since installing one downloads it.
func (w *install) selectGo() bool {
	if w.mode == modeFetch {
		// A hosted run installs no Go, and this job runs no program from the
		// workspace tree: it selects a go command the runner already holds
		// outside the workspace.
		if w.FindGoBinary() {
			return true
		}
		w.Emit.Diagnostic("error", "No go command outside the workspace qualifies for workspace-fetch: "+
			"a hosted run installs no Go, so put the pinned Go on the runner's PATH, in GOROOT, or in the Putnami home", "", 0)
		return false
	}
	if w.mode != modeOffline {
		return w.ResolveGoBinary()
	}
	if w.FindGoBinary() {
		return true
	}
	missing := "No installed go command qualifies"
	if requested := w.RequestedGoVersion(); requested != "" {
		missing = "No installed go command qualifies for Go " + requested
	}
	w.Emit.Diagnostic("error", missing+", and module downloads are off on this run: "+
		"a hosted run installs no Go, so put the pinned Go on the runner's PATH, in GOROOT, or in the Putnami home", "", 0)
	return false
}

// syncGoWork rewrites go.work to use every Go project at version, keeping its
// replace directives, and aligns each project's go directive with it.
func (w *install) syncGoWork(version string) bool {
	existed := workspacejob.FileExists(w.goWork)
	var replaces []workspacejob.Replace
	if existed {
		if work, err := w.ReadWorkFile(w.goWork); err == nil {
			replaces = work.Replace
		}
	}

	var content strings.Builder
	content.WriteString("go " + version + "\n\nuse (")
	for _, project := range w.projects {
		content.WriteString("\n\t./" + project)
	}
	content.WriteString("\n)\n")
	if err := os.WriteFile(w.goWork, []byte(content.String()), 0o644); err != nil {
		w.Emit.Diagnostic("error", fmt.Sprintf("Cannot write %s: %v", w.goWork, err), "", 0)
		return false
	}
	count := strconv.Itoa(len(w.projects))
	if existed {
		w.Emit.Log("info", "Synced go.work with "+count+" module(s)")
	} else {
		w.Emit.Log("info", "Created go.work with "+count+" module(s) (go "+version+")")
	}

	for _, replace := range replaces {
		if replace.Old.Path == "" || replace.New.Path == "" {
			continue
		}
		oldSpec, newSpec := replace.Old.Spec(), replace.New.Spec()
		if err := w.Inherit(true, nil, w.GoBinary, "work", "edit", "-replace="+oldSpec+"="+newSpec, w.goWork); err != nil {
			w.Emit.Diagnostic("warning", "Failed to preserve go.work replace "+oldSpec, "", 0)
		}
	}

	aligned := 0
	for _, project := range w.projects {
		goMod := filepath.Join(w.WorkspaceRoot, project, "go.mod")
		if workspacejob.GoDirective(goMod) == version {
			continue
		}
		if err := w.Inherit(false, nil, w.GoBinary, "mod", "edit", "-go="+version, goMod); err != nil {
			w.Emit.Diagnostic("warning", "Failed to align "+project+"/go.mod to Go "+version, "", 0)
			continue
		}
		aligned++
	}
	if aligned > 0 {
		w.Emit.Log("info", "Aligned go.mod go directive in "+strconv.Itoa(aligned)+" module(s) to Go "+version)
	}
	return true
}

// declaresGo reports whether the workspace declares Go: a go.work, a root
// go.mod, a Go project, or a Go release the workspace lock pins.
func (w *install) declaresGo() bool {
	if len(w.projects) > 0 {
		return true
	}
	if workspacejob.FileExists(w.goWork) || workspacejob.FileExists(filepath.Join(w.WorkspaceRoot, "go.mod")) {
		return true
	}
	_, err := workspacejob.ReadGoLock(filepath.Join(w.WorkspaceRoot, workspacejob.LockFileName))
	return err == nil
}

// useMembers adds to an existing go.work, with `go work use`, every Go member
// it does not use yet. It removes no use directive, rewrites nothing else, and
// creates no go.work: the go directive of a go.work it created would be the
// running go's release, which the Go pin derives from.
func (w *install) useMembers() bool {
	if len(w.projects) == 0 || !workspacejob.FileExists(w.goWork) {
		return true
	}
	work, err := w.ReadWorkFile(w.goWork)
	if err != nil {
		w.Emit.Diagnostic("error", fmt.Sprintf("Cannot read %s: %v", w.goWork, err), w.goWork, 0)
		return false
	}
	used := make(map[string]bool, len(work.Use))
	for _, use := range work.Use {
		used[path.Clean(filepath.ToSlash(use.DiskPath))] = true
	}
	var missing []string
	for _, member := range w.projects {
		if !used[member] {
			missing = append(missing, "./"+member)
		}
	}
	if len(missing) == 0 {
		return true
	}
	args := append([]string{"-C", w.WorkspaceRoot, "work", "use"}, missing...)
	if output, err := w.Combined([]string{"GOWORK=" + w.goWork}, w.GoBinary, args...); err != nil {
		w.Emit.Diagnostic("error", "Failed to add the workspace's Go projects to go.work: "+output, w.goWork, 0)
		return false
	}
	w.Emit.Log("info", "Added "+strconv.Itoa(len(missing))+" Go project(s) to go.work: "+strings.Join(missing, " "))
	return true
}

// warmModules downloads the complete build list of the workspace, and of each
// standalone member, into the module cache, so a task graph run with
// GOPROXY=off never needs the network after `putnami install`.
//
// Coordinates come from `go list -m` over the whole build list and are passed
// to `go mod download` as explicit arguments: the argument-less
// `go mod download all` rewrites go.work.sum. `go list -m all` itself can
// append to go.work.sum, so every checksum file the phase could touch is
// snapshotted and restored: the warm-up fills the module cache and rewrites no
// committed file.
func (w *install) warmModules() bool {
	w.Emit.PhaseStart("modules")
	started := time.Now()

	var snapshot workspacejob.Snapshot
	paths := make([]string, 0, 1+2*len(w.projects))
	paths = append(paths, filepath.Join(w.WorkspaceRoot, "go.work.sum"))
	for _, project := range w.projects {
		paths = append(paths,
			filepath.Join(w.WorkspaceRoot, project, "go.mod"),
			filepath.Join(w.WorkspaceRoot, project, "go.sum"))
	}
	for _, path := range paths {
		if err := snapshot.Add(path); err != nil {
			w.Emit.Diagnostic("error", "Cannot snapshot the workspace checksum files before the module warm-up", "", 0)
			w.Emit.PhaseEnd("modules", "failed")
			return false
		}
	}
	// restore reports whether every checksum file is back as it was, and emits
	// an error for each one that is not.
	restore := func() bool {
		err := snapshot.Restore(
			func(path string) {
				w.Emit.Log("info", "Restored "+path+"; the warm-up fills the module cache and rewrites no committed file")
			},
			func(path string) {
				w.Emit.Log("info", "Removed "+path+"; it did not exist before the warm-up")
			})
		if err != nil {
			w.Emit.Diagnostic("error", "Cannot restore the workspace checksum files after the module warm-up: "+err.Error(), "", 0)
			return false
		}
		return true
	}
	// A signal between here and the restore below still restores, and the
	// trap stays armed to the end of the job as the script's did.
	w.Trap().Arm(func() { restore() })

	total := 0
	rootGoMod := filepath.Join(w.WorkspaceRoot, "go.mod")
	if workspacejob.FileExists(w.goWork) || workspacejob.FileExists(rootGoMod) {
		gowork := ""
		if workspacejob.FileExists(w.goWork) {
			gowork = w.goWork
		}
		count, ok := w.download("workspace", w.WorkspaceRoot, gowork, true)
		if !ok {
			restore()
			w.Emit.PhaseEnd("modules", "failed")
			return false
		}
		total += count

		// A member that publishes a Go module or opts into standalone builds with
		// GOWORK=off in publish, deploy and extension jobs, and that graph can
		// differ from the workspace's, so it is warmed too.
		for _, project := range w.projects {
			goMod := filepath.Join(w.WorkspaceRoot, project, "go.mod")
			if !workspacejob.FileExists(goMod) || !workspacejob.UsesStandaloneMetadata(goMod) {
				continue
			}
			count, ok := w.download(project, filepath.Join(w.WorkspaceRoot, project), "off", false)
			if !ok {
				restore()
				w.Emit.PhaseEnd("modules", "failed")
				return false
			}
			total += count
		}
	} else {
		w.Emit.Log("info", "No go.work or root go.mod; nothing to pre-download")
	}
	if !restore() {
		w.Emit.PhaseEnd("modules", "failed")
		return false
	}

	elapsed := int(time.Since(started).Seconds())
	w.Emit.Log("info", fmt.Sprintf("Downloaded %d module coordinate(s) in %ds", total, elapsed))
	w.Emit.Metric("modules-downloaded", total, "count")
	w.Emit.PhaseEnd("modules", "success")
	return true
}

// workspaceModuleHint names the cause when the go command failed on a version
// of a module go.work uses: a go.mod requires it at a version, such as
// `require example.com/lib v0.0.0`, and the go command resolves every required
// version through the module proxy, whether go.work uses the module or not. The
// go command's own error names only the version and the proxy's answer. The
// hint is "" when no such module appears in errOut.
func (w *install) workspaceModuleHint(errOut string) string {
	if !workspacejob.FileExists(w.goWork) {
		return ""
	}
	work, err := w.ReadWorkFile(w.goWork)
	if err != nil {
		return ""
	}
	for _, use := range work.Use {
		dir := filepath.Join(filepath.Dir(w.goWork), filepath.FromSlash(use.DiskPath))
		mod, err := w.ReadModFile(filepath.Join(dir, "go.mod"))
		if err != nil || mod.Module.Path == "" || !namesModuleVersion(errOut, mod.Module.Path) {
			continue
		}
		return fmt.Sprintf(" (%s is a module go.work uses, but a go.mod requires it at a version, and the go "+
			"command resolves every required version through the module proxy, go.work or not: add a replace "+
			"directive for %s that points at %s to the go.mod that requires it, or run `putnami deps prune` "+
			"when nothing imports it)", mod.Module.Path, mod.Module.Path, use.DiskPath)
	}
	return ""
}

// namesModuleVersion reports whether text names a version of module, as
// "<module>@<version>", and not only a longer path that ends with module.
func namesModuleVersion(text, module string) bool {
	needle := module + "@"
	for offset := 0; ; {
		i := strings.Index(text[offset:], needle)
		if i < 0 {
			return false
		}
		at := offset + i
		if at == 0 || strings.ContainsRune(" \t\n\"'", rune(text[at-1])) {
			return true
		}
		offset = at + len(needle)
	}
}

// download resolves the build list of one module graph and fills the module
// cache with it, returning the number of coordinates downloaded.
//
// gowork is the GOWORK the graph resolves under: the workspace go.work, or
// "off" for a member that must resolve standalone. A resolution failure fails
// the job when required is set; otherwise the graph is skipped with a warning,
// because a standalone member may legitimately resolve only through go.work.
func (w *install) download(label, dir, gowork string, required bool) (count int, ok bool) {
	// Both commands below resolve modules through the proxy chain, so on
	// workspace-fetch both get the job credential, and only for their run.
	w.withCredential([]string{"GOWORK=" + gowork}, func(env []string) bool {
		count, ok = w.downloadWith(env, label, dir, required)
		return ok
	})
	return count, ok
}

func (w *install) downloadWith(env []string, label, dir string, required bool) (int, bool) {
	// Standard output is the coordinate list and standard error the
	// diagnostics: a notice on standard error must not read as a coordinate.
	out, errOut, err := w.Split(env, w.GoBinary, "-C", dir, "list", "-m", "-f", buildListTemplate, "all")
	if err != nil {
		if required {
			w.Emit.Diagnostic("error", "Failed to resolve the "+label+" build list: "+errOut+w.workspaceModuleHint(errOut), "", 0)
			return 0, false
		}
		w.Emit.Log("warn", "Skipping the "+label+" build list: "+errOut)
		return 0, true
	}

	var coordinates []string
	for _, coordinate := range strings.Split(out, "\n") {
		if coordinate == "" {
			continue
		}
		if !workspacejob.ValidModuleCoordinate(coordinate) {
			w.Emit.Diagnostic("error", "Refusing to download the malformed module coordinate '"+coordinate+
				"' from the "+label+" build list", "", 0)
			return 0, false
		}
		coordinates = append(coordinates, coordinate)
	}
	if len(coordinates) == 0 {
		w.Emit.Log("info", "The "+label+" build list resolves no downloadable module")
		return 0, true
	}

	for start := 0; start < len(coordinates); start += downloadBatch {
		end := min(start+downloadBatch, len(coordinates))
		args := append([]string{"-C", dir, "mod", "download"}, coordinates[start:end]...)
		if output, err := w.Combined(env, w.GoBinary, args...); err != nil {
			w.Emit.Diagnostic("error", "Failed to download the "+label+" build list: "+output, "", 0)
			return 0, false
		}
	}
	return len(coordinates), true
}

func sortedUnique(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	unique := out[:0]
	for i, value := range out {
		if i > 0 && value == out[i-1] {
			continue
		}
		unique = append(unique, value)
	}
	return unique
}
