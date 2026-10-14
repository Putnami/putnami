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

// The internal/jobs split (#256 slice 1).
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
// the property.

// jobsSplitPackage is one package split out of internal/jobs.
type jobsSplitPackage struct {
	// dir is the module-relative directory of the package.
	dir string
	// allowed lists the import path prefixes the package may import besides
	// the standard library.
	allowed []string
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
				// The package's own path is allowed: an external test package imports it.
				if path == jobsSplitModulePath+"/"+pkg.dir || jobsSplitImportAllowed(path, pkg.allowed) {
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
