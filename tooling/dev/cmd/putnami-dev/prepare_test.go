package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	// lockedCompilerEnvironment names the Go compiler the host resolves for
	// the runtimeCompiler toolchain at the version putnami.lock.json pins. It
	// is the only compiler bin/prepare runs.
	lockedCompilerEnvironment = "PUTNAMI_DEV_GO"
	// lockedVersionEnvironment names the GOVERSION putnami.lock.json pins, as
	// the host passes it with that compiler.
	lockedVersionEnvironment = "PUTNAMI_DEV_GO_VERSION"
	// pinnedVersion is the GOVERSION the tests pin.
	pinnedVersion = "go1.26.0"
)

// writeFakeGo writes an executable Go stand-in at path. Asked for
// `env GOVERSION`, it records the environment of that query in log.version
// and prints version. Otherwise it records its arguments and the build
// environment in log and writes the file a -o argument names. It exits 0.
func writeFakeGo(t *testing.T, path, log, version string) {
	t.Helper()
	script := `#!/bin/sh
if [ "$#" -eq 2 ] && [ "$1" = env ] && [ "$2" = GOVERSION ]; then
  {
    printf 'GOTOOLCHAIN=%s\n' "${GOTOOLCHAIN-unset}"
    printf 'GOROOT=%s\n' "${GOROOT-unset}"
    printf 'GOENV=%s\n' "${GOENV-unset}"
  } > '` + log + `.version'
  printf '%s\n' '` + version + `'
  exit 0
fi
{
  printf 'args=%s\n' "$*"
  printf 'GOWORK=%s\n' "${GOWORK-unset}"
  printf 'GOTOOLCHAIN=%s\n' "${GOTOOLCHAIN-unset}"
  printf 'CGO_ENABLED=%s\n' "${CGO_ENABLED-unset}"
  printf 'GOFLAGS=%s\n' "${GOFLAGS-unset}"
  printf 'GOROOT=%s\n' "${GOROOT-unset}"
  printf 'GOOS=%s\n' "${GOOS-unset}"
} > '` + log + `'
previous=""
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then printf 'binary' > "$argument"; fi
  previous="$argument"
done
`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// prepareFixture is one isolated run of bin/prepare: a stray Go compiler that
// is first on PATH and under GOROOT, and the output directory.
type prepareFixture struct {
	script    string
	strayRoot string
	strayLog  string
	output    string
	work      string
}

func newPrepareFixture(t *testing.T) prepareFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("bin/prepare is a bash script")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	script, err := filepath.Abs(filepath.Join(extensionRoot, "bin", "prepare"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fixture := prepareFixture{
		script:    script,
		strayRoot: filepath.Join(dir, "stray"),
		strayLog:  filepath.Join(dir, "stray.log"),
		output:    filepath.Join(dir, "output"),
		work:      filepath.Join(dir, "work"),
	}
	writeFakeGo(t, filepath.Join(fixture.strayRoot, "bin", "go"), fixture.strayLog, pinnedVersion)
	if err := os.MkdirAll(fixture.work, 0o755); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// ranStray reports whether bin/prepare ran the stray compiler for any
// purpose.
func (f prepareFixture) ranStray() bool {
	for _, log := range []string{f.strayLog, f.strayLog + ".version"} {
		if _, err := os.Stat(log); !os.IsNotExist(err) {
			return true
		}
	}
	return false
}

// assertNoRuntime fails the test when bin/prepare left any file in
// compiled/, a partial build included.
func (f prepareFixture) assertNoRuntime(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.output, "compiled"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("bin/prepare left %d entries in compiled/, want none", len(entries))
	}
}

// readRecorded reads the key=value lines a fake compiler recorded.
func readRecorded(t *testing.T, log string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the locked compiler did not record %s: %v", filepath.Base(log), err)
	}
	recorded := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, _ := strings.Cut(line, "=")
		recorded[key] = value
	}
	return recorded
}

// run runs bin/prepare with the stray compiler on PATH and as GOROOT, plus
// extra environment entries, and returns its exit code and stderr.
func (f prepareFixture) run(t *testing.T, extra ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("bash", f.script, "--output", f.output)
	cmd.Dir = f.work
	cmd.Env = append([]string{
		"PATH=" + filepath.Join(f.strayRoot, "bin") + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin",
		"GOROOT=" + f.strayRoot,
		"HOME=" + f.work,
	}, extra...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), stderr.String()
	}
	if err != nil {
		t.Fatalf("run bin/prepare: %v", err)
	}
	return 0, stderr.String()
}

// TestPrepareRefusesWithoutTheLockedCompiler pins that bin/prepare never
// falls back to a Go compiler on PATH or under GOROOT: without the compiler
// the host resolved at the lock's version, it fails, names the remedy and
// writes no runtime.
func TestPrepareRefusesWithoutTheLockedCompiler(t *testing.T) {
	t.Parallel()
	pin := lockedVersionEnvironment + "=" + pinnedVersion
	for name, extra := range map[string][]string{
		"unset":             {pin},
		"empty":             {pin, lockedCompilerEnvironment + "="},
		"not an executable": {pin, lockedCompilerEnvironment + "=" + filepath.Join(t.TempDir(), "missing-go")},
		"a directory":       {pin, lockedCompilerEnvironment + "=" + t.TempDir()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newPrepareFixture(t)
			code, stderr := fixture.run(t, extra...)
			if code != 1 {
				t.Errorf("exit %d, want 1; stderr %q", code, stderr)
			}
			if !strings.Contains(stderr, "putnami.lock.json") || !strings.Contains(stderr, "putnami install") {
				t.Errorf("stderr %q does not name the lock and the remedy", stderr)
			}
			if fixture.ranStray() {
				t.Error("bin/prepare ran a Go compiler from PATH or GOROOT")
			}
			fixture.assertNoRuntime(t)
		})
	}
}

// TestPrepareRefusesWithoutThePinnedVersion pins that bin/prepare runs no
// compiler when the host passes no pinned version to check it against.
func TestPrepareRefusesWithoutThePinnedVersion(t *testing.T) {
	t.Parallel()
	for name, extra := range map[string][]string{
		"unset": nil,
		"empty": {lockedVersionEnvironment + "="},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newPrepareFixture(t)
			lockedLog := filepath.Join(t.TempDir(), "locked.log")
			locked := filepath.Join(t.TempDir(), "go-locked", "bin", "go")
			writeFakeGo(t, locked, lockedLog, pinnedVersion)

			code, stderr := fixture.run(t, append(extra, lockedCompilerEnvironment+"="+locked)...)
			if code != 1 {
				t.Errorf("exit %d, want 1; stderr %q", code, stderr)
			}
			if !strings.Contains(stderr, lockedVersionEnvironment) {
				t.Errorf("stderr %q does not name %s", stderr, lockedVersionEnvironment)
			}
			for _, log := range []string{lockedLog, lockedLog + ".version"} {
				if _, err := os.Stat(log); !os.IsNotExist(err) {
					t.Errorf("bin/prepare ran the compiler without a pinned version (%s)", filepath.Base(log))
				}
			}
			if fixture.ranStray() {
				t.Error("bin/prepare ran a Go compiler from PATH or GOROOT")
			}
			fixture.assertNoRuntime(t)
		})
	}
}

// TestPrepareRefusesACompilerAtAnotherVersion pins that bin/prepare asks the
// resolved compiler for its GOVERSION in the environment it builds with and
// builds nothing when the answer differs from the pinned version.
func TestPrepareRefusesACompilerAtAnotherVersion(t *testing.T) {
	t.Parallel()
	fixture := newPrepareFixture(t)
	lockedLog := filepath.Join(t.TempDir(), "locked.log")
	locked := filepath.Join(t.TempDir(), "go-other", "bin", "go")
	writeFakeGo(t, locked, lockedLog, "go1.25.4")

	code, stderr := fixture.run(t,
		lockedCompilerEnvironment+"="+locked,
		lockedVersionEnvironment+"="+pinnedVersion,
		"GOTOOLCHAIN=auto",
	)
	if code != 1 {
		t.Errorf("exit %d, want 1; stderr %q", code, stderr)
	}
	for _, want := range []string{"go1.25.4", pinnedVersion, "putnami.lock.json", "putnami install"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q does not name %s", stderr, want)
		}
	}
	queried := readRecorded(t, lockedLog+".version")
	if queried["GOTOOLCHAIN"] != "local" || queried["GOROOT"] != "unset" {
		t.Errorf("the version query ran with %v, want the build environment", queried)
	}
	if _, err := os.Stat(lockedLog); !os.IsNotExist(err) {
		t.Error("bin/prepare built with a compiler at another version")
	}
	if fixture.ranStray() {
		t.Error("bin/prepare ran a Go compiler from PATH or GOROOT")
	}
	fixture.assertNoRuntime(t)
}

// TestPrepareBuildsWithTheLockedCompiler pins how bin/prepare builds the
// runtime: only with the compiler the host resolved, once that compiler
// reports the pinned version, outside any Go
// workspace, with the compiler's own root and toolchain, no cgo, no VCS stamp,
// no host paths and no ambient target, and it publishes the executable whole.
func TestPrepareBuildsWithTheLockedCompiler(t *testing.T) {
	t.Parallel()
	fixture := newPrepareFixture(t)
	lockedLog := filepath.Join(t.TempDir(), "locked.log")
	locked := filepath.Join(t.TempDir(), "go-locked", "bin", "go")
	writeFakeGo(t, locked, lockedLog, pinnedVersion)

	code, stderr := fixture.run(t,
		lockedCompilerEnvironment+"="+locked,
		lockedVersionEnvironment+"="+pinnedVersion,
		"GOWORK="+filepath.Join(fixture.work, "go.work"),
		"GOTOOLCHAIN=auto",
		"GOOS=windows",
		"CGO_ENABLED=1",
	)
	if code != 0 {
		t.Fatalf("exit %d; stderr %q", code, stderr)
	}
	if fixture.ranStray() {
		t.Error("bin/prepare ran a Go compiler from PATH or GOROOT")
	}
	queried := readRecorded(t, lockedLog+".version")
	if queried["GOTOOLCHAIN"] != "local" || queried["GOROOT"] != "unset" || queried["GOENV"] != "off" {
		t.Errorf("the version query ran with %v, want the build environment", queried)
	}
	recorded := readRecorded(t, lockedLog)
	args := strings.Fields(recorded["args"])
	if len(args) != 4 || args[0] != "build" || args[1] != "-o" || args[3] != "./cmd/putnami-dev" {
		t.Errorf("the compiler ran with %q, want build -o <file> ./cmd/putnami-dev", recorded["args"])
	}
	for key, want := range map[string]string{
		"GOWORK":      "off",
		"GOTOOLCHAIN": "local",
		"CGO_ENABLED": "0",
		"GOFLAGS":     "-mod=mod -buildvcs=false -trimpath",
		"GOROOT":      "unset",
		"GOOS":        "unset",
	} {
		if recorded[key] != want {
			t.Errorf("the compiler ran with %s=%q, want %q", key, recorded[key], want)
		}
	}
	compiled := filepath.Join(fixture.output, "compiled")
	info, err := os.Stat(filepath.Join(compiled, "putnami-dev"))
	if err != nil {
		t.Fatalf("bin/prepare did not publish the runtime: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("the runtime mode %v is not executable", info.Mode())
	}
	entries, err := os.ReadDir(compiled)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("compiled/ holds %d entries, want only the runtime", len(entries))
	}
}
