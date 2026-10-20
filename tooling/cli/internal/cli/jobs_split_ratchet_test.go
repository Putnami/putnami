package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The internal/jobs split.
//
// internal/jobs was split into packages so that a change in one part stops
// recompiling and rerunning the tests of another. Go reruns a package's tests
// when anything they import changes, so the split holds only while each
// package imports what its entry below allows. The compiler refuses an import
// that closes a cycle between two of them; it does not refuse an import of
// any other CLI package, which would pull that package's changes back into
// the split-out tests. This test does.
//
// The last code left in internal/jobs moved to internal/jobs/publication, so
// the directory holds the split-out packages and no Go file of its own.
// TestJobsSplitLeavesNoJobsPackage keeps it that way.
//
// Test files count: a test binary is rebuilt when its test files' imports
// change, so an allowance that held only for production files would not hold
// the property. An import only the tests need is a test-only allowance, which
// a production file fails on.

// jobsSplitPackage is one package split out of internal/jobs.
type jobsSplitPackage struct {
	// dir is the module-relative directory of the package.
	dir string
	// allowed lists the import path prefixes every file of the package may
	// import besides the standard library.
	allowed []string
	// testOnly lists the import path prefixes only the package's _test.go
	// files may import besides allowed.
	testOnly []string
	// why states what the package is, so the allowance can be judged.
	why string
}

// jobsSplitModulePath is the module path the package directories are relative to.
const jobsSplitModulePath = "go.putnami.dev/tooling/cli"

var jobsSplitPackages = []jobsSplitPackage{
	{
		dir:     "internal/jobs/machineprobe",
		allowed: []string{"go.putnami.dev/cli/model/jobs"},
		why: "machine probes: they read the runner's procfs, sysfs and rusage and publish the " +
			"model's RunnerEnvironment types, nothing else",
	},
	{
		dir:      "internal/jobs/procdeadline",
		allowed:  nil,
		testOnly: []string{"go.putnami.dev/tooling/cli/internal/fixtureproc"},
		why: "process deadlines: they bound a started child by the time it runs, from the host's own " +
			"process readings; only the tests start fixture programs",
	},
	{
		dir:     "internal/jobs/cachekey/framing",
		allowed: nil,
		why:     "cache key framing: it writes labeled fields to a hash and imports the standard library only",
	},
	{
		dir: "internal/jobs/cachekey",
		allowed: []string{
			"go.putnami.dev/cli/model/jobs",
			"go.putnami.dev/protocol/cache",
			"go.putnami.dev/protocol/distribution",
			"go.putnami.dev/protocol/extension",
			"go.putnami.dev/protocol/features",
			"go.putnami.dev/protocol/runtime",
			"go.putnami.dev/protocol/workspace",
			"go.putnami.dev/sdk/extension/dirlink",
			"go.putnami.dev/sdk/extension/releaseset",
			"go.putnami.dev/tooling/cli/internal/artifactstore",
			"go.putnami.dev/tooling/cli/internal/extension",
			"go.putnami.dev/tooling/cli/internal/git",
			"go.putnami.dev/tooling/cli/internal/jobs/cachekey/framing",
			"go.putnami.dev/tooling/cli/internal/jobs/machineprobe",
			"go.putnami.dev/tooling/cli/internal/jobs/toolchain",
			"go.putnami.dev/tooling/cli/internal/store",
			"go.putnami.dev/tooling/cli/internal/workspace",
		},
		testOnly: []string{
			"go.putnami.dev/cli/model/extension",
			"go.putnami.dev/cli/model/workspace",
			"go.putnami.dev/protocol/cli",
			"go.putnami.dev/protocol/job",
			"go.putnami.dev/sdk/extension/profiles",
		},
		why: "task cache keys: a key hashes the task's declared files and params, its project and " +
			"workspace membership, its extension's installed tree or runtime sources, its runtime " +
			"toolchains, the host platform and the release baseline the git history names; it reads " +
			"them through the workspace, extension and store models, and never imports the scheduler " +
			"or the runner. It also converts a task's result to and from its cache entry, whose events " +
			"name protocol features",
	},
	{
		dir: "internal/jobs/planner",
		allowed: []string{
			"go.putnami.dev/cli/model/jobs",
			"go.putnami.dev/protocol/job",
			"go.putnami.dev/tooling/cli/internal/extension",
			"go.putnami.dev/tooling/cli/internal/jobs/cachekey",
			"go.putnami.dev/tooling/cli/internal/jobs/toolchain",
			"go.putnami.dev/tooling/cli/internal/workspace",
		},
		testOnly: []string{
			"go.putnami.dev/protocol/cli",
			"go.putnami.dev/protocol/extension",
			"go.putnami.dev/protocol/features/spectest",
			"go.putnami.dev/protocol/workspace",
			"go.putnami.dev/tooling/cli/internal/store",
		},
		why: "the planner: it turns a selection of projects and commands into the typed plan of a " +
			"run, from the workspace and extension models, the job model and the task cache key " +
			"package (a plan stamps the identities and shared-execution digests the keys fold in, " +
			"and reads the runtime toolchain identity). It never imports the scheduler or the " +
			"runner: the scheduler reads the plan and the plan-time halves of the DAG state and the " +
			"invocation relation, and the engine is the only package that calls Plan. Its tests " +
			"read the committed first-party manifests through the protocol packages and write " +
			"fixture workspaces and a store root",
	},
	{
		dir: "internal/jobs/toolchain",
		allowed: []string{
			"go.putnami.dev/cli/model/extension",
			"go.putnami.dev/cli/model/jobs",
			"go.putnami.dev/protocol/extension",
			"go.putnami.dev/sdk/extension/envkeys",
			"go.putnami.dev/tooling/cli/internal/extension",
			"go.putnami.dev/tooling/cli/internal/jobs/cachekey/framing",
			"go.putnami.dev/tooling/cli/internal/jobs/machineprobe",
			"go.putnami.dev/tooling/cli/internal/jobs/procdeadline",
			"go.putnami.dev/tooling/cli/internal/lockfile",
			"go.putnami.dev/tooling/cli/internal/runcredential",
			"go.putnami.dev/tooling/cli/internal/store",
		},
		testOnly: []string{
			"go.putnami.dev/cli/model/workspace",
			"go.putnami.dev/protocol/features/spectest",
			"go.putnami.dev/sdk/extension/pkgmeta",
			"go.putnami.dev/tooling/cli/internal/fixtureproc",
			"go.putnami.dev/tooling/cli/internal/hometest",
		},
		why: "runtime toolchains: they resolve an extension's declared toolchains against the lock of " +
			"the workspace root the caller names, which needs the extension model, the lock, the " +
			"artifact store root, and the run credential's custody mark and hosted check before a " +
			"probe starts repository code",
	},
	{
		dir: "internal/jobs/remotecache",
		allowed: []string{
			"go.putnami.dev/cli/model/jobs",
			"go.putnami.dev/protocol/cache",
			"go.putnami.dev/protocol/extension",
			"go.putnami.dev/protocol/features",
			"go.putnami.dev/tooling/cli/internal/cacheprovider",
			"go.putnami.dev/tooling/cli/internal/extension",
			"go.putnami.dev/tooling/cli/internal/flock",
			"go.putnami.dev/tooling/cli/internal/git",
			"go.putnami.dev/tooling/cli/internal/jobs/cachekey",
			"go.putnami.dev/tooling/cli/internal/runcredential",
			"go.putnami.dev/tooling/cli/internal/store",
			"go.putnami.dev/tooling/cli/internal/workspace",
		},
		testOnly: []string{
			"go.putnami.dev/protocol/job",
		},
		why: "remote cache client: it negotiates, restores and shares task cache entries with a cache " +
			"provider session, under the run credential and trust policy; the caller hands it the " +
			"provider's launch, so it never imports the scheduler or the runner",
	},
	{
		dir: "internal/jobs/scheduler",
		allowed: []string{
			"go.putnami.dev/cli/model/jobs",
			"go.putnami.dev/protocol/cache",
			"go.putnami.dev/protocol/cli",
			"go.putnami.dev/protocol/distribution",
			"go.putnami.dev/protocol/extension",
			"go.putnami.dev/protocol/job",
			"go.putnami.dev/protocol/registry",
			"go.putnami.dev/protocol/runner",
			"go.putnami.dev/protocol/runtime",
			"go.putnami.dev/sdk/extension/dirlink",
			"go.putnami.dev/sdk/extension/envkeys",
			"go.putnami.dev/sdk/extension/pkgmeta",
			"go.putnami.dev/sdk/extension/proctree",
			"go.putnami.dev/sdk/extension/releaseset",
			"go.putnami.dev/tooling/cli/internal/abort",
			"go.putnami.dev/tooling/cli/internal/artifactstore",
			"go.putnami.dev/tooling/cli/internal/extension",
			"go.putnami.dev/tooling/cli/internal/flock",
			"go.putnami.dev/tooling/cli/internal/git",
			"go.putnami.dev/tooling/cli/internal/hooks",
			"go.putnami.dev/tooling/cli/internal/iox",
			"go.putnami.dev/tooling/cli/internal/jobs/cachekey",
			"go.putnami.dev/tooling/cli/internal/jobs/machineprobe",
			"go.putnami.dev/tooling/cli/internal/jobs/planner",
			"go.putnami.dev/tooling/cli/internal/jobs/procdeadline",
			"go.putnami.dev/tooling/cli/internal/jobs/toolchain",
			"go.putnami.dev/tooling/cli/internal/runcredential",
			"go.putnami.dev/tooling/cli/internal/store",
			"go.putnami.dev/tooling/cli/internal/useragent",
			"go.putnami.dev/tooling/cli/internal/workspace",
		},
		testOnly: []string{
			"go.putnami.dev/cli/model/extension",
			"go.putnami.dev/cli/model/workspace",
			"go.putnami.dev/protocol/diagnostic",
			"go.putnami.dev/protocol/features",
			"go.putnami.dev/protocol/workspace",
			"go.putnami.dev/sdk/extension/context",
			"go.putnami.dev/sdk/extension/dockerpublish",
			"go.putnami.dev/sdk/extension/jsonl",
			"go.putnami.dev/sdk/extension/ownerperm",
			"go.putnami.dev/sdk/extension/registrycred",
			"go.putnami.dev/tooling/cli/internal/cacheprovider",
			"go.putnami.dev/tooling/cli/internal/fixtureproc",
			"go.putnami.dev/tooling/cli/internal/jobs/remotecache",
			"go.putnami.dev/tooling/cli/internal/lockfile",
			"golang.org/x/sys/windows",
		},
		why: "the scheduler, the runner and the cache use: it runs the jobs of the plan the planner " +
			"builds, restores and stores their task cache entries, and starts extension runtimes, " +
			"providers and host RPC under the run credential and the process capabilities. The " +
			"release-set and publication code in internal/jobs/publication imports it, so it never " +
			"imports internal/jobs/publication. Its tests wire in a remote cache client and a fake " +
			"cache provider, and read the publish and registry fixtures of the SDK",
	},
	{
		dir: "internal/jobs/publication",
		allowed: []string{
			"github.com/google/go-containerregistry/pkg/name",
			"go.putnami.dev/cli/model/jobs",
			"go.putnami.dev/cli/model/workspace",
			"go.putnami.dev/protocol/ci",
			"go.putnami.dev/protocol/diagnostic",
			"go.putnami.dev/protocol/distribution",
			"go.putnami.dev/protocol/extension",
			"go.putnami.dev/protocol/job",
			"go.putnami.dev/protocol/put",
			"go.putnami.dev/protocol/registry",
			"go.putnami.dev/protocol/runner",
			"go.putnami.dev/protocol/runtime",
			"go.putnami.dev/protocol/workspace",
			"go.putnami.dev/sdk/extension/gomodpublish",
			"go.putnami.dev/sdk/extension/memberprobe",
			"go.putnami.dev/sdk/extension/pkgmeta",
			"go.putnami.dev/sdk/extension/publicationoutbox",
			"go.putnami.dev/sdk/extension/publicationupload",
			"go.putnami.dev/sdk/extension/putpublish",
			"go.putnami.dev/sdk/extension/releaseset",
			"go.putnami.dev/tooling/cli/internal/cmderr",
			"go.putnami.dev/tooling/cli/internal/extension",
			"go.putnami.dev/tooling/cli/internal/git",
			"go.putnami.dev/tooling/cli/internal/jobs/cachekey",
			"go.putnami.dev/tooling/cli/internal/jobs/planner",
			"go.putnami.dev/tooling/cli/internal/jobs/scheduler",
			"go.putnami.dev/tooling/cli/internal/runcredential",
			"go.putnami.dev/tooling/cli/internal/store",
		},
		testOnly: []string{
			"github.com/google/go-containerregistry/pkg/registry",
			"github.com/google/go-containerregistry/pkg/v1/random",
			"go.putnami.dev/cli/model/extension",
			"go.putnami.dev/protocol/cli",
			"go.putnami.dev/protocol/features/spectest",
			"go.putnami.dev/sdk/extension/context",
			"go.putnami.dev/sdk/extension/dockerpublish",
			"go.putnami.dev/sdk/extension/jsonl",
			"go.putnami.dev/sdk/extension/npmpublish",
			"go.putnami.dev/sdk/extension/oci",
			"go.putnami.dev/sdk/extension/profiles",
			"go.putnami.dev/sdk/extension/registrycred",
			"go.putnami.dev/tooling/cli/internal/credentialprovider/providertest",
			"go.putnami.dev/tooling/cli/internal/workspace",
		},
		why: "the release-set, publication and deploy-target preparation of a run: it adds the " +
			"release-set, publication and deploy jobs and targets to the plan the planner builds, " +
			"and the scheduler runs that plan without importing it. It reads the CI document, the " +
			"release-set, registry and publication protocols and the SDK's publish formats, reads " +
			"the git history, and starts the release-set provider under the run credential. Its " +
			"tests run publication plans through the scheduler against a test credential provider " +
			"and an in-process OCI registry",
	},
}

// jobsParentDir is the directory the split emptied. It holds the split-out
// packages and no Go file of its own.
const jobsParentDir = "internal/jobs"

// TestJobsSplitLeavesNoJobsPackage fails when internal/jobs holds a Go file.
// The last of its code moved to internal/jobs/publication, so the package
// go.putnami.dev/tooling/cli/internal/jobs no longer exists. A file there
// would start it again: a package every split-out one sits under, whose
// imports no entry above bounds.
func TestJobsSplitLeavesNoJobsPackage(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(jobsParentDir)))
	if err != nil {
		t.Fatalf("%s: %v", jobsParentDir, err)
	}
	packages := 0
	for _, entry := range entries {
		if entry.IsDir() {
			packages++
			continue
		}
		if strings.HasSuffix(entry.Name(), ".go") {
			t.Errorf("%s/%s puts a Go file in %s, which holds packages only. Move it to the package "+
				"under %s that owns it, or to a new package with its own entry in jobsSplitPackages.",
				jobsParentDir, entry.Name(), jobsParentDir, jobsParentDir)
		}
	}
	if packages == 0 {
		t.Fatalf("%s holds no package directory — the split moved; move this check with it", jobsParentDir)
	}
}

func TestJobsSplitPackagesImportOnlyTheirAllowance(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	for _, pkg := range jobsSplitPackages {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(pkg.dir)))
		if err != nil {
			t.Errorf("%s: %v — the package moved; move its entry with it", pkg.dir, err)
			continue
		}
		parsed := 0
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
				continue
			}
			rel := pkg.dir + "/" + entry.Name()
			file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, filepath.FromSlash(rel)), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
			parsed++
			for _, spec := range file.Imports {
				path, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatalf("%s: import %s: %v", rel, spec.Path.Value, err)
				}
				if jobsSplitFileImportAllowed(pkg, entry.Name(), path) {
					continue
				}
				t.Errorf("%s imports %s, which %s does not allow (%s).\n"+
					"  A split-out package that imports another CLI package reruns its tests on that "+
					"package's changes, which is what the split removes. Move the code that needs the "+
					"import back to the importer, or widen the allowance here and say why.",
					rel, path, pkg.dir, pkg.why)
			}
		}
		if parsed == 0 {
			t.Errorf("%s holds no Go file — the package moved; move its entry with it", pkg.dir)
		}
	}
}

func TestJobsSplitImportAllowed(t *testing.T) {
	t.Parallel()
	allowed := []string{"go.putnami.dev/cli/model/jobs"}
	for path, want := range map[string]bool{
		"os":                                              true,
		"path/filepath":                                   true,
		"go.putnami.dev/cli/model/jobs":                   true,
		"go.putnami.dev/cli/model/jobsx":                  false,
		"go.putnami.dev/cli/model/workspace":              false,
		"go.putnami.dev/tooling/cli/internal/jobs":        false,
		"go.putnami.dev/tooling/cli/internal/workspace":   false,
		"golang.org/x/sys/unix":                           false,
		"github.com/google/go-containerregistry/pkg/name": false,
	} {
		if got := jobsSplitImportAllowed(path, allowed); got != want {
			t.Errorf("jobsSplitImportAllowed(%q) = %v, want %v", path, got, want)
		}
	}

	pkg := jobsSplitPackage{
		dir:      "internal/jobs/example",
		allowed:  []string{"go.putnami.dev/cli/model/jobs"},
		testOnly: []string{"go.putnami.dev/tooling/cli/internal/fixtureproc"},
	}
	for _, c := range []struct {
		file, path string
		want       bool
	}{
		{"probe.go", "os", true},
		{"probe.go", "go.putnami.dev/cli/model/jobs", true},
		{"probe_test.go", "go.putnami.dev/cli/model/jobs", true},
		{"probe_test.go", "go.putnami.dev/tooling/cli/internal/jobs/example", true},
		{"probe_test.go", "go.putnami.dev/tooling/cli/internal/fixtureproc", true},
		{"probe_linux_test.go", "go.putnami.dev/tooling/cli/internal/fixtureproc", true},
		{"probe.go", "go.putnami.dev/tooling/cli/internal/fixtureproc", false},
		{"probe_linux.go", "go.putnami.dev/tooling/cli/internal/fixtureproc", false},
		{"test_helpers.go", "go.putnami.dev/tooling/cli/internal/fixtureproc", false},
		{"probe_test.go", "go.putnami.dev/tooling/cli/internal/workspace", false},
	} {
		if got := jobsSplitFileImportAllowed(pkg, c.file, c.path); got != c.want {
			t.Errorf("jobsSplitFileImportAllowed(%s, %q) = %v, want %v", c.file, c.path, got, c.want)
		}
	}
}

// jobsSplitFileImportAllowed reports whether the file named name in pkg may
// import path: the package's own path, which an external test package
// imports, or an allowed import, or, in a _test.go file only, a test-only one.
func jobsSplitFileImportAllowed(pkg jobsSplitPackage, name, path string) bool {
	if path == jobsSplitModulePath+"/"+pkg.dir || jobsSplitImportAllowed(path, pkg.allowed) {
		return true
	}
	return strings.HasSuffix(name, "_test.go") && jobsSplitImportAllowed(path, pkg.testOnly)
}

// jobsSplitImportAllowed reports whether path is the standard library or
// starts with one of the allowed prefixes. A standard-library path has no dot
// in its first element.
func jobsSplitImportAllowed(path string, allowed []string) bool {
	first, _, _ := strings.Cut(path, "/")
	if !strings.Contains(first, ".") {
		return true
	}
	for _, prefix := range allowed {
		if path == prefix || strings.HasPrefix(path, strings.TrimSuffix(prefix, "/")+"/") {
			return true
		}
	}
	return false
}

// jobsSchedulerDir holds the scheduler and the runner. Their production files
// reach the remote cache client only through the cachekey.RemoteCache
// interface the engine hands them, so they do not import the client.
const jobsSchedulerDir = "internal/jobs/scheduler"

// TestJobsSplitSchedulerDoesNotImportTheRemoteCacheClient fails when a
// production file of internal/jobs/scheduler imports internal/jobs/remotecache. The
// engine loads the client; a scheduler import of it, for any name, would make
// the scheduler depend on the client again and rerun its tests on the
// client's changes. Its tests may import it to wire a client in.
func TestJobsSplitSchedulerDoesNotImportTheRemoteCacheClient(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(jobsSchedulerDir)))
	if err != nil {
		t.Fatalf("%s: %v", jobsSchedulerDir, err)
	}
	parsed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		rel := jobsSchedulerDir + "/" + name
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, filepath.FromSlash(rel)), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		parsed++
		if _, imported := jobsImportName(file, remoteCacheImportPath); imported {
			t.Errorf("%s imports %s. The scheduler and the runner reach the remote cache client "+
				"only through the cachekey.RemoteCache interface the engine hands them. Move the "+
				"name the file needs to cachekey, or read it through that interface.",
				rel, remoteCacheImportPath)
		}
	}
	if parsed == 0 {
		t.Fatalf("%s holds no production Go file — the scheduler moved; move this check with it", jobsSchedulerDir)
	}
}

// jobsSplitTestSeams are, by import path, the exported package variables the
// split left writable by every package in the module. Each one is a seam a
// test replaces or a table a key reads. Only a test file may write one: a
// production write would change task cache keys, or the provider a run
// starts, for every caller in the process.
var jobsSplitTestSeams = map[string]map[string]string{
	jobsSplitModulePath + "/internal/jobs/remotecache": {
		"SpawnProviderSession": "tests swap it to run the fake provider in-process; production spawns the provider",
	},
	jobsSplitModulePath + "/internal/jobs/cachekey": {
		"WorkspaceSourceState":    "a cache key input; tests replace it to key a job the way a root in the other source state would",
		"EmptiedOutputIDs":        "a cache key input; tests replace it to key a job the way an engine that emptied nothing did",
		"ProjectWalkExcludedDirs": "a table that shapes cache keys and project walks; nothing may add, remove or replace an entry",
	},
}

// jobsSplitSeamWrite is one write to a seam outside its declaring package.
type jobsSplitSeamWrite struct {
	line   int
	pkg    string
	symbol string
}

func TestJobsSplitSeamsAreWrittenOnlyByTests(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	for importPath, seams := range jobsSplitTestSeams {
		dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(importPath, jobsSplitModulePath+"/")))
		declared := jobsSplitPackageVars(t, dir)
		for symbol := range seams {
			if !declared[symbol] {
				t.Errorf("%s declares no package variable %s — it moved or stopped being a variable; "+
					"update jobsSplitTestSeams", importPath, symbol)
			}
		}
	}

	var violations []string
	scanned := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", ".putnami", "node_modules", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		scanned++
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		for _, write := range jobsSplitSeamWrites(fset, file) {
			violations = append(violations, filepath.ToSlash(rel)+":"+strconv.Itoa(write.line)+"  "+
				write.pkg[strings.LastIndex(write.pkg, "/")+1:]+"."+write.symbol+" — "+
				jobsSplitTestSeams[write.pkg][write.symbol])
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatalf("scanned no Go files under %s — the walk root is wrong", root)
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("%d production write(s) to a test seam:\n    %s\n"+
			"  Only a test may replace these variables. Pass the value to the caller instead, or "+
			"make the seam a per-instance option.", len(violations), strings.Join(violations, "\n    "))
	}
}

// TestJobsSplitSeamWritesDetectsEveryWriteShape keeps the walk above from
// passing vacuously: it feeds the detector each way Go writes a package
// variable, and the reads the scheduler makes today.
func TestJobsSplitSeamWritesDetectsEveryWriteShape(t *testing.T) {
	t.Parallel()
	const src = `package probe

import (
	ck "go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/jobs/remotecache"
)

func writes() {
	remotecache.SpawnProviderSession = nil
	ck.WorkspaceSourceState, _ = nil, 0
	ck.ProjectWalkExcludedDirs["dist"] = false
	delete(ck.ProjectWalkExcludedDirs, "vendor")
	clear(ck.ProjectWalkExcludedDirs)
	seam := &ck.EmptiedOutputIDs
	_ = seam
}

func reads(ck map[string]bool) {
	_ = remotecache.SpawnProviderSession
	_ = ck["dist"]
	ck["vendor"] = true
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "probe.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var got []string
	for _, write := range jobsSplitSeamWrites(fset, file) {
		got = append(got, strconv.Itoa(write.line)+" "+write.symbol)
	}
	want := []string{
		"9 SpawnProviderSession",
		"10 WorkspaceSourceState",
		"11 ProjectWalkExcludedDirs",
		"12 ProjectWalkExcludedDirs",
		"13 ProjectWalkExcludedDirs",
		"14 EmptiedOutputIDs",
	}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("jobsSplitSeamWrites = %v, want %v", got, want)
	}
}

// jobsSplitSeamWrites returns every write in file to a seam of
// jobsSplitTestSeams, named through the seam's import: an assignment, an
// increment, a map entry write, a delete or clear call, or a taken address.
// The declaring package names its seams unqualified, so its writes are not
// reported.
func jobsSplitSeamWrites(fset *token.FileSet, file *ast.File) []jobsSplitSeamWrite {
	locals := map[string]string{}
	for importPath := range jobsSplitTestSeams {
		if local, imported := jobsImportName(file, importPath); imported {
			locals[local] = importPath
		}
	}
	if len(locals) == 0 {
		return nil
	}
	var writes []jobsSplitSeamWrite
	record := func(target ast.Expr) {
		for {
			if paren, ok := target.(*ast.ParenExpr); ok {
				target = paren.X
				continue
			}
			if index, ok := target.(*ast.IndexExpr); ok {
				target = index.X
				continue
			}
			break
		}
		selector, ok := target.(*ast.SelectorExpr)
		if !ok {
			return
		}
		pkg, ok := selector.X.(*ast.Ident)
		// pkg.Obj != nil means a local declaration shadows the import name.
		if !ok || pkg.Obj != nil {
			return
		}
		importPath, ok := locals[pkg.Name]
		if !ok {
			return
		}
		if _, seam := jobsSplitTestSeams[importPath][selector.Sel.Name]; !seam {
			return
		}
		writes = append(writes, jobsSplitSeamWrite{
			line:   fset.Position(selector.Sel.Pos()).Line,
			pkg:    importPath,
			symbol: selector.Sel.Name,
		})
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				record(lhs)
			}
		case *ast.IncDecStmt:
			record(n.X)
		case *ast.RangeStmt:
			if n.Tok == token.ASSIGN {
				if n.Key != nil {
					record(n.Key)
				}
				if n.Value != nil {
					record(n.Value)
				}
			}
		case *ast.UnaryExpr:
			if n.Op == token.AND {
				record(n.X)
			}
		case *ast.CallExpr:
			if fn, ok := n.Fun.(*ast.Ident); ok && (fn.Name == "delete" || fn.Name == "clear") && len(n.Args) > 0 {
				record(n.Args[0])
			}
		}
		return true
	})
	return writes
}

// jobsSplitPackageVars returns the package-level variable names the
// production files in dir declare.
func jobsSplitPackageVars(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	vars := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				for _, ident := range spec.(*ast.ValueSpec).Names {
					vars[ident.Name] = true
				}
			}
		}
	}
	return vars
}
