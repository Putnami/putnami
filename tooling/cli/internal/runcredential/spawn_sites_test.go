package runcredential

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// spawnVerdict is how a process spawn of the CLI keeps the custody rule: no
// process started after repository code receives the run credential.
type spawnVerdict int

const (
	// marked: the spawn may start repository code, and the named function
	// records it before the spawn (MarkRepositoryCodeStarted).
	marked spawnVerdict = iota + 1
	// holder: the spawn hands the process the run credential, and the named
	// function starts it through StartHolder.
	holder
	// hostedNever: a hosted run never reaches the spawn, because the named
	// function asks Hosted and returns before it.
	hostedNever
	// noRepositoryCode: the spawn starts a program the repository does not
	// control; why says which.
	noRepositoryCode
	// testSupport: the package only serves tests.
	testSupport
	// alias: a package-level variable that holds a spawn helper. Each use of
	// the variable is a spawn site of its own, which the inventory
	// classifies.
	alias
)

// spawnSite classifies the spawns of one function. by names the function,
// as "<file>:<function>", that records repository code, starts the holder, or
// keeps a hosted run away from the spawn.
type spawnSite struct {
	spawns  int
	verdict spawnVerdict
	by      string
	// via names the call in by that leads to the spawn, when by is not the
	// function that spawns. A hostedNever site asks Hosted before that call.
	via string
	// native: the holder starts only as its extension's native runtime, so
	// by calls RequireNativeHolder inside the function StartHolder runs.
	native bool
	why    string
}

// spawnSites is the inventory of every process spawn under internal/: each
// use of a spawnCalls function, keyed by "<file>:<function>", where a
// package-level variable counts as a function of its own name. A new spawn
// fails TestEverySpawnSiteIsClassified until it is classified here. When
// unsure, mark it: that fails closed.
var spawnSites = map[string]spawnSite{
	// Repository code, recorded before the spawn.
	"commands/cachecmd/cache_verify.go:addDetachedWorktree": {spawns: 1, verdict: marked, by: "commands/cachecmd/cache_verify.go:addDetachedWorktree",
		why: "the checkout runs the post-checkout hook and smudge filters"},
	"commands/completion/completion_install.go:writeCompletionFromBinary": {spawns: 1, verdict: marked, by: "commands/completion/completion_install.go:writeCompletionFromBinary",
		why: "the workspace's CLI may be a build of its source"},
	"commands/lifecycle/channel.go:newChannelClient": {spawns: 1, verdict: marked, by: "commands/lifecycle/channel.go:newChannelClient",
		why: "putnami cloud release-set, a CLI without the run credential that loads the workspace's extensions"},
	"commands/lifecycle/deps.go:runGoInModule": {spawns: 1, verdict: marked, by: "commands/lifecycle/deps.go:runGoInModule",
		why: "go in a workspace module"},
	"commands/lifecycle/upgrade.go:newUpgradeReleaseSetResolver": {spawns: 1, verdict: marked, by: "commands/lifecycle/upgrade.go:newUpgradeReleaseSetResolver",
		why: "putnami cloud release-set, a CLI without the run credential that loads the workspace's extensions"},
	"commands/shared/subprocess.go:RunGroupCombined": {spawns: 1, verdict: marked, by: "commands/shared/subprocess.go:markRepositoryCode",
		why: "go in a workspace module, or a nested CLI that runs the workspace's hooks and jobs"},
	"commands/shared/subprocess.go:RunGroupStreaming": {spawns: 1, verdict: marked, by: "commands/shared/subprocess.go:markRepositoryCode",
		why: "go in a workspace module, or a nested CLI that runs the workspace's hooks and jobs"},
	"commands/treecmd/verify.go:workspacePlan": {spawns: 1, verdict: marked, by: "commands/treecmd/verify.go:workspacePlan",
		why: "the workspace's CLI answers the native plan, and it may be a build of the workspace's source"},
	"commands/versioncmd/version.go:getBinaryVersion": {spawns: 1, verdict: marked, by: "commands/versioncmd/version.go:getBinaryVersion",
		why: "a listed CLI may be a build of the workspace's source"},
	"commands/versioncmd/version_release.go:runGit": {spawns: 1, verdict: marked, by: "commands/versioncmd/version_release.go:runGit",
		why: "add runs clean filters; commit and push run the repository's hooks"},
	"commands/versioncmd/version_source.go:buildStampedCLI": {spawns: 1, verdict: marked, by: "commands/versioncmd/version_source.go:buildStampedCLI",
		why: "go build of the workspace's source"},
	"hooks/cache_command.go:RunCacheCommand": {spawns: 1, verdict: marked, by: "hooks/cache_command.go:RunCacheCommand",
		why: "an extension cache command runs in the workspace"},
	"hooks/cache_command.go:StartDetachedCacheGC": {spawns: 1, verdict: marked, by: "hooks/cache_command.go:StartDetachedCacheGC",
		why: "an extension cache command runs in the workspace"},
	"hooks/cli_hooks.go:runShellCommand": {spawns: 1, verdict: marked, by: "hooks/cli_hooks.go:runShellCommand",
		why: "a workspace hook"},
	"hooks/hooks.go:RunOnInstallHookWithWriters": {spawns: 1, verdict: marked, by: "hooks/hooks.go:RunOnInstallHookWithWriters",
		why: "an extension onInstall hook"},
	"hooks/hooks.go:RunPreBuildHook": {spawns: 1, verdict: marked, by: "hooks/hooks.go:RunPreBuildHook",
		why: "an extension preBuild hook"},
	"jobs/extension_runtime.go:runRuntimePrepareCommand": {spawns: 1, verdict: marked, by: "jobs/extension_runtime.go:prepareOrLoadExtensionRuntimeWithDigestAndEnv",
		why: "the prepare command of an extension not installed from the artifact store"},
	"jobs/extension_runtime.go:startRuntimeHandshake": {spawns: 1, verdict: marked, by: "jobs/extension_runtime.go:prepareOrLoadExtensionRuntimeWithDigestAndEnv",
		why: "the runtime of an extension not installed from the artifact store"},
	"jobs/release_set.go:newReleaseSetProvider": {spawns: 1, verdict: marked, by: "jobs/release_set.go:newReleaseSetProvider",
		why: "putnami cloud release-set, a CLI without the run credential that loads the workspace's extensions"},
	"jobs/runner.go:(*jobInvocation).execCommand": {spawns: 1, verdict: marked, by: "jobs/runner.go:(*jobInvocation).startJob",
		why: "a job; a job that receives the credential starts as a holder in the same function"},
	"mcp/extension_tools.go:invokeExtensionTool": {spawns: 1, verdict: marked, by: "mcp/extension_tools.go:invokeExtensionTool",
		why: "an extension tool runs in the workspace"},
	"runnerprovider/session.go:Spawn": {spawns: 1, verdict: marked, by: "runnerprovider/launch.go:LaunchSpecFor",
		why: "the runner provider of an extension not installed from the artifact store"},
	"sessionreporter/process.go:spawn": {spawns: 1, verdict: marked, by: "engine/session_reporting.go:reportingResolver",
		why: "the session reporter of an extension not installed from the artifact store"},
	"workspace/probe_exec.go:(*ExecProbeProvider).Probe": {spawns: 1, verdict: marked, by: "workspace/probe_exec.go:(*ExecProbeProvider).Probe",
		why: "the workspace probe of an extension not installed from the artifact store"},

	// Credential holders, started through StartHolder.
	"cacheprovider/session.go:Spawn": {spawns: 1, verdict: holder, by: "jobs/remote_provider.go:(*RemoteCache).ensureProvider",
		native: true, why: "the remote cache provider"},
	"credentialprovider/session.go:Spawn": {spawns: 1, verdict: holder, by: "credentialprovider/resolve.go:New",
		native: true, why: "the credential provider"},
	"runcredential/descriptor_unix.go:Exec": {spawns: 2, verdict: holder, by: "runcredential/descriptor_unix.go:Exec",
		why: "the pinned or upgraded CLI replaces this process"},

	// Spawns a hosted run never reaches.
	"commands/lifecycle/projects_create_goframework.go:ensureModuleOriginCredential": {spawns: 1, verdict: hostedNever,
		by:  "commands/lifecycle/projects_create_goframework.go:ensureModuleOriginCredential",
		why: "the netrc child of @putnami/cloud, a CLI without the run credential that loads the workspace's extensions"},
	"extension/http_client.go:AuthorizeRegistryRequest": {spawns: 1, verdict: hostedNever, by: "extension/http_client.go:authorizeRegistryRequest",
		via: "resolve", why: "putnami cloud registry-token, the host-keyed registry credential"},
	"extension/http_client.go:AuthorizeRegistryRequestWithCLI": {spawns: 1, verdict: hostedNever, by: "extension/http_client.go:authorizeRegistryRequest",
		via: "resolve", why: "putnami cloud registry-token, the host-keyed registry credential"},

	// Variables that hold a spawn helper; their uses are classified above.
	"extension/http_client.go:ResolveRegistryToken":        {spawns: 1, verdict: alias},
	"extension/http_client.go:ResolveRegistryTokenWithCLI": {spawns: 1, verdict: alias},

	// Programs the repository does not control. A fresh checkout's git
	// configuration and hooks directory are the runner's, so a git verb that
	// runs no hook runs no repository code.
	"commands/agentctx/context_pack.go:resolveWorkspaceRevision": {spawns: 1, verdict: noRepositoryCode,
		why: "git rev-parse"},
	"commands/cachecmd/cache_verify.go:removeDetachedWorktrees": {spawns: 1, verdict: noRepositoryCode,
		why: "git worktree remove runs no hook"},
	"commands/completion/completion_install.go:zshCompletionDuplicates": {spawns: 1, verdict: noRepositoryCode,
		why: "zsh runs a fixed script"},
	"commands/lifecycle/workspace_init.go:WorkspaceInit": {spawns: 1, verdict: noRepositoryCode,
		why: "git init in a directory without a repository"},
	"commands/treecmd/verify.go:gitCommand": {spawns: 1, verdict: noRepositoryCode,
		why: "read verbs only: rev-parse, merge-base, diff --name-only, ls-files"},
	"commands/versioncmd/version_release.go:gitOutput": {spawns: 1, verdict: noRepositoryCode,
		why: "read verbs only: rev-parse, status, config --get"},
	"commands/versioncmd/version_release.go:gitTagExists": {spawns: 1, verdict: noRepositoryCode,
		why: "git rev-parse"},
	"compose/lease.go:defaultReapDeps": {spawns: 1, verdict: noRepositoryCode,
		why: "docker with the digest-pinned postgres image of dbtestenv"},
	"compose/process_start_ps.go:processStartTime": {spawns: 1, verdict: noRepositoryCode,
		why: "ps"},
	"compose/up.go:defaultUpDeps": {spawns: 1, verdict: noRepositoryCode,
		why: "docker with the digest-pinned postgres image of dbtestenv"},
	"git/candidate.go:CandidatePaths": {spawns: 1, verdict: noRepositoryCode,
		why: "git ls-files"},
	"git/git.go:runCaptureEnv": {spawns: 1, verdict: noRepositoryCode,
		why: "read verbs only: show, rev-parse, rev-list, status, diff, merge-base, config --get, for-each-ref, ls-files, symbolic-ref"},
	"githooks/install.go:Install": {spawns: 1, verdict: noRepositoryCode,
		why: "git rev-parse"},
	"jobs/environment.go:hostCPUModel": {spawns: 1, verdict: noRepositoryCode,
		why: "sysctl"},
	"jobs/environment_memory.go:totalMemoryBytesAt": {spawns: 1, verdict: noRepositoryCode,
		why: "sysctl"},
	"jobs/runtime_toolchains.go:probeRuntimeToolchain": {spawns: 1, verdict: noRepositoryCode,
		why: "the version probe of a pinned toolchain"},
	"jobs/toolchain_cache.go:probeToolVersion": {spawns: 1, verdict: noRepositoryCode,
		why: "the --version of a toolchain"},
	"launch/credential_probe.go:credentialProbe": {spawns: 1, verdict: noRepositoryCode,
		why: "the --help of the pinned CLI, which receives no credential"},
	"launch/launch_windows.go:reexec": {spawns: 1, verdict: noRepositoryCode,
		why: "the pinned CLI; Capture refuses --credential-fd on Windows"},
	"runnersource/capture.go:IgnoredPaths": {spawns: 1, verdict: noRepositoryCode,
		why: "git check-ignore with core.fsmonitor off"},
	"runnersource/capture.go:sourceGitOptional": {spawns: 1, verdict: noRepositoryCode,
		why: "git read verbs with core.fsmonitor off"},
	"store/cache_hash.go:collectGitFiles": {spawns: 1, verdict: noRepositoryCode,
		why: "git rev-parse"},

	// Test support.
	"cli/clitest/clitest.go:GitOutput":                                {spawns: 1, verdict: testSupport},
	"cli/clitest/clitest.go:RunGit":                                   {spawns: 1, verdict: testSupport},
	"cli/clitest/runnerfixture.go:(*fixtureProvider).startSupervisor": {spawns: 1, verdict: testSupport},
	"cli/clitest/runnerfixture.go:superviseFixtureAttempt":            {spawns: 1, verdict: testSupport},
	"fixtureproc/fixtureproc.go:Warm":                                 {spawns: 1, verdict: testSupport},
	"fixtureproc/fixtureproc.go:buildHelper":                          {spawns: 1, verdict: testSupport},
	"fixtureproc/fixtureproc.go:startOutputHolder":                    {spawns: 1, verdict: testSupport},
}

// Every process spawn under internal/ is classified in spawnSites, and every
// function the inventory says records repository code, starts a holder, or
// asks Hosted, does. A function that records its own spawn records it before
// the spawn, and a function that keeps a hosted run away from a spawn asks
// Hosted before it. A holder that must be native checks it where StartHolder
// runs it.
func TestEverySpawnSiteIsClassified(t *testing.T) {
	t.Parallel()
	found, functions := scanSpawnSites(t, "..")

	var problems []string
	for key, spawns := range found {
		site, ok := spawnSites[key]
		switch {
		case !ok:
			problems = append(problems, "unclassified spawn site "+key+": record repository code before it (MarkRepositoryCodeStarted), "+
				"start it as a holder (StartHolder), or say why the repository does not control it; then add it to spawnSites. "+
				"When unsure, mark it.")
		case site.spawns != len(spawns):
			problems = append(problems, key+" spawns "+strconv.Itoa(len(spawns))+" times; spawnSites says "+
				strconv.Itoa(site.spawns)+". Classify the new spawn.")
		}
	}
	for key, site := range spawnSites {
		if _, ok := found[key]; !ok {
			problems = append(problems, "spawnSites lists "+key+", which spawns nothing; remove it")
			continue
		}
		if problem := checkSpawnSite(key, site, found[key], functions); problem != "" {
			problems = append(problems, problem)
		}
	}
	sort.Strings(problems)
	for _, problem := range problems {
		t.Error(problem)
	}
}

// The scan finds a spawn helper used through a variable, in
// its own package or another, and a composite literal of a spawning type; a
// type name alone spawns nothing. The checks refuse a hostedNever site that
// asks Hosted after the spawn, and a native holder whose check does not run
// inside StartHolder.
//
// The scan also finds a value of a spawning type built by new or by a
// variable declared without a value, and an exec.Cmd literal, which starts at
// its first Start. A hostedNever site needs a guard at the top level of its
// function, `if runcredential.Hosted() { return }` or an || chain with Hosted,
// that ends before the spawn: a Hosted call in a nested block, a negated one,
// an && chain, or a guard that does not return, is refused.
//
// The scan does not follow values across functions. It misses a method value
// of a spawning type that a function returns without building the value, and
// a spawning value that is the zero value of a field of another struct.
func TestTheSpawnScanFindsWhatHidesASpawn(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	parse := func(rel, src string) scannedFile {
		file, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		return scannedFile{rel: rel, pkg: internalImport + "/" + pathpkg.Dir(rel), file: file}
	}
	files := []scannedFile{
		parse("seam/seam.go", `package seam
import "go.putnami.dev/sdk/extension/registrycred"
var Resolve = registrycred.ResolveToken
var again = Resolve
func Local(host string) { again(host) }
`),
		parse("user/user.go", `package user
import (
	"os/exec"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/seam"
)
func Remote(host string) { seam.Resolve(host) }
func Literal() *releaseset.Client { return &releaseset.Client{Executable: "putnami"} }
func Typed() { var client *releaseset.Client; _ = client }
func New() { c := new(releaseset.Client); c.Executable = "putnami" }
func Zero() { var c releaseset.Client; c.Executable = "putnami" }
func NewCmd() *exec.Cmd { return new(exec.Cmd) }
func CmdLiteral() error { cmd := &exec.Cmd{Path: "x"}; return cmd.Start() }
`),
		parse("order/order.go", `package order
import (
	"os/exec"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)
func first(resolve func()) { if runcredential.Hosted() { return }; resolve() }
func late(resolve func()) { resolve(); if runcredential.Hosted() { return } }
func selfFirst() { if runcredential.Hosted() { return }; _ = exec.Command("x").Run() }
func selfLate() { _ = exec.Command("x").Run(); if runcredential.Hosted() { return } }
func negated(resolve func()) { if !runcredential.Hosted() { log() }; resolve() }
func logged(resolve func()) { if runcredential.Hosted() { log() }; resolve() }
func nested(resolve func(), x bool) { if x { if runcredential.Hosted() { return } }; resolve() }
func both(resolve func(), x bool) { if x && runcredential.Hosted() { return }; resolve() }
func either(resolve func(), x bool) { if x || (runcredential.Hosted()) { return }; resolve() }
var held = func(resolve func()) { if runcredential.Hosted() { return }; resolve() }
func literalMarked() { cmd := &exec.Cmd{}; runcredential.MarkRepositoryCodeStarted("x"); _ = cmd.Run() }
func literalLate() { cmd := &exec.Cmd{}; _ = cmd.Run(); runcredential.MarkRepositoryCodeStarted("x") }
func inside(start func() error) error {
	return runcredential.StartHolder("h", func() error {
		if err := runcredential.RequireNativeHolder("h", "", ""); err != nil { return err }
		return start()
	})
}
func outside(start func() error) error {
	if err := runcredential.RequireNativeHolder("h", "", ""); err != nil { return err }
	return runcredential.StartHolder("h", start)
}
`),
	}
	aliases := spawnAliases(files)
	found := map[string][]token.Pos{}
	functions := map[string]scannedFunction{}
	for _, file := range files {
		scanFile(file, aliases, found, functions)
	}

	got := map[string]int{}
	for key, spawns := range found {
		got[key] = len(spawns)
	}
	want := map[string]int{
		"seam/seam.go:Resolve": 1, "seam/seam.go:again": 1, "seam/seam.go:Local": 1,
		"user/user.go:Remote": 1, "user/user.go:Literal": 1,
		"user/user.go:New": 1, "user/user.go:Zero": 1, "user/user.go:NewCmd": 1, "user/user.go:CmdLiteral": 1,
		"order/order.go:selfFirst": 1, "order/order.go:selfLate": 1,
		"order/order.go:literalMarked": 1, "order/order.go:literalLate": 1,
	}
	if !maps.Equal(got, want) {
		t.Errorf("spawns = %v, want %v", got, want)
	}
	for key, alias := range map[string]bool{"seam/seam.go:Resolve": true, "seam/seam.go:again": true, "seam/seam.go:Local": false} {
		if functions[key].alias != alias {
			t.Errorf("%s alias = %t, want %t", key, functions[key].alias, alias)
		}
	}

	spawn := []token.Pos{token.NoPos}
	for _, tc := range []struct {
		key     string
		site    spawnSite
		problem string
	}{
		{"x", spawnSite{spawns: 1, verdict: hostedNever, by: "order/order.go:first", via: "resolve"}, ""},
		{"x", spawnSite{spawns: 1, verdict: hostedNever, by: "order/order.go:late", via: "resolve"}, "calls resolve before a guard"},
		{"x", spawnSite{spawns: 1, verdict: hostedNever, by: "order/order.go:first"}, "(via)"},
		{"order/order.go:selfFirst", spawnSite{spawns: 1, verdict: hostedNever, by: "order/order.go:selfFirst"}, ""},
		{"order/order.go:selfLate", spawnSite{spawns: 1, verdict: hostedNever, by: "order/order.go:selfLate"}, "starts before a guard"},
		{"x", spawnSite{spawns: 1, verdict: hostedNever, by: "order/order.go:negated", via: "resolve"}, "calls resolve before a guard"},
		{"x", spawnSite{spawns: 1, verdict: hostedNever, by: "order/order.go:logged", via: "resolve"}, "calls resolve before a guard"},
		{"x", spawnSite{spawns: 1, verdict: hostedNever, by: "order/order.go:nested", via: "resolve"}, "calls resolve before a guard"},
		{"x", spawnSite{spawns: 1, verdict: hostedNever, by: "order/order.go:both", via: "resolve"}, "calls resolve before a guard"},
		{"x", spawnSite{spawns: 1, verdict: hostedNever, by: "order/order.go:either", via: "resolve"}, ""},
		{"x", spawnSite{spawns: 1, verdict: hostedNever, by: "order/order.go:held", via: "resolve"}, ""},
		{"order/order.go:literalMarked", spawnSite{spawns: 1, verdict: marked, by: "order/order.go:literalMarked"}, ""},
		{"order/order.go:literalLate", spawnSite{spawns: 1, verdict: marked, by: "order/order.go:literalLate"}, "starts before MarkRepositoryCodeStarted"},
		{"x", spawnSite{spawns: 1, verdict: holder, by: "order/order.go:inside", native: true}, ""},
		{"x", spawnSite{spawns: 1, verdict: holder, by: "order/order.go:outside", native: true}, "RequireNativeHolder"},
		{"x", spawnSite{spawns: 1, verdict: marked, by: "order/order.go:outside", native: true}, "only a holder is native"},
		{"seam/seam.go:Resolve", spawnSite{spawns: 1, verdict: alias}, ""},
		{"seam/seam.go:Local", spawnSite{spawns: 1, verdict: alias}, "only a package-level variable"},
	} {
		spawns := spawn
		if positions, ok := found[tc.key]; ok {
			spawns = positions
		}
		problem := checkSpawnSite(tc.key, tc.site, spawns, functions)
		if (tc.problem == "") != (problem == "") || !strings.Contains(problem, tc.problem) {
			t.Errorf("%s by %s: problem %q, want one with %q", tc.key, tc.site.by, problem, tc.problem)
		}
	}
}

// checkSpawnSite returns what is wrong with the classification of the spawns
// of key, or "".
func checkSpawnSite(key string, site spawnSite, spawns []token.Pos, functions map[string]scannedFunction) string {
	if site.native && site.verdict != holder {
		return key + ": only a holder is native"
	}
	if site.via != "" && site.verdict != hostedNever {
		return key + ": only a hostedNever site names via"
	}
	switch site.verdict {
	case marked, holder, hostedNever:
		want := "MarkRepositoryCodeStarted"
		switch site.verdict {
		case holder:
			want = "StartHolder"
		case hostedNever:
			want = "Hosted"
		}
		fn, ok := functions[site.by]
		if !ok {
			return key + ": " + site.by + " does not exist"
		}
		calls := fn.calls[want]
		if len(calls) == 0 {
			return key + ": " + site.by + " does not call " + want
		}
		if site.by == key && site.verdict == marked {
			if start, ok := functions[key].firstStart(spawns[0]); ok && calls[0] > start {
				return key + ": the process starts before " + want
			}
		}
		if site.verdict == hostedNever {
			if problem := checkHostedFirst(key, site, spawns, fn); problem != "" {
				return problem
			}
		}
		if site.native && !fn.checksNativeHolder() {
			return key + ": " + site.by + " does not call RequireNativeHolder inside the function StartHolder runs"
		}
	case noRepositoryCode:
		if site.by != "" || strings.TrimSpace(site.why) == "" {
			return key + ": a site that runs no repository code says why and names no function"
		}
	case testSupport:
		if !strings.HasPrefix(key, "cli/clitest/") && !strings.HasPrefix(key, "fixtureproc/") {
			return key + ": only cli/clitest and fixtureproc are test support"
		}
	case alias:
		if site.by != "" || !functions[key].alias {
			return key + ": only a package-level variable that holds a spawn helper is an alias, and it names no function"
		}
	default:
		return key + ": no verdict"
	}
	return ""
}

// checkHostedFirst returns what is wrong with a hostedNever site whose
// function by asks Hosted, or "": a Hosted guard of by (hostedGuards) ends
// before the spawn when by spawns itself, and before its first call of via
// otherwise, so a hosted run returns before either.
func checkHostedFirst(key string, site spawnSite, spawns []token.Pos, by scannedFunction) string {
	guarded := func(at token.Pos) bool { return len(by.guards) > 0 && by.guards[0] < at }
	const guard = "a guard `if runcredential.Hosted() { return }` at its top level"
	if site.by == key {
		if site.via != "" {
			return key + ": a site that spawns itself names no via"
		}
		if !guarded(spawns[0]) {
			return key + ": the process starts before " + guard
		}
		return ""
	}
	if site.via == "" {
		return key + ": name the call of " + site.by + " that leads to the spawn (via)"
	}
	via := by.local[site.via]
	if len(via) == 0 {
		return key + ": " + site.by + " does not call " + site.via
	}
	if !guarded(via[0]) {
		return key + ": " + site.by + " calls " + site.via + " before " + guard
	}
	return ""
}

// scannedFunction is one function of the scanned tree: the runcredential
// calls it makes and its calls of a bare name, such as a function parameter,
// by name; the StartHolder calls; the positions of the calls that can start a
// process, and the ends of its Hosted guards (hostedGuards), in source order.
// alias is true for a package-level variable that holds a spawn helper.
type scannedFunction struct {
	calls   map[string][]token.Pos
	local   map[string][]token.Pos
	holders []callSpan
	starts  []token.Pos
	guards  []token.Pos
	alias   bool
}

// hostedGuards returns the end of each Hosted guard of body, a function body
// or a package-level variable that holds a function literal. A guard is a
// statement at the top level of the function, `if <cond> { ...; return }`,
// whose cond is a Hosted call or an || chain with one: a hosted run returns
// there, so it reaches no statement after the guard. A Hosted call in a
// nested block, a negated one, or one in an && chain guards nothing.
func hostedGuards(body ast.Node, imports map[string]string, inRuncredential bool) []token.Pos {
	block, ok := body.(*ast.BlockStmt)
	if value, isValue := body.(*ast.ValueSpec); isValue && len(value.Values) == 1 {
		if lit, isLit := value.Values[0].(*ast.FuncLit); isLit {
			block, ok = lit.Body, true
		}
	}
	if !ok || block == nil {
		return nil
	}
	var hosted func(ast.Expr) bool
	hosted = func(cond ast.Expr) bool {
		switch cond := cond.(type) {
		case *ast.ParenExpr:
			return hosted(cond.X)
		case *ast.BinaryExpr:
			return cond.Op == token.LOR && (hosted(cond.X) || hosted(cond.Y))
		case *ast.CallExpr:
			if len(cond.Args) != 0 {
				return false
			}
			switch fun := cond.Fun.(type) {
			case *ast.SelectorExpr:
				pkg, ok := fun.X.(*ast.Ident)
				return ok && fun.Sel.Name == "Hosted" && imports[pkg.Name] == runcredentialImport
			case *ast.Ident:
				return inRuncredential && fun.Name == "Hosted"
			}
		}
		return false
	}
	var guards []token.Pos
	for _, stmt := range block.List {
		guard, ok := stmt.(*ast.IfStmt)
		if !ok || !hosted(guard.Cond) || len(guard.Body.List) == 0 {
			continue
		}
		if _, returns := guard.Body.List[len(guard.Body.List)-1].(*ast.ReturnStmt); returns {
			guards = append(guards, guard.End())
		}
	}
	return guards
}

// callSpan is the source range of one call, arguments included.
type callSpan struct{ from, to token.Pos }

// checksNativeHolder reports whether the function calls RequireNativeHolder
// inside a StartHolder call: in the function StartHolder runs before the
// spawn, when no repository code can run between the check and the start.
func (f scannedFunction) checksNativeHolder() bool {
	for _, check := range f.calls["RequireNativeHolder"] {
		for _, span := range f.holders {
			if check > span.from && check < span.to {
				return true
			}
		}
	}
	return false
}

// firstStart is the position where the process built at spawn starts: the
// spawn itself for an exec, else the first Start, Run, Output or
// CombinedOutput call after it. It is false when the function returns the
// command unstarted.
func (f scannedFunction) firstStart(spawn token.Pos) (token.Pos, bool) {
	for _, start := range f.starts {
		if start >= spawn {
			return start, true
		}
	}
	return token.NoPos, false
}

// cliModule is the import path of the CLI's module, and internalImport that
// of its internal/ directory, the root the scan walks.
const (
	cliModule           = "go.putnami.dev/tooling/cli"
	internalImport      = cliModule + "/internal"
	runcredentialImport = internalImport + "/runcredential"
)

// scannedFile is one non-test file of the scanned tree: its slash path under
// the root, the import path of its package, and its syntax.
type scannedFile struct {
	rel  string
	pkg  string
	file *ast.File
}

// scanSpawnSites parses every non-test Go file under root, the CLI's
// internal/ directory, and returns the positions of the process spawns of
// each function, and every function. Both are keyed "<file>:<function>".
func scanSpawnSites(t *testing.T, root string) (map[string][]token.Pos, map[string]scannedFunction) {
	t.Helper()
	fset := token.NewFileSet()
	var files []scannedFile
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		files = append(files, scannedFile{rel: rel, pkg: internalImport + "/" + pathpkg.Dir(rel), file: file})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	aliases := spawnAliases(files)
	found := map[string][]token.Pos{}
	functions := map[string]scannedFunction{}
	for _, file := range files {
		scanFile(file, aliases, found, functions)
	}
	return found, functions
}

// spawnCalls names, per import path, the functions that build or start a
// process, including the SDK helpers in the CLI's build that do. A use is a
// call or a value, such as a package variable that holds the helper. An
// exec.Cmd starts at its first Start, Run, Output or CombinedOutput; a use of
// any other function is where its process starts, because it starts one
// itself or returns what starts one:
//   - registrycred starts `putnami cloud registry-token`;
//   - a releaseset client starts `putnami cloud release-set`;
//   - a dbtestenv provider or job starts docker;
//   - the SDK's exec.Run starts the command it names;
//   - the oci daemon helpers start docker: the export saves a daemon image,
//     and a content push without a layout tags and pushes one. The CLI
//     uploads a packed layout with oci.PushLayout, which starts nothing.
//
// TestEveryDependencySpawnHelperIsInventoried keeps the SDK entries complete.
var spawnCalls = map[string]map[string]bool{
	"os/exec":               {"Command": true, "CommandContext": true},
	"os":                    {"StartProcess": true},
	"syscall":               {"Exec": true, "ForkExec": true, "StartProcess": true},
	"golang.org/x/sys/unix": {"Exec": true},
	"go.putnami.dev/sdk/extension/registrycred": {
		"ResolveToken": true, "ResolveTokenWithCLI": true, "EnsureNativeCredential": true,
	},
	"go.putnami.dev/sdk/extension/releaseset": {"NewClient": true},
	"go.putnami.dev/sdk/extension/dbtestenv": {
		"SelectProvider": true, "NewProvisioner": true, "UpJob": true, "DownJob": true,
	},
	"go.putnami.dev/sdk/extension/exec": {"Run": true},
	"go.putnami.dev/sdk/extension/oci": {
		"ExportDaemonImageToLayout": true, "PushContent": true, "PushContentWithDigest": true,
	},
}

// startMethods are the functions that start a built process: the methods of
// an exec.Cmd or of the process tree that wraps one, and
// proctree.StartDetached.
var startMethods = map[string]bool{"Start": true, "Run": true, "Output": true, "CombinedOutput": true, "StartDetached": true}

// spawnTypes names, per import path, the types whose methods start a
// process. A value of one built without the constructor spawnCalls names, by a
// composite literal, new(T) or a variable declared without a value, is a
// spawn: it starts where the value is built, or, for an exec.Cmd, at its first
// Start, Run, Output or CombinedOutput.
var spawnTypes = map[string]map[string]bool{
	"os/exec": {"Cmd": true},
	"go.putnami.dev/sdk/extension/releaseset": {"Client": true},
}

// spawnTypeValue returns the node that builds a value of a spawn type, and the
// type, when node is one: a composite literal, a call of new, or a variable
// declared with the type and no value. A pointer type builds no value. named
// reports whether a type expression names a spawn type.
func spawnTypeValue(node ast.Node, named func(ast.Expr) bool) (ast.Expr, bool) {
	switch node := node.(type) {
	case *ast.CompositeLit:
		return node.Type, node.Type != nil && named(node.Type)
	case *ast.CallExpr:
		if fun, ok := node.Fun.(*ast.Ident); ok && fun.Name == "new" && len(node.Args) == 1 {
			return node.Args[0], named(node.Args[0])
		}
	case *ast.ValueSpec:
		return node.Type, node.Type != nil && len(node.Values) == 0 && named(node.Type)
	}
	return nil, false
}

// spawnAliasSet is the package-level variables of the scanned tree that hold
// a spawn helper: their names by import path, and the identifiers that
// declare them.
type spawnAliasSet struct {
	names map[string]map[string]bool
	decls map[*ast.Ident]bool
}

// spawnAliases finds the package-level variables whose value is a spawn
// helper: a spawnCalls function or another such variable. A use of one
// spawns as the helper does, by its name in its own package or by a
// package-qualified name in another.
func spawnAliases(files []scannedFile) spawnAliasSet {
	set := spawnAliasSet{names: map[string]map[string]bool{}, decls: map[*ast.Ident]bool{}}
	for changed := true; changed; {
		changed = false
		for _, file := range files {
			imports := fileImports(file.file)
			for _, decl := range file.file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.VAR {
					continue
				}
				for _, spec := range gen.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range value.Names {
						if i >= len(value.Values) || set.decls[name] || !set.helper(file.pkg, imports, value.Values[i]) {
							continue
						}
						if set.names[file.pkg] == nil {
							set.names[file.pkg] = map[string]bool{}
						}
						set.names[file.pkg][name.Name] = true
						set.decls[name] = true
						changed = true
					}
				}
			}
		}
	}
	return set
}

// helper reports whether expr, in a file of package pkg with imports, names a
// spawn helper: a spawnCalls function or an alias.
func (s spawnAliasSet) helper(pkg string, imports map[string]string, expr ast.Expr) bool {
	switch expr := expr.(type) {
	case *ast.SelectorExpr:
		x, ok := expr.X.(*ast.Ident)
		if !ok {
			return false
		}
		path := imports[x.Name]
		return spawnCalls[path][expr.Sel.Name] || s.names[path][expr.Sel.Name]
	case *ast.Ident:
		return s.names[pkg][expr.Name]
	}
	return false
}

// fileImports maps the names a file imports packages under to their import
// paths.
func fileImports(file *ast.File) map[string]string {
	imports := map[string]string{}
	for _, spec := range file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		name := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = path
	}
	return imports
}

func scanFile(file scannedFile, aliases spawnAliasSet, found map[string][]token.Pos, functions map[string]scannedFunction) {
	imports := fileImports(file.file)
	inRuncredential := file.pkg == runcredentialImport
	for _, decl := range file.file.Decls {
		for _, unit := range declarationUnits(decl) {
			key := file.rel + ":" + unit.name
			// Declarations that share a key, such as two type declarations,
			// add up.
			fn := functions[key]
			if fn.calls == nil {
				fn.calls, fn.local = map[string][]token.Pos{}, map[string][]token.Pos{}
			}
			if value, ok := unit.body.(*ast.ValueSpec); ok {
				for _, name := range value.Names {
					fn.alias = fn.alias || aliases.decls[name]
				}
			}
			spawn := func(at token.Pos, starts bool) {
				found[key] = append(found[key], at)
				if starts {
					fn.starts = append(fn.starts, at)
				}
			}
			record := func(name string, call *ast.CallExpr) {
				fn.calls[name] = append(fn.calls[name], call.Pos())
				if name == "StartHolder" {
					fn.holders = append(fn.holders, callSpan{from: call.Pos(), to: call.End()})
				}
			}
			// spawnTypePath is the import path of the spawn type a type
			// expression names, or "".
			spawnTypePath := func(typ ast.Expr) string {
				sel, ok := typ.(*ast.SelectorExpr)
				if !ok {
					return ""
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok {
					return ""
				}
				if path := imports[pkg.Name]; spawnTypes[path][sel.Sel.Name] {
					return path
				}
				return ""
			}
			fn.guards = append(fn.guards, hostedGuards(unit.body, imports, inRuncredential)...)
			selected := map[*ast.Ident]bool{}
			ast.Inspect(unit.body, func(node ast.Node) bool {
				if typ, ok := spawnTypeValue(node, func(typ ast.Expr) bool { return spawnTypePath(typ) != "" }); ok {
					spawn(node.Pos(), spawnTypePath(typ) != "os/exec")
				}
				switch node := node.(type) {
				case *ast.SelectorExpr:
					selected[node.Sel] = true
					pkg, ok := node.X.(*ast.Ident)
					if !ok {
						return true
					}
					if path := imports[pkg.Name]; spawnCalls[path][node.Sel.Name] || aliases.names[path][node.Sel.Name] {
						spawn(node.Pos(), path != "os/exec")
					}
				case *ast.Ident:
					if !selected[node] && !aliases.decls[node] && aliases.names[file.pkg][node.Name] {
						spawn(node.Pos(), true)
					}
				case *ast.CallExpr:
					switch fun := node.Fun.(type) {
					case *ast.SelectorExpr:
						if startMethods[fun.Sel.Name] {
							fn.starts = append(fn.starts, node.Pos())
						}
						if pkg, ok := fun.X.(*ast.Ident); ok && imports[pkg.Name] == runcredentialImport {
							record(fun.Sel.Name, node)
						}
					case *ast.Ident:
						fn.local[fun.Name] = append(fn.local[fun.Name], node.Pos())
						if inRuncredential {
							record(fun.Name, node)
						}
					}
				}
				return true
			})
			sortPositions(fn.starts)
			sortPositions(fn.guards)
			for _, calls := range fn.calls {
				sortPositions(calls)
			}
			for _, calls := range fn.local {
				sortPositions(calls)
			}
			functions[key] = fn
		}
	}
}

// sortPositions sorts positions in source order.
func sortPositions(positions []token.Pos) {
	sort.Slice(positions, func(i, j int) bool { return positions[i] < positions[j] })
}

// declarationUnit is a part of a file that the scan keys on its own: a
// function body, a package-level variable or constant, or the file's other
// declarations together.
type declarationUnit struct {
	name string
	body ast.Node
}

// declarationUnits splits decl into its units. A function without a body has
// none. A package-level variable is named after its names, so a function
// literal it holds is classified on its own.
func declarationUnits(decl ast.Decl) []declarationUnit {
	switch decl := decl.(type) {
	case *ast.FuncDecl:
		if decl.Body == nil {
			return nil
		}
		return []declarationUnit{{name: functionName(decl), body: decl.Body}}
	case *ast.GenDecl:
		units := make([]declarationUnit, 0, len(decl.Specs))
		for _, spec := range decl.Specs {
			name := "package-level declarations"
			if value, ok := spec.(*ast.ValueSpec); ok {
				names := make([]string, len(value.Names))
				for i, ident := range value.Names {
					names[i] = ident.Name
				}
				name = strings.Join(names, ",")
			}
			units = append(units, declarationUnit{name: name, body: spec})
		}
		return units
	}
	return nil
}

// functionName is "Name" for a function and "(*T).Name" or "T.Name" for a
// method.
func functionName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
		if index, ok := recv.(*ast.IndexExpr); ok {
			recv = index.X
		}
		if ident, ok := recv.(*ast.Ident); ok {
			return "(*" + ident.Name + ")." + fn.Name.Name
		}
	}
	if index, ok := recv.(*ast.IndexExpr); ok {
		recv = index.X
	}
	if ident, ok := recv.(*ast.Ident); ok {
		return ident.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// In a dependency, a function reaches a start when it builds a
// value of a spawning type by new or by a variable declared without a value,
// as by a literal, whether the type is its package's own or an exec.Cmd. A
// pointer variable builds nothing.
func TestSpawnHelpersFollowNewAndZeroValues(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "dep.go", `package dep
import "os/exec"
type Runner struct{ path string }
func (r Runner) Run() error { return exec.Command(r.path).Run() }
func Built() *Runner { return new(Runner) }
func Zero() Runner { var r Runner; return r }
func Pointer() *Runner { var r *Runner; return r }
func Cmd() *exec.Cmd { return new(exec.Cmd) }
func CmdZero() { var c exec.Cmd; _ = c }
func CmdPointer() { var c *exec.Cmd; _ = c }
`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	functions, types := spawnHelpers([]*ast.File{file})
	if want := map[string]bool{"Built": true, "Zero": true, "Cmd": true, "CmdZero": true}; !maps.Equal(functions, want) {
		t.Errorf("functions = %v, want %v", functions, want)
	}
	if want := map[string]bool{"Runner": true}; !maps.Equal(types, want) {
		t.Errorf("types = %v, want %v", types, want)
	}
}

// Every go.putnami.dev package outside the CLI's module that the CLI's
// non-test code imports, directly or through another package, lists in
// spawnCalls each exported function or variable that reaches a process
// start, and in spawnTypes each exported type with a method that does. The
// scan of internal/ then finds every use of them. An entry that reaches no
// start, or names a package the CLI does not import, is stale.
func TestEveryDependencySpawnHelperIsInventoried(t *testing.T) {
	t.Parallel()
	cliRoot := filepath.Join("..", "..")
	modules := replacedModules(t, filepath.Join(cliRoot, "go.mod"))
	fset := token.NewFileSet()
	packages := map[string][]*ast.File{}
	var queue []string
	enqueue := func(files []*ast.File) {
		for _, file := range files {
			for _, path := range fileImports(file) {
				if !strings.HasPrefix(path, "go.putnami.dev/") || path == cliModule || strings.HasPrefix(path, cliModule+"/") {
					continue
				}
				if _, seen := packages[path]; !seen {
					packages[path] = nil
					queue = append(queue, path)
				}
			}
		}
	}
	enqueue(parseTree(t, fset, cliRoot))
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		dir, ok := packageDir(modules, path)
		if !ok {
			t.Errorf("%s: no replace directive of the CLI's go.mod names its module", path)
			continue
		}
		files := parsePackage(t, fset, dir)
		packages[path] = files
		enqueue(files)
	}

	var problems []string
	for path, files := range packages {
		functions, types := spawnHelpers(files)
		for name := range functions {
			if !spawnCalls[path][name] {
				problems = append(problems, path+"."+name+" starts a process: add it to spawnCalls")
			}
		}
		for name := range types {
			if !spawnTypes[path][name] {
				problems = append(problems, path+"."+name+" has a method that starts a process: add it to spawnTypes")
			}
		}
		for name := range spawnCalls[path] {
			if !functions[name] {
				problems = append(problems, "spawnCalls lists "+path+"."+name+", which starts no process: remove it")
			}
		}
		for name := range spawnTypes[path] {
			if !types[name] {
				problems = append(problems, "spawnTypes lists "+path+"."+name+", whose methods start no process: remove it")
			}
		}
	}
	for _, listed := range []map[string]map[string]bool{spawnCalls, spawnTypes} {
		for path := range listed {
			if _, imported := packages[path]; strings.HasPrefix(path, "go.putnami.dev/") && !imported {
				problems = append(problems, path+" is listed, but the CLI does not import it: remove it")
			}
		}
	}
	sort.Strings(problems)
	for _, problem := range problems {
		t.Error(problem)
	}
}

// replacedModules reads the replace directives of the go.mod at path: each
// module path and the directory that builds it.
func replacedModules(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	modules := map[string]string{}
	inBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "replace (":
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "replace "):
			line = strings.TrimPrefix(line, "replace ")
		case !inBlock:
			continue
		}
		from, to, ok := strings.Cut(line, "=>")
		if !ok || len(strings.Fields(from)) == 0 || len(strings.Fields(to)) == 0 {
			continue
		}
		modules[strings.Fields(from)[0]] = filepath.Join(filepath.Dir(path), filepath.FromSlash(strings.Fields(to)[0]))
	}
	if len(modules) == 0 {
		t.Fatalf("%s has no replace directive", path)
	}
	return modules
}

// packageDir is the directory of the package at import path, in the longest
// replaced module that contains it.
func packageDir(modules map[string]string, path string) (string, bool) {
	best := ""
	for module := range modules {
		if (path == module || strings.HasPrefix(path, module+"/")) && len(module) > len(best) {
			best = module
		}
	}
	if best == "" {
		return "", false
	}
	return filepath.Join(modules[best], filepath.FromSlash(strings.TrimPrefix(path, best))), true
}

// parseTree parses every non-test Go file under root, outside testdata.
func parseTree(t *testing.T, fset *token.FileSet, root string) []*ast.File {
	t.Helper()
	var files []*ast.File
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files = append(files, file)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// parsePackage parses the non-test Go files of the package in dir, for
// every platform.
func parsePackage(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	return files
}

// spawnHelpers returns the exported functions and variables of the package
// made of files that reach a process start, and its exported types with a
// method that does. A declaration reaches a start when it uses a spawnCalls
// function or builds a spawnTypes value of another package, uses a function
// or variable of its own package that reaches one, or builds a value of a
// type of its own package with a method that does. A value is built by a
// composite literal, new or a variable declared without a value
// (spawnTypeValue). A call x.M counts as a use of every method M of the
// package, so the answer errs toward reaching.
func spawnHelpers(files []*ast.File) (functions, types map[string]bool) {
	type body struct {
		node    ast.Node
		imports map[string]string
	}
	// Declarations are keyed by name, and methods by "T.M". Declarations
	// for other platforms share a key and add up.
	declarations := map[string][]body{}
	receivers := map[string]string{}
	methods := map[string][]string{}
	for _, file := range files {
		imports := fileImports(file)
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Body == nil {
					continue
				}
				key := decl.Name.Name
				if receiver := receiverTypeName(decl); receiver != "" {
					key = receiver + "." + decl.Name.Name
					receivers[key] = receiver
					methods[decl.Name.Name] = append(methods[decl.Name.Name], key)
				}
				declarations[key] = append(declarations[key], body{decl.Body, imports})
			case *ast.GenDecl:
				if decl.Tok != token.VAR {
					continue
				}
				for _, spec := range decl.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range value.Names {
						declarations[name.Name] = append(declarations[name.Name], body{value, imports})
					}
				}
			}
		}
	}

	reaches := map[string]bool{}
	spawning := map[string]bool{}
	uses := func(b body) bool {
		used := false
		selected := map[*ast.Ident]bool{}
		// named reports whether a type expression names a type of this
		// package with a method that reaches a start, or a spawnTypes type.
		named := func(typ ast.Expr) bool {
			switch typ := typ.(type) {
			case *ast.Ident:
				return spawning[typ.Name]
			case *ast.SelectorExpr:
				x, ok := typ.X.(*ast.Ident)
				return ok && spawnTypes[b.imports[x.Name]][typ.Sel.Name]
			}
			return false
		}
		ast.Inspect(b.node, func(node ast.Node) bool {
			if _, ok := spawnTypeValue(node, named); ok {
				used = true
				return false
			}
			switch node := node.(type) {
			case *ast.SelectorExpr:
				selected[node.Sel] = true
				if x, ok := node.X.(*ast.Ident); ok {
					if path, ok := b.imports[x.Name]; ok {
						used = used || spawnCalls[path][node.Sel.Name]
						return false
					}
				}
				for _, method := range methods[node.Sel.Name] {
					used = used || reaches[method]
				}
			case *ast.Ident:
				used = used || (!selected[node] && reaches[node.Name])
			}
			return !used
		})
		return used
	}
	for changed := true; changed; {
		changed = false
		for key, bodies := range declarations {
			if reaches[key] {
				continue
			}
			for _, b := range bodies {
				if uses(b) {
					reaches[key] = true
					if receiver := receivers[key]; receiver != "" {
						spawning[receiver] = true
					}
					changed = true
					break
				}
			}
		}
	}

	functions, types = map[string]bool{}, map[string]bool{}
	for key := range reaches {
		if receivers[key] == "" && ast.IsExported(key) {
			functions[key] = true
		}
	}
	for receiver := range spawning {
		if ast.IsExported(receiver) {
			types[receiver] = true
		}
	}
	return functions, types
}

// receiverTypeName is the name of the type a method is declared on, or ""
// for a function.
func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	switch typ := recv.(type) {
	case *ast.IndexExpr:
		recv = typ.X
	case *ast.IndexListExpr:
		recv = typ.X
	}
	if ident, ok := recv.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}
