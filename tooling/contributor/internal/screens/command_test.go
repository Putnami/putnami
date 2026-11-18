package screens

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// fakeCapturer answers each shot with a PNG-like payload naming the shot,
// and with the status statuses gives its URL, 200 otherwise.
type fakeCapturer struct {
	mu        sync.Mutex
	statuses  map[string]int
	redirects map[string]string
	// pad is the size of each PNG payload, when it is more than the payload.
	pad   int
	shots []Shot
}

func (f *fakeCapturer) Capture(_ context.Context, shot Shot) (Captured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shots = append(f.shots, shot)
	status := 200
	if s, ok := f.statuses[shot.URL]; ok {
		status = s
	}
	final := shot.URL
	if to, ok := f.redirects[shot.URL]; ok {
		final = to
	}
	png := []byte(fmt.Sprintf("png %s %s %s", shot.URL, shot.Viewport.Name, shot.Scheme))
	if f.pad > len(png) {
		png = append(png, make([]byte, f.pad-len(png))...)
	}
	return Captured{PNG: png, Status: status, URL: final}, nil
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, file, content string) {
	t.Helper()
	full := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func inventoryJSON(t *testing.T, inv *Inventory) string {
	t.Helper()
	data, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// webRepo commits a web project on main, then branches topic with change
// applied and committed. It returns the repository directory.
func webRepo(t *testing.T, change func(dir string)) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "apps/web/schema/http-routes.json", inventoryJSON(t, sampleWeb(t)))
	write(t, dir, "apps/web/src/app/tasks/page.tsx", "export default 1\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "base")
	git(t, dir, "checkout", "-q", "-b", "topic")
	change(dir)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "change")
	return dir
}

type run struct {
	code           int
	stdout, stderr string
	capturer       *fakeCapturer
}

func runCommand(t *testing.T, dir string, capturer *fakeCapturer, args ...string) run {
	t.Helper()
	if capturer == nil {
		capturer = &fakeCapturer{}
	}
	var stdout, stderr bytes.Buffer
	command := Command{Root: dir, Git: SystemGit(dir),
		Launch: func(context.Context, string, *StorageState) (Capturer, func() error, error) {
			return capturer, func() error { return nil }, nil
		},
		Getenv:   func(string) string { return "" },
		LookPath: func(string) (string, error) { return "", errors.New("none") },
		Stdout:   &stdout, Stderr: &stderr}
	code := command.Run(context.Background(), append(args, "--browser", "/fake/chrome"))
	return run{code: code, stdout: stdout.String(), stderr: stderr.String(), capturer: capturer}
}

var serverArgs = []string{"--project", "apps/web", "--base", "main",
	"--base-url", "http://127.0.0.1:3001/", "--head-url", "http://127.0.0.1:3000"}

// TestOnePageGetsEightCapturesBoundToTheHead runs the command on a change to
// one page and reads the manifest and its eight files.
func TestOnePageGetsEightCapturesBoundToTheHead(t *testing.T) {
	t.Parallel()
	dir := webRepo(t, func(dir string) { write(t, dir, "apps/web/src/app/tasks/page.tsx", "export default 2\n") })
	got := runCommand(t, dir, nil, serverArgs...)
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	out := filepath.Join(dir, ".putnami", "screenshots", "apps-web")
	manifest, err := ReadManifest(filepath.Join(out, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	head := git(t, dir, "rev-parse", "HEAD")
	if manifest.HeadSHA != head || manifest.BaseSHA != git(t, dir, "rev-parse", "main") || manifest.Project != "apps/web" {
		t.Fatalf("manifest binding = %s %s %s", manifest.Project, manifest.BaseSHA, manifest.HeadSHA)
	}
	if len(manifest.Captures) != 8 || len(got.capturer.shots) != 8 {
		t.Fatalf("%d captures, %d shots", len(manifest.Captures), len(got.capturer.shots))
	}
	if got.capturer.shots[0].URL != "http://127.0.0.1:3001/tasks" || got.capturer.shots[4].URL != "http://127.0.0.1:3000/tasks" {
		t.Fatalf("urls %s, %s", got.capturer.shots[0].URL, got.capturer.shots[4].URL)
	}
	if problems := Check(manifest, out, head, "apps/web", manifest.Pages, nil); len(problems) > 0 {
		t.Fatalf("the manifest it wrote fails its check: %v", problems)
	}
	if manifest.Captures[0].Status != 200 || manifest.Captures[0].URL != "http://127.0.0.1:3001/tasks" {
		t.Fatalf("first capture = %+v", manifest.Captures[0])
	}
	if !strings.Contains(got.stdout, "Captured 1 page of apps/web in 8 files.") || !strings.Contains(got.stdout, "/tasks (src/app/tasks/page.tsx): before and after") {
		t.Fatalf("stdout = %s", got.stdout)
	}
	// A second run replaces the files the first manifest lists, and keeps a
	// file it does not list.
	write(t, out, "notes.md", "mine\n")
	if again := runCommand(t, dir, nil, serverArgs...); again.code != 0 {
		t.Fatalf("second run exit %d: %s", again.code, again.stderr)
	}
	if _, err := os.Stat(filepath.Join(out, "notes.md")); err != nil {
		t.Fatalf("the second run removed a file it did not write: %v", err)
	}
	spectest.Proves(t, screensFeature, capturesReq, "one-page-gets-eight-captures")
	spectest.Proves(t, screensFeature, capturesReq, "the-manifest-binds-the-head-and-hashes-each-capture")
}

// TestAPageOneSideLacksGetsFourCaptures captures a new page after only, and
// needs no base server for it.
func TestAPageOneSideLacksGetsFourCaptures(t *testing.T) {
	t.Parallel()
	dir := webRepo(t, func(dir string) {
		inv := sampleWeb(t)
		inv.Routes = append(inv.Routes, webInventory(t, "exact /about src/app/about/page.tsx").Routes[0])
		write(t, dir, "apps/web/schema/http-routes.json", inventoryJSON(t, inv))
		write(t, dir, "apps/web/src/app/about/page.tsx", "export default 3\n")
	})
	got := runCommand(t, dir, nil, "--project", "apps/web", "--base", "main", "--head-url", "http://127.0.0.1:3000",
		"--out", "shots")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	manifest, err := ReadManifest(filepath.Join(dir, "shots", ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Captures) != 4 || manifest.Pages[0].Before || !manifest.Pages[0].After {
		t.Fatalf("manifest = %+v", manifest)
	}
	spectest.Proves(t, screensFeature, capturesReq, "a-page-one-side-lacks-gets-four-captures")
}

// TestAnErrorStatusFailsTheRun fails on a 404 and writes no manifest.
func TestAnErrorStatusFailsTheRun(t *testing.T) {
	t.Parallel()
	dir := webRepo(t, func(dir string) { write(t, dir, "apps/web/src/app/tasks/page.tsx", "export default 2\n") })
	capturer := &fakeCapturer{statuses: map[string]int{"http://127.0.0.1:3000/tasks": 404}}
	got := runCommand(t, dir, capturer, serverArgs...)
	if got.code != 1 || !strings.Contains(got.stderr, "after /tasks answered HTTP 404") {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, ".putnami", "screenshots", "apps-web", ManifestFile)); !os.IsNotExist(err) {
		t.Fatalf("a manifest was written: %v", err)
	}
	spectest.Proves(t, screensFeature, capturesReq, "an-error-status-fails-the-run")
}

// TestADirtyProjectIsRefused refuses a project directory with an uncommitted
// edit, before any capture.
func TestADirtyProjectIsRefused(t *testing.T) {
	t.Parallel()
	dir := webRepo(t, func(dir string) { write(t, dir, "apps/web/src/app/tasks/page.tsx", "export default 2\n") })
	write(t, dir, "apps/web/src/app/tasks/page.tsx", "export default 9\n")
	got := runCommand(t, dir, nil, serverArgs...)
	if got.code != 1 || !strings.Contains(got.stderr, "apps/web holds uncommitted changes") || len(got.capturer.shots) != 0 {
		t.Fatalf("exit %d, %d shots: %s", got.code, len(got.capturer.shots), got.stderr)
	}
	spectest.Proves(t, screensFeature, capturesReq, "a-dirty-project-is-refused")
}

// TestAChangeWithoutAPageCapturesNothing exits 0 with no manifest, and
// refuses a project that is no web project.
func TestAChangeWithoutAPageCapturesNothing(t *testing.T) {
	t.Parallel()
	dir := webRepo(t, func(dir string) {
		write(t, dir, "apps/web/README.md", "docs\n")
		write(t, dir, "lib/src/x.ts", "export {}\n")
	})
	got := runCommand(t, dir, nil, serverArgs...)
	if got.code != 0 || !strings.Contains(got.stdout, "touches no page of apps/web") {
		t.Fatalf("exit %d: %s %s", got.code, got.stdout, got.stderr)
	}
	got = runCommand(t, dir, nil, "--project", "lib", "--base", "main", "--head-url", "http://127.0.0.1:3000")
	if got.code != 1 || !strings.Contains(got.stderr, "lib is no web project") {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
}

// TestUsageErrorsExitTwo refuses unknown, repeated and missing flags.
func TestUsageErrorsExitTwo(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--project", "a", "--base", "main"},
		{"--project", "a", "--project", "b", "--base", "main", "--head-url", "http://x"},
		{"--project", "a", "--base", "main", "--head-url", "http://x", "--nope"},
		{"--project", "a", "--base", "main", "--head-url"},
	} {
		var stderr bytes.Buffer
		if code := (Command{Stderr: &stderr}).Run(context.Background(), args); code != 2 {
			t.Fatalf("%v: exit %d (%s)", args, code, stderr.String())
		}
	}
	if _, err := joinURL("ftp://x", "/"); err == nil {
		t.Fatal("an ftp URL was accepted")
	}
	if got, _ := joinURL("http://h:1/prefix/", "/a%20b"); got != "http://h:1/prefix/a%20b" {
		t.Fatalf("joinURL = %s", got)
	}
}

// TestARedirectIsNotedAndARootBelowTheTopLevelReadsItsPaths runs the command
// in a workspace root below the repository's top level, and notes the page
// that ends at another URL.
func TestARedirectIsNotedAndARootBelowTheTopLevelReadsItsPaths(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	ws := filepath.Join(repo, "ws")
	write(t, ws, "apps/web/schema/http-routes.json", inventoryJSON(t, sampleWeb(t)))
	write(t, ws, "apps/web/src/app/tasks/page.tsx", "export default 1\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "base")
	git(t, repo, "checkout", "-q", "-b", "topic")
	write(t, ws, "apps/web/src/app/tasks/page.tsx", "export default 2\n")
	git(t, repo, "commit", "-qam", "change")
	capturer := &fakeCapturer{redirects: map[string]string{"http://127.0.0.1:3000/tasks": "http://127.0.0.1:3000/login"}}
	got := runCommand(t, ws, capturer, serverArgs...)
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if len(capturer.shots) != 8 || !strings.Contains(got.stdout, "note: after /tasks ended at http://127.0.0.1:3000/login") ||
		strings.Count(got.stdout, "note:") != 1 {
		t.Fatalf("%d shots, stdout %s", len(capturer.shots), got.stdout)
	}
}
