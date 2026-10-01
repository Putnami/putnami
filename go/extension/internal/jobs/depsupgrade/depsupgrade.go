// Package depsupgrade is the deps-upgrade job `putnami upgrade` runs: it
// upgrades the workspace's go.putnami.dev/* release lock (the go.work replace
// block) and the member metadata of the Go projects that must resolve
// independently of go.work.
//
// It is the Go port of the former bin/deps-upgrade script, and it starts no
// shell: see package workspacejob.
package depsupgrade

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"

	"go.putnami.dev/go/extension/internal/workspacejob"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Usage is the job's help line.
const Usage = "Usage: deps-upgrade --putnamiContext <file> [--version <version>]"

const (
	statusOK     = "OK"
	statusFailed = "FAILED"
)

// PublishedModules are the go.putnami.dev modules pinned in go.work even
// before anything in the workspace references them, so later `go get` calls
// resolve the same release. Modules already replaced in go.work and modules
// the workspace requires are pinned as well (see pinModules), so a missing
// entry here cannot strand a stale pin.
var PublishedModules = []string{
	"go.putnami.dev/api",
	"go.putnami.dev/app",
	"go.putnami.dev/cache",
	"go.putnami.dev/client",
	"go.putnami.dev/config",
	"go.putnami.dev/database",
	"go.putnami.dev/errors",
	"go.putnami.dev/events",
	"go.putnami.dev/grpc",
	"go.putnami.dev/http",
	"go.putnami.dev/inject",
	"go.putnami.dev/logger",
	"go.putnami.dev/migratecli",
	"go.putnami.dev/migration",
	"go.putnami.dev/openapi",
	"go.putnami.dev/parallel",
	"go.putnami.dev/platform",
	"go.putnami.dev/proto",
	"go.putnami.dev/protocol/cache",
	"go.putnami.dev/protocol/capabilities",
	"go.putnami.dev/protocol/config",
	"go.putnami.dev/protocol/contracts",
	"go.putnami.dev/protocol/diagnostic",
	"go.putnami.dev/protocol/events",
	"go.putnami.dev/protocol/extension",
	"go.putnami.dev/protocol/identity",
	"go.putnami.dev/protocol/infra",
	"go.putnami.dev/protocol/job",
	"go.putnami.dev/protocol/keyring",
	"go.putnami.dev/protocol/migration",
	"go.putnami.dev/protocol/platform",
	"go.putnami.dev/protocol/runtime",
	"go.putnami.dev/protocol/storage",
	"go.putnami.dev/protocol/telemetry",
	"go.putnami.dev/protocol/template",
	"go.putnami.dev/protocol/transaction",
	"go.putnami.dev/protocol/workspace",
	"go.putnami.dev/schema",
	"go.putnami.dev/security",
	"go.putnami.dev/storage",
	"go.putnami.dev/telemetry",
}

// managedPrefix is the module path prefix of the modules the job pins.
const managedPrefix = "go.putnami.dev/"

// defaultGoProxy is the GOPROXY SetupGoEnv supplies when the caller named
// none; only that default gives way to the declared proxy chain.
const defaultGoProxy = "https://proxy.golang.org,direct"

var (
	semverPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-.+][0-9A-Za-z.-]+)?$`)
	exactPattern  = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([-.+][0-9A-Za-z.-]+)?$`)
)

// Options is what the job reads from its arguments and parameters.
type Options struct {
	// Version is the selector: an exact version, "latest" or a channel name.
	Version string
	// DryRun reports the rewrites without writing any file.
	DryRun bool
	// Origin is the module server that answers a channel query, without a
	// trailing slash, or "".
	Origin string
	// Proxy is the declared GOPROXY chain, or "".
	Proxy string
	// ReleaseSet is the release the orchestrator resolved, or nil.
	ReleaseSet *ReleaseSet
	// ContextFile is the job context document, named by the diagnostics about
	// a malformed release set.
	ContextFile string
}

// Run is the deps-upgrade job.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	j := workspacejob.New(context.Background(), emit, os.Environ(), ctx.WorkspaceRoot, ctx.Project.FullPath)
	defer j.Trap().Disarm()
	contextFile := ContextFileArg(os.Args[1:])
	opts, ok := ParseOptions(j, args, ctx.Params, contextFile)
	if !ok {
		return statusFailed, nil, nil
	}
	return run(j, opts, workspacejob.MemberPaths(ctx.WorkspaceProjects)), nil, nil
}

// ContextFileArg returns the value of the last --putnamiContext in args, or "".
func ContextFileArg(args []string) string {
	file := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--putnamiContext" && i+1 < len(args) {
			file = args[i+1]
			i++
		}
	}
	return file
}

// ParseOptions reads the selector, the dry-run switch, the release set and the
// registry endpoints from the job arguments and parameters, as the script did
// before it resolved Go. It reports false after emitting the diagnostic when
// the release set is malformed.
func ParseOptions(j *workspacejob.Job, args []string, params map[string]json.RawMessage, contextFile string) (Options, bool) {
	opts := Options{ContextFile: contextFile}
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--version", "--channel":
			opts.Version = ""
			if i+1 < len(args) {
				opts.Version = args[i+1]
				i++
			}
		case "--dry-run":
			opts.DryRun = true
		default:
			if opts.Version == "" {
				opts.Version = arg
			}
		}
	}

	if opts.Version == "" {
		opts.Version = "latest"
		for _, key := range []string{"putnamiVersion", "putnami-version", "putnamiChannel", "putnami-channel"} {
			if raw, ok := workspacejob.Param(params, key); ok {
				opts.Version = workspacejob.JQRawText(raw)
				break
			}
		}
	}
	if opts.Version == "" || opts.Version == "null" || opts.Version == "stable" {
		opts.Version = "latest"
	}
	if semverPattern.MatchString(opts.Version) {
		opts.Version = "v" + opts.Version
	}
	if !opts.DryRun {
		for _, key := range []string{"dryRun", "dry-run"} {
			if raw, ok := workspacejob.Param(params, key); ok {
				opts.DryRun = workspacejob.JQRawText(raw) == "true"
				break
			}
		}
	}

	// The orchestrator resolves a channel or an immutable release once and
	// hands every language the validated response. In set mode each coordinate
	// owns its exact member version: nothing is probed.
	if raw, ok := workspacejob.Param(params, "releaseSet"); ok {
		set, problem := ParseReleaseSet(raw)
		if problem != "" {
			j.Emit.Diagnostic("error", problem, contextFile, 0)
			return opts, false
		}
		opts.ReleaseSet = set
	}

	origin := workspacejob.ParamText(params, "registries", "go", "origin")
	if origin == "null" {
		origin = ""
	}
	if registry := j.Env.Get("GO_REGISTRY_URL"); registry != "" {
		origin = registry
	}
	// The tests point this at a file:// tree or a local HTTP server.
	if proxy := j.Env.Get("PUTNAMI_GO_MODULE_PROXY"); proxy != "" {
		origin = proxy
	}
	opts.Origin = strings.TrimSuffix(origin, "/")
	proxy, ok := declaredProxy(params, workspacejob.OriginHost(opts.Origin))
	if !ok {
		j.Emit.Diagnostic("error", "registries.go.proxy is neither an array nor an object of proxy URLs", contextFile, 0)
		return opts, false
	}
	opts.Proxy = proxy
	return opts, true
}

// declaredProxy is the declared GOPROXY chain without the entries the origin
// host serves: the origin never leads the chain, because Go asks only the
// first proxy whether it relays the checksum database and a vanity catch-all
// answers yes and then serves nothing. GONOPROXY routes the workspace's own
// modules to the origin instead. It reports false for a declared value that is
// neither an array nor an object, which has no entries to read.
func declaredProxy(params map[string]json.RawMessage, originHost string) (string, bool) {
	raw, _ := workspacejob.Param(params, "registries", "go", "proxy")
	entries, ok := workspacejob.IterateOrEmpty(raw)
	if !ok {
		return "", false
	}
	var chain []string
	for _, entry := range entries {
		text, isString := workspacejob.JSONString(entry)
		if !isString {
			continue
		}
		host := strings.TrimPrefix(strings.TrimPrefix(text, "https://"), "http://")
		// jq's split("/")[0] of an empty string is null, which differs from
		// every host.
		if host != "" {
			host, _, _ = strings.Cut(host, "/")
			if host == originHost {
				continue
			}
		}
		chain = append(chain, text)
	}
	return strings.Join(chain, ","), true
}

// goCommandAuth is the GOAUTH the job's go command applies, from its
// environment or its env file, or the environment's alone when go cannot say.
func goCommandAuth(j *workspacejob.Job) string {
	goAuth, err := j.Output(nil, j.GoBinary, "env", "GOAUTH")
	if err != nil {
		return j.Env.Get("GOAUTH")
	}
	return strings.TrimSpace(goAuth)
}

// upgrade is one run of the job.
type upgrade struct {
	*workspacejob.Job
	Options
	fetcher  *fetcher
	resolved map[string]string
	goWork   string
	// projects are the Go projects the job upgrades
	// (workspacejob.WorkspaceGoProjects).
	projects []string

	snapshot workspacejob.Snapshot
	active   bool
}

// run executes the job and returns the result status. members are the
// project paths of the workspace membership.
func run(j *workspacejob.Job, opts Options, members []string) string {
	u := &upgrade{
		Job:      j,
		Options:  opts,
		fetcher:  newFetcher(j),
		resolved: map[string]string{},
		goWork:   joinPath(j.WorkspaceRoot, "go.work"),
	}
	// A failure after the transaction began restores every surface it
	// snapshotted, as the script's EXIT trap did.
	defer u.rollback()

	// An upgrade asks module channels over its own HTTP client, which the
	// offline GOPROXY does not govern, and it rewrites the locked files.
	if j.OfflineDependencies() {
		j.Emit.Diagnostic("error", "deps upgrade reaches the network and rewrites go.mod and go.sum, "+
			"so it does not run when dependencies are offline (a hosted run)", "", 0)
		return statusFailed
	}

	if !j.ResolveGoBinary() {
		j.Emit.Log("error", "Go binary not found")
		return statusFailed
	}
	j.SetupGoEnv(workspacejob.OriginHost(u.Origin))
	u.fetcher.goAuth = func() string { return goCommandAuth(j) }
	// SetupGoEnv supplies the public default when the caller named no chain.
	// A declared chain replaces that default, and only then: a caller that
	// named its own proxy keeps it.
	if u.Proxy != "" && j.Env.Get("GOPROXY") == defaultGoProxy {
		j.Env.Set("GOPROXY", u.Proxy)
	}

	j.Emit.PhaseStart("scan")
	u.projects, _ = workspacejob.WorkspaceGoProjects(j.WorkspaceRoot, members)
	if len(u.projects) == 0 {
		j.Emit.Log("info", "No Go projects found")
	} else {
		j.Emit.Log("info", "Found "+strconv.Itoa(len(u.projects))+" Go project(s)")
	}
	j.Emit.PhaseEnd("scan", "success")

	return u.upgrade()
}

func (u *upgrade) upgrade() string {
	u.Emit.PhaseStart("upgrade")
	if !u.resolveRelease() {
		return statusFailed
	}

	if !u.DryRun && !u.begin() {
		u.Emit.PhaseEnd("upgrade", "failed")
		u.Emit.Diagnostic("error", "Cannot snapshot Go metadata before dependency upgrade", "", 0)
		return statusFailed
	}

	if len(u.projects) == 0 {
		if workspacejob.FileExists(u.goWork) {
			u.Emit.Log("info", "No Go projects found; updating root go.work replaces")
			if !u.pinGoWork() {
				return u.failUpgrade()
			}
		}
		// Standalone discovery is workspace-wide: go.work members can publish
		// or opt into GOWORK=off metadata even when the job-driving project is
		// not Go.
		if !u.propagate() {
			return u.failUpgrade()
		}
		if u.DryRun {
			u.Emit.PhaseEnd("upgrade", "success")
			u.Emit.Log("info", "Dry run complete; no Go files were changed")
			return statusOK
		}
		u.Emit.PhaseEnd("upgrade", "success")
		if !u.syncAndReconcile() {
			return statusFailed
		}
		u.commit()
		return statusOK
	}

	if !u.pinGoWork() {
		return u.failUpgrade()
	}

	if u.DryRun {
		for _, project := range u.projects {
			goMod := u.projectGoMod(project)
			if !workspacejob.UsesStandaloneMetadata(goMod) {
				u.Emit.Log("info", "Would keep "+goMod+" on its workspace baseline")
				continue
			}
			if workspacejob.FileExists(goMod) && !u.reconcileProject(goMod) {
				return u.failUpgrade()
			}
		}
		if !u.propagate() {
			return u.failUpgrade()
		}
		u.Emit.PhaseEnd("upgrade", "success")
		u.Emit.Log("info", "Dry run complete; no Go files were changed")
		return statusOK
	}

	upgraded := 0
	for _, project := range u.projects {
		count, ok := u.upgradeProject(project)
		if !ok {
			return u.failUpgrade()
		}
		upgraded += count
	}

	// Re-read the post-tidy graph: modules the loop introduced are not in the
	// pre-upgrade go.work.
	if !u.pinGoWork() {
		return u.failUpgrade()
	}
	// Runs last so the get, tidy and normalize passes cannot revert it.
	if !u.propagate() {
		return u.failUpgrade()
	}
	u.Emit.PhaseEnd("upgrade", "success")

	if !u.syncAndReconcile() {
		return statusFailed
	}
	u.commit()

	if upgraded > 0 {
		u.Emit.Log("info", "Upgraded "+strconv.Itoa(upgraded)+" dependency(ies)")
	} else {
		u.Emit.Log("info", "All go.putnami.dev/* dependencies are up to date")
	}
	return statusOK
}

func (u *upgrade) failUpgrade() string {
	u.Emit.PhaseEnd("upgrade", "failed")
	return statusFailed
}

// resolveRelease resolves every published module before the first rewrite,
// so a channel a module does not serve fails the job with the workspace
// untouched, and reports the resolved versions.
func (u *upgrade) resolveRelease() bool {
	if u.ReleaseSet != nil {
		u.Emit.Log("info", "Resolved release set "+u.ReleaseSet.ID+" ("+u.ReleaseSet.Digest+") with "+
			strconv.Itoa(len(u.ReleaseSet.Versions))+" Go member(s)")
		members := u.publishedPinModules()
		if len(members) > 0 && !u.reportResolved("release set "+u.ReleaseSet.ID, members) {
			u.Emit.PhaseEnd("upgrade", "failed")
			return false
		}
		return true
	}

	for _, module := range PublishedModules {
		if _, ok := u.targetVersion(module); !ok {
			u.Emit.PhaseEnd("upgrade", "failed")
			u.reportUnresolved(module)
			return false
		}
	}
	selector := u.Version
	if !exactPattern.MatchString(u.Version) {
		selector = u.Version + " via go-proxy"
	}
	if !u.reportResolved(selector, PublishedModules) {
		u.Emit.PhaseEnd("upgrade", "failed")
		return false
	}
	return true
}

// upgradeProject upgrades the go.putnami.dev requirements of one standalone
// project with `go get`, tidies it and normalizes its pins. It returns the
// number of requirements upgraded, and false when the job must fail.
func (u *upgrade) upgradeProject(project string) (int, bool) {
	projectDir := joinPath(u.WorkspaceRoot, project)
	goMod := joinPath(projectDir, "go.mod")
	if !workspacejob.FileExists(goMod) {
		return 0, true
	}
	if !workspacejob.UsesStandaloneMetadata(goMod) {
		u.Emit.Log("info", "Keeping "+goMod+" and go.sum on their workspace baseline")
		return 0, true
	}
	if !u.reconcileProject(goMod) {
		return 0, false
	}

	// An already-current requirement with a checksum is skipped, so a second
	// upgrade cannot reclassify an indirect requirement by running `go get`
	// on it again. A current one without a checksum still runs through
	// `go get`: that is how the first pass materializes its closure.
	deps := u.managedRequires(goMod)
	if len(deps) == 0 {
		return 0, true
	}
	upgraded := 0
	for _, dep := range deps {
		target, ok := u.targetVersion(dep.Path)
		if !ok {
			u.emitMissing(dep.Path)
			return 0, false
		}
		// go.sum is read for every requirement: the `go get` of an earlier one
		// may have added this one's checksum.
		if dep.Version == target {
			if goSum, err := os.ReadFile(joinPath(projectDir, "go.sum")); err == nil &&
				strings.Contains(string(goSum), dep.Path+" "+target+" ") {
				continue
			}
		}
		u.Emit.Log("info", "Upgrading "+dep.Path+" in "+project+" to "+target)
		output, err := u.Combined([]string{"GOWORK=off"}, u.GoBinary, "-C", projectDir, "get", dep.Path+"@"+target)
		if err != nil {
			u.Emit.Log("warn", "Failed to upgrade "+dep.Path+" in "+project+" ("+output+
				"); final checksum reconciliation will verify the resulting graph")
			continue
		}
		if !u.warnEmptyModule(dep.Path, target) {
			return 0, false
		}
		upgraded++
	}

	// A workspace-only module can legitimately fail under GOWORK=off here; the
	// final download pass is authoritative.
	if output, err := u.Combined([]string{"GOWORK=off"}, u.GoBinary, "-C", projectDir, "mod", "tidy"); err != nil {
		u.Emit.Log("warn", "go mod tidy did not complete for "+project+" ("+output+
			"); final checksum reconciliation will verify the resulting graph")
	}

	// Tidy can materialize indirect framework modules: normalize any
	// malformed target-prefixed pin it leaves.
	if !u.normalizePins(goMod) {
		return 0, false
	}
	return upgraded, true
}

// managedRequires returns the go.putnami.dev requirements of goMod, sorted by
// path then version with duplicates dropped, or none when it cannot be read.
func (u *upgrade) managedRequires(goMod string) []workspacejob.Module {
	mod, err := u.ReadModFile(goMod)
	if err != nil {
		return nil
	}
	seen := map[workspacejob.Module]bool{}
	var deps []workspacejob.Module
	for _, require := range mod.Require {
		if !strings.HasPrefix(require.Path, managedPrefix) {
			continue
		}
		dep := workspacejob.Module{Path: require.Path, Version: require.Version}
		if seen[dep] {
			continue
		}
		seen[dep] = true
		deps = append(deps, dep)
	}
	sortModules(deps)
	return deps
}

// warnEmptyModule warns when the module just fetched holds no .go file at its
// root: a publication that shipped no package. It reports false, after an
// error diagnostic, when the go command cannot name its module cache.
func (u *upgrade) warnEmptyModule(module, version string) bool {
	modCache, stderr, err := u.Split(nil, u.GoBinary, "env", "GOMODCACHE")
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		u.Emit.Diagnostic("error", "Cannot read GOMODCACHE from "+u.GoBinary+": "+detail, "", 0)
		return false
	}
	if modCache == "" {
		return true
	}
	dir := modCache + string(os.PathSeparator) + module + "@" + version
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return true
	}
	if !containsGoFile(dir) {
		u.Emit.Log("warn", "Module "+module+"@"+version+" contains no .go files — it may not be properly published")
	}
	return true
}

func (u *upgrade) projectGoMod(project string) string {
	return joinPath(u.WorkspaceRoot, project, "go.mod")
}
