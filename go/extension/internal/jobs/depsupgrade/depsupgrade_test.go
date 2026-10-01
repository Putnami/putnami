package depsupgrade

// These cases replicate the go.work shapes seen in real consumer workspaces
// (mixed replace blocks, scattered standalone pins, stale protocol pins, local
// directory overrides) and assert the job rewrites them into one consolidated
// replace block, fails closed, and rolls back every surface it touched.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/go/extension/internal/releaseplan"
	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
)

// The signal case runs the job in a child process: these variables turn the
// test binary into that child. brokenGo turns it into a go command that fails
// every invocation.
const (
	helperWorkspace = "DEPSUPGRADE_HELPER_WORKSPACE"
	helperParams    = "DEPSUPGRADE_HELPER_PARAMS"
	brokenGo        = "DEPSUPGRADE_BROKEN_GO"
)

func TestMain(m *testing.M) {
	if os.Getenv(brokenGo) != "" {
		_, _ = os.Stderr.WriteString("go: broken toolchain\n")
		os.Exit(1)
	}
	jobtest.ServeFake()
	if workspace := os.Getenv(helperWorkspace); workspace != "" {
		os.Exit(runHelper(workspace, os.Getenv(helperParams)))
	}
	os.Exit(m.Run())
}

// runHelper runs the job as the runtime does, with its events printed on
// standard output as they are emitted, and returns its exit status.
func runHelper(workspace, document string) int {
	j := workspacejob.New(context.Background(), jobtest.Printer{W: os.Stdout}, os.Environ(), workspace, "")
	j.Stdout = os.Stderr
	var params map[string]json.RawMessage
	if err := json.Unmarshal([]byte(document), &params); err != nil {
		return 2
	}
	status := statusFailed
	if opts, ok := ParseOptions(j, nil, params, ""); ok {
		status = run(j, opts, nil)
	}
	j.Trap().Disarm()
	if status != statusOK {
		return 1
	}
	return 0
}

// upgradeRun is one deps-upgrade run under test.
type upgradeRun struct {
	job    *workspacejob.Job
	rec    *jobtest.Recorder
	status string
}

// runUpgrade runs the job in-process on ws with the job parameters params.
// origin is both the module origin channel queries go to and the GOPROXY the
// go command downloads from, as the script's tests had it; cacheRoot is the
// Go cache root, shared by the runs of one test. members are the project paths
// of the workspace membership the job context carries.
//
// PATH holds the shell traps, then the directory of the real go command and
// nothing else, and every run checks what it owes: nothing but events on the
// job's standard output, which is the JSONL stream, and no shell program
// started.
func runUpgrade(t *testing.T, ws, cacheRoot, params, origin string, members ...string) *upgradeRun {
	t.Helper()
	goBinary := jobtest.RequireGo(t)
	fakes := jobtest.NewFakes(t)
	path := fakes.Dir + string(filepath.ListSeparator) + filepath.Dir(goBinary)
	overrides := append(fakes.Environ(), "PATH="+path, "PUTNAMI_GO_MODULE_PROXY="+origin)
	env := jobtest.Env(t, origin, cacheRoot, overrides...)
	rec := &jobtest.Recorder{}
	j, stdout, _ := jobtest.NewJob(t, rec, env, ws)

	status := statusFailed
	if opts, ok := ParseOptions(j, nil, jobtest.Params(t, params), filepath.Join(ws, "context.json")); ok {
		status = run(j, opts, members)
	}
	if stdout.Len() > 0 {
		t.Errorf("a command wrote to the job event stream:\n%s", stdout)
	}
	if invocations := fakes.Invocations(t); len(invocations) != 0 {
		t.Errorf("deps-upgrade started shell programs: %v", invocations)
	}
	return &upgradeRun{job: j, rec: rec, status: status}
}

func (r *upgradeRun) requireStatus(t *testing.T, want string) {
	t.Helper()
	if r.status != want {
		t.Fatalf("deps-upgrade = %s, want %s:\n%s", r.status, want, r.rec.Transcript())
	}
}

func (r *upgradeRun) requireLogs(t *testing.T, want ...string) {
	t.Helper()
	for _, text := range want {
		if !r.rec.Contains(text) {
			t.Errorf("output does not contain %q:\n%s", text, r.rec.Transcript())
		}
	}
}

func (r *upgradeRun) forbidLogs(t *testing.T, forbidden ...string) {
	t.Helper()
	for _, text := range forbidden {
		if r.rec.Contains(text) {
			t.Errorf("output contains %q:\n%s", text, r.rec.Transcript())
		}
	}
}

// sandboxWorkspace lays out a workspace whose go.work mirrors the messy
// pre-fix state: a replace block mixing local-path replaces with stale
// protocol pins at one stamp, standalone framework pins at another stamp, a
// local directory override, and two use'd modules (one of which shadows a
// published go.putnami.dev module).
func sandboxWorkspace(t *testing.T) string {
	t.Helper()
	ws := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, ws, "local/mylib/go.mod", `module example.com/mylib

go 1.22

require (
	go.putnami.dev/http v0.1.0-46554d75
	go.putnami.dev/protocol/cache v0.1.0-46554d75
	go.putnami.dev/web v0.1.0-46554d75
)
`)
	jobtest.WriteFile(t, ws, "local/client/go.mod", "module go.putnami.dev/client\n\ngo 1.22\n")
	jobtest.WriteFile(t, ws, "go.work", `go 1.22

use (
	./local/client
	./local/mylib
)

replace (
	example.com/locallib v0.0.0 => ./local/mylib
	go.putnami.dev/protocol/cache => go.putnami.dev/protocol/cache v0.1.0-46554d75
	go.putnami.dev/protocol/storage => go.putnami.dev/protocol/storage v0.1.0-46554d75
)

replace go.putnami.dev/api => go.putnami.dev/api v0.1.0-fbe79cd0

replace go.putnami.dev/http => go.putnami.dev/http v0.1.0-fbe79cd0

replace go.putnami.dev/inject => ./local/inject-fork
`)
	return ws
}

// sandboxRequires are the go.putnami.dev modules the sandbox workspace
// requires beyond the published list.
var sandboxRequires = []string{"go.putnami.dev/http", "go.putnami.dev/protocol/cache", "go.putnami.dev/web"}

// writeLatestProxy creates a file:// module proxy tree that answers /@latest
// for every module the run pins, and serves modules. A channel is resolved
// per module, so the fixture answers for every module the run pins.
func writeLatestProxy(t *testing.T, version string, modules ...jobtest.ProxyModule) string {
	t.Helper()
	proxy := jobtest.WriteModuleProxy(t, modules...)
	for _, modulePath := range PublishedModules {
		jobtest.WriteLatestInfo(t, proxy, modulePath, version)
	}
	// A go.putnami.dev module the workspace requires but the base list does not
	// name resolves for itself too.
	for _, module := range modules {
		jobtest.WriteLatestInfo(t, proxy, module.Path, version)
	}
	return proxy
}

// writeChannelProxy answers channel with version for every module the
// sandbox run pins, and serves the sandbox requirements at version.
func writeChannelProxy(t *testing.T, dir, channel, version string) {
	t.Helper()
	jobtest.WriteModuleProxyAt(t, dir, jobtest.SimpleProxyModules(version, sandboxRequires...)...)
	for _, modulePath := range append(append([]string(nil), PublishedModules...), sandboxRequires...) {
		jobtest.WriteChannelInfo(t, dir, modulePath, channel, version)
	}
}

func readGoWork(t *testing.T, ws string) string {
	t.Helper()
	return jobtest.ReadFile(t, filepath.Join(ws, "go.work"))
}

// releaseSetContextParams is the parameter object the CLI hands the job for
// an upgrade to a release set: the channel head the orchestrator resolved.
func releaseSetContextParams(t *testing.T, goVersions, npmVersions map[string]string, dryRun bool) string {
	t.Helper()
	members := make([]distribution.ReleaseSetMember, 0, len(goVersions)+len(npmVersions))
	for coordinate, version := range goVersions {
		members = append(members, distribution.ReleaseSetMember{
			Ecosystem:            releaseplan.GoEcosystem,
			Coordinate:           coordinate,
			Version:              version,
			ArtifactDigest:       "sha256:" + strings.Repeat("a", 64),
			Dependencies:         []distribution.ReleaseSetDependency{},
			SourceRevision:       strings.Repeat("1", 40),
			SelectionFingerprint: "sha256:" + strings.Repeat("c", 64),
		})
	}
	for coordinate, version := range npmVersions {
		members = append(members, distribution.ReleaseSetMember{
			Ecosystem:            "npm",
			Coordinate:           coordinate,
			Version:              version,
			ArtifactDigest:       "sha256:" + strings.Repeat("b", 64),
			Dependencies:         []distribution.ReleaseSetDependency{},
			SourceRevision:       strings.Repeat("1", 40),
			SelectionFingerprint: "sha256:" + strings.Repeat("d", 64),
		})
	}
	releaseSet := distribution.NormalizeReleaseSet(&distribution.ReleaseSet{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Members:         members,
	})
	ref, diagnostics := distribution.DeriveReleaseSetRef(releaseSet)
	if len(diagnostics) != 0 {
		t.Fatalf("derive release set: %v", diagnostics)
	}
	params, err := json.Marshal(map[string]any{
		// The upgrade hands the extension the CHANNEL HEAD shape. An immutable
		// release is named by no channel, so its generation is 0.
		"releaseSet": distribution.ChannelHead{Ref: ref, Generation: 0, ReleaseSet: releaseSet},
		"dryRun":     dryRun,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(params)
}

func TestDepsUpgrade_LatestConsolidatesGoWorkPins(t *testing.T) {
	t.Parallel()
	ws := sandboxWorkspace(t)
	cacheRoot := jobtest.CacheRoot(t)
	const target = "v0.1.0-aaaa1111"
	proxy := jobtest.FileURL(writeLatestProxy(t, target, jobtest.SimpleProxyModules(target, sandboxRequires...)...))

	r := runUpgrade(t, ws, cacheRoot, `{"putnamiVersion":"latest"}`, proxy)
	r.requireStatus(t, statusOK)
	r.requireLogs(t,
		"Resolved go.putnami.dev/* from latest via go-proxy:",
		"go.putnami.dev/*  v0.1.0-aaaa1111  revision aaaa1111",
		"Skipping go.putnami.dev/inject (replaced by a local directory)",
		"Skipping local workspace module go.putnami.dev/client",
		"Reconciled go.work.sum",
	)

	work := readGoWork(t, ws)
	// Every managed pin moved to the resolved stamp; no stale stamps survive.
	for _, stale := range []string{"46554d75", "fbe79cd0"} {
		if strings.Contains(work, stale) {
			t.Errorf("stale stamp %s still present in go.work:\n%s", stale, work)
		}
	}
	for _, mod := range []string{
		"go.putnami.dev/api",            // previously a standalone pin
		"go.putnami.dev/http",           // previously a standalone pin
		"go.putnami.dev/protocol/cache", // previously pinned inside the block
		"go.putnami.dev/protocol/storage",
		"go.putnami.dev/protocol/workspace", // from the published base list
		"go.putnami.dev/telemetry",          // from the published base list
		"go.putnami.dev/web",                // discovered from use'd go.mod requires
	} {
		pin := mod + " => " + mod + " " + target
		if !strings.Contains(work, pin) {
			t.Errorf("missing pin %q in go.work:\n%s", pin, work)
		}
		if got := strings.Count(work, mod+" => "); got != 1 {
			t.Errorf("module %s has %d replace entries, want exactly 1:\n%s", mod, got, work)
		}
	}

	// Local-path replaces are preserved untouched.
	for _, kept := range []string{
		"example.com/locallib v0.0.0 => ./local/mylib",
		"go.putnami.dev/inject => ./local/inject-fork",
	} {
		if !strings.Contains(work, kept) {
			t.Errorf("local replace %q was modified:\n%s", kept, work)
		}
	}
	if strings.Contains(work, "go.putnami.dev/inject => go.putnami.dev/inject") {
		t.Errorf("local directory override was clobbered by a version pin:\n%s", work)
	}
	// Modules use'd locally are not pinned.
	if strings.Contains(work, "go.putnami.dev/client =>") {
		t.Errorf("local workspace module must not be pinned:\n%s", work)
	}
	// The result still parses as a valid workspace file.
	if out, err := exec.Command(jobtest.RequireGo(t), "work", "edit", "-json", filepath.Join(ws, "go.work")).CombinedOutput(); err != nil {
		t.Fatalf("rewritten go.work does not parse: %v\n%s", err, out)
	}
	if staged, _ := filepath.Glob(filepath.Join(ws, "go.work.putnami-upgrade.*")); len(staged) != 0 {
		t.Errorf("staged go.work copies were left behind: %v", staged)
	}

	// Running the upgrade again must be a byte-for-byte no-op.
	second := runUpgrade(t, ws, cacheRoot, `{"putnamiVersion":"latest"}`, proxy)
	second.requireStatus(t, statusOK)
	if work2 := readGoWork(t, ws); work2 != work {
		t.Errorf("deps-upgrade is not idempotent.\nfirst:\n%s\nsecond:\n%s", work, work2)
	}
}

func TestDepsUpgrade_ExactVersionNeedsNoSelectorLookup(t *testing.T) {
	t.Parallel()
	ws := sandboxWorkspace(t)
	const target = "v0.1.0-bbbb2222"
	proxy := jobtest.FileURL(jobtest.WriteModuleProxy(t, jobtest.SimpleProxyModules(target, sandboxRequires...)...))

	r := runUpgrade(t, ws, jobtest.CacheRoot(t), `{"putnamiVersion":"0.1.0-bbbb2222"}`, proxy)
	r.requireStatus(t, statusOK)
	r.requireLogs(t, "go.putnami.dev/*  v0.1.0-bbbb2222  revision bbbb2222", "Resolved go.putnami.dev/* from v0.1.0-bbbb2222:")
	if work := readGoWork(t, ws); !strings.Contains(work, "go.putnami.dev/api => go.putnami.dev/api v0.1.0-bbbb2222") {
		t.Errorf("expected exact version pin in go.work:\n%s", work)
	}
}

func TestDepsUpgrade_ReleaseSetPinsEachGoModuleIndependently(t *testing.T) {
	t.Parallel()
	ws := sandboxWorkspace(t)
	versions := map[string]string{
		"go.putnami.dev/api":              "v1.1.0",
		"go.putnami.dev/http":             "v1.2.0",
		"go.putnami.dev/protocol/cache":   "v1.3.0",
		"go.putnami.dev/protocol/storage": "v1.4.0",
		"go.putnami.dev/web":              "v1.5.0",
	}
	params := releaseSetContextParams(t, versions, map[string]string{"@putnami/web": "2.0.0"}, false)
	modules := make([]jobtest.ProxyModule, 0, len(versions))
	for modulePath, version := range versions {
		modules = append(modules, jobtest.SimpleProxyModules(version, modulePath)...)
	}
	proxy := jobtest.FileURL(jobtest.WriteModuleProxy(t, modules...))

	r := runUpgrade(t, ws, jobtest.CacheRoot(t), params, proxy)
	r.requireStatus(t, statusOK)
	r.requireLogs(t, "Resolved release set rs_", "with 5 Go member(s)")
	work := readGoWork(t, ws)
	for modulePath, version := range versions {
		if want := modulePath + " => " + modulePath + " " + version; !strings.Contains(work, want) {
			t.Errorf("missing heterogeneous pin %q:\n%s", want, work)
		}
	}
	if strings.Contains(work, "v0.1.0-46554d75") || strings.Contains(work, "v0.1.0-fbe79cd0") {
		t.Errorf("stale uniform versions survived release-set upgrade:\n%s", work)
	}
	// Only the set's Go members are pinned: the npm member and the published
	// modules the set does not name stay out of go.work.
	if strings.Contains(work, "go.putnami.dev/telemetry") || strings.Contains(work, "@putnami/web") {
		t.Errorf("a module outside the release set was pinned:\n%s", work)
	}
}

func TestDepsUpgrade_ReleaseSetMissingRequiredModuleFailsAndRollsBack(t *testing.T) {
	t.Parallel()
	ws := sandboxWorkspace(t)
	before := readGoWork(t, ws)
	params := releaseSetContextParams(t, map[string]string{
		"go.putnami.dev/api":              "v1.1.0",
		"go.putnami.dev/http":             "v1.2.0",
		"go.putnami.dev/protocol/cache":   "v1.3.0",
		"go.putnami.dev/protocol/storage": "v1.4.0",
		// go.putnami.dev/web is required by a workspace member but absent.
	}, nil, false)

	r := runUpgrade(t, ws, jobtest.CacheRoot(t), params, "file:///nonexistent-proxy")
	r.requireStatus(t, statusFailed)
	r.requireLogs(t, "has no exact Go member for go.putnami.dev/web")
	if got := r.rec.PhaseStatus("upgrade"); got != "failed" {
		t.Errorf("upgrade phase = %q, want failed", got)
	}
	if after := readGoWork(t, ws); after != before {
		t.Fatalf("go.work changed despite failed release-set transaction.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestDepsUpgrade_ReleaseSetDryRunReportsExactMixedPins(t *testing.T) {
	t.Parallel()
	ws := sandboxWorkspace(t)
	before := readGoWork(t, ws)
	params := releaseSetContextParams(t, map[string]string{
		"go.putnami.dev/api":              "v1.1.0",
		"go.putnami.dev/http":             "v1.2.0",
		"go.putnami.dev/protocol/cache":   "v1.3.0",
		"go.putnami.dev/protocol/storage": "v1.4.0",
		"go.putnami.dev/web":              "v1.5.0",
	}, nil, true)

	r := runUpgrade(t, ws, jobtest.CacheRoot(t), params, "file:///nonexistent-proxy")
	r.requireStatus(t, statusOK)
	r.requireLogs(t,
		"Resolved release set rs_",
		"Would pin go.work replace go.putnami.dev/http => go.putnami.dev/http v1.2.0",
		"Would pin go.work replace go.putnami.dev/protocol/cache => go.putnami.dev/protocol/cache v1.3.0",
		"Dry run complete; no Go files were changed",
	)
	if after := readGoWork(t, ws); after != before {
		t.Fatalf("release-set dry run changed go.work.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// fileStates reads each path, remembering which ones exist.
func fileStates(t *testing.T, paths ...string) map[string]*string {
	t.Helper()
	states := make(map[string]*string, len(paths))
	for _, path := range paths {
		if content, ok := jobtest.ReadOptional(t, path); ok {
			states[path] = &content
		} else {
			states[path] = nil
		}
	}
	return states
}

// requireFileStates fails when a path differs from its recorded state: other
// content, a deleted file, or a created one.
func requireFileStates(t *testing.T, want map[string]*string) {
	t.Helper()
	for path, before := range want {
		after, exists := jobtest.ReadOptional(t, path)
		switch {
		case before == nil && exists:
			t.Errorf("%s was created by a failed transaction:\n%s", path, after)
		case before != nil && !exists:
			t.Errorf("%s was not restored", path)
		case before != nil && after != *before:
			t.Errorf("%s changed after a failed transaction.\nbefore:\n%s\nafter:\n%s", path, *before, after)
		}
	}
}

func TestDepsUpgrade_ChecksumFailureRollsBackEveryMetadataSurface(t *testing.T) {
	t.Parallel()
	ws := sandboxWorkspace(t)
	// Only one of the three required modules exists. Version resolution is
	// exact, so the upgrade reaches its rewrite phase, then checksum download
	// fails on the incomplete graph.
	proxy := jobtest.FileURL(jobtest.WriteModuleProxy(t, jobtest.SimpleProxyModules("v0.1.0-bbbb3333", "go.putnami.dev/http")...))
	before := fileStates(t,
		filepath.Join(ws, "go.work"),
		filepath.Join(ws, "go.work.sum"),
		filepath.Join(ws, "local", "mylib", "go.mod"),
		filepath.Join(ws, "local", "mylib", "go.sum"),
	)

	r := runUpgrade(t, ws, jobtest.CacheRoot(t), `{"putnamiVersion":"0.1.0-bbbb3333"}`, proxy)
	r.requireStatus(t, statusFailed)
	r.requireLogs(t, "Workspace checksum reconciliation failed (go mod download all): ")
	if got := r.rec.PhaseStatus("checksums"); got != "failed" {
		t.Errorf("checksums phase = %q, want failed", got)
	}
	requireFileStates(t, before)
}

func TestDepsUpgrade_UnresolvableLatestFails(t *testing.T) {
	t.Parallel()
	ws := sandboxWorkspace(t)
	before := readGoWork(t, ws)

	r := runUpgrade(t, ws, jobtest.CacheRoot(t), `{"putnamiVersion":"latest"}`, "file:///nonexistent-proxy")
	r.requireStatus(t, statusFailed)
	r.requireLogs(t, `Cannot resolve channel "latest" for go.putnami.dev/api: proxy file:///nonexistent-proxy/go.putnami.dev/api/@latest did not answer`)
	if after := readGoWork(t, ws); after != before {
		t.Errorf("go.work must not change when resolution fails.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// Without an origin a channel cannot be asked for at all, and the diagnostic
// names the missing declaration.
func TestDepsUpgrade_ChannelWithoutAnOriginFails(t *testing.T) {
	t.Parallel()
	ws := sandboxWorkspace(t)
	r := runUpgrade(t, ws, jobtest.CacheRoot(t), `{"putnamiChannel":"canary"}`, "")
	r.requireStatus(t, statusFailed)
	r.requireLogs(t, `Cannot resolve channel "canary" for go.putnami.dev/api: no module origin is declared (registries.go.origin)`)
}

// A channel is a native Go version query: the proxy answers `@v/<channel>.info`
// exactly as it does for the go command, so upgrade needs no release set and no
// namespace to follow one.
func TestDepsUpgrade_ChannelResolvesThroughTheProxyVersionQuery(t *testing.T) {
	t.Parallel()
	ws := sandboxWorkspace(t)
	const target = "v0.1.0-20260902173000-cafe1234"
	proxyDir := t.TempDir()
	writeChannelProxy(t, proxyDir, "canary", target)
	server := httptest.NewServer(http.FileServer(http.Dir(proxyDir)))
	t.Cleanup(server.Close)

	r := runUpgrade(t, ws, jobtest.CacheRoot(t), `{"putnamiChannel":"canary"}`, server.URL)
	r.requireStatus(t, statusOK)
	r.requireLogs(t,
		"Resolved go.putnami.dev/* from canary via go-proxy:",
		"go.putnami.dev/*  "+target+"  revision cafe1234",
	)
	if work := readGoWork(t, ws); !strings.Contains(work, "go.putnami.dev/api => go.putnami.dev/api "+target) {
		t.Errorf("expected the channel version pinned in go.work:\n%s", work)
	}
}

// A proxy that does not serve the channel must say so, with the module, the
// channel, the host and the status, and must not touch go.work.
func TestDepsUpgrade_UnservedChannelNamesModuleChannelHostAndStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status int
		detail string
	}{
		{http.StatusNotFound, "does not serve that channel (HTTP 404)"},
		{http.StatusGone, "does not serve that channel (HTTP 410)"},
		{http.StatusUnauthorized, "answered HTTP 401"},
		{http.StatusOK, "answered HTTP 200 without a usable version"},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			t.Parallel()
			ws := sandboxWorkspace(t)
			before := readGoWork(t, ws)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"Version":"not-a-version"}`))
			}))
			t.Cleanup(server.Close)

			r := runUpgrade(t, ws, jobtest.CacheRoot(t), `{"putnamiChannel":"canary"}`, server.URL)
			r.requireStatus(t, statusFailed)
			host := strings.TrimPrefix(server.URL, "http://")
			r.requireLogs(t, `Cannot resolve channel "canary" for go.putnami.dev/api: proxy `+host+" "+tc.detail)
			if after := readGoWork(t, ws); after != before {
				t.Errorf("go.work changed after an unserved channel.\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestDepsUpgrade_DryRunLeavesGoWorkUntouched(t *testing.T) {
	t.Parallel()
	ws := sandboxWorkspace(t)
	before := readGoWork(t, ws)

	r := runUpgrade(t, ws, jobtest.CacheRoot(t), `{"putnamiVersion":"0.1.0-cccc3333","dryRun":true}`, "file:///nonexistent-proxy")
	r.requireStatus(t, statusOK)
	r.requireLogs(t, "Would pin go.work replace go.putnami.dev/api => go.putnami.dev/api v0.1.0-cccc3333")
	if after := readGoWork(t, ws); after != before {
		t.Errorf("dry run modified go.work.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestDepsUpgrade_ReconcilesIntroducedFrameworkClosure(t *testing.T) {
	t.Parallel()
	const target = "v0.1.0-bbbb4444"
	const malformedTarget = target + "-3f3afbc"

	introduced := []string{
		"go.putnami.dev/protocol/capabilities",
		"go.putnami.dev/protocol/contracts",
		"go.putnami.dev/protocol/identity",
		"go.putnami.dev/protocol/keyring",
		"go.putnami.dev/protocol/telemetry",
		"go.putnami.dev/protocol/transaction",
		// Not in the published base list: proves the final module graph, rather
		// than a hand-maintained list, owns the reconciliation.
		"go.putnami.dev/protocol/closure",
	}
	var requires, imports strings.Builder
	for _, module := range introduced {
		requires.WriteString("\t" + module + " " + target + "\n")
		imports.WriteString("\t_ \"" + module + "\"\n")
	}
	modules := append(jobtest.SimpleProxyModules(target, introduced...), jobtest.ProxyModule{
		Path:    "go.putnami.dev/app",
		Version: target,
		GoMod:   "module go.putnami.dev/app\n\ngo 1.22\n\nrequire (\n" + requires.String() + ")\n",
		Source:  "package app\n\nimport (\n" + imports.String() + ")\n",
	})
	proxy := jobtest.FileURL(jobtest.WriteModuleProxy(t, modules...))

	ws := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, ws, "putnami.workspace.json", `{"projects":["service"]}`)
	jobtest.WriteFile(t, ws, "service/putnami.json", `{"publish":["go"]}`)
	jobtest.WriteFile(t, ws, "go.work", "go 1.22\n\nuse ./service\n")
	jobtest.WriteFile(t, ws, "service/go.mod", "module example.com/service\n\ngo 1.22\n\nrequire go.putnami.dev/app "+
		malformedTarget+"\n\nexclude go.putnami.dev/protocol/capabilities "+target+"\n")
	jobtest.WriteFile(t, ws, "service/service.go", "package service\n\nimport _ \"go.putnami.dev/app\"\n")

	cacheRoot := jobtest.CacheRoot(t)
	r := runUpgrade(t, ws, cacheRoot, `{"putnamiVersion":"0.1.0-bbbb4444"}`, proxy)
	r.requireStatus(t, statusOK)
	serviceMod := filepath.Join(ws, "service", "go.mod")
	r.requireLogs(t,
		"Found 1 Go project(s)",
		"Removed go.mod exclude go.putnami.dev/protocol/capabilities "+target+" from "+serviceMod,
		"Normalized go.putnami.dev/app "+malformedTarget+" to "+target+" in "+serviceMod,
		"Reconciled standalone checksums for "+serviceMod,
	)

	work := readGoWork(t, ws)
	goMod := jobtest.ReadFile(t, serviceMod)
	moduleSum, ok := jobtest.ReadOptional(t, filepath.Join(ws, "service", "go.sum"))
	if !ok || !strings.Contains(moduleSum, target) {
		t.Errorf("service/go.sum was not reconciled at %s:\n%s", target, moduleSum)
	}
	for _, module := range introduced {
		if pin := module + " => " + module + " " + target; !strings.Contains(work, pin) {
			t.Errorf("missing final-closure pin %q in go.work:\n%s", pin, work)
		}
		if !strings.Contains(goMod, module+" "+target) {
			t.Errorf("missing normalized module requirement %q in go.mod:\n%s", module+" "+target, goMod)
		}
	}
	if strings.Contains(goMod, malformedTarget) {
		t.Errorf("malformed target prerelease survived in go.mod:\n%s", goMod)
	}
	if strings.Contains(goMod, "exclude go.putnami.dev/protocol/capabilities "+target) {
		t.Errorf("self-excluding target release survived in go.mod:\n%s", goMod)
	}

	// The resulting module must also settle outside workspace mode, where the
	// workspace's replace directives cannot mask a bad go.mod requirement.
	tidy := exec.Command(jobtest.RequireGo(t), "mod", "tidy")
	tidy.Dir = filepath.Join(ws, "service")
	tidy.Env = jobtest.Env(t, proxy, cacheRoot, "GOWORK=off", "GOMODCACHE="+filepath.Join(cacheRoot, "mod"),
		"GOCACHE="+filepath.Join(cacheRoot, "build"))
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("module-mode go mod tidy failed after upgrade: %v\n%s", err, out)
	}
}

// A standalone Go project the workspace declares only through includes is
// upgraded like one the legacy "projects" member names: `go get` moves its
// go.putnami.dev requirement to the release.
func TestDepsUpgrade_UpgradesAStandaloneProjectDeclaredThroughIncludes(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "included-members-are-go-projects", "included-standalone-members-are-upgraded")
	t.Parallel()
	const previous = "v0.1.0-aaaa1111"
	const target = "v0.1.0-bbbb4444"
	proxy := jobtest.FileURL(jobtest.WriteModuleProxy(t, append(
		jobtest.SimpleProxyModules(previous, "go.putnami.dev/app"),
		jobtest.SimpleProxyModules(target, "go.putnami.dev/app")...)...))

	ws := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, ws, "putnami.workspace.json", `{"includes":["service"]}`)
	jobtest.WriteFile(t, ws, "service/putnami.json", `{"publish":["go"]}`)
	jobtest.WriteFile(t, ws, "go.work", "go 1.22\n\nuse ./service\n")
	jobtest.WriteFile(t, ws, "service/go.mod", "module example.com/service\n\ngo 1.22\n\nrequire go.putnami.dev/app "+previous+"\n")
	jobtest.WriteFile(t, ws, "service/service.go", "package service\n\nimport _ \"go.putnami.dev/app\"\n")

	r := runUpgrade(t, ws, jobtest.CacheRoot(t), `{"putnamiVersion":"0.1.0-bbbb4444"}`, proxy, "service")
	r.requireStatus(t, statusOK)
	r.requireLogs(t, "Found 1 Go project(s)", "Upgrading go.putnami.dev/app in service to "+target)
	if goMod := jobtest.ReadFile(t, filepath.Join(ws, "service", "go.mod")); !strings.Contains(goMod, "go.putnami.dev/app "+target) {
		t.Errorf("service/go.mod does not require go.putnami.dev/app %s:\n%s", target, goMod)
	}
}

// writeMemberRequireWorkspace lays out a workspace exercising the boundary
// between workspace-locked and independently resolvable member metadata:
//
//   - service (a declared project) publishes Go modules and is therefore an
//     independently resolvable boundary: it requires go.putnami.dev/http
//     (direct) and go.putnami.dev/logger (// indirect) at the v0.0.0
//     placeholder, plus a go.example.com/private/identity v0.0.0 require
//     that the go.putnami.dev/ prefix filter must never touch.
//   - tool opts into options["@putnami/go"].standalone and therefore receives
//     the same exact member metadata without claiming a Go publish channel.
//   - application requires a published framework module but has no standalone
//     contract. Its go.mod and go.sum must stay byte-for-byte on their authored
//     workspace baseline; go.work is its release lock.
//   - internal requires two go.putnami.dev modules that are LOCAL to the
//     workspace: go.putnami.dev/inject (use'd) and go.putnami.dev/database
//     (replaced by a local directory). Those stay at v0.0.0 because go.work
//     resolves them from the file system.
//   - forked requires go.putnami.dev/cache but overrides it with a
//     member-local, version-qualified directory replace. go.work knows nothing
//     about this fork, so the require must stay at v0.0.0: bumping it would
//     orphan the replace and send a GOWORK=off build to the proxy.
func writeMemberRequireWorkspace(t *testing.T) string {
	t.Helper()
	ws := jobtest.RealTempDir(t)
	write := func(rel, content string) { jobtest.WriteFile(t, ws, rel, content) }

	write("putnami.workspace.json", `{"projects":["service","tool","application","internal","forked"]}`)

	write("service/putnami.json", `{"publish":["go"]}`)
	write("service/go.mod", "module example.com/service\n\ngo 1.22\n\nrequire (\n"+
		"\tgo.putnami.dev/http v0.0.0\n"+
		"\tgo.example.com/private/identity v0.0.0\n"+
		"\tgo.putnami.dev/logger v0.0.0 // indirect\n"+
		")\n")
	write("service/service.go", "package service\n\nimport (\n"+
		"\t_ \"go.example.com/private/identity\"\n"+
		"\t_ \"go.putnami.dev/http\"\n"+
		")\n")

	write("tool/putnami.json", `{"options":{"@putnami/go":{"standalone":true}}}`)
	write("tool/go.mod", "module example.com/tool\n\ngo 1.22\n\nrequire go.putnami.dev/protocol/cache v0.0.0\n")
	write("tool/tool.go", "package tool\n\nimport _ \"go.putnami.dev/protocol/cache\"\n")

	write("application/go.mod", "module example.com/application\n\ngo 1.22\n\nrequire go.putnami.dev/http v0.0.0\n")
	write("application/application.go", "package application\n\nimport _ \"go.putnami.dev/http\"\n")

	write("internal/go.mod", "module example.com/internal\n\ngo 1.22\n\nrequire (\n"+
		"\tgo.putnami.dev/inject v0.0.0\n"+
		"\tgo.putnami.dev/database v0.0.0\n"+
		")\n")
	write("internal/internal.go", "package internal\n\nimport (\n\t_ \"go.putnami.dev/inject\"\n\t_ \"go.putnami.dev/database\"\n)\n")

	write("local/inject/go.mod", "module go.putnami.dev/inject\n\ngo 1.22\n")
	write("local/inject/inject.go", "package inject\n")
	write("local/database-fork/go.mod", "module go.putnami.dev/database\n\ngo 1.22\n")
	write("local/database-fork/database.go", "package database\n")

	write("forked/go.mod", "module example.com/forked\n\ngo 1.22\n\n"+
		"require go.putnami.dev/cache v0.0.0\n\n"+
		"replace go.putnami.dev/cache v0.0.0 => ../local/cache-fork\n")
	write("forked/forked.go", "package forked\n\nimport _ \"go.putnami.dev/cache\"\n")
	write("local/cache-fork/go.mod", "module go.putnami.dev/cache\n\ngo 1.22\n")
	write("local/cache-fork/cache.go", "package cache\n")

	write("go.work", "go 1.22\n\n"+
		"use (\n\t./application\n\t./forked\n\t./internal\n\t./local/inject\n\t./service\n\t./tool\n)\n\n"+
		"replace go.putnami.dev/database => ./local/database-fork\n")
	return ws
}

func writeMemberRequireProxy(t *testing.T, target string) string {
	t.Helper()
	modules := jobtest.SimpleProxyModules(target, "go.putnami.dev/cache", "go.putnami.dev/logger", "go.putnami.dev/protocol/cache")
	modules = append(modules, jobtest.ProxyModule{
		Path:    "go.putnami.dev/http",
		Version: target,
		GoMod:   "module go.putnami.dev/http\n\ngo 1.22\n\nrequire go.putnami.dev/logger " + target + "\n",
		Source:  "package http\n\nimport _ \"go.putnami.dev/logger\"\n",
	})
	modules = append(modules, jobtest.SimpleProxyModules("v0.0.0",
		"go.example.com/private/identity",
		"go.putnami.dev/protocol/cache",
		"go.putnami.dev/database",
		"go.putnami.dev/http",
		"go.putnami.dev/inject",
		"go.putnami.dev/logger",
	)...)
	return jobtest.FileURL(jobtest.WriteModuleProxy(t, modules...))
}

// placeholderRequire matches a go.putnami.dev requirement still at the
// v0.0.0 placeholder.
var placeholderRequire = regexp.MustCompile(`go\.putnami\.dev/.* v0\.0\.0`)

// TestDepsUpgrade_PropagatesMemberRequireVersions pins the core
// version-propagation invariant: declared standalone go.putnami.dev/*
// requires move off the v0.0.0 placeholder onto the resolved version (so
// go.work-less resolution never asks the private proxy for @v0.0.0), while
// workspace-only, private, and local modules are left on their authored
// baselines.
func TestDepsUpgrade_PropagatesMemberRequireVersions(t *testing.T) {
	t.Parallel()
	const target = "v0.1.0-dddd4444"
	ws := writeMemberRequireWorkspace(t)
	service := filepath.Join(ws, "service", "go.mod")
	tool := filepath.Join(ws, "tool", "go.mod")
	application := filepath.Join(ws, "application", "go.mod")
	internal := filepath.Join(ws, "internal", "go.mod")
	forked := filepath.Join(ws, "forked", "go.mod")
	applicationBefore := fileStates(t, application, filepath.Join(ws, "application", "go.sum"))
	proxy := writeMemberRequireProxy(t, target)
	cacheRoot := jobtest.CacheRoot(t)

	r := runUpgrade(t, ws, cacheRoot, `{"putnamiVersion":"0.1.0-dddd4444"}`, proxy)
	r.requireStatus(t, statusOK)

	// Published requires moved onto the resolved version; the direct require and
	// the // indirect require are both rewritten and the marker is preserved.
	serviceMod := jobtest.ReadFile(t, service)
	for _, want := range []string{
		"go.putnami.dev/http " + target,
		"go.putnami.dev/logger " + target + " // indirect",
		// Private modules are outside the go.putnami.dev/ prefix and never move.
		"go.example.com/private/identity v0.0.0",
	} {
		if !strings.Contains(serviceMod, want) {
			t.Errorf("service go.mod lacks %q:\n%s", want, serviceMod)
		}
	}
	if leftover := placeholderRequire.FindAllString(serviceMod, -1); len(leftover) != 0 {
		t.Errorf("published go.putnami.dev require left at v0.0.0 in consumer go.mod: %v\n%s", leftover, serviceMod)
	}
	// Explicit standalone opt-in has the same exact member-metadata contract as
	// Go publication without pretending that this application is a module feed.
	if toolMod := jobtest.ReadFile(t, tool); !strings.Contains(toolMod, "go.putnami.dev/protocol/cache "+target) {
		t.Errorf("standalone tool requirement not pinned to %s:\n%s", target, toolMod)
	}
	// Workspace-only modules use the root lock. Upgrade must not turn their
	// authored manifests into duplicate generated lockfiles.
	requireFileStates(t, applicationBefore)

	// Local workspace go.putnami.dev modules resolve from the file system via
	// go.work, so their requires stay at v0.0.0.
	internalMod := jobtest.ReadFile(t, internal)
	for _, want := range []string{"go.putnami.dev/inject v0.0.0", "go.putnami.dev/database v0.0.0"} {
		if !strings.Contains(internalMod, want) {
			t.Errorf("local require %q must stay at v0.0.0:\n%s", want, internalMod)
		}
	}
	// A member go.mod that forks a published module with its own
	// version-qualified directory replace keeps its require at v0.0.0.
	forkedMod := jobtest.ReadFile(t, forked)
	if !strings.Contains(forkedMod, "go.putnami.dev/cache v0.0.0 => ../local/cache-fork") ||
		!strings.Contains(forkedMod, "require go.putnami.dev/cache v0.0.0") {
		t.Errorf("member-local directory replace must remain matched by its require:\n%s", forkedMod)
	}

	// The rewrite is observable and only touches the published modules.
	if !r.rec.Contains("Pinned require go.putnami.dev/http "+target) &&
		!r.rec.Contains("Upgrading go.putnami.dev/http in service to "+target) {
		t.Errorf("expected update log for go.putnami.dev/http:\n%s", r.rec.Transcript())
	}
	if !r.rec.Contains("Pinned require go.putnami.dev/logger "+target) &&
		!r.rec.Contains("Upgrading go.putnami.dev/logger in service to "+target) {
		t.Errorf("expected update log for go.putnami.dev/logger:\n%s", r.rec.Transcript())
	}
	r.forbidLogs(t,
		"Pinned require go.putnami.dev/inject",
		"Pinned require go.putnami.dev/database",
		"Pinned require go.putnami.dev/cache",
		"go.example.com/private/identity",
	)
	r.requireLogs(t, "Keeping "+application+" and go.sum on their workspace baseline")

	// A second run is a byte-for-byte no-op on the member go.mod files.
	second := runUpgrade(t, ws, cacheRoot, `{"putnamiVersion":"0.1.0-dddd4444"}`, proxy)
	second.requireStatus(t, statusOK)
	for path, first := range map[string]string{service: serviceMod, internal: internalMod, forked: forkedMod} {
		if got := jobtest.ReadFile(t, path); got != first {
			t.Errorf("require propagation is not idempotent for %s.\nfirst:\n%s\nsecond:\n%s", path, first, got)
		}
	}
	second.forbidLogs(t, "Pinned require go.putnami.dev/http")
	requireFileStates(t, applicationBefore)
}

// The dry-run path reports the would-be member require rewrites and changes
// nothing on disk.
func TestDepsUpgrade_DryRunPropagatesMemberRequireVersions(t *testing.T) {
	t.Parallel()
	const target = "v0.1.0-eeee5555"
	ws := writeMemberRequireWorkspace(t)
	tool := filepath.Join(ws, "tool", "go.mod")
	application := filepath.Join(ws, "application", "go.mod")
	members := []string{"service", "tool", "application", "internal", "forked"}
	paths := make([]string, 0, 2*len(members))
	for _, member := range members {
		paths = append(paths, filepath.Join(ws, member, "go.mod"), filepath.Join(ws, member, "go.sum"))
	}
	before := fileStates(t, append(paths, filepath.Join(ws, "go.work"), filepath.Join(ws, "go.work.sum"))...)

	r := runUpgrade(t, ws, jobtest.CacheRoot(t), `{"putnamiVersion":"0.1.0-eeee5555","dryRun":true}`, "file:///nonexistent-proxy")
	r.requireStatus(t, statusOK)
	r.requireLogs(t,
		"Would pin require go.putnami.dev/http "+target,
		"Would pin require go.putnami.dev/logger "+target,
		"Would pin require go.putnami.dev/protocol/cache "+target+" in "+tool,
		"Would keep "+application+" on its workspace baseline",
	)
	r.forbidLogs(t,
		"Would pin require go.putnami.dev/http "+target+" in "+application,
		"Would pin require go.putnami.dev/inject",
		"Would pin require go.putnami.dev/database",
		"Would pin require go.putnami.dev/cache",
	)
	requireFileStates(t, before)
}

// writeNoGoProjectsWorkspace reshapes the member-require workspace into the
// shape that regressed require propagation: the workspace declares only a
// non-Go project, so the job scans zero Go projects, while go.work still uses
// every member module holding v0.0.0 placeholders.
func writeNoGoProjectsWorkspace(t *testing.T) string {
	t.Helper()
	ws := writeMemberRequireWorkspace(t)
	jobtest.WriteFile(t, ws, "putnami.workspace.json", `{"projects":["docs"]}`)
	jobtest.WriteFile(t, ws, "docs/package.json", `{"name":"docs"}`)
	return ws
}

// Require propagation is workspace-wide. The go.work members carry the v0.0.0
// placeholders regardless of which project's job drives the upgrade, so an
// empty project scan must not skip the rewrite.
func TestDepsUpgrade_PropagatesWithoutDeclaredGoProjects(t *testing.T) {
	t.Parallel()
	const target = "v0.1.0-ffff6666"
	ws := writeNoGoProjectsWorkspace(t)
	// The standalone tool also requires a module only go.work supplies, so its
	// GOWORK=off graph cannot resolve and its checksums are left alone.
	jobtest.WriteFile(t, ws, "tool/go.mod", "module example.com/tool\n\ngo 1.22\n\nrequire (\n"+
		"\tgo.putnami.dev/inject v0.0.0\n\tgo.putnami.dev/protocol/cache v0.0.0\n)\n")
	service := filepath.Join(ws, "service", "go.mod")
	internal := filepath.Join(ws, "internal", "go.mod")
	forked := filepath.Join(ws, "forked", "go.mod")
	beforeService := jobtest.ReadFile(t, service)
	cacheRoot := jobtest.CacheRoot(t)

	// Dry run first: the same branch must report the would-be rewrites.
	dry := runUpgrade(t, ws, cacheRoot, `{"putnamiVersion":"0.1.0-ffff6666","dryRun":true}`, "file:///nonexistent-proxy")
	dry.requireStatus(t, statusOK)
	dry.requireLogs(t,
		"No Go projects found",
		"No Go projects found; updating root go.work replaces",
		"Would pin require go.putnami.dev/http "+target,
		"Dry run complete; no Go files were changed",
	)
	if after := jobtest.ReadFile(t, service); after != beforeService {
		t.Errorf("dry run modified service go.mod.\nbefore:\n%s\nafter:\n%s", beforeService, after)
	}

	r := runUpgrade(t, ws, cacheRoot, `{"putnamiVersion":"0.1.0-ffff6666"}`, writeMemberRequireProxy(t, target))
	r.requireStatus(t, statusOK)
	// The go.mod paths keep the spelling go.work gives them, as the script's did.
	sep := string(filepath.Separator)
	r.requireLogs(t,
		"No Go projects found",
		"Pinned require go.putnami.dev/protocol/cache "+target+" in "+ws+sep+"."+sep+"tool"+sep+"go.mod",
		"Skipping standalone checksum reconciliation for "+ws+sep+"."+sep+"tool"+sep+"go.mod (requires workspace-local modules)",
		"Reconciled standalone checksums for "+ws+sep+"."+sep+"service"+sep+"go.mod",
	)
	r.forbidLogs(t, "Pinned require go.putnami.dev/inject")
	if _, exists := jobtest.ReadOptional(t, filepath.Join(ws, "tool", "go.sum")); exists {
		t.Errorf("the checksums of a member that needs go.work were reconciled without it")
	}
	if workSum := jobtest.ReadFile(t, filepath.Join(ws, "go.work.sum")); !strings.Contains(workSum, target) {
		t.Errorf("go.work.sum does not contain target %s:\n%s", target, workSum)
	}
	if serviceSum := jobtest.ReadFile(t, filepath.Join(ws, "service", "go.sum")); !strings.Contains(serviceSum, target) {
		t.Errorf("service/go.sum does not contain target %s:\n%s", target, serviceSum)
	}

	serviceMod := jobtest.ReadFile(t, service)
	for _, want := range []string{
		"go.putnami.dev/http " + target,
		"go.putnami.dev/logger " + target + " // indirect",
		"go.example.com/private/identity v0.0.0",
	} {
		if !strings.Contains(serviceMod, want) {
			t.Errorf("service go.mod lacks %q:\n%s", want, serviceMod)
		}
	}
	if leftover := placeholderRequire.FindAllString(serviceMod, -1); len(leftover) != 0 {
		t.Errorf("published go.putnami.dev require left at v0.0.0 in consumer go.mod: %v\n%s", leftover, serviceMod)
	}
	// The local-module skips still apply on this branch.
	internalMod := jobtest.ReadFile(t, internal)
	for _, want := range []string{"go.putnami.dev/inject v0.0.0", "go.putnami.dev/database v0.0.0"} {
		if !strings.Contains(internalMod, want) {
			t.Errorf("local require %q must stay at v0.0.0:\n%s", want, internalMod)
		}
	}
	if forkedMod := jobtest.ReadFile(t, forked); !strings.Contains(forkedMod, "go.putnami.dev/cache v0.0.0 => ../local/cache-fork") {
		t.Errorf("member-local directory replace must remain matched by its require:\n%s", forkedMod)
	}
}

// Impacted publication ended the one-version-for-everything assumption: a
// release advances the channel projection of every member of the set, but each
// member keeps the version of the publication that last CHANGED it. So two
// modules on one channel legitimately differ, and each must be pinned to what
// its own `@v/<channel>.info` answers.
//
// Probing one module and spreading its answer would pin every other module to a
// version its channel does not point at: a graph that was never published
// together, and one the origin may not even serve.
func TestDepsUpgrade_ChannelResolvesEachModuleIndependently(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/go-project-toolchain", "per-module-channel-resolution", "modules-pin-independently")
	ws := sandboxWorkspace(t)
	const inherited = "v0.1.0-aaaa1111"
	const moved = "v0.1.0-bbbb2222"

	proxyDir := t.TempDir()
	writeChannelProxy(t, proxyDir, "canary", inherited)
	// Only http changed in the last publication, so only its projection moved.
	jobtest.WriteModuleProxyAt(t, proxyDir, jobtest.SimpleProxyModules(moved, "go.putnami.dev/http")...)
	jobtest.WriteChannelInfo(t, proxyDir, "go.putnami.dev/http", "canary", moved)
	server := httptest.NewServer(http.FileServer(http.Dir(proxyDir)))
	t.Cleanup(server.Close)

	r := runUpgrade(t, ws, jobtest.CacheRoot(t), `{"putnamiChannel":"canary"}`, server.URL)
	r.requireStatus(t, statusOK)
	work := readGoWork(t, ws)
	for _, want := range []string{
		"go.putnami.dev/http => go.putnami.dev/http " + moved,
		"go.putnami.dev/api => go.putnami.dev/api " + inherited,
		"go.putnami.dev/protocol/cache => go.putnami.dev/protocol/cache " + inherited,
	} {
		if !strings.Contains(work, want) {
			t.Errorf("go.work does not pin %q:\n%s", want, work)
		}
	}
	// A mixed channel prints one row per module, each with its own revision:
	// the collapsed single-row form would hide exactly the difference an
	// operator is reading the table for.
	r.requireLogs(t,
		"go.putnami.dev/http  "+moved+"  revision bbbb2222",
		"go.putnami.dev/api  "+inherited+"  revision aaaa1111",
	)
	r.forbidLogs(t, "go.putnami.dev/*  ")
}

// The origin a channel is resolved against comes from the workspace
// `registries` section the job context carries, not from a built-in host.
// PUTNAMI_GO_MODULE_PROXY and GO_REGISTRY_URL stay overrides on top of it,
// which is what every other case in this file uses. The declared proxy chain
// replaces the default GOPROXY without the origin at its head.
func TestDepsUpgrade_OriginComesFromTheRegistriesParam(t *testing.T) {
	t.Parallel()
	ws := sandboxWorkspace(t)
	const target = "v0.1.0-cccc3333"
	proxyDir := t.TempDir()
	server := httptest.NewServer(http.FileServer(http.Dir(proxyDir)))
	t.Cleanup(server.Close)
	// A channel name is data, not a built-in: this case follows "next" while
	// every other one follows "canary".
	writeChannelProxy(t, proxyDir, "next", target)

	params := `{"putnamiChannel":"next","dryRun":true,"registries":{"go":{"origin":` + strconv.Quote(server.URL+"/") +
		`,"proxy":["` + server.URL + `/","https://mirror.example.test","direct"]}}}`
	before := readGoWork(t, ws)
	// No PUTNAMI_GO_MODULE_PROXY, no GO_REGISTRY_URL and no GOPROXY: the
	// declared entry is the only source of the origin and of the chain.
	r := runUpgrade(t, ws, jobtest.CacheRoot(t), params, "")
	r.requireStatus(t, statusOK)
	r.requireLogs(t, "go.putnami.dev/*  "+target+"  revision cccc3333")
	if after := readGoWork(t, ws); after != before {
		t.Errorf("dry run rewrote go.work:\n%s", after)
	}
	host := strings.TrimPrefix(server.URL, "http://")
	if got, want := r.job.Env.Get("GOPROXY"), "https://mirror.example.test,direct"; got != want {
		t.Errorf("GOPROXY = %q, want the declared chain without the origin %q", got, want)
	}
	if got := r.job.Env.Get("GONOPROXY"); got != host+"/*" {
		t.Errorf("GONOPROXY = %q, want the origin routed around the proxies", got)
	}
}

// A caller that named its own GOPROXY keeps it: only the default gives way to
// the declared chain.
func TestDepsUpgrade_ACallerProxyWinsOverTheDeclaredChain(t *testing.T) {
	t.Parallel()
	ws := sandboxWorkspace(t)
	params := `{"putnamiVersion":"0.1.0-cccc3333","dryRun":true,"registries":{"go":{"proxy":["https://mirror.example.test"]}}}`
	r := runUpgrade(t, ws, jobtest.CacheRoot(t), params, "file:///caller-proxy")
	r.requireStatus(t, statusOK)
	if got := r.job.Env.Get("GOPROXY"); got != "file:///caller-proxy" {
		t.Errorf("GOPROXY = %q, want the caller's", got)
	}
}

func TestDepsUpgrade_MalformedReleaseSetFailsBeforeGo(t *testing.T) {
	t.Parallel()
	ws := sandboxWorkspace(t)
	before := readGoWork(t, ws)
	r := runUpgrade(t, ws, jobtest.CacheRoot(t), `{"releaseSet":{"releaseSet":{"members":null}}}`, "")
	r.requireStatus(t, statusFailed)
	events := r.rec.Events()
	if len(events) != 1 || events[0].Kind != "diagnostic" || events[0].Message != problemInvalidMap ||
		events[0].File != filepath.Join(ws, "context.json") {
		t.Fatalf("events = %v, want only the invalid-map diagnostic naming the context file", events)
	}
	if after := readGoWork(t, ws); after != before {
		t.Errorf("a malformed release set changed go.work")
	}
}

// A SIGTERM or a SIGINT during the upgrade rolls back every surface the
// transaction snapshotted, then ends the job with the status a shell trap
// gave: 143 and 130. The job runs in a child process whose go command blocks
// in the checksum phase, after go.work was already rewritten.
func TestDepsUpgrade_SignalRollsBackThenExitsWithTheShellStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a process cannot be sent SIGTERM or SIGINT on Windows")
	}
	t.Parallel()
	for _, tc := range []struct {
		signal syscall.Signal
		code   int
	}{
		{syscall.SIGTERM, 143},
		{syscall.SIGINT, 130},
	} {
		t.Run(tc.signal.String(), func(t *testing.T) {
			t.Parallel()
			ws := sandboxWorkspace(t)
			before := fileStates(t,
				filepath.Join(ws, "go.work"),
				filepath.Join(ws, "go.work.sum"),
				filepath.Join(ws, "local", "mylib", "go.mod"),
				filepath.Join(ws, "local", "mylib", "go.sum"),
			)
			realGo := jobtest.RequireGo(t)
			fakes := jobtest.NewFakes(t)
			ready := filepath.Join(t.TempDir(), "ready")
			fakes.Go(t, realGo, "mod download all", ready)
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(self)
			cmd.Env = jobtest.Env(t, "file:///nonexistent-proxy", jobtest.CacheRoot(t), append(fakes.Environ(),
				"PATH="+fakes.Dir,
				"PUTNAMI_GO_MODULE_PROXY=",
				helperWorkspace+"="+ws,
				helperParams+"="+`{"putnamiVersion":"0.1.0-bbbb3333"}`,
			)...)
			var output strings.Builder
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()

			deadline := time.After(2 * time.Minute)
			for waiting := true; waiting; {
				select {
				case err := <-done:
					t.Fatalf("the job ended before its checksum phase: %v\n%s", err, output.String())
				case <-deadline:
					_ = cmd.Process.Kill()
					t.Fatalf("the job never reached its checksum phase:\n%s", output.String())
				case <-time.After(20 * time.Millisecond):
					_, waiting = jobtest.ReadOptional(t, ready)
					waiting = !waiting
				}
			}
			if work := readGoWork(t, ws); !strings.Contains(work, "go.putnami.dev/api => go.putnami.dev/api v0.1.0-bbbb3333") {
				t.Fatalf("go.work was not rewritten before the checksum phase, so a rollback proves nothing:\n%s", work)
			}

			if err := cmd.Process.Signal(tc.signal); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
			case <-time.After(time.Minute):
				_ = cmd.Process.Kill()
				t.Fatalf("the job did not end after %s:\n%s", tc.signal, output.String())
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != tc.code {
				t.Fatalf("exit = %v, want status %d:\n%s", err, tc.code, output.String())
			}
			requireFileStates(t, before)
			if staged, _ := filepath.Glob(filepath.Join(ws, "go.work.putnami-upgrade.*")); len(staged) != 0 {
				t.Errorf("staged go.work copies were left behind: %v", staged)
			}
			if invocations := fakes.Invocations(t); len(invocations) != 0 {
				t.Errorf("deps-upgrade started shell programs: %v", invocations)
			}
		})
	}
}

func TestParseReleaseSet(t *testing.T) {
	const ref = `"ref":{"id":"rs_1","digest":"sha256:d"}`
	for _, tc := range []struct {
		name     string
		document string
		want     map[string]string
		problem  string
	}{
		{"array members", `{` + ref + `,"releaseSet":{"members":[` +
			`{"ecosystem":"go","coordinate":"go.putnami.dev/a","version":"v1.0.0"},` +
			`null,` +
			`{"ecosystem":"npm","coordinate":"@putnami/a","version":"1.0.0"},` +
			`{"ecosystem":7}]}}`,
			map[string]string{"go.putnami.dev/a": "v1.0.0"}, ""},
		{"object members", `{` + ref + `,"releaseSet":{"members":{"x":{"ecosystem":"go","coordinate":"go.putnami.dev/a","version":"v1.0.0"}}}}`,
			map[string]string{"go.putnami.dev/a": "v1.0.0"}, ""},
		{"no go member", `{` + ref + `,"releaseSet":{"members":[]}}`, map[string]string{}, ""},
		{"not an object", `[]`, nil, problemInvalidMap},
		{"no release set", `{` + ref + `}`, nil, problemInvalidMap},
		{"null members", `{` + ref + `,"releaseSet":{"members":null}}`, nil, problemInvalidMap},
		{"scalar members", `{` + ref + `,"releaseSet":{"members":"x"}}`, nil, problemInvalidMap},
		{"scalar member", `{` + ref + `,"releaseSet":{"members":[1]}}`, nil, problemInvalidMap},
		{"numeric version", `{` + ref + `,"releaseSet":{"members":[{"ecosystem":"go","coordinate":"go.putnami.dev/a","version":1}]}}`,
			nil, problemProjection},
		{"duplicate coordinate", `{` + ref + `,"releaseSet":{"members":[` +
			`{"ecosystem":"go","coordinate":"go.putnami.dev/a","version":"v1.0.0"},` +
			`{"ecosystem":"go","coordinate":"go.putnami.dev/a","version":"v2.0.0"}]}}`, nil, problemProjection},
		{"no ref", `{"releaseSet":{"members":[]}}`, nil, problemMissingRefID},
		{"numeric digest", `{"ref":{"id":"rs_1","digest":1},"releaseSet":{"members":[]}}`, nil, problemMissingRefID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, problem := ParseReleaseSet(json.RawMessage(tc.document))
			if problem != tc.problem {
				t.Fatalf("problem = %q, want %q", problem, tc.problem)
			}
			if tc.problem != "" {
				if set != nil {
					t.Fatalf("a refused set was returned: %+v", set)
				}
				return
			}
			if set.ID != "rs_1" || set.Digest != "sha256:d" {
				t.Errorf("ref = %s %s", set.ID, set.Digest)
			}
			if len(set.Versions) != len(tc.want) {
				t.Fatalf("versions = %v, want %v", set.Versions, tc.want)
			}
			for coordinate, version := range tc.want {
				if set.Versions[coordinate] != version {
					t.Errorf("versions = %v, want %v", set.Versions, tc.want)
				}
			}
		})
	}
}

// optionsJob is a job for the argument parsing cases, with environ.
func optionsJob(t *testing.T, environ ...string) (*workspacejob.Job, *jobtest.Recorder) {
	t.Helper()
	rec := &jobtest.Recorder{}
	j, _, _ := jobtest.NewJob(t, rec, environ, t.TempDir())
	return j, rec
}

func TestParseOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		params  string
		environ []string
		want    Options
	}{
		{"default", nil, `{}`, nil, Options{Version: "latest"}},
		{"semver gains its v", []string{"--version", "1.2.3"}, `{}`, nil, Options{Version: "v1.2.3"}},
		{"channel flag", []string{"--channel", "canary"}, `{}`, nil, Options{Version: "canary"}},
		{"positional", []string{"v2.0.0-rc.1"}, `{}`, nil, Options{Version: "v2.0.0-rc.1"}},
		{"first positional wins", []string{"a", "b"}, `{}`, nil, Options{Version: "a"}},
		{"flag without a value falls back", []string{"--version"}, `{"putnamiVersion":"0.3.0"}`, nil, Options{Version: "v0.3.0"}},
		{"stable is latest", nil, `{"putnamiChannel":"stable"}`, nil, Options{Version: "latest"}},
		{"null is latest", nil, `{"putnamiVersion":null}`, nil, Options{Version: "latest"}},
		{"version param before channel", nil, `{"putnami-channel":"canary","putnami-version":"1.0.0"}`, nil, Options{Version: "v1.0.0"}},
		{"argument over param", []string{"--channel", "next"}, `{"putnamiVersion":"1.0.0"}`, nil, Options{Version: "next"}},
		{"dry-run flag", []string{"--dry-run"}, `{"dryRun":false}`, nil, Options{Version: "latest", DryRun: true}},
		{"dry-run param", nil, `{"dry-run":"true"}`, nil, Options{Version: "latest", DryRun: true}},
		{"dry-run param false", nil, `{"dryRun":"yes"}`, nil, Options{Version: "latest"}},
		{"declared origin", nil, `{"registries":{"go":{"origin":"https://go.example.test/"}}}`, nil,
			Options{Version: "latest", Origin: "https://go.example.test"}},
		{"null origin", nil, `{"registries":{"go":{"origin":null}}}`, nil, Options{Version: "latest"}},
		{"registry URL over declared", nil, `{"registries":{"go":{"origin":"https://a.test"}}}`,
			[]string{"GO_REGISTRY_URL=https://b.test/"}, Options{Version: "latest", Origin: "https://b.test"}},
		{"module proxy over all", nil, `{"registries":{"go":{"origin":"https://a.test"}}}`,
			[]string{"GO_REGISTRY_URL=https://b.test", "PUTNAMI_GO_MODULE_PROXY=file:///c"}, Options{Version: "latest", Origin: "file:///c"}},
		{"declared chain", nil, `{"registries":{"go":{"origin":"https://a.test","proxy":["https://a.test/mod","https://m.test","direct"]}}}`, nil,
			Options{Version: "latest", Origin: "https://a.test", Proxy: "https://m.test,direct"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j, rec := optionsJob(t, tc.environ...)
			got, ok := ParseOptions(j, tc.args, jobtest.Params(t, tc.params), "ctx.json")
			if !ok {
				t.Fatalf("ParseOptions refused:\n%s", rec.Transcript())
			}
			tc.want.ContextFile = "ctx.json"
			if got != tc.want {
				t.Errorf("ParseOptions = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDeclaredProxy(t *testing.T) {
	for _, tc := range []struct {
		proxy, host, want string
		ok                bool
	}{
		{`["https://a.test","direct"]`, "", "https://a.test,direct", true},
		{`["https://a.test/path","http://b.test","off"]`, "a.test", "http://b.test,off", true},
		{`["http://b.test/x"]`, "b.test", "", true},
		{`["",3,null,"direct"]`, "a.test", ",direct", true},
		{`{"x":"https://a.test","y":"direct"}`, "a.test", "direct", true},
		{`null`, "", "", true},
		{`false`, "", "", true},
		{`"https://a.test"`, "", "", false},
		{`3`, "", "", false},
		{`true`, "", "", false},
	} {
		params := jobtest.Params(t, `{"registries":{"go":{"proxy":`+tc.proxy+`}}}`)
		if got, ok := declaredProxy(params, tc.host); got != tc.want || ok != tc.ok {
			t.Errorf("declaredProxy(%s, %q) = (%q, %v), want (%q, %v)", tc.proxy, tc.host, got, ok, tc.want, tc.ok)
		}
	}
	if got, ok := declaredProxy(map[string]json.RawMessage{}, ""); got != "" || !ok {
		t.Errorf("declaredProxy(no registries) = (%q, %v)", got, ok)
	}
}

// A declared proxy chain that has no entries to read fails the job before it
// resolves anything, as the script's jq did, with a diagnostic on the context
// file instead of jq's exit status.
func TestParseOptionsRefusesAProxyThatIsNotAList(t *testing.T) {
	j, rec := optionsJob(t)
	if _, ok := ParseOptions(j, nil, jobtest.Params(t, `{"registries":{"go":{"proxy":"https://m.test"}}}`), "ctx.json"); ok {
		t.Fatalf("ParseOptions accepted a string proxy:\n%s", rec.Transcript())
	}
	if !strings.Contains(rec.Transcript(), "diagnostic error: registries.go.proxy is neither an array nor an object of proxy URLs [ctx.json]") {
		t.Errorf("missing the proxy diagnostic on the context file:\n%s", rec.Transcript())
	}
}

// A go command that cannot name its module cache fails the upgrade, as the
// script's `go env GOMODCACHE` did under `set -e`, with its error on the
// stream.
func TestWarnEmptyModuleFailsWhenGoCannotNameTheModuleCache(t *testing.T) {
	j, rec := optionsJob(t, brokenGo+"=1")
	j.GoBinary = os.Args[0]
	u := &upgrade{Job: j}
	if u.warnEmptyModule("go.putnami.dev/api", "v1.0.0") {
		t.Fatalf("warnEmptyModule went on without a module cache:\n%s", rec.Transcript())
	}
	if want := "Cannot read GOMODCACHE from " + os.Args[0] + ": go: broken toolchain"; !rec.Contains(want) {
		t.Errorf("missing %q:\n%s", want, rec.Transcript())
	}
}

// A go.work the job cannot create fails the upgrade instead of leaving the
// workspace unpinned behind a success.
func TestEnsureGoWorkFailsWhenItCannotWriteGoWork(t *testing.T) {
	j, rec := optionsJob(t, brokenGo+"=1")
	j.GoBinary = os.Args[0]
	goWork := filepath.Join(t.TempDir(), "missing", "go.work")
	u := &upgrade{Job: j, goWork: goWork}
	if u.ensureGoWork() {
		t.Fatalf("ensureGoWork succeeded without writing %s:\n%s", goWork, rec.Transcript())
	}
	if !rec.Contains("Cannot create "+goWork) || rec.Contains("Created go.work") {
		t.Errorf("want the write failure and no creation log:\n%s", rec.Transcript())
	}
}

func TestContextFileArg(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, ""},
		{[]string{"--putnamiContext", "a.json"}, "a.json"},
		{[]string{"--putnamiContext", "a.json", "--x", "--putnamiContext", "b.json"}, "b.json"},
		{[]string{"--putnamiContext"}, ""},
		{[]string{"--putnamiContext", "a.json", "--putnamiContext"}, "a.json"},
	} {
		if got := ContextFileArg(tc.args); got != tc.want {
			t.Errorf("ContextFileArg(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestURLHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"https://go.example.test/x/@v/list":  "go.example.test",
		"http://127.0.0.1:8080":              "127.0.0.1:8080",
		"https://ci:token@go.example.test/x": "go.example.test",
		"file:///tmp/proxy/x":                "file:///tmp/proxy/x",
		"go.example.test/x":                  "go.example.test/x",
	} {
		if got := urlHost(in); got != want {
			t.Errorf("urlHost(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"go.putnami.dev/http":      "go.putnami.dev/http",
		"github.com/BurntSushi/To": "github.com/!burnt!sushi/!to",
	} {
		if got := escapeModulePath(in); got != want {
			t.Errorf("escapeModulePath(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"v0.1.0-20260902173000-cafe1234": "cafe1234",
		"v0.1.0-aaaa1111":                "aaaa1111",
		"v1.2.0":                         "-",
	} {
		if got := sourceRevision(in); got != want {
			t.Errorf("sourceRevision(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVersionField(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
		ok   bool
	}{
		{`{"Version":"v1.0.0","Time":"x"}`, "v1.0.0", true},
		{`{"Version":null}`, "", true},
		{`{"Version":false}`, "", true},
		{`{}`, "", true},
		{`null`, "", true},
		{``, "", true},
		{`{"Version":"v1"} {"Version":"v2"}`, "v1\nv2", true},
		{`{"Version":3}`, "3", true},
		{`[1]`, "", false},
		{`"v1.0.0"`, "", false},
		{`{"Version":`, "", false},
		{`<html>`, "", false},
	} {
		got, ok := versionField([]byte(tc.body))
		if got != tc.want || ok != tc.ok {
			t.Errorf("versionField(%q) = %q, %v; want %q, %v", tc.body, got, ok, tc.want, tc.ok)
		}
	}
}

// The fetch follows redirects as `curl -L` did; the status probe reports the
// first answer, as `curl -w '%{http_code}'` without -L did. Both send the
// job's User-Agent.
func TestFetcher(t *testing.T) {
	var (
		mu     sync.Mutex
		agents []string
	)
	record := func(r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		agents = append(agents, r.UserAgent())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		http.Redirect(w, r, "/target", http.StatusFound)
	})
	mux.HandleFunc("/target", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_, _ = w.Write([]byte(`{"Version":"v1.0.0"}`))
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	j, _ := optionsJob(t, "PUTNAMI_CLI_USER_AGENT=putnami-cli/test")
	f := newFetcher(j)
	body, err := f.fetch(server.URL + "/moved")
	if err != nil || string(body) != `{"Version":"v1.0.0"}` {
		t.Fatalf("fetch(moved) = %q, %v", body, err)
	}
	mu.Lock()
	if len(agents) != 2 || agents[0] != "putnami-cli/test" || agents[1] != "putnami-cli/test" {
		t.Errorf("User-Agent of the redirected fetch = %v", agents)
	}
	mu.Unlock()
	if _, err := f.fetch(server.URL + "/missing"); !errors.Is(err, errHTTPStatus) {
		t.Errorf("fetch(missing) error = %v, want an HTTP status error", err)
	}
	if _, err := f.fetch("ftp://example.test/x"); err == nil {
		t.Error("an unsupported scheme was fetched")
	}
	file := jobtest.WriteFile(t, t.TempDir(), "x/@latest", `{"Version":"v2.0.0"}`)
	if body, err := f.fetch(jobtest.FileURL(file)); err != nil || string(body) != `{"Version":"v2.0.0"}` {
		t.Errorf("fetch(file) = %q, %v", body, err)
	}
	for url, want := range map[string]string{
		server.URL + "/moved":            "302",
		server.URL + "/missing":          "404",
		server.URL + "/target":           "200",
		jobtest.FileURL(file):            "000",
		"http://127.0.0.1:1/unreachable": "000",
		"://bad":                         "000",
	} {
		if got := f.status(url); got != want {
			t.Errorf("status(%s) = %q, want %q", url, got, want)
		}
	}
}

// TestDepsUpgrade_RefusesOfflineDependencies proves an upgrade does not run on
// a hosted run: it would reach the network outside GOPROXY and rewrite the
// locked files.
func TestDepsUpgrade_RefusesOfflineDependencies(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"deps-upgrade-refuses-offline-dependencies")
	t.Parallel()
	ws := sandboxWorkspace(t)
	before := readGoWork(t, ws)
	fakes := jobtest.NewFakes(t)
	env := jobtest.Env(t, "off", jobtest.CacheRoot(t),
		append(fakes.Environ(), "PATH="+fakes.Dir, "PUTNAMI_OFFLINE_DEPENDENCIES=1")...)
	rec := &jobtest.Recorder{}
	j, _, _ := jobtest.NewJob(t, rec, env, ws)
	opts, ok := ParseOptions(j, nil, jobtest.Params(t, `{"putnamiVersion":"0.1.0-bbbb2222"}`), filepath.Join(ws, "context.json"))
	if !ok {
		t.Fatal("ParseOptions refused the parameters")
	}
	if status := run(j, opts, nil); status != statusFailed {
		t.Fatalf("status = %q, want %q", status, statusFailed)
	}
	if after := readGoWork(t, ws); after != before {
		t.Errorf("go.work changed:\n%s", after)
	}
	if invocations := fakes.Invocations(t); len(invocations) != 0 {
		t.Errorf("deps-upgrade started programs: %v", invocations)
	}
}
