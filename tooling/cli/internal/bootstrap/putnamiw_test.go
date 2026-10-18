// Package bootstrap_test exercises the putnamiw bash wrapper: the --download
// path's prebuilt-CLI content-addressed sharing and staging cleanup, the
// historical mtime staleness gate, and the content-keyed from-source build
// cache. The Go-side artifact store has careful concurrency tests; these
// guard the bash reimplementation of the same publish/cleanup logic, and the
// key/rebuild decisions that determine WHICH engine a gate actually ran.
package bootstrap_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// findPutnamiw walks up from the test's working directory to the repo root that
// holds the putnamiw script.
func findPutnamiw(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		cand := filepath.Join(dir, "putnamiw")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("putnamiw not found above the test working directory")
		}
		dir = parent
	}
}

// writeExec writes an executable script.
func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// downloadEnv builds an isolated environment for a --download run: a copy of the
// script in a scratch "repo", a stub bin dir (stubbed curl + tar so no network
// or real archive is needed), a private PUTNAMI_HOME store, and a private TMPDIR.
// It returns the script path, the PUTNAMI_HOME, the TMPDIR, and the env slice.
func downloadEnv(t *testing.T, tarBody string) (script, home, tmp string, env []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("putnamiw is a bash script; skip on Windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	root := t.TempDir()
	src := findPutnamiw(t)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	script = filepath.Join(repo, "putnamiw")
	if err := os.WriteFile(script, data, 0o755); err != nil {
		t.Fatal(err)
	}

	stubBin := filepath.Join(root, "bin")
	// Stub curl: write fixed bytes to the -o target (no network).
	writeExec(t, filepath.Join(stubBin, "curl"), `#!/usr/bin/env bash
out=""
while [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift 2;; *) shift;; esac; done
[ -n "$out" ] && printf 'FAKE-ARCHIVE' > "$out"
exit 0
`)
	// Stub tar: extract a deterministic compiled/putnami into the -C dir. A stable
	// body keeps the content hash stable across runs, so the second run reuses.
	writeExec(t, filepath.Join(stubBin, "tar"), tarBody)

	home = filepath.Join(root, "home")
	tmp = filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	env = append(os.Environ(),
		"PATH="+stubBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PUTNAMI_HOME="+home,
		"TMPDIR="+tmp,
	)
	return script, home, tmp, env
}

const okTar = `#!/usr/bin/env bash
dir=""
while [ $# -gt 0 ]; do case "$1" in -C) dir="$2"; shift 2;; *) shift;; esac; done
mkdir -p "$dir/compiled"
cat > "$dir/compiled/putnami" <<'EOF'
#!/usr/bin/env bash
echo "stub-putnami $*"
EOF
chmod +x "$dir/compiled/putnami"
exit 0
`

func cliDir(home string) string { return filepath.Join(home, "artifacts", "cli") }

// soleBlob returns the single cli/<sha> dir, failing if there is not exactly one.
func soleBlob(t *testing.T, home string) string {
	t.Helper()
	entries, err := os.ReadDir(cliDir(home))
	if err != nil {
		t.Fatalf("read cli store: %v", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) != 1 {
		t.Fatalf("want exactly one CLI blob, got %v", dirs)
	}
	return filepath.Join(cliDir(home), dirs[0])
}

func TestPutnamiwDownload_PublishesSharedBlobAndStampsRecency(t *testing.T) {
	script, home, tmp, env := downloadEnv(t, okTar)

	cmd := exec.Command("bash", script, "--download", "ping")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("download run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "stub-putnami ping") {
		t.Errorf("expected the published CLI to be exec'd with passthrough args, got:\n%s", out)
	}

	blob := soleBlob(t, home)
	if fi, err := os.Stat(filepath.Join(blob, "putnami")); err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("published CLI binary missing or not executable: %v", err)
	}
	// Recency sidecar in the Go-GC-readable format (unix-nanoseconds).
	lu, err := os.ReadFile(filepath.Join(blob, "lastused"))
	if err != nil {
		t.Fatalf("lastused sidecar not written: %v", err)
	}
	if s := strings.TrimSpace(string(lu)); len(s) < 10 || !strings.HasSuffix(s, "000000000") {
		t.Errorf("lastused = %q, want unix-nanoseconds (second-granularity)", s)
	}
	// The worktree CLI is a symlink into the shared blob.
	link := filepath.Join(filepath.Dir(script), ".putnami", "bin", "putnami")
	if target, err := os.Readlink(link); err != nil {
		t.Errorf("CLI should be a symlink into the store: %v", err)
	} else if filepath.Dir(target) != blob {
		t.Errorf("symlink target = %q, want a file under %q", target, blob)
	}
	// No staging leaks: artifacts/tmp/ holds no leftover, and TMPDIR is clean.
	assertNoLeftovers(t, home, tmp)
}

func TestPutnamiwDownload_ReusesExistingBlob(t *testing.T) {
	script, home, tmp, env := downloadEnv(t, okTar)

	run := func() {
		cmd := exec.Command("bash", script, "--download", "ping")
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("download run failed: %v\n%s", err, out)
		}
	}
	run()
	blob := soleBlob(t, home)
	bin := filepath.Join(blob, "putnami")
	fi1, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}

	run() // second run: must reuse the same content-addressed blob, not republish
	if got := soleBlob(t, home); got != blob {
		t.Errorf("second run created a new blob %q, want reuse of %q", got, blob)
	}
	fi2, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fi1, fi2) {
		t.Error("second run rewrote the shared CLI inode instead of reusing it")
	}
	assertNoLeftovers(t, home, tmp)
}

func TestPutnamiwDownload_CleansStagingOnFailure(t *testing.T) {
	// A tar that fails after curl has written the archive: the EXIT trap must
	// reclaim the registered temp archive/dir rather than orphan them.
	script, home, tmp, env := downloadEnv(t, "#!/usr/bin/env bash\nexit 1\n")

	cmd := exec.Command("bash", script, "--download", "ping")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected failure when tar fails, got success:\n%s", out)
	}
	assertNoLeftovers(t, home, tmp)
}

// assertNoLeftovers checks that neither the artifact staging dir nor TMPDIR has
// orphaned putnami temp files.
func assertNoLeftovers(t *testing.T, home, tmp string) {
	t.Helper()
	if entries, err := os.ReadDir(filepath.Join(home, "artifacts", "tmp")); err == nil {
		for _, e := range entries {
			t.Errorf("leftover artifact staging entry: %s", e.Name())
		}
	}
	if entries, err := os.ReadDir(tmp); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "putnami-dl.") || strings.HasPrefix(e.Name(), "putnami-extract.") {
				t.Errorf("leftover temp in TMPDIR: %s", e.Name())
			}
		}
	}
}

// ─── from-source staleness gate ──────────────────────────────────────────────
//
// These exercise the fast-path rule that a from-source workspace must never run
// a binary older than its tooling/cli sources — even when that binary is reached
// through a `cli use`/`upgrade` symlink, which previously bypassed the
// staleness check and silently shadowed merged fixes (the stale-binary trap).

// sourceRepo builds an isolated from-source workspace that drives the staleness
// gate without a real toolchain: a copy of putnamiw, a go.work pin, a
// tooling/cli/cmd/putnami source tree, and a stub `go` that satisfies version
// resolution and, on `build -o OUT`, writes a sentinel binary printing
// "RAN_BUILT". The caller installs a pin and sets mtimes, then runs runScript.
func sourceRepo(t *testing.T) (repo string, env []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("putnamiw is a bash script; skip on Windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := t.TempDir()
	data, err := os.ReadFile(findPutnamiw(t))
	if err != nil {
		t.Fatal(err)
	}
	repo = filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "tooling", "cli", "cmd", "putnami"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "putnamiw"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.work"), []byte("go 1.23.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "tooling", "cli", "cmd", "putnami", "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stubBin := filepath.Join(root, "bin")
	// Stub go: version resolution + a `build` that writes a sentinel binary so no
	// real compiler (or network toolchain download) is needed.
	writeExec(t, filepath.Join(stubBin, "go"), `#!/usr/bin/env bash
case "$1" in
  env) case "$2" in GOVERSION) echo "go1.26.1";; GOROOT) echo "/fake/goroot";; *) echo "";; esac;;
  version) echo "go version go1.26.1 host";;
  build) shift; out=""
         while [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift 2;; *) shift;; esac; done
         [ -n "$out" ] && { printf '#!/usr/bin/env bash\necho RAN_BUILT\n' > "$out"; chmod +x "$out"; };;
esac
exit 0
`)
	env = append(os.Environ(),
		"PATH="+stubBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PUTNAMI_HOME="+filepath.Join(root, "home", ".putnami"),
	)
	return repo, env
}

// installPin writes a sibling pinned CLI printing "RAN_PINNED" and symlinks
// .putnami/bin/putnami at it — the relative-sibling form `cli use` creates.
// It returns the symlink path and the target binary path.
func installPin(t *testing.T, repo string) (link, target string) {
	t.Helper()
	binDir := filepath.Join(repo, ".putnami", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target = filepath.Join(binDir, "putnami-go-0.1.0-oldsha")
	writeExec(t, target, "#!/usr/bin/env bash\necho RAN_PINNED\n")
	link = filepath.Join(binDir, "putnami")
	if err := os.Symlink("putnami-go-0.1.0-oldsha", link); err != nil {
		t.Fatal(err)
	}
	return link, target
}

func chtime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func srcMain(repo string) string {
	return filepath.Join(repo, "tooling", "cli", "cmd", "putnami", "main.go")
}

func runScript(t *testing.T, repo string, env []string) string {
	t.Helper()
	cmd := exec.Command("bash", "./putnamiw", "ping")
	cmd.Dir = repo
	cmd.Env = env
	out, _ := cmd.CombinedOutput() // sentinels exit 0; assert on output, not status
	return string(out)
}

// A stale pin (sources newer than the binary it points at) must rebuild from
// source — even though the SYMLINK's own mtime is newest, the trap a naive
// `find -newer <symlink>` would fall for. The gate resolves to the target.
func TestPutnamiwStaleness_RebuildsOverStalePin(t *testing.T) {
	repo, env := sourceRepo(t)
	link, target := installPin(t, repo)
	now := time.Now()
	chtime(t, target, now.Add(-72*time.Hour)) // pinned binary is OLDEST
	chtime(t, srcMain(repo), now.Add(-24*time.Hour))
	// Recreate the symlink last so its own (lstat) mtime is newest.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("putnami-go-0.1.0-oldsha", link); err != nil {
		t.Fatal(err)
	}

	out := runScript(t, repo, env)
	if !strings.Contains(out, "RAN_BUILT") {
		t.Errorf("a stale pin must trigger a from-source rebuild; got:\n%s", out)
	}
	if strings.Contains(out, "RAN_PINNED") {
		t.Errorf("the stale pinned binary must not run; got:\n%s", out)
	}
}

// An up-to-date pin (newer than the sources) runs as-is — no needless rebuild.
func TestPutnamiwStaleness_RunsFreshPinAsIs(t *testing.T) {
	repo, env := sourceRepo(t)
	_, target := installPin(t, repo)
	now := time.Now()
	chtime(t, srcMain(repo), now.Add(-72*time.Hour))
	chtime(t, target, now)

	out := runScript(t, repo, env)
	if !strings.Contains(out, "RAN_PINNED") {
		t.Errorf("an up-to-date pin must run as-is; got:\n%s", out)
	}
	if strings.Contains(out, "RAN_BUILT") {
		t.Errorf("no rebuild expected for a fresh pin; got:\n%s", out)
	}
}

// A consumer workspace has no CLI sources, so a pin is version-governed and runs
// as-is — the gate must not try (and fail) to rebuild from absent sources.
func TestPutnamiwStaleness_ConsumerWorkspaceRunsPin(t *testing.T) {
	repo, env := sourceRepo(t)
	_, target := installPin(t, repo)
	if err := os.RemoveAll(filepath.Join(repo, "tooling")); err != nil {
		t.Fatal(err)
	}
	chtime(t, target, time.Now().Add(-72*time.Hour)) // old, but nothing to compare against

	out := runScript(t, repo, env)
	if !strings.Contains(out, "RAN_PINNED") {
		t.Errorf("a consumer-workspace pin must run as-is; got:\n%s", out)
	}
	if strings.Contains(out, "RAN_BUILT") {
		t.Errorf("a consumer workspace has no sources to rebuild from; got:\n%s", out)
	}
}

// ─── Bun's package cache ─────────────────────────────────────────────────────

// The wrapper names Bun's package cache for the CLI only on a machine that
// holds a Bun of its own: bun on PATH, or .bun in the home directory. On a
// machine with neither it exports no cache directory and creates none, so the
// Bun that Putnami installs under the Putnami home keeps its cache under its
// install and nothing is written to .bun in the home directory. An explicit
// cache directory is exported as it is.
func TestPutnamiwBunCache_FollowsTheBunOfTheMachine(t *testing.T) {
	sentinel := "#!/usr/bin/env bash\n" +
		"echo \"BUN_CACHE=${BUN_INSTALL_CACHE_DIR:-<unset>}\"\n" +
		"echo \"PUTNAMI_BUN_CACHE=${PUTNAMI_BUN_CACHE_DIR:-<unset>}\"\n"
	// The system directories hold bash and the tools the wrapper calls, and on
	// a machine prepared for these tests no bun.
	const systemPath = "/usr/bin:/bin"

	run := func(t *testing.T, prepare func(home, stubBin string) []string) (home, cache, putnamiCache string) {
		t.Helper()
		repo, _ := sourceRepo(t)
		if err := os.RemoveAll(filepath.Join(repo, "tooling")); err != nil {
			t.Fatal(err)
		}
		_, target := installPin(t, repo)
		writeExec(t, target, sentinel)

		root := filepath.Dir(repo)
		home = filepath.Join(root, "userhome")
		stubBin := filepath.Join(root, "tools")
		for _, dir := range []string{home, stubBin} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		env := []string{
			"HOME=" + home,
			"PUTNAMI_HOME=" + filepath.Join(home, ".putnami"),
			"PATH=" + stubBin + string(os.PathListSeparator) + systemPath,
		}
		env = append(env, prepare(home, stubBin)...)
		out, code := runIn(t, repo, env, "ping")
		if code != 0 {
			t.Fatalf("the wrapper failed (%d):\n%s", code, out)
		}
		return home, sentinelField(t, out, "BUN_CACHE"), sentinelField(t, out, "PUTNAMI_BUN_CACHE")
	}
	dirExists := func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && info.IsDir()
	}

	t.Run("a machine with no bun", func(t *testing.T) {
		for _, dir := range filepath.SplitList(systemPath) {
			if _, err := os.Stat(filepath.Join(dir, "bun")); err == nil {
				t.Skipf("%s holds a bun", dir)
			}
		}
		home, cache, putnamiCache := run(t, func(string, string) []string { return nil })
		if cache != "<unset>" || putnamiCache != "<unset>" {
			t.Errorf("BUN_INSTALL_CACHE_DIR = %q, PUTNAMI_BUN_CACHE_DIR = %q; want neither exported", cache, putnamiCache)
		}
		if _, err := os.Lstat(filepath.Join(home, ".bun")); !os.IsNotExist(err) {
			t.Errorf("the wrapper created .bun in the home directory: %v", err)
		}
	})

	t.Run("bun on PATH", func(t *testing.T) {
		home, cache, putnamiCache := run(t, func(_, stubBin string) []string {
			writeExec(t, filepath.Join(stubBin, "bun"), "#!/usr/bin/env bash\necho 1.4.0\n")
			return nil
		})
		want := filepath.Join(home, ".bun", "install", "cache")
		if cache != want || putnamiCache != want {
			t.Errorf("BUN_INSTALL_CACHE_DIR = %q, PUTNAMI_BUN_CACHE_DIR = %q; want %q for both", cache, putnamiCache, want)
		}
		if !dirExists(want) {
			t.Errorf("the wrapper did not create %s", want)
		}
	})

	t.Run("a bun home directory", func(t *testing.T) {
		home, cache, putnamiCache := run(t, func(home, _ string) []string {
			if err := os.MkdirAll(filepath.Join(home, ".bun"), 0o755); err != nil {
				t.Fatal(err)
			}
			return nil
		})
		want := filepath.Join(home, ".bun", "install", "cache")
		if cache != want || putnamiCache != want {
			t.Errorf("BUN_INSTALL_CACHE_DIR = %q, PUTNAMI_BUN_CACHE_DIR = %q; want %q for both", cache, putnamiCache, want)
		}
	})

	t.Run("an explicit cache directory", func(t *testing.T) {
		var explicit string
		home, cache, putnamiCache := run(t, func(home, _ string) []string {
			explicit = filepath.Join(home, "elsewhere", "bun-cache")
			return []string{"BUN_INSTALL_CACHE_DIR=" + explicit}
		})
		if cache != explicit || putnamiCache != explicit {
			t.Errorf("BUN_INSTALL_CACHE_DIR = %q, PUTNAMI_BUN_CACHE_DIR = %q; want %q for both", cache, putnamiCache, explicit)
		}
		if !dirExists(explicit) {
			t.Errorf("the wrapper did not create %s", explicit)
		}
		if _, err := os.Lstat(filepath.Join(home, ".bun")); !os.IsNotExist(err) {
			t.Errorf("the wrapper created .bun in the home directory: %v", err)
		}
	})
}

// ─── content-keyed from-source build cache ───────────────────────────────────
//
// In a git work tree the wrapper keys its build on the CONTENT of the sources it
// compiles, not on file mtimes and not on tooling/cli alone. That fixes two
// things at once: an edit under protocols/ or the extension SDK no longer leaves
// a stale binary running until somebody remembers --bootstrap, and two worktrees
// at the same tree state share one blob and one build.
//
// The stub `go` records every invocation, so "did it rebuild?" is answered by
// counting `build` lines rather than by reading log prose.

// gitSourceRepo builds an isolated from-source workspace that IS a git work
// tree, with sources both inside and outside tooling/cli, and a stub `go` that
// logs its invocations. It returns the repo, the shared PUTNAMI_HOME, and the
// env slice. GO_BUILD_FAIL names a file whose existence makes the stub's `build`
// fail; it does not exist until a test creates it. The CLI the stub builds fails
// when SENTINEL_FAIL exists, or when SENTINEL_NEEDS names no executable file.
func gitSourceRepo(t *testing.T) (repo, home string, env []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("putnamiw is a bash script; skip on Windows")
	}
	for _, tool := range []string{"bash", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	root := t.TempDir()
	repo = filepath.Join(root, "repo")
	seedSourceTree(t, repo)

	stubBin := filepath.Join(root, "bin")
	goLog := filepath.Join(root, "go.log")
	buildFail := filepath.Join(root, "go-build-fail")
	sentinelFail := filepath.Join(root, "sentinel-fail")
	writeExec(t, filepath.Join(stubBin, "go"), `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$GO_LOG"
case "$1" in
  env) case "$2" in GOVERSION) echo "go1.26.1";; GOROOT) echo "/fake/goroot";; *) echo "";; esac;;
  version) echo "go version go1.26.1 host";;
  build) printf '%s\n' "$PWD" >> "$GO_LOG.cwd"; shift; out=""
         while [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift 2;; *) shift;; esac; done
         if [ -f "$GO_BUILD_FAIL" ]; then echo "stub go: build refused" >&2; exit 1; fi
         [ -n "$out" ] && { cat > "$out" <<'SENTINEL'
#!/usr/bin/env bash
if [ -f "$SENTINEL_FAIL" ]; then echo "sentinel: $* refused" >&2; exit 7; fi
if [ -n "${SENTINEL_NEEDS:-}" ] && [ ! -x "$SENTINEL_NEEDS" ]; then echo "sentinel: $SENTINEL_NEEDS is missing" >&2; exit 8; fi
echo RAN_BUILT
echo "ARGS=$*"
echo "CWD=$PWD"
echo "SELF=$0"
echo "FROM_SOURCE=${PUTNAMI_FROM_SOURCE:-<unset>}"
echo "FROM_SOURCE_KEY=${PUTNAMI_FROM_SOURCE_KEY:-<unset>}"
echo "GOROOT=${GOROOT:-<unset>}"
echo "GOTOOLCHAIN=${GOTOOLCHAIN:-<unset>}"
echo "PATH_HEAD=${PATH%%:*}"
SENTINEL
         chmod +x "$out"; };;
esac
exit 0
`)

	home = filepath.Join(root, "home", ".putnami")
	env = append(os.Environ(),
		"PATH="+stubBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PUTNAMI_HOME="+home,
		"GO_LOG="+goLog,
		"GO_BUILD_FAIL="+buildFail,
		"SENTINEL_FAIL="+sentinelFail,
	)
	return repo, home, env
}

// seedSourceTree writes the wrapper plus one source file inside tooling/cli and
// one OUTSIDE it (under protocols/), then commits everything. The outside file
// is the regression this feature exists for.
func seedSourceTree(t *testing.T, repo string) {
	t.Helper()
	data, err := os.ReadFile(findPutnamiw(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{
		filepath.Join(repo, "tooling", "cli", "cmd", "putnami"),
		filepath.Join(repo, "protocols", "workspace"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(repo, rel), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("putnamiw", string(data), 0o755)
	write("go.work", "go 1.23.0\n", 0o644)
	write(filepath.Join("tooling", "cli", "cmd", "putnami", "main.go"), "package main\n\nfunc main() {}\n", 0o644)
	write(filepath.Join("protocols", "workspace", "lock.go"), "package workspace\n", 0o644)

	git(t, repo, "init", "-q", ".")
	git(t, repo, "add", "-A")
	git(t, repo, "-c", "commit.gpgsign=false", "commit", "-qm", "seed")
}

// git runs one git command in dir with a fixed identity, so the fixture does not
// depend on (or write to) the developer's git configuration.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=putnami test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=putnami test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// runIn runs the wrapper in dir and returns its combined output and exit code.
func runIn(t *testing.T, dir string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"./putnamiw"}, args...)...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run putnamiw: %v\n%s", err, out)
	}
	return string(out), code
}

// goBuildCount counts how many times the stub `go` was asked to build.
func goBuildCount(t *testing.T, env []string) int {
	t.Helper()
	data, err := os.ReadFile(envValue(env, "GO_LOG"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "build ") {
			count++
		}
	}
	return count
}

func envValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], key+"="); ok {
			return value
		}
	}
	return ""
}

func sourceBlobs(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "artifacts", "cli-source"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}
	sort.Strings(dirs)
	return dirs
}

// A second run at the same tree state must reuse the published blob and never
// invoke `go build` again.
func TestPutnamiwSourceKey_SecondRunReusesTheBlob(t *testing.T) {
	repo, home, env := gitSourceRepo(t)

	out, code := runIn(t, repo, env, "ping")
	if code != 0 || !strings.Contains(out, "RAN_BUILT") {
		t.Fatalf("first run: code=%d out=%s", code, out)
	}
	if got := goBuildCount(t, env); got != 1 {
		t.Fatalf("first run invoked `go build` %d times, want 1", got)
	}
	blobs := sourceBlobs(t, home)
	if len(blobs) != 1 {
		t.Fatalf("want exactly one from-source blob, got %v", blobs)
	}

	out, code = runIn(t, repo, env, "ping")
	if code != 0 || !strings.Contains(out, "RAN_BUILT") {
		t.Fatalf("second run: code=%d out=%s", code, out)
	}
	if got := goBuildCount(t, env); got != 1 {
		t.Errorf("second run rebuilt (`go build` count %d, want 1); the content key did not hold", got)
	}
	if strings.Contains(out, "Building putnami CLI") {
		t.Errorf("second run announced a build:\n%s", out)
	}
	if got := sourceBlobs(t, home); len(got) != 1 || got[0] != blobs[0] {
		t.Errorf("blobs = %v, want reuse of %v", got, blobs)
	}
}

// Two worktrees at the same tree state, sharing one PUTNAMI_HOME, must share one
// blob AND one build: the second adopts the first's binary without compiling.
func TestPutnamiwSourceKey_SiblingWorktreesShareOneBuild(t *testing.T) {
	repo, home, env := gitSourceRepo(t)
	sibling := filepath.Join(filepath.Dir(repo), "sibling")
	if out, err := exec.Command("cp", "-R", repo, sibling).CombinedOutput(); err != nil {
		t.Fatalf("copy worktree: %v\n%s", err, out)
	}

	if _, code := runIn(t, repo, env, "ping"); code != 0 {
		t.Fatalf("first worktree run failed (%d)", code)
	}
	if got := goBuildCount(t, env); got != 1 {
		t.Fatalf("first worktree: `go build` count %d, want 1", got)
	}

	out, code := runIn(t, sibling, env, "ping")
	if code != 0 || !strings.Contains(out, "RAN_BUILT") {
		t.Fatalf("sibling run: code=%d out=%s", code, out)
	}
	if got := goBuildCount(t, env); got != 1 {
		t.Errorf("the sibling worktree rebuilt (`go build` count %d, want 1)", got)
	}
	if got := sourceBlobs(t, home); len(got) != 1 {
		t.Errorf("blobs = %v, want one shared blob", got)
	}
	// The sibling's CLI is an absolute symlink into that shared blob.
	link := filepath.Join(sibling, ".putnami", "bin", "putnami")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("sibling CLI is not a symlink into the store: %v", err)
	}
	if !strings.HasPrefix(target, filepath.Join(home, "artifacts", "cli-source")) {
		t.Errorf("sibling symlink target = %q, want a blob under the shared store", target)
	}
}

// The regression this feature exists for: a change OUTSIDE tooling/cli must
// invalidate the binary. The old mtime gate watched tooling/cli only, so an edit
// under protocols/ shipped a stale engine behind a green run.
func TestPutnamiwSourceKey_EditOutsideToolingCliForcesRebuild(t *testing.T) {
	repo, home, env := gitSourceRepo(t)
	if _, code := runIn(t, repo, env, "ping"); code != 0 {
		t.Fatalf("first run failed (%d)", code)
	}
	if got := goBuildCount(t, env); got != 1 {
		t.Fatalf("first run: `go build` count %d, want 1", got)
	}

	outside := filepath.Join(repo, "protocols", "workspace", "lock.go")
	if err := os.WriteFile(outside, []byte("package workspace\n\n// changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := runIn(t, repo, env, "ping")
	if code != 0 || !strings.Contains(out, "RAN_BUILT") {
		t.Fatalf("run after the protocols/ edit: code=%d out=%s", code, out)
	}
	if got := goBuildCount(t, env); got != 2 {
		t.Errorf("`go build` count %d, want 2: an edit under protocols/ must change the key", got)
	}
	if got := sourceBlobs(t, home); len(got) != 2 {
		t.Errorf("blobs = %v, want two distinct keys", got)
	}
	spectest.Proves(t, "cli/engine-provenance", "content-keyed-source-build",
		"an-edit-anywhere-in-the-keyed-pathspec-rebuilds-and-an-unchanged-tree-reuses")
}

// An untracked new file is source too: `git status --untracked-files=all` names
// it and its working-tree bytes go into the key.
func TestPutnamiwSourceKey_UntrackedFileForcesRebuild(t *testing.T) {
	repo, _, env := gitSourceRepo(t)
	if _, code := runIn(t, repo, env, "ping"); code != 0 {
		t.Fatalf("first run failed (%d)", code)
	}

	added := filepath.Join(repo, "tooling", "cli", "cmd", "putnami", "extra.go")
	if err := os.WriteFile(added, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, code := runIn(t, repo, env, "ping"); code != 0 {
		t.Fatalf("run after adding an untracked file failed (%d)", code)
	}
	if got := goBuildCount(t, env); got != 2 {
		t.Errorf("`go build` count %d, want 2: an untracked .go file must change the key", got)
	}

	// Editing that untracked file again must move the key once more: the key
	// hashes its BYTES, not merely the fact that it exists.
	if err := os.WriteFile(added, []byte("package main\n\n// edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, code := runIn(t, repo, env, "ping"); code != 0 {
		t.Fatalf("run after editing the untracked file failed (%d)", code)
	}
	if got := goBuildCount(t, env); got != 3 {
		t.Errorf("`go build` count %d, want 3: untracked bytes must be part of the key", got)
	}
}

// A failed build is a hard failure. It must NOT fall back to the blob a previous
// run cached: a gate that ran an older engine while reporting on this commit is
// exactly the dishonest green this test guards against.
func TestPutnamiwSourceKey_FailedBuildNeverRunsACachedBlob(t *testing.T) {
	repo, _, env := gitSourceRepo(t)
	if _, code := runIn(t, repo, env, "ping"); code != 0 {
		t.Fatalf("first run failed (%d)", code)
	}

	// Move the tree so the cached blob no longer matches, then break the build.
	if err := os.WriteFile(filepath.Join(repo, "protocols", "workspace", "lock.go"),
		[]byte("package workspace\n\n// broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envValue(env, "GO_BUILD_FAIL"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := runIn(t, repo, env, "ping")
	if code == 0 {
		t.Fatalf("a failed build must exit non-zero, got 0:\n%s", out)
	}
	if strings.Contains(out, "RAN_BUILT") {
		t.Errorf("a failed build ran a cached binary anyway:\n%s", out)
	}
	if !strings.Contains(out, "Failed to build putnami CLI") {
		t.Errorf("the failure must say what broke:\n%s", out)
	}
}

// The rebuild runs from the workspace, whatever directory the caller is in. Go
// resolves the main module and go.work from its working directory, not from the
// package path, so a rebuild started from a scratch repository failed with
// "cannot find main module" (every agent script that fingerprints a temp repo).
func TestPutnamiwSourceKey_BuildsFromTheWorkspaceWhateverTheCallerDirectory(t *testing.T) {
	repo, _, env := gitSourceRepo(t)
	caller := t.TempDir()
	git(t, caller, "init", "-q", ".")

	cmd := exec.Command("bash", filepath.Join(repo, "putnamiw"), "ping")
	cmd.Dir = caller
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "RAN_BUILT") {
		t.Fatalf("run from another directory: %v\n%s", err, out)
	}
	logged, err := os.ReadFile(envValue(env, "GO_LOG") + ".cwd")
	if err != nil {
		t.Fatal(err)
	}
	built, err := filepath.EvalSymlinks(strings.TrimSpace(string(logged)))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if built != want {
		t.Errorf("`go build` ran in %s, want the workspace %s", built, want)
	}
}

// --bootstrap rebuilds and republishes even when the key already has a blob.
func TestPutnamiwSourceKey_BootstrapForcesRebuild(t *testing.T) {
	repo, home, env := gitSourceRepo(t)
	if _, code := runIn(t, repo, env, "ping"); code != 0 {
		t.Fatalf("first run failed (%d)", code)
	}
	before := sourceBlobs(t, home)

	if _, code := runIn(t, repo, env, "--bootstrap", "ping"); code != 0 {
		t.Fatalf("--bootstrap run failed (%d)", code)
	}
	if got := goBuildCount(t, env); got != 2 {
		t.Errorf("--bootstrap must rebuild: `go build` count %d, want 2", got)
	}
	if after := sourceBlobs(t, home); len(after) != len(before) {
		t.Errorf("--bootstrap must republish under the same key: blobs %v → %v", before, after)
	}
}

// A workspace that is NOT a git work tree (a tarball checkout) has no key, so it
// keeps the historical mtime gate and publishes nothing to the shared store.
// This is asserted, not assumed: the fixture would silently exercise the key
// path if the temp dir happened to sit inside a repository.
func TestPutnamiwSourceKey_NonGitWorkspaceUsesTheMtimeGate(t *testing.T) {
	repo, env := sourceRepo(t)
	assertNotAGitWorkTree(t, repo)

	out := runScript(t, repo, env)
	if !strings.Contains(out, "RAN_BUILT") {
		t.Fatalf("a non-git from-source workspace must still build:\n%s", out)
	}
	home := envValue(env, "PUTNAMI_HOME")
	if blobs := sourceBlobs(t, home); len(blobs) != 0 {
		t.Errorf("a keyless workspace must publish nothing to the shared store, got %v", blobs)
	}
	// Keyless builds keep the historical per-worktree binary and the empty
	// presence-only marker.
	binary := filepath.Join(repo, ".putnami", "bin", "putnami")
	if fi, err := os.Lstat(binary); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("keyless build should be a regular per-worktree file: %v %v", fi, err)
	}
	marker, err := os.ReadFile(filepath.Join(repo, ".putnami", "bin", ".from-source"))
	if err != nil {
		t.Fatalf("keyless build must still mark provenance: %v", err)
	}
	if len(marker) != 0 {
		t.Errorf("keyless marker = %q, want empty (no key to record)", marker)
	}
}

// assertNotAGitWorkTree fails loudly rather than skipping, so the mtime-gate
// tests can never pass vacuously by accidentally exercising the key path.
func assertNotAGitWorkTree(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		return
	}
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("fixture %s is inside a git work tree (%s), so it exercises the content-key path "+
			"instead of the mtime gate these tests cover. Run the suite with TMPDIR outside a repository.",
			dir, strings.TrimSpace(string(out)))
	}
}

// The marker alone does not vouch for a binary. `putnami cli use` and
// `upgrade --from-source` both re-point .putnami/bin/putnami without touching
// the marker, so the fast path also checks that the link resolves to THIS key's
// blob. Without that check the wrapper would run a binary the marker never
// described, and would tell the launcher it came from this tree.
func TestPutnamiwSourceKey_RepointedLinkIsNotTrusted(t *testing.T) {
	repo, home, env := gitSourceRepo(t)
	if _, code := runIn(t, repo, env, "ping"); code != 0 {
		t.Fatalf("first run failed (%d)", code)
	}
	blobs := sourceBlobs(t, home)
	if len(blobs) != 1 {
		t.Fatalf("want one blob after the first run, got %v", blobs)
	}

	// Re-point the CLI at a foreign binary, exactly as `cli use` would,
	// leaving the marker's key untouched.
	binDir := filepath.Join(repo, ".putnami", "bin")
	foreign := filepath.Join(binDir, "putnami-go-0.1.0-elsewhere")
	writeExec(t, foreign, "#!/usr/bin/env bash\necho RAN_FOREIGN\n")
	link := filepath.Join(binDir, "putnami")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("putnami-go-0.1.0-elsewhere", link); err != nil {
		t.Fatal(err)
	}

	out, code := runIn(t, repo, env, "ping")
	if code != 0 {
		t.Fatalf("run after re-pointing failed (%d):\n%s", code, out)
	}
	if strings.Contains(out, "RAN_FOREIGN") {
		t.Errorf("the wrapper ran a re-pointed foreign binary:\n%s", out)
	}
	if !strings.Contains(out, "RAN_BUILT") {
		t.Errorf("the wrapper must restore this key's blob:\n%s", out)
	}
	// The tree did not change, so the store already holds the right blob: it is
	// re-linked, not rebuilt.
	if got := goBuildCount(t, env); got != 1 {
		t.Errorf("`go build` count %d, want 1: an unchanged tree needs a relink, not a rebuild", got)
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("CLI should be re-linked into the store: %v", err)
	}
	if want := filepath.Join(home, "artifacts", "cli-source", blobs[0], "putnami"); target != want {
		t.Errorf("link target = %q, want %q", target, want)
	}
}

// ─── the session engine pin ──────────────────────────────────────────────────
//
// A keyed from-source run must hand the CLI an identity no concurrent invocation
// can change: the immutable store blob, plus the key that names it. The engine
// link stays published for humans and for tools that put .putnami/bin on PATH,
// but it is not what the kernel starts and not what the launcher checks.

// sentinelField reads one `KEY=value` line the built sentinel prints.
func sentinelField(t *testing.T, out, key string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return value
		}
	}
	t.Fatalf("sentinel printed no %s line:\n%s", key, out)
	return ""
}

// The wrapper execs the store blob, not .putnami/bin/putnami, and exports the key
// that names it, on every keyed path into the CLI. os.Executable() reports the
// path the kernel started, the CLI advertises that path to extensions which call
// back into it, and every re-entrant child then presents it as its own identity —
// so exec'ing the symlink would give the whole process tree an identity another
// run can redefine. The cold build, the warm fast path, and the store hit are all
// covered, because a pin that appeared on only one of them would make the proof
// depend on cache state.
func TestPutnamiwSourceKey_ExecsTheBlobAndExportsTheSessionPin(t *testing.T) {
	repo, home, env := gitSourceRepo(t)
	out, code := runIn(t, repo, env, "ping") // cold build
	if code != 0 {
		t.Fatalf("cold run failed (%d):\n%s", code, out)
	}
	blobs := sourceBlobs(t, home)
	if len(blobs) != 1 {
		t.Fatalf("want one blob, got %v", blobs)
	}
	key := blobs[0]
	blob := filepath.Join(home, "artifacts", "cli-source", key, "putnami")

	// The wrapper resolves its own directory, so on macOS /var becomes /private/var.
	// The launcher compares resolved paths for exactly this reason.
	wantRoot, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := sentinelField(t, out, "FROM_SOURCE"); got != wantRoot {
		t.Errorf("PUTNAMI_FROM_SOURCE = %q, want the workspace root %q", got, wantRoot)
	}

	assertPinned := func(path, out string) {
		t.Helper()
		if got := sentinelField(t, out, "SELF"); got != blob {
			t.Errorf("%s started %q; it must start the immutable store blob %q", path, got, blob)
		}
		if got := sentinelField(t, out, "FROM_SOURCE_KEY"); got != key {
			t.Errorf("%s pinned %q, want the source key %q", path, got, key)
		}
	}
	assertPinned("the cold build", out)

	// Warm fast path: marker and link both agree with the key.
	if out, code = runIn(t, repo, env, "ping"); code != 0 {
		t.Fatalf("warm run failed (%d):\n%s", code, out)
	}
	assertPinned("the warm fast path", out)

	// Store hit: a fresh worktree at the same tree state adopts the blob without
	// compiling. Wiping .putnami/bin drops both the marker and the link.
	if err := os.RemoveAll(filepath.Join(repo, ".putnami", "bin")); err != nil {
		t.Fatal(err)
	}
	if out, code = runIn(t, repo, env, "ping"); code != 0 {
		t.Fatalf("store-hit run failed (%d):\n%s", code, out)
	}
	assertPinned("the store hit", out)

	if got := goBuildCount(t, env); got != 1 {
		t.Errorf("`go build` count %d, want 1: warm and store-hit paths must not compile", got)
	}
	spectest.Proves(t, "cli/engine-provenance", "foreign-engine-refused",
		"a-keyed-from-source-run-execs-the-immutable-blob-and-exports-its-key")
}

// The wrapper runs the CLI with the caller's GOROOT, PATH and GOTOOLCHAIN on a
// run that builds and on a run that reuses the blob. The resolved Go's GOROOT,
// its bin directory first on PATH, and GOTOOLCHAIN=local reach the build
// command alone.
func TestPutnamiwSourceKey_BuildEnvironmentStaysOutOfTheCLI(t *testing.T) {
	repo, _, env := gitSourceRepo(t)
	stubBin := strings.Split(envValue(env, "PATH"), string(os.PathListSeparator))[0]
	env = append(env, "GOROOT=/caller/goroot", "GOTOOLCHAIN=auto")

	assertCallerEnvironment := func(path, out string) {
		t.Helper()
		for key, want := range map[string]string{"GOROOT": "/caller/goroot", "GOTOOLCHAIN": "auto", "PATH_HEAD": stubBin} {
			if got := sentinelField(t, out, key); got != want {
				t.Errorf("%s ran the CLI with %s=%q, want the caller's %q", path, key, got, want)
			}
		}
	}

	out, code := runIn(t, repo, env, "ping")
	if code != 0 {
		t.Fatalf("cold run failed (%d):\n%s", code, out)
	}
	if got := goBuildCount(t, env); got != 1 {
		t.Fatalf("cold run invoked `go build` %d times, want 1", got)
	}
	assertCallerEnvironment("the cold build", out)

	if out, code = runIn(t, repo, env, "ping"); code != 0 {
		t.Fatalf("warm run failed (%d):\n%s", code, out)
	}
	assertCallerEnvironment("the warm fast path", out)
	spectest.Proves(t, "cli/engine-provenance", "build-toolchain-stays-in-the-build",
		"the-cli-receives-the-callers-go-environment-whether-or-not-the-run-builds")
}

// A system Go older than the workspace's Go is not the build's Go, and neither
// is the toolchain that Go downloads into its module cache under
// GOTOOLCHAIN=auto: the wrapper builds with the managed install under the
// Putnami home, where the CLI it runs looks for the Go the lock pins. The
// workspace's Go is the higher of the go and toolchain lines of go.work, as
// the lock pins it. The build command receives the managed Go's GOROOT, its
// bin directory first on PATH, and GOTOOLCHAIN=local; the CLI receives the
// caller's.
func TestPutnamiwSourceKey_OlderSystemGoBuildsWithTheManagedInstall(t *testing.T) {
	for name, goWork := range map[string]string{
		"go line":        "go 1.30.0\n",
		"toolchain line": "go 1.26.0\n\ntoolchain go1.30.0\n",
	} {
		t.Run(name, func(t *testing.T) {
			repo, home, env := gitSourceRepo(t)
			stubBin := strings.Split(envValue(env, "PATH"), string(os.PathListSeparator))[0]
			if err := os.WriteFile(filepath.Join(repo, "go.work"), []byte(goWork), 0o644); err != nil {
				t.Fatal(err)
			}
			wrongBuild := envValue(env, "GO_LOG") + ".wrong"
			modCacheRoot := filepath.Join(t.TempDir(), "toolchain@v0.0.1-go1.30.0")
			writeExec(t, filepath.Join(modCacheRoot, "bin", "go"), `#!/usr/bin/env bash
case "$1" in
  env) [ "$2" = GOVERSION ] && echo go1.30.0; [ "$2" = GOROOT ] && echo "`+modCacheRoot+`";;
  build) echo module-cache >> "`+wrongBuild+`";;
esac
exit 0
`)
			systemBin := t.TempDir()
			writeExec(t, filepath.Join(systemBin, "go"), `#!/usr/bin/env bash
case "$1" in
  env) case "$2" in
         GOVERSION) if [ "$GOTOOLCHAIN" = auto ]; then echo go1.30.0; else echo go1.26.1; fi;;
         GOROOT) if [ "$GOTOOLCHAIN" = auto ]; then echo "`+modCacheRoot+`"; else echo /system/goroot; fi;;
       esac;;
  build) echo system >> "`+wrongBuild+`";;
esac
exit 0
`)
			managedRoot := filepath.Join(home, "toolchains", "go", "go-1.30.0", "go")
			buildEnv := envValue(env, "GO_LOG") + ".managed"
			writeExec(t, filepath.Join(managedRoot, "bin", "go"), `#!/usr/bin/env bash
case "$1" in
  env) [ "$2" = GOVERSION ] && echo go1.30.0; [ "$2" = GOROOT ] && echo "`+managedRoot+`"; exit 0;;
  build) printf '%s|%s|%s\n' "${GOROOT:-<unset>}" "${GOTOOLCHAIN:-<unset>}" "${PATH%%:*}" >> "`+buildEnv+`";;
esac
exec "`+filepath.Join(stubBin, "go")+`" "$@"
`)
			env = append(env, "PATH="+systemBin+string(os.PathListSeparator)+envValue(env, "PATH"))

			out, code := runIn(t, repo, env, "ping")
			if code != 0 || !strings.Contains(out, "RAN_BUILT") {
				t.Fatalf("run failed (%d):\n%s", code, out)
			}
			if data, err := os.ReadFile(wrongBuild); err == nil {
				t.Fatalf("the CLI was built by %s, want the managed install", strings.TrimSpace(string(data)))
			}
			data, err := os.ReadFile(buildEnv)
			if err != nil {
				t.Fatalf("the managed Go did not build the CLI: %v\n%s", err, out)
			}
			want := managedRoot + "|local|" + filepath.Join(managedRoot, "bin")
			if got := strings.TrimSpace(string(data)); got != want {
				t.Errorf("build environment GOROOT|GOTOOLCHAIN|PATH head = %q, want %q", got, want)
			}
			if got := sentinelField(t, out, "PATH_HEAD"); got != systemBin {
				t.Errorf("the CLI ran with PATH head %q, want the caller's %q", got, systemBin)
			}
		})
	}
	spectest.Proves(t, "cli/engine-provenance", "build-toolchain-stays-in-the-build",
		"an-older-system-go-builds-with-the-managed-install")
}

// The wrapper installs a Go archive it downloads only when the archive's
// SHA-256 is the one the workspace lock records for that release and platform,
// or, when the lock pins another release, the one go.dev publishes beside the
// archive. Any other archive stops the run, and nothing is installed or built.
func TestPutnamiwSourceKey_DownloadedGoMustMatchItsSHA256(t *testing.T) {
	requireGoDownloadPlatform(t)
	const archive = "FAKE-GO-ARCHIVE"
	sum := sha256.Sum256([]byte(archive))
	digest := hex.EncodeToString(sum[:])
	wrong := strings.Repeat("0", 64)

	cases := []struct {
		name, lockVersion, lockDigest, published string
		installs                                 bool
		message                                  string
	}{
		{name: "the lock records the archive", lockVersion: "1.30.0", lockDigest: digest, installs: true},
		{name: "the lock records another archive", lockVersion: "1.30.0", lockDigest: wrong, published: digest, message: "Refusing Go 1.30.0"},
		{name: "go.dev publishes the archive", lockVersion: "1.29.0", lockDigest: wrong, published: digest, installs: true},
		{name: "go.dev publishes another archive", lockVersion: "1.29.0", lockDigest: digest, published: wrong, message: "Refusing Go 1.30.0"},
		{name: "no checksum anywhere", lockVersion: "1.29.0", lockDigest: digest, message: "Cannot verify Go 1.30.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, home, env := gitSourceRepo(t)
			stubBin := strings.Split(envValue(env, "PATH"), string(os.PathListSeparator))[0]
			if err := os.WriteFile(filepath.Join(repo, "go.work"), []byte("go 1.30.0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			writeGoLock(t, repo, c.lockVersion, c.lockDigest)
			downloadBin, _ := fakeGoDownload(t, archive, c.published, filepath.Join(stubBin, "go"))
			env = append(env, "PATH="+downloadBin+string(os.PathListSeparator)+envValue(env, "PATH"))

			out, code := runIn(t, repo, env, "ping")
			installed := filepath.Join(home, "toolchains", "go", "go-1.30.0", "go", "bin", "go")
			_, statErr := os.Stat(installed)
			if c.installs {
				if code != 0 || !strings.Contains(out, "RAN_BUILT") || statErr != nil {
					t.Fatalf("run = %d, install %v, want the verified Go installed and the CLI run:\n%s", code, statErr, out)
				}
				return
			}
			if code == 0 || !strings.Contains(out, c.message) {
				t.Errorf("run = %d, want a failure naming %q:\n%s", code, c.message, out)
			}
			if statErr == nil {
				t.Errorf("an unverified archive was installed at %s", installed)
			}
			if got := goBuildCount(t, env); got != 0 {
				t.Errorf("`go build` ran %d times after an unverified download, want 0", got)
			}
		})
	}
	spectest.Proves(t, "cli/engine-provenance", "build-toolchain-stays-in-the-build",
		"a-downloaded-go-is-installed-only-at-its-locked-or-published-sha256")
}

// requireGoDownloadPlatform skips the test on a platform for which the wrapper
// downloads no Go.
func requireGoDownloadPlatform(t *testing.T) {
	t.Helper()
	switch platform := runtime.GOOS + "/" + runtime.GOARCH; platform {
	case "darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64":
	default:
		t.Skipf("the wrapper downloads no Go for %s", platform)
	}
}

// writeGoLock writes a workspace lock in the layout the CLI writes, pinning Go
// version with digest as the SHA-256 of its archive for the host platform.
func writeGoLock(t *testing.T, repo, version, digest string) {
	t.Helper()
	type goPin struct {
		Version     string            `json:"version"`
		Integrities map[string]string `json:"integrities"`
		Source      string            `json:"source"`
	}
	type lockFile struct {
		Version    int              `json:"version"`
		Toolchains map[string]goPin `json:"toolchains"`
	}
	lock, err := json.MarshalIndent(lockFile{
		Version: 3,
		Toolchains: map[string]goPin{"go": {
			Version:     version,
			Integrities: map[string]string{runtime.GOOS + "/" + runtime.GOARCH: digest},
			Source:      "https://go.dev/dl/",
		}},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "putnami.lock.json"), lock, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeGoDownload writes a curl that answers a .sha256 URL with published, or
// fails when published is empty, and any other URL with archive, and a tar that
// extracts any archive to a go/bin/go that runs stubGo. The curl appends each
// URL it is given to curlLog. It returns the directory to put first on PATH.
func fakeGoDownload(t *testing.T, archive, published, stubGo string) (bin, curlLog string) {
	t.Helper()
	bin = t.TempDir()
	curlLog = filepath.Join(t.TempDir(), "curl.log")
	writeExec(t, filepath.Join(bin, "curl"), `#!/usr/bin/env bash
out=""; url=""
while [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift 2;; -*) shift;; *) url="$1"; shift;; esac; done
printf '%s\n' "$url" >> "`+curlLog+`"
case "$url" in
  *.sha256) [ -n "`+published+`" ] || exit 22; echo "`+published+`"; exit 0;;
esac
printf '`+archive+`' > "$out"
`)
	writeExec(t, filepath.Join(bin, "tar"), `#!/usr/bin/env bash
dir=""
while [ $# -gt 0 ]; do case "$1" in -C) dir="$2"; shift 2;; *) shift;; esac; done
mkdir -p "$dir/go/bin"
printf '#!/usr/bin/env bash\nexec "%s" "$@"\n' "`+stubGo+`" > "$dir/go/bin/go"
chmod +x "$dir/go/bin/go"
`)
	return bin, curlLog
}

// sha256_of, which checks a downloaded Go archive, hashes with openssl on a
// host that has neither sha256sum nor shasum.
func TestPutnamiwSHA256OfFallsBackToOpenSSL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("putnamiw is a bash script; skip on Windows")
	}
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	data, err := os.ReadFile(findPutnamiw(t))
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	start := strings.Index(script, "\nsha256_of() {\n")
	if start < 0 {
		t.Fatal("putnamiw defines no sha256_of function")
	}
	end := strings.Index(script[start:], "\n}\n")
	if end < 0 {
		t.Fatal("sha256_of has no closing brace")
	}
	function := script[start : start+end+3]

	tools := t.TempDir()
	for _, tool := range []string{"awk", "openssl"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not available", tool)
		}
		if err := os.Symlink(path, filepath.Join(tools, tool)); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(t.TempDir(), "archive")
	if err := os.WriteFile(file, []byte("FAKE-GO-ARCHIVE"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bashPath, "-c", function+`sha256_of "$1"`, "bash", file)
	cmd.Env = []string{"PATH=" + tools}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sha256_of: %v\n%s", err, out)
	}
	sum := sha256.Sum256([]byte("FAKE-GO-ARCHIVE"))
	if got, want := strings.TrimSpace(string(out)), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("sha256_of with openssl alone = %q, want %q", got, want)
	}
	spectest.Proves(t, "cli/engine-provenance", "build-toolchain-stays-in-the-build",
		"a-downloaded-go-is-installed-only-at-its-locked-or-published-sha256")
}

// A concurrent `./putnamiw` at another tree state re-points the shared engine
// link — legitimately, that is how the wrapper keeps the engine current. The pin
// a live run already holds must be unaffected, because the blob it names is
// content-addressed and nothing rewrites it. This is the wrapper half of the
// fix; the launcher half is in internal/launch.
func TestPutnamiwSourceKey_ConcurrentRelinkLeavesAnEarlierPinIntact(t *testing.T) {
	repo, home, env := gitSourceRepo(t)
	if out, code := runIn(t, repo, env, "ping"); code != 0 {
		t.Fatalf("first run failed (%d):\n%s", code, out)
	}
	first := sourceBlobs(t, home)
	if len(first) != 1 {
		t.Fatalf("want one blob, got %v", first)
	}
	firstBlob := filepath.Join(home, "artifacts", "cli-source", first[0], "putnami")

	// Another run at a DIFFERENT tree state: a new key, a new blob, and the shared
	// link moved to it.
	if err := os.WriteFile(filepath.Join(repo, "protocols", "workspace", "lock.go"),
		[]byte("package workspace\n\n// concurrent edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runIn(t, repo, env, "ping")
	if code != 0 {
		t.Fatalf("second run failed (%d):\n%s", code, out)
	}
	second := sourceBlobs(t, home)
	if len(second) != 2 {
		t.Fatalf("want two blobs after an edit, got %v", second)
	}
	if got := sentinelField(t, out, "SELF"); got == firstBlob {
		t.Errorf("the second run must start its own blob, not %q", firstBlob)
	}

	// The first run's pin still names bytes that exist and are unchanged: the
	// store is immutable and published first-writer-wins, so a process started
	// from it keeps a valid identity for its whole life.
	info, err := os.Stat(firstBlob)
	if err != nil {
		t.Fatalf("the earlier blob must survive a concurrent relink: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("the earlier blob is no longer executable: %v", info.Mode())
	}
	spectest.Proves(t, "cli/engine-provenance", "content-keyed-source-build",
		"a-concurrent-run-at-another-tree-state-never-rewrites-an-earlier-blob")
}

// The pin is actively UNSET on every path that did not select a keyed blob, not
// merely left alone. An inherited value would otherwise launder the next binary
// into looking like this tree's engine: a nested wrapper call from a checkout that
// HAS a key, into one that does not, would hand the keyless build a pin naming
// some other tree's blob. The keyless mtime fallback is that path — no git work
// tree, so no content key and no store blob to name.
func TestPutnamiwSourceKey_KeylessRunUnsetsAnInheritedPin(t *testing.T) {
	repo, _, env := gitSourceRepo(t)
	// Drop the work tree: no git, so no source key, so the mtime fallback runs.
	if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	env = append(env,
		"PUTNAMI_FROM_SOURCE_KEY=726e724cdeadbeef", // inherited from somewhere else
	)

	out, code := runIn(t, repo, env, "ping")
	if code != 0 {
		t.Fatalf("keyless run failed (%d):\n%s", code, out)
	}
	if got := sentinelField(t, out, "FROM_SOURCE_KEY"); got != "<unset>" {
		t.Errorf("PUTNAMI_FROM_SOURCE_KEY = %q; a keyless run must unset an inherited pin", got)
	}
	// The root marker is still exported: the binary did come from this tree, and
	// the launcher's fallback proof (the engine link) is what admits it there. The
	// wrapper resolves its own directory, so compare against the resolved spelling.
	wantRoot, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := sentinelField(t, out, "FROM_SOURCE"); got != wantRoot {
		t.Errorf("PUTNAMI_FROM_SOURCE = %q, want the workspace root %q", got, wantRoot)
	}
	if got := sentinelField(t, out, "SELF"); got != filepath.Join(wantRoot, ".putnami", "bin", "putnami") {
		t.Errorf("a keyless build runs the per-worktree binary, got %q", got)
	}
}

// ─── build-only mode ─────────────────────────────────────────────────────────
//
// `./putnamiw --print-engine` builds or reuses the keyed store blob, runs only
// `putnami extensions prepare` with it, and prints one JSON line naming it. A
// launcher then starts that blob itself, in another process, with the
// environment it chooses. Nothing under .putnami/bin moves, so the build has no
// side effect a later process could read as its own.

// printedEngine is the one JSON line --print-engine writes on stdout.
type printedEngine struct {
	Path string `json:"path"`
	Key  string `json:"key"`
	Root string `json:"root"`
}

// runSplit runs the wrapper in dir and returns stdout, stderr, and the exit
// code separately: the mode's contract is about what reaches stdout.
func runSplit(t *testing.T, dir string, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"./putnamiw"}, args...)...)
	cmd.Dir = dir
	cmd.Env = env
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run putnamiw: %v\n%s", err, errOut.String())
	}
	return out.String(), errOut.String(), code
}

// decodeEngine parses stdout as exactly one JSON line with exactly the three
// documented members.
func decodeEngine(t *testing.T, stdout string) printedEngine {
	t.Helper()
	if !strings.HasSuffix(stdout, "\n") || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("stdout must be exactly one line, got %q", stdout)
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	var engine printedEngine
	if err := dec.Decode(&engine); err != nil {
		t.Fatalf("stdout is not the documented JSON object: %v\n%q", err, stdout)
	}
	return engine
}

// samePath compares two spellings of one path after resolving symlinks, since
// the temporary directory has a /var and a /private/var spelling on macOS.
func samePath(t *testing.T, got, want string) bool {
	t.Helper()
	g, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("resolve %s: %v", got, err)
	}
	w, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("resolve %s: %v", want, err)
	}
	return g == w
}

func assertNoWorkspaceBin(t *testing.T, repo string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(repo, ".putnami", "bin")); !os.IsNotExist(err) {
		t.Errorf("--print-engine wrote .putnami/bin: %v", err)
	}
}

// The mode builds the blob once, prints its absolute path, key and root, and
// leaves .putnami/bin absent. The one command it starts with the blob is
// `extensions prepare`, in the workspace root, admitted by the key that names
// the blob, with its output on stderr. A second call at the same tree state
// reuses the blob without compiling and prints the same line.
func TestPutnamiwPrintEngine_PrintsTheBlobAndRunsOnlyExtensionsPrepare(t *testing.T) {
	repo, home, env := gitSourceRepo(t)

	stdout, stderr, code := runSplit(t, repo, env, "--print-engine")
	if code != 0 {
		t.Fatalf("--print-engine failed (%d):\n%s", code, stderr)
	}
	if strings.Contains(stdout, "RAN_BUILT") {
		t.Errorf("--print-engine wrote the output of the CLI on stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "Building putnami CLI") {
		t.Errorf("progress must go to stderr, got:\n%s", stderr)
	}
	engine := decodeEngine(t, stdout)
	if got := strings.Count(stderr, "RAN_BUILT"); got != 1 {
		t.Fatalf("--print-engine started the CLI %d times, want once:\n%s", got, stderr)
	}
	if got := sentinelField(t, stderr, "ARGS"); got != "extensions prepare" {
		t.Errorf("--print-engine ran `putnami %s`, want only `putnami extensions prepare`", got)
	}
	if got := sentinelField(t, stderr, "CWD"); !samePath(t, got, repo) {
		t.Errorf("extensions prepare ran in %q, want the workspace %q", got, repo)
	}
	if got := sentinelField(t, stderr, "SELF"); !samePath(t, got, engine.Path) {
		t.Errorf("extensions prepare ran %q, want the printed blob %q", got, engine.Path)
	}
	if got := sentinelField(t, stderr, "FROM_SOURCE"); !samePath(t, got, repo) {
		t.Errorf("extensions prepare names the source %q, want the workspace %q", got, repo)
	}
	if got := sentinelField(t, stderr, "FROM_SOURCE_KEY"); got != engine.Key {
		t.Errorf("extensions prepare names the key %q, want the printed key %q", got, engine.Key)
	}

	blobs := sourceBlobs(t, home)
	if len(blobs) != 1 {
		t.Fatalf("want one from-source blob, got %v", blobs)
	}
	if engine.Key != blobs[0] {
		t.Errorf("key = %q, want the store key %q", engine.Key, blobs[0])
	}
	if !filepath.IsAbs(engine.Path) || !filepath.IsAbs(engine.Root) {
		t.Errorf("path %q and root %q must be absolute", engine.Path, engine.Root)
	}
	if !samePath(t, engine.Path, filepath.Join(home, "artifacts", "cli-source", engine.Key, "putnami")) {
		t.Errorf("path = %q, want the store blob of key %s", engine.Path, engine.Key)
	}
	if !samePath(t, engine.Root, repo) {
		t.Errorf("root = %q, want the workspace %q", engine.Root, repo)
	}
	assertNoWorkspaceBin(t, repo)
	if got := goBuildCount(t, env); got != 1 {
		t.Fatalf("`go build` count %d, want 1", got)
	}

	again, stderr, code := runSplit(t, repo, env, "--print-engine")
	if code != 0 {
		t.Fatalf("second --print-engine failed (%d):\n%s", code, stderr)
	}
	if again != stdout {
		t.Errorf("second call printed %q, want the same line %q", again, stdout)
	}
	if got := goBuildCount(t, env); got != 1 {
		t.Errorf("second call rebuilt (`go build` count %d, want 1)", got)
	}
	assertNoWorkspaceBin(t, repo)

	// The printed binary is the one a normal run executes for this key.
	out, code := runIn(t, repo, env, "ping")
	if code != 0 {
		t.Fatalf("normal run after --print-engine failed (%d):\n%s", code, out)
	}
	if got := sentinelField(t, out, "SELF"); !samePath(t, got, engine.Path) {
		t.Errorf("a normal run started %q, want the printed blob %q", got, engine.Path)
	}
	if got := sentinelField(t, out, "FROM_SOURCE_KEY"); got != engine.Key {
		t.Errorf("a normal run pinned %q, want the printed key %q", got, engine.Key)
	}
	if got := goBuildCount(t, env); got != 1 {
		t.Errorf("the normal run rebuilt (`go build` count %d, want 1)", got)
	}
	spectest.Proves(t, "cli/engine-provenance", "build-only-engine",
		"print-engine-names-the-keyed-blob-and-runs-only-extensions-prepare")
}

// An existing workspace link and marker stay byte-identical when --print-engine
// builds a blob for another tree state. --bootstrap rebuilds under the same key.
func TestPutnamiwPrintEngine_LeavesTheWorkspaceLinkAlone(t *testing.T) {
	repo, home, env := gitSourceRepo(t)
	if out, code := runIn(t, repo, env, "ping"); code != 0 {
		t.Fatalf("first run failed (%d):\n%s", code, out)
	}
	link := filepath.Join(repo, ".putnami", "bin", "putnami")
	marker := filepath.Join(repo, ".putnami", "bin", ".from-source")
	targetBefore, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	markerBefore, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repo, "protocols", "workspace", "lock.go"),
		[]byte("package workspace\n\n// edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runSplit(t, repo, env, "--print-engine")
	if code != 0 {
		t.Fatalf("--print-engine failed (%d):\n%s", code, stderr)
	}
	engine := decodeEngine(t, stdout)
	if got := goBuildCount(t, env); got != 2 {
		t.Fatalf("`go build` count %d, want 2: the edit must change the key", got)
	}
	if strings.HasSuffix(filepath.Dir(targetBefore), engine.Key) {
		t.Fatalf("the edit did not move the key: %s", engine.Key)
	}
	if got, err := os.Readlink(link); err != nil || got != targetBefore {
		t.Errorf("link = %q (%v), want it unchanged at %q", got, err, targetBefore)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != string(markerBefore) {
		t.Errorf("marker = %q (%v), want it unchanged at %q", got, err, markerBefore)
	}

	again, stderr, code := runSplit(t, repo, env, "--bootstrap", "--print-engine")
	if code != 0 {
		t.Fatalf("--bootstrap --print-engine failed (%d):\n%s", code, stderr)
	}
	if again != stdout {
		t.Errorf("--bootstrap printed %q, want the same blob %q", again, stdout)
	}
	if got := goBuildCount(t, env); got != 3 {
		t.Errorf("--bootstrap must rebuild: `go build` count %d, want 3", got)
	}
	if got := sourceBlobs(t, home); len(got) != 2 {
		t.Errorf("blobs = %v, want two keys", got)
	}
	if got, err := os.Readlink(link); err != nil || got != targetBefore {
		t.Errorf("after --bootstrap, link = %q (%v), want it unchanged at %q", got, err, targetBefore)
	}
}

// A failed build exits non-zero with nothing on stdout, even when an older blob
// is cached, and so does a failed `extensions prepare`. Every refusal does the
// same before building anything.
func TestPutnamiwPrintEngine_FailsWithNothingOnStdout(t *testing.T) {
	t.Run("failed build", func(t *testing.T) {
		repo, _, env := gitSourceRepo(t)
		if _, _, code := runSplit(t, repo, env, "--print-engine"); code != 0 {
			t.Fatalf("first --print-engine failed (%d)", code)
		}
		if err := os.WriteFile(filepath.Join(repo, "protocols", "workspace", "lock.go"),
			[]byte("package workspace\n\n// broken\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(envValue(env, "GO_BUILD_FAIL"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, code := runSplit(t, repo, env, "--print-engine")
		if code == 0 || stdout != "" {
			t.Fatalf("a failed build must exit non-zero with empty stdout: code=%d stdout=%q", code, stdout)
		}
		for _, want := range []string{"stub go: build refused", "Failed to build putnami CLI"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr must carry %q:\n%s", want, stderr)
			}
		}
		assertNoWorkspaceBin(t, repo)
	})

	t.Run("failed extensions prepare", func(t *testing.T) {
		repo, home, env := gitSourceRepo(t)
		if err := os.WriteFile(envValue(env, "SENTINEL_FAIL"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, code := runSplit(t, repo, env, "--print-engine")
		if code == 0 || stdout != "" {
			t.Fatalf("a failed extensions prepare must exit non-zero with empty stdout: code=%d stdout=%q", code, stdout)
		}
		for _, want := range []string{"sentinel: extensions prepare refused", "putnami extensions prepare failed"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr must carry %q:\n%s", want, stderr)
			}
		}
		if got := sourceBlobs(t, home); len(got) != 1 {
			t.Errorf("blobs = %v, want the one blob the build published", got)
		}
		assertNoWorkspaceBin(t, repo)
	})

	refusals := []struct {
		name  string
		setup func(t *testing.T, repo string)
		args  []string
		want  string
	}{
		{
			name: "no CLI sources",
			setup: func(t *testing.T, repo string) {
				if err := os.RemoveAll(filepath.Join(repo, "tooling")); err != nil {
					t.Fatal(err)
				}
			},
			args: []string{"--print-engine"},
			want: "needs the CLI sources under tooling/cli",
		},
		{
			name: "no content key",
			setup: func(t *testing.T, repo string) {
				if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
					t.Fatal(err)
				}
			},
			args: []string{"--print-engine"},
			want: "needs a content key",
		},
		{
			name: "a command",
			args: []string{"--print-engine", "build", "--all"},
			want: "takes no command",
		},
		{
			name: "--download",
			args: []string{"--print-engine", "--download"},
			want: "cannot be combined with --download",
		},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			repo, home, env := gitSourceRepo(t)
			if tc.setup != nil {
				tc.setup(t, repo)
			}
			stdout, stderr, code := runSplit(t, repo, env, tc.args...)
			if code == 0 || stdout != "" {
				t.Fatalf("want a non-zero exit with empty stdout: code=%d stdout=%q", code, stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr must say %q:\n%s", tc.want, stderr)
			}
			if got := goBuildCount(t, env); got != 0 {
				t.Errorf("a refusal compiled (`go build` count %d)", got)
			}
			if got := sourceBlobs(t, home); len(got) != 0 {
				t.Errorf("a refusal published %v", got)
			}
			assertNoWorkspaceBin(t, repo)
		})
	}
}

// `extensions prepare` resolves a prepare toolchain to exactly the Go release
// the lock pins and installs none. So before it runs that command,
// --print-engine installs that release under the Putnami home, on a run that
// builds the blob and on one that reuses it, unless $GOROOT/bin/go or the go on
// PATH runs as that release, probed as the CLI probes it: GOROOT unset and
// GOTOOLCHAIN=local. A Go it cannot verify stops the mode before the command
// starts, with nothing on stdout.
func TestPutnamiwPrintEngine_InstallsTheLockedGoBeforeExtensionsPrepare(t *testing.T) {
	requireGoDownloadPlatform(t)
	const archive = "FAKE-GO-ARCHIVE"
	sum := sha256.Sum256([]byte(archive))
	digest := hex.EncodeToString(sum[:])
	setup := func(t *testing.T, version, lockDigest string) (repo, managed, curlLog string, env []string) {
		repo, home, env := gitSourceRepo(t)
		stubGo := filepath.Join(strings.Split(envValue(env, "PATH"), string(os.PathListSeparator))[0], "go")
		writeGoLock(t, repo, version, lockDigest)
		downloadBin, curlLog := fakeGoDownload(t, archive, "", stubGo)
		managed = filepath.Join(home, "toolchains", "go", "go-"+version)
		env = append(env,
			"PATH="+downloadBin+string(os.PathListSeparator)+envValue(env, "PATH"),
			"SENTINEL_NEEDS="+filepath.Join(managed, "go", "bin", "go"))
		return repo, managed, curlLog, env
	}
	downloads := func(t *testing.T, curlLog string) int {
		t.Helper()
		data, err := os.ReadFile(curlLog)
		if os.IsNotExist(err) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(data), "\n")
	}

	t.Run("the go on PATH runs as another release", func(t *testing.T) {
		repo, managed, curlLog, env := setup(t, "1.30.0", digest)
		for i, run := range []string{"a run that builds", "a run that reuses the blob"} {
			stdout, stderr, code := runSplit(t, repo, env, "--print-engine")
			if code != 0 {
				t.Fatalf("%s: --print-engine failed (%d):\n%s", run, code, stderr)
			}
			decodeEngine(t, stdout)
			if got := sentinelField(t, stderr, "ARGS"); got != "extensions prepare" {
				t.Errorf("%s: ran `putnami %s`, want `putnami extensions prepare`", run, got)
			}
			if got := downloads(t, curlLog); got != i+1 {
				t.Errorf("%s: %d downloads, want %d", run, got, i+1)
			}
			if err := os.RemoveAll(managed); err != nil {
				t.Fatal(err)
			}
		}
		if got := goBuildCount(t, env); got != 1 {
			t.Errorf("`go build` count %d, want 1: the second run reuses the blob", got)
		}
	})

	t.Run("the go on PATH runs as the locked release", func(t *testing.T) {
		repo, managed, curlLog, env := setup(t, "1.26.1", digest)
		env = append(env, "SENTINEL_NEEDS=")
		stdout, stderr, code := runSplit(t, repo, env, "--print-engine")
		if code != 0 {
			t.Fatalf("--print-engine failed (%d):\n%s", code, stderr)
		}
		decodeEngine(t, stdout)
		if got := downloads(t, curlLog); got != 0 {
			t.Errorf("%d downloads, want none: the go on PATH runs as the locked release", got)
		}
		if _, err := os.Stat(managed); !os.IsNotExist(err) {
			t.Errorf("a Go was installed at %s: %v", managed, err)
		}
	})

	t.Run("GOROOT holds the locked release", func(t *testing.T) {
		repo, managed, curlLog, env := setup(t, "1.30.0", digest)
		goroot := t.TempDir()
		writeExec(t, filepath.Join(goroot, "bin", "go"), `#!/usr/bin/env bash
if [ "$1 $2" = "env GOVERSION" ] && [ -z "${GOROOT:-}" ] && [ "${GOTOOLCHAIN:-}" = local ]; then echo go1.30.0; fi
exit 0
`)
		env = append(env, "GOROOT="+goroot, "SENTINEL_NEEDS=")
		stdout, stderr, code := runSplit(t, repo, env, "--print-engine")
		if code != 0 {
			t.Fatalf("--print-engine failed (%d):\n%s", code, stderr)
		}
		decodeEngine(t, stdout)
		if got := downloads(t, curlLog); got != 0 {
			t.Errorf("%d downloads, want none: $GOROOT/bin/go runs as the locked release", got)
		}
		if _, err := os.Stat(managed); !os.IsNotExist(err) {
			t.Errorf("a Go was installed at %s: %v", managed, err)
		}
	})

	t.Run("the locked release fails its SHA-256", func(t *testing.T) {
		repo, managed, _, env := setup(t, "1.30.0", strings.Repeat("0", 64))
		stdout, stderr, code := runSplit(t, repo, env, "--print-engine")
		if code == 0 || stdout != "" {
			t.Fatalf("want a non-zero exit with empty stdout: code=%d stdout=%q", code, stdout)
		}
		if !strings.Contains(stderr, "Refusing Go 1.30.0") {
			t.Errorf("stderr must refuse the archive:\n%s", stderr)
		}
		if strings.Contains(stderr, "RAN_BUILT") {
			t.Errorf("extensions prepare ran after a refused Go:\n%s", stderr)
		}
		if _, err := os.Stat(managed); !os.IsNotExist(err) {
			t.Errorf("an unverified Go was installed at %s: %v", managed, err)
		}
	})
	spectest.Proves(t, "cli/engine-provenance", "print-engine-installs-the-locked-go",
		"print-engine-installs-the-locked-go-before-extensions-prepare")
}
