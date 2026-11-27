package repository

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/specreport"
	"go.putnami.dev/tooling/cli/internal/repotest"
)

// cliModulePath is the module whose marked tests this project runs.
const cliModulePath = "go.putnami.dev/tooling/cli"

// repotestImportPath is the package of the marker.
const repotestImportPath = cliModulePath + "/internal/repotest"

// cliModuleDir is that module's directory from the repository root.
const cliModuleDir = "tooling/cli"

// markedTest is one test function of the CLI's module whose first statement is
// repotest.Require.
type markedTest struct {
	pkg  string // import path
	name string
}

func (m markedTest) String() string {
	return strings.TrimPrefix(m.pkg, cliModulePath+"/") + "." + m.name
}

// TestTheCLITestsThatReadTheRepository runs every test of @putnami/cli marked
// with repotest.Require, in its own package, with the marker's environment
// set, and reports each one as a subtest. A marked test that does not run, a
// failure to build its package included, fails here.
func TestTheCLITestsThatReadTheRepository(t *testing.T) {
	cliDir := filepath.Join(repositoryRoot(t), filepath.FromSlash(cliModuleDir))
	marked := markedTests(t, cliDir)
	if len(marked) == 0 {
		t.Fatalf("no test under %s calls %s first, so this project runs nothing", cliModuleDir, repotest.Call)
	}

	fragmentDir := t.TempDir()
	results, packageOutput := runMarkedTests(t, cliDir, marked, fragmentDir)
	proofs := proofsByTest(t, cliDir, fragmentDir, marked)
	for _, test := range marked {
		t.Run(test.String(), func(t *testing.T) {
			// A proof is reported by the project whose tree declares it, and
			// the marked test's file is the CLI's: this subtest, which passes,
			// fails or skips with the marked test, declares each check the
			// marked test proved, so the CLI's features keep their evidence.
			for _, proof := range proofs[test] {
				spectest.Proves(t, proof.Feature, proof.Requirement, proof.Check)
			}
			result, ran := results[test]
			switch {
			case !ran:
				t.Fatalf("%s did not run:\n%s", test, packageOutput[test.pkg])
			case result.action == "fail":
				t.Fatalf("%s failed:\n%s", test, result.output)
			case result.action == "skip":
				t.Skipf("%s skipped:\n%s", test, result.output)
			}
		})
	}
}

// TestTheMarkerIsTheFirstStatementOfATestFunction keeps the discovery honest:
// a repotest.Require call anywhere else, such as in a helper or after a
// statement that reads the repository, is one this project would not find, or
// one that lets the CLI's suite read before it skips.
func TestTheMarkerIsTheFirstStatementOfATestFunction(t *testing.T) {
	cliDir := filepath.Join(repositoryRoot(t), filepath.FromSlash(cliModuleDir))
	walkTestFiles(t, cliDir, func(rel string, fset *token.FileSet, file *ast.File) {
		for _, spec := range file.Imports {
			if spec.Path.Value == `"`+repotestImportPath+`"` && spec.Name != nil {
				t.Errorf("%s:%d imports %s as %s: the marker is found by its package name, so import it unnamed",
					rel, fset.Position(spec.Pos()).Line, repotestImportPath, spec.Name.Name)
			}
		}
		first := map[ast.Node]bool{}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && isTestFunc(fn) && fn.Body != nil && len(fn.Body.List) > 0 {
				first[fn.Body.List[0]] = true
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			stmt, ok := node.(*ast.ExprStmt)
			if !ok || !isMarker(stmt) {
				return true
			}
			if !first[stmt] {
				t.Errorf("%s:%d: %s is not the first statement of a Test function, so %s does not run it "+
					"and the CLI's suite may read the repository before it skips",
					rel, fset.Position(stmt.Pos()).Line, repotest.Call, "@putnami/cli-repository")
			}
			return true
		})
	})
}

// markedTests lists, sorted, the test functions of the CLI's module whose
// first statement is the marker.
func markedTests(t *testing.T, cliDir string) []markedTest {
	t.Helper()
	var marked []markedTest
	walkTestFiles(t, cliDir, func(rel string, _ *token.FileSet, file *ast.File) {
		pkg := cliModulePath
		if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." {
			pkg += "/" + dir
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !isTestFunc(fn) || fn.Body == nil || len(fn.Body.List) == 0 {
				continue
			}
			if stmt, ok := fn.Body.List[0].(*ast.ExprStmt); ok && isMarker(stmt) {
				marked = append(marked, markedTest{pkg: pkg, name: fn.Name.Name})
			}
		}
	})
	sort.Slice(marked, func(i, j int) bool { return marked[i].String() < marked[j].String() })
	return marked
}

// walkTestFiles parses every _test.go file of the CLI's module, skipping the
// testdata trees go test never compiles.
func walkTestFiles(t *testing.T, cliDir string, visit func(rel string, fset *token.FileSet, file *ast.File)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(cliDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); path != cliDir && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// A file the host's build constraints exclude is one go test does not
		// compile here, so its tests run on the hosts that compile it.
		if match, matchErr := build.Default.MatchFile(filepath.Dir(path), entry.Name()); matchErr != nil || !match {
			return matchErr
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		rel, relErr := filepath.Rel(cliDir, path)
		if relErr != nil {
			return relErr
		}
		visit(rel, fset, file)
		return nil
	})
	if err != nil {
		t.Fatalf("read the tests of %s: %v", cliModuleDir, err)
	}
}

func isTestFunc(fn *ast.FuncDecl) bool {
	return fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") && fn.Name.Name != "TestMain"
}

func isMarker(stmt *ast.ExprStmt) bool {
	call, ok := stmt.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name+"."+sel.Sel.Name == repotest.Call
}

type testResult struct {
	action string
	output string
}

// testEvent is one line of `go test -json`.
type testEvent struct {
	Action  string
	Package string
	Test    string
	Output  string
}

// runMarkedTests runs the marked tests in one go test invocation over their
// packages, their spec fragments written to fragmentDir, and returns the
// outcome of each and the output of each package.
func runMarkedTests(t *testing.T, cliDir string, marked []markedTest, fragmentDir string) (map[markedTest]testResult, map[string]string) {
	t.Helper()
	packages := map[string]bool{}
	names := map[string]bool{}
	for _, test := range marked {
		packages[test.pkg] = true
		names[regexp.QuoteMeta(test.name)] = true
	}
	args := []string{"test", "-json", "-count=1", "-timeout=20m", "-run", "^(" + strings.Join(sortedKeys(names), "|") + ")$"}
	if raceEnabled {
		args = append(args, "-race")
	}
	args = append(args, sortedKeys(packages)...)

	cmd := exec.Command(goBinary(t), args...)
	cmd.Dir = cliDir
	cmd.Env = append(os.Environ(), repotest.Env+"=1", spectest.FragmentDirEnv+"="+fragmentDir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		var exited *exec.ExitError
		if !errors.As(err, &exited) {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
		}
	}

	wanted := map[markedTest]bool{}
	for _, test := range marked {
		wanted[test] = true
	}
	outputs := map[markedTest]*strings.Builder{}
	results := map[markedTest]testResult{}
	packageOutput := map[string]string{}
	pkgOut := map[string]*strings.Builder{}
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for scanner.Scan() {
		var event testEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		top, _, _ := strings.Cut(event.Test, "/")
		key := markedTest{pkg: event.Package, name: top}
		if event.Test == "" {
			if pkgOut[event.Package] == nil {
				pkgOut[event.Package] = &strings.Builder{}
			}
			pkgOut[event.Package].WriteString(event.Output)
			continue
		}
		if !wanted[key] {
			continue
		}
		if outputs[key] == nil {
			outputs[key] = &strings.Builder{}
		}
		outputs[key].WriteString(event.Output)
		if event.Test == top && (event.Action == "pass" || event.Action == "fail" || event.Action == "skip") {
			results[key] = testResult{action: event.Action}
		}
	}
	for key, result := range results {
		result.output = outputs[key].String()
		results[key] = result
	}
	for pkg, out := range pkgOut {
		packageOutput[pkg] = out.String()
	}
	if stderr.Len() > 0 {
		for pkg := range packages {
			packageOutput[pkg] += stderr.String()
		}
	}
	return results, packageOutput
}

// proofsByTest reads the spec fragments the marked tests wrote and groups them
// by the test that declared them: its package directory and root test name.
func proofsByTest(t *testing.T, cliDir, fragmentDir string, tests []markedTest) map[markedTest][]spectest.Fragment {
	t.Helper()
	marked := map[markedTest]bool{}
	for _, test := range tests {
		marked[test] = true
	}
	fragments, warnings := specreport.ReadFragments(fragmentDir)
	for _, warning := range warnings {
		t.Errorf("spec fragments of the marked tests: %s", warning)
	}
	root, err := filepath.EvalSymlinks(cliDir)
	if err != nil {
		t.Fatalf("resolve %s: %v", cliDir, err)
	}
	proofs := map[markedTest][]spectest.Fragment{}
	for _, fragment := range fragments {
		file, err := filepath.EvalSymlinks(fragment.File)
		if err != nil {
			file = fragment.File
		}
		rel, err := filepath.Rel(root, filepath.Dir(file))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("spec fragment (%s, %s, %s) is declared in %s, outside %s", fragment.Feature, fragment.Requirement, fragment.Check, fragment.File, cliModuleDir)
			continue
		}
		pkg := cliModulePath
		if rel != "." {
			pkg += "/" + filepath.ToSlash(rel)
		}
		key := markedTest{pkg: pkg, name: fragment.Symbol}
		proofs[key] = append(proofs[key], fragment)
	}
	for key := range proofs {
		if !marked[key] {
			t.Errorf("spec fragments %v name %s, which is no marked test: a proof declared outside the marked test's own package is reported by nobody", proofs[key], key)
		}
		sort.Slice(proofs[key], func(i, j int) bool {
			a, b := proofs[key][i], proofs[key][j]
			return a.Feature+"\x00"+a.Requirement+"\x00"+a.Check < b.Feature+"\x00"+b.Requirement+"\x00"+b.Check
		})
	}
	return proofs
}

// goBinary is the go command on PATH, the toolchain the step runs under.
func goBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go command: %v", err)
	}
	return path
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// repositoryRoot walks up to the directory holding putnami.workspace.json.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, wsproto.WorkspaceConfigFilename)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no %s above the test working directory", wsproto.WorkspaceConfigFilename)
		}
		dir = parent
	}
}

// TestTheInventoryListsEveryMarkedTest keeps the README's list of the tests
// this project runs equal to the marked tests, so a reader sees where each
// repository check of the CLI runs.
func TestTheInventoryListsEveryMarkedTest(t *testing.T) {
	cliDir := filepath.Join(repositoryRoot(t), filepath.FromSlash(cliModuleDir))
	want := make([]string, 0)
	for _, test := range markedTests(t, cliDir) {
		want = append(want, test.String())
	}
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	_, rest, found := strings.Cut(string(data), "<!-- marked-tests:start -->\n")
	block, _, closed := strings.Cut(rest, "<!-- marked-tests:end -->")
	if !found || !closed {
		t.Fatal("README.md has no marked-tests block")
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(block), "\n") {
		got = append(got, strings.TrimSuffix(strings.TrimPrefix(line, "- `"), "`"))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("README.md lists the marked tests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// cliOwnInputsOutsideItsTree are the CLI suite's declared test inputs outside
// tooling/cli: the protocol corpora its own conformance tests reproduce.
var cliOwnInputsOutsideItsTree = map[string]bool{
	"../../protocols/ci/fixtures/change-plan/**": true,
	"../../protocols/ci/fixtures/impact-plan/**": true,
}

// TestTheCLISuiteKeysOnItsOwnTree is the regression guard for the split: a
// test input of @putnami/cli outside its tree, beyond the protocol corpora,
// makes a change to that file re-run the CLI's whole suite. Mark the test
// that reads it with repotest.Require and declare the file here instead.
func TestTheCLISuiteKeysOnItsOwnTree(t *testing.T) {
	root := repositoryRoot(t)
	cliPatterns := testFilePatterns(t, filepath.Join(root, filepath.FromSlash(cliModuleDir), "putnami.json"))
	own := testFilePatterns(t, "putnami.json")
	declared := map[string]bool{}
	for _, pattern := range own {
		declared[pattern] = true
	}
	for _, pattern := range cliPatterns {
		outside := strings.HasPrefix(strings.TrimPrefix(pattern, "git:"), "../")
		if outside && !cliOwnInputsOutsideItsTree[pattern] {
			t.Errorf("@putnami/cli declares the test input %q outside its tree, so a change there re-runs its whole suite; "+
				"mark the test that reads it with %s and declare the input in tooling/cli-repository", pattern, repotest.Call)
		}
		if outside && !declared[pattern] {
			t.Errorf("@putnami/cli declares the test input %q, which this project does not: the marked tests run here", pattern)
		}
	}
	if !declared["git:../cli/**"] {
		t.Error("this project does not declare git:../cli/**, so a change to a marked test or its fixture is a cache hit here")
	}
}

func testFilePatterns(t *testing.T, manifest string) []string {
	t.Helper()
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read %s: %v", manifest, err)
	}
	var decoded struct {
		Options struct {
			Test struct {
				FilePatterns []string `json:"filePatterns"`
			} `json:"test"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("parse %s: %v", manifest, err)
	}
	return decoded.Options.Test.FilePatterns
}
