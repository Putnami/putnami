package cli

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The internal/jobs split.
//
// internal/jobs is being split into packages so that a change in one part
// stops recompiling and rerunning the tests of another. Go reruns a package's
// tests when anything they import changes, so the split holds only while each
// package imports what its entry below allows. The compiler already refuses an
// import of internal/jobs from a package internal/jobs imports, because that
// is a cycle; it does not refuse an import of any other CLI package, which
// would pull that package's changes back into the split-out tests. This test
// does.
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
