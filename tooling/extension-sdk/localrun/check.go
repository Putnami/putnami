package localrun

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// definingPackages name the hosted-run variables without reading them: the
// protocol package that declares them, and this one.
var definingPackages = []string{"go.putnami.dev/protocol/extension", ImportPath}

// hostedRunReference matches a use of a hosted-run variable in Go source: the
// protocol constant or the variable's name.
var hostedRunReference = regexp.MustCompile(
	`\b(OfflineDependenciesEnv|JobCredentialFDEnv|PUTNAMI_OFFLINE_DEPENDENCIES|PUTNAMI_JOB_CREDENTIAL_FD)\b`)

// listedPackage is the part of `go list -json` output CheckModule reads.
type listedPackage struct {
	ImportPath   string
	Dir          string
	Standard     bool
	GoFiles      []string
	CgoFiles     []string
	Imports      []string
	TestImports  []string
	XTestImports []string
	Deps         []string
	Module       *struct {
		Main    bool
		Replace *struct{ Version string }
	}
}

// local reports whether the package's source is a directory of this checkout
// and not a downloaded module.
func (p listedPackage) local() bool {
	if p.Standard || p.Module == nil {
		return false
	}
	return p.Module.Main || (p.Module.Replace != nil && p.Module.Replace.Version == "")
}

// CheckModule fails t when a test package of the Go module that holds the
// working directory can read a hosted-run variable from the environment it
// inherits. That is a package whose test binary links a package that names
// one of HostedRunVars, and whose test files do not import this package. It
// also fails t when a file that is not a test file imports this package.
//
// It lists the module with the `go` command of the test's PATH, for the host
// platform.
func CheckModule(t testing.TB) {
	t.Helper()
	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("localrun: %v", err)
	}
	problems, err := checkModule(root)
	if err != nil {
		t.Fatalf("localrun: %v", err)
	}
	for _, problem := range problems {
		t.Error(problem)
	}
}

func moduleRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", errors.New("the working directory is in no Go module")
	}
	return filepath.Dir(gomod), nil
}

func checkModule(root string) ([]string, error) {
	packages, err := listPackages(root)
	if err != nil {
		return nil, err
	}
	readers, err := readerPackages(packages)
	if err != nil {
		return nil, err
	}

	var problems []string
	for importPath, pkg := range packages {
		if !pkg.local() || !strings.HasPrefix(pkg.Dir, root) {
			continue
		}
		if slices.Contains(pkg.Imports, ImportPath) && importPath != ImportPath {
			problems = append(problems, fmt.Sprintf(
				"%s imports %s outside a test file: only a test file may import it", importPath, ImportPath))
		}
		testBinary, tested := packages[importPath+".test"]
		if !tested || importPath == ImportPath {
			continue
		}
		if slices.Contains(pkg.TestImports, ImportPath) || slices.Contains(pkg.XTestImports, ImportPath) {
			continue
		}
		if reader := firstReader(testBinary.Deps, readers); reader != "" {
			problems = append(problems, fmt.Sprintf(
				"the tests of %s link %s, which names a variable of a hosted run's job environment: add `import _ %q` to a test file of the package, and set the variable in the tests of the hosted behavior",
				importPath, reader, ImportPath))
		}
	}
	sort.Strings(problems)
	return problems, nil
}

// listPackages returns the packages of the module at root, their test
// binaries and all their dependencies, by import path. A package that `go
// list` reports once for each test binary that recompiles it is kept once.
func listPackages(root string) (map[string]listedPackage, error) {
	cmd := exec.Command("go", "list", "-test", "-deps", "-json", "./...")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list in %s: %w: %s", root, err, strings.TrimSpace(stderr.String()))
	}
	packages := map[string]listedPackage{}
	decoder := json.NewDecoder(bytes.NewReader(out))
	for {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			return packages, nil
		} else if err != nil {
			return nil, fmt.Errorf("go list in %s: %w", root, err)
		}
		pkg.ImportPath = plainImportPath(pkg.ImportPath)
		if _, seen := packages[pkg.ImportPath]; !seen {
			packages[pkg.ImportPath] = pkg
		}
	}
}

// plainImportPath removes the " [p.test]" suffix `go list -test` gives a
// package recompiled for a test binary.
func plainImportPath(importPath string) string {
	plain, _, _ := strings.Cut(importPath, " [")
	return plain
}

// readerPackages returns the local packages with a file, other than a test
// file, that names a hosted-run variable.
func readerPackages(packages map[string]listedPackage) (map[string]bool, error) {
	readers := map[string]bool{}
	for importPath, pkg := range packages {
		// A test binary's own package holds only the generated test main.
		if !pkg.local() || strings.HasSuffix(importPath, ".test") || slices.Contains(definingPackages, importPath) {
			continue
		}
		for _, name := range slices.Concat(pkg.GoFiles, pkg.CgoFiles) {
			source, err := os.ReadFile(filepath.Join(pkg.Dir, name))
			if err != nil {
				return nil, err
			}
			if hostedRunReference.Match(source) {
				readers[importPath] = true
				break
			}
		}
	}
	return readers, nil
}

// firstReader returns the first of deps, the dependencies of a test binary,
// that is a reader, or "".
func firstReader(deps []string, readers map[string]bool) string {
	for _, dep := range deps {
		if dep = plainImportPath(dep); readers[dep] {
			return dep
		}
	}
	return ""
}
