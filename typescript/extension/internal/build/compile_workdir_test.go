package build

import (
	"os"
	stdexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeCompileBun stands in for bun build --compile. Like Bun, it writes its
// temporary executable in its working directory, then renames it to the
// --outfile. While the temporary file exists, it records what git lists as
// untracked in the project: what a source binding computed at that moment sees.
const fakeCompileBun = `#!/bin/sh
out=""
while [ $# -gt 0 ]; do
  if [ "$1" = "--outfile" ]; then out="$2"; fi
  shift
done
tmp=".0123456789abcdef-00000000.bun-build"
echo binary > "$tmp"
git -C "$FAKE_BUN_PROJECT" ls-files --others --exclude-standard -- . >> "$FAKE_BUN_SNAPSHOT"
mv "$tmp" "$out"
`

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := stdexec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestRunCompileLeavesNoUntrackedFileInTheProject guards the working directory
// of the compile: from the project root, Bun's temporary executable was an
// untracked file of the project while the compile ran. The workspace ignores .putnami, as the
// .gitignore that putnami init writes does, and holds the compile output there.
func TestRunCompileLeavesNoUntrackedFileInTheProject(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake bun is a POSIX shell script")
	}
	if _, err := stdexec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	projectPath := filepath.Join(root, "apps", "hello")
	outputPath := filepath.Join(root, ".putnami", "out", "apps", "hello", "build", "compile")
	if err := os.MkdirAll(filepath.Join(projectPath, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".putnami\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "src", "main.ts"), []byte("console.log(1);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "-q")
	runGit(t, root, "add", "-A")

	bunBin := filepath.Join(t.TempDir(), "bun")
	if err := os.WriteFile(bunBin, []byte(fakeCompileBun), 0o755); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "untracked")
	t.Setenv("FAKE_BUN_PROJECT", projectPath)
	t.Setenv("FAKE_BUN_SNAPSHOT", snapshot)

	files, errs, err := RunCompile(bunBin, projectPath, outputPath, "./src/main.ts", []string{"bun-linux-x64"})
	if err != nil || len(errs) > 0 {
		t.Fatalf("RunCompile: err=%v errors=%v", err, errs)
	}
	if want := filepath.Join(outputPath, "main-linux-x64"); len(files) != 1 || files[0] != want {
		t.Fatalf("files = %v, want [%s]", files, want)
	}
	seen, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatalf("the fake bun recorded no snapshot: %v", err)
	}
	if untracked := strings.TrimSpace(string(seen)); untracked != "" {
		t.Errorf("git listed untracked files in the project during the compile: %q", strings.Fields(untracked))
	}
	entries, err := os.ReadDir(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "main-linux-x64" {
			t.Errorf("compile output holds %q after the compile, want only the executable", e.Name())
		}
	}
}

// TestRunCompileKeepsTheProjectInputs compiles with the real Bun from outside
// the project, and checks the two inputs Bun takes from its working directory
// still come from the project: the relative entrypoint and the bunfig.toml.
func TestRunCompileKeepsTheProjectInputs(t *testing.T) {
	bunBin, err := stdexec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	projectPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectPath, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := "declare const FROM_BUNFIG: string;\nconsole.log(FROM_BUNFIG);\n"
	if err := os.WriteFile(filepath.Join(projectPath, "src", "main.ts"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	bunfig := "[define]\nFROM_BUNFIG = \"'project-bunfig'\"\n"
	if err := os.WriteFile(filepath.Join(projectPath, "bunfig.toml"), []byte(bunfig), 0o644); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "compile")
	target := "bun-" + runtime.GOOS + "-" + bunArch(runtime.GOARCH)

	files, errs, err := RunCompile(bunBin, projectPath, outputPath, "./src/main.ts", []string{target})
	if err != nil || len(errs) > 0 {
		t.Fatalf("RunCompile: err=%v errors=%v", err, errs)
	}
	if len(files) != 1 {
		t.Fatalf("files = %v, want one executable", files)
	}
	out, err := stdexec.Command(files[0]).Output()
	if err != nil {
		t.Fatalf("run %s: %v", files[0], err)
	}
	if got := strings.TrimSpace(string(out)); got != "project-bunfig" {
		t.Errorf("the executable printed %q, want the define of the project's bunfig.toml", got)
	}
	entries, err := os.ReadDir(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "src" && e.Name() != "bunfig.toml" {
			t.Errorf("the compile left %q in the project", e.Name())
		}
	}
}
