package scripts

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// preparedRuntimes are the extensions whose bin/prepare compiles a Go runtime
// with the replace closure completion, and the command each one builds.
var preparedRuntimes = []struct {
	extension string
	command   string
}{
	{extension: "go", command: "putnami-go"},
	{extension: "python", command: "putnami-python"},
	{extension: "typescript", command: "putnami-ts"},
}

// closureFixture is a module tree under one root. Each key is a slash path
// relative to the root, each value the file content.
type closureFixture map[string]string

// The fixture modules: lib requires and replaces leaf, and the main module of
// each test requires both and decides which it replaces itself.
const (
	leafGoMod = "module example.com/leaf\n\ngo 1.25\n"
	libGoMod  = "module example.com/lib\n\ngo 1.25\n\nrequire example.com/leaf v0.0.0\n\n" +
		"replace example.com/leaf => ../leaf\n"
	libSource = "package lib\n\nimport \"example.com/leaf\"\n\nfunc Name() string { return \"lib+\" + leaf.Name }\n"
)

func leafSource(name string) string {
	return "package leaf\n\nconst Name = \"" + name + "\"\n"
}

func mainSource(imports ...string) string {
	var b strings.Builder
	b.WriteString("package main\n\nimport (\n\t\"fmt\"\n")
	for _, path := range imports {
		b.WriteString("\t\"" + path + "\"\n")
	}
	b.WriteString(")\n\nfunc main() { fmt.Println(lib.Name(), leaf.Name) }\n")
	return b.String()
}

// writeClosureFixture writes files under a new root and the main source of
// command under ext, and returns the root.
func writeClosureFixture(t *testing.T, command string, files closureFixture) string {
	t.Helper()
	root := t.TempDir()
	files["ext/cmd/"+command+"/main.go"] = mainSource("example.com/leaf", "example.com/lib")
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// runPrepare runs the bin/prepare script of extension in moduleDir with no
// module proxy, so a module the build cannot find locally fails instead of
// resolving from the network.
func runPrepare(t *testing.T, extension, moduleDir, output string) ([]byte, error) {
	t.Helper()
	script := filepath.Join(prepareRepoRoot(t), extension, "extension", "bin", "prepare")
	cmd := exec.Command("bash", script, "--output", output)
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(), "GOPROXY=off")
	return cmd.CombinedOutput()
}

func requirePrepareTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("prepare scripts are Unix-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Fatalf("bash not available: %v", err)
	}
	if root := os.Getenv("GOROOT"); root != "" {
		if _, err := os.Stat(filepath.Join(root, "bin", "go")); err == nil {
			return
		}
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("no go toolchain to build with: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// outputEntries lists every path under output, relative and slash-separated.
func outputEntries(t *testing.T, output string) []string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(output, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(output, path)
		if err != nil {
			return err
		}
		if rel != "." {
			entries = append(entries, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// A module the main go.mod requires but does not replace builds from the
// directory another module of the replace closure replaces it with. The
// module's own go.mod and go.sum stay as they were, the output holds only the
// runtime, and a second run builds the same bytes.
func TestPrepareBuildsAModuleTheReplaceClosureProvides(t *testing.T) {
	t.Parallel()
	requirePrepareTools(t)
	spectest.Proves(t, "go/go-project-toolchain", "prepare-completes-the-replace-closure",
		"a-module-the-closure-provides-builds-without-its-replace")
	spectest.Proves(t, "go/go-project-toolchain", "prepare-completes-the-replace-closure",
		"prepare-writes-neither-go-mod-nor-go-sum")
	spectest.Proves(t, "go/go-project-toolchain", "prepare-completes-the-replace-closure",
		"two-runs-build-the-same-bytes")

	for _, test := range preparedRuntimes {
		t.Run(test.extension, func(t *testing.T) {
			t.Parallel()
			goMod := "module example.com/ext\n\ngo 1.25\n\n" +
				"require (\n\texample.com/leaf v0.0.0\n\texample.com/lib v0.0.0\n)\n\n" +
				"replace example.com/lib => ../lib\n"
			root := writeClosureFixture(t, test.command, closureFixture{
				"ext/go.mod":   goMod,
				"lib/go.mod":   libGoMod,
				"lib/lib.go":   libSource,
				"leaf/go.mod":  leafGoMod,
				"leaf/leaf.go": leafSource("leaf"),
			})
			moduleDir := filepath.Join(root, "ext")

			builds := make([][]byte, 0, 2)
			for range 2 {
				output := t.TempDir()
				if out, err := runPrepare(t, test.extension, moduleDir, output); err != nil {
					t.Fatalf("prepare failed: %v\n%s", err, out)
				}
				if got, want := outputEntries(t, output), []string{"compiled", "compiled/" + test.command}; strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("output holds %v, want %v", got, want)
				}
				runtimePath := filepath.Join(output, "compiled", test.command)
				printed, err := exec.Command(runtimePath).Output()
				if err != nil {
					t.Fatalf("run the prepared runtime: %v", err)
				}
				if got := strings.TrimSpace(string(printed)); got != "lib+leaf leaf" {
					t.Errorf("the prepared runtime printed %q, want %q", got, "lib+leaf leaf")
				}
				data, err := os.ReadFile(runtimePath)
				if err != nil {
					t.Fatal(err)
				}
				builds = append(builds, data)
			}
			if !bytes.Equal(builds[0], builds[1]) {
				t.Error("two runs over one tree built different runtimes")
			}
			if got := readFile(t, filepath.Join(moduleDir, "go.mod")); got != goMod {
				t.Errorf("prepare rewrote go.mod:\n%s", got)
			}
			if _, err := os.Stat(filepath.Join(moduleDir, "go.sum")); !os.IsNotExist(err) {
				t.Errorf("prepare created go.sum: %v", err)
			}
		})
	}
}

// A replace in the main go.mod wins over the replace another module of the
// closure declares for the same module.
func TestPrepareKeepsTheReplaceTheModuleDeclares(t *testing.T) {
	t.Parallel()
	requirePrepareTools(t)
	spectest.Proves(t, "go/go-project-toolchain", "prepare-completes-the-replace-closure",
		"the-module-own-replace-wins")

	for _, test := range preparedRuntimes {
		t.Run(test.extension, func(t *testing.T) {
			t.Parallel()
			root := writeClosureFixture(t, test.command, closureFixture{
				"ext/go.mod": "module example.com/ext\n\ngo 1.25\n\n" +
					"require (\n\texample.com/leaf v0.0.0\n\texample.com/lib v0.0.0\n)\n\n" +
					"replace (\n\texample.com/leaf => ../own\n\texample.com/lib => ../lib\n)\n",
				"lib/go.mod":   libGoMod,
				"lib/lib.go":   libSource,
				"leaf/go.mod":  leafGoMod,
				"leaf/leaf.go": leafSource("closure"),
				"own/go.mod":   leafGoMod,
				"own/leaf.go":  leafSource("own"),
			})
			output := t.TempDir()
			if out, err := runPrepare(t, test.extension, filepath.Join(root, "ext"), output); err != nil {
				t.Fatalf("prepare failed: %v\n%s", err, out)
			}
			printed, err := exec.Command(filepath.Join(output, "compiled", test.command)).Output()
			if err != nil {
				t.Fatalf("run the prepared runtime: %v", err)
			}
			if got := strings.TrimSpace(string(printed)); got != "lib+own own" {
				t.Errorf("the prepared runtime printed %q, want %q", got, "lib+own own")
			}
		})
	}
}

// Two directories of the closure that provide one module, which the main
// go.mod does not replace, stop the build with both directories named.
func TestPrepareRefusesTwoProvidersOfOneModule(t *testing.T) {
	t.Parallel()
	requirePrepareTools(t)
	spectest.Proves(t, "go/go-project-toolchain", "prepare-completes-the-replace-closure",
		"two-providers-of-one-module-stop-the-build")

	for _, test := range preparedRuntimes {
		t.Run(test.extension, func(t *testing.T) {
			t.Parallel()
			goMod := "module example.com/ext\n\ngo 1.25\n\n" +
				"require (\n\texample.com/fork v0.0.0\n\texample.com/leaf v0.0.0\n\texample.com/lib v0.0.0\n)\n\n" +
				"replace (\n\texample.com/fork => ../fork\n\texample.com/lib => ../lib\n)\n"
			root := writeClosureFixture(t, test.command, closureFixture{
				"ext/go.mod": goMod,
				"lib/go.mod": libGoMod,
				"lib/lib.go": libSource,
				"fork/go.mod": "module example.com/fork\n\ngo 1.25\n\nrequire example.com/leaf v0.0.0\n\n" +
					"replace example.com/leaf => ../leaf-copy\n",
				"leaf/go.mod":       leafGoMod,
				"leaf/leaf.go":      leafSource("leaf"),
				"leaf-copy/go.mod":  leafGoMod,
				"leaf-copy/leaf.go": leafSource("copy"),
			})
			moduleDir := filepath.Join(root, "ext")
			output := t.TempDir()
			out, err := runPrepare(t, test.extension, moduleDir, output)
			if err == nil {
				t.Fatalf("prepare built with two providers of example.com/leaf:\n%s", out)
			}
			if !strings.Contains(string(out), "provides example.com/leaf from both ../leaf-copy and ../leaf;") {
				t.Errorf("prepare output does not name the module and both directories:\n%s", out)
			}
			if _, err := os.Stat(filepath.Join(output, "compiled", test.command)); !os.IsNotExist(err) {
				t.Errorf("a refused build left a runtime: %v", err)
			}
			if got := readFile(t, filepath.Join(moduleDir, "go.mod")); got != goMod {
				t.Errorf("prepare rewrote go.mod:\n%s", got)
			}
		})
	}
}

// The build adds no requirement: a go.mod that misses one fails, and stays as
// it was, instead of resolving a version the module does not state.
func TestPrepareAddsNoRequirement(t *testing.T) {
	t.Parallel()
	requirePrepareTools(t)
	spectest.Proves(t, "go/go-project-toolchain", "prepare-completes-the-replace-closure",
		"the-build-adds-no-requirement")

	for _, test := range preparedRuntimes {
		t.Run(test.extension, func(t *testing.T) {
			t.Parallel()
			goMod := "module example.com/ext\n\ngo 1.25\n\nrequire example.com/lib v0.0.0\n\n" +
				"replace example.com/lib => ../lib\n"
			root := writeClosureFixture(t, test.command, closureFixture{
				"ext/go.mod":   goMod,
				"lib/go.mod":   libGoMod,
				"lib/lib.go":   libSource,
				"leaf/go.mod":  leafGoMod,
				"leaf/leaf.go": leafSource("leaf"),
			})
			moduleDir := filepath.Join(root, "ext")
			out, err := runPrepare(t, test.extension, moduleDir, t.TempDir())
			if err == nil {
				t.Fatalf("prepare built a module whose go.mod misses a requirement:\n%s", out)
			}
			if !strings.Contains(string(out), "updates to go.mod needed") {
				t.Errorf("prepare failed for another reason than the missing requirement:\n%s", out)
			}
			if got := readFile(t, filepath.Join(moduleDir, "go.mod")); got != goMod {
				t.Errorf("prepare rewrote go.mod:\n%s", got)
			}
		})
	}
}
