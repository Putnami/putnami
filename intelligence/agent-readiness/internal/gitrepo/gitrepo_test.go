package gitrepo_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/intelligence/agent-readiness/internal/gitrepo"
	"go.putnami.dev/intelligence/agent-readiness/internal/gittest"
	"go.putnami.dev/intelligence/agent-readiness/payload"
)

func TestRepoReadsHead(t *testing.T) {
	ctx := context.Background()
	r := gittest.New(t)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	r.Write("a.txt", "one\ntwo\n")
	r.Write("bin.dat", "\x00\x01")
	r.Write("empty.txt", "")
	r.Write("dir/b.go", "package dir\n")
	r.Commit("first", "dev@example.test", at)
	r.Write("dir/b.go", "package dir\n\nconst X = 1\n")
	r.Commit("second", "dev@example.test", at.Add(time.Hour))

	repo, err := gitrepo.Open(ctx, r.Dir+"/dir")
	if err != nil {
		t.Fatal(err)
	}
	head, headAt, err := repo.Head(ctx)
	if err != nil || len(head) != 40 || headAt != at.Add(time.Hour).Unix() {
		t.Fatalf("Head = %s %d %v", head, headAt, err)
	}
	files, err := repo.Tree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]gitrepo.File{}
	for _, file := range files {
		byPath[file.Path] = file
	}
	if f := byPath["a.txt"]; !f.Text || f.Lines != 2 || f.Size != 8 {
		t.Fatalf("a.txt = %+v", f)
	}
	if f := byPath["bin.dat"]; f.Text {
		t.Fatalf("bin.dat reads as text: %+v", f)
	}
	if f := byPath["empty.txt"]; !f.Text || f.Lines != 0 {
		t.Fatalf("empty.txt = %+v", f)
	}
	contents, err := repo.ReadFiles(ctx, []string{"a.txt", "missing.txt", "dir", "dir/b.go", "bad\nname"})
	if err != nil {
		t.Fatal(err)
	}
	if string(contents["a.txt"]) != "one\ntwo\n" || len(contents) != 2 {
		t.Fatalf("ReadFiles = %q", contents)
	}
	dates, err := repo.Dates(ctx, []string{"dir/b.go", "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if dates.Added["dir/b.go"] != at.Unix() || dates.Changed["dir/b.go"] != at.Add(time.Hour).Unix() || dates.Changed["a.txt"] != at.Unix() {
		t.Fatalf("Dates = %+v", dates)
	}
	matches, err := repo.Grep(ctx, "nothing-matches-this")
	if err != nil || len(matches) != 0 {
		t.Fatalf("Grep = %v, %v", matches, err)
	}
	matches, err = repo.Grep(ctx, "const", "dir")
	if err != nil || len(matches) != 1 || matches[0].Path != "dir/b.go" || matches[0].Line != 3 {
		t.Fatalf("Grep(const) = %v, %v", matches, err)
	}
	roots, err := repo.RootCommits(ctx)
	if err != nil || len(roots) != 1 {
		t.Fatalf("RootCommits = %v, %v", roots, err)
	}
	if count, err := repo.CountCommits(ctx); err != nil || count != 2 {
		t.Fatalf("CountCommits = %d, %v", count, err)
	}
	if _, err := repo.Output(ctx, "no-such-command"); err == nil {
		t.Fatal("an unknown git command succeeded")
	}
}

func TestRepoDoesNotFetchMissingPromisorObjects(t *testing.T) {
	source := gittest.New(t)
	source.Write("a.txt", "missing from the partial clone\n")
	now := time.Now()
	source.Commit("one blob", "dev@example.test", now.Add(-200*24*time.Hour))
	source.Git("config", "uploadpack.allowFilter", "true")

	home := t.TempDir()
	baseEnv := append(os.Environ(), "HOME="+home, "GIT_CONFIG_GLOBAL="+filepath.Join(home, "gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	run := func(extraEnv []string, args ...string) ([]byte, error) {
		cmd := exec.Command("git", args...)
		cmd.Env = append(append([]string(nil), baseEnv...), extraEnv...)
		return cmd.CombinedOutput()
	}
	clone := filepath.Join(t.TempDir(), "partial")
	sourcePath := filepath.ToSlash(source.Dir)
	if runtime.GOOS == "windows" {
		sourcePath = "/" + sourcePath
	}
	sourceURL := (&url.URL{Scheme: "file", Path: sourcePath}).String()
	if out, err := run(nil, "clone", "--quiet", "--filter=blob:none", "--no-checkout", sourceURL, clone); err != nil {
		t.Fatalf("make partial clone: %v: %s", err, out)
	}
	out, err := run(nil, "-C", clone, "rev-parse", "HEAD:a.txt")
	if err != nil {
		t.Fatalf("find promised blob: %v: %s", err, out)
	}
	blob := strings.TrimSpace(string(out))
	if _, err := run([]string{"GIT_NO_LAZY_FETCH=1"}, "--no-lazy-fetch", "-C", clone, "cat-file", "-e", blob); err == nil {
		t.Fatal("partial-clone fixture contains the blob it should promise")
	}

	var requests atomic.Int64
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer remote.Close()
	if out, err := run(nil, "-C", clone, "remote", "set-url", "origin", remote.URL+"/repo.git"); err != nil {
		t.Fatalf("set trap remote: %v: %s", err, out)
	}
	if _, err := run([]string{"GIT_NO_LAZY_FETCH=0"}, "-C", clone, "cat-file", "-p", blob); err == nil || requests.Load() == 0 {
		t.Fatalf("fixture did not attempt a lazy fetch: error=%v requests=%d", err, requests.Load())
	}
	requests.Store(0)

	objects := filepath.Join(clone, ".git", "objects")
	snapshot := func() map[string]int64 {
		t.Helper()
		files := map[string]int64{}
		if err := filepath.WalkDir(objects, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				files[path] = info.Size()
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return files
	}
	before := snapshot()
	repo, err := gitrepo.Open(context.Background(), clone)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := repo.ReadFiles(context.Background(), []string{"a.txt"})
	if err == nil || len(contents) != 0 {
		t.Fatalf("missing HEAD blob read = %q, %v", contents, err)
	}
	contents, err = repo.ReadFiles(context.Background(), []string{"absent.txt"})
	if err != nil || len(contents) != 0 {
		t.Fatalf("absent optional path read = %q, %v", contents, err)
	}
	if _, err := repo.Tree(context.Background()); err == nil {
		t.Fatal("Tree accepted an unavailable HEAD blob size")
	}
	if _, err := payload.Collect(context.Background(), payload.Options{
		Dir: clone, CollectorVersion: "test", Now: now, Salt: []byte("partial-clone-test-salt"),
	}); err == nil {
		t.Fatal("collection accepted an unavailable old HEAD blob")
	}
	if requests.Load() != 0 {
		t.Fatalf("collector contacted promisor remote %d times", requests.Load())
	}
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatalf("collector wrote to object store: before=%v after=%v", before, after)
	}
}

func TestRepoRequiresNoLazyFetchSupport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses a POSIX executable shim")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf 'unknown option: --no-lazy-fetch\\n' >&2\nexit 129\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := gitrepo.Open(context.Background(), t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "git must support --no-lazy-fetch") {
		t.Fatalf("unsupported Git was accepted or not explained: %v", err)
	}
}

func TestUnquote(t *testing.T) {
	for in, want := range map[string]string{`"a\tb"`: "a\tb", "plain": "plain", `"broken\q"`: `"broken\q"`} {
		if got := gitrepo.Unquote(in); got != want {
			t.Errorf("Unquote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoStopsOnCancel(t *testing.T) {
	r := gittest.New(t)
	r.Write("a.txt", "a\n")
	r.Commit("first", "dev@example.test", time.Now())
	repo, err := gitrepo.Open(context.Background(), r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.Tree(ctx); err == nil {
		t.Fatal("Tree ran on a canceled context")
	}
	if err := repo.Stream(ctx, func(string) error { return nil }, "log"); err == nil {
		t.Fatal("Stream ran on a canceled context")
	}
	if _, err := repo.ReadFiles(ctx, []string{"a.txt"}); err == nil {
		t.Fatal("ReadFiles ran on a canceled context")
	}
}

// A path that only a merge added, as a subtree merge does, dates from the
// oldest commit that changed it after.
func TestDatesOfAPathAMergeAdded(t *testing.T) {
	ctx := context.Background()
	r := gittest.New(t)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	r.Write("a.txt", "one\n")
	r.Commit("first", "dev@example.test", at)
	r.Git("checkout", "-q", "-b", "side")
	r.Write("b.txt", "two\n")
	r.Commit("side", "dev@example.test", at.Add(time.Hour))
	r.Git("checkout", "-q", "-")
	r.Git("merge", "-q", "--no-ff", "--no-commit", "side")
	r.Write("sub/README.md", "# sub\n")
	r.Commit("merge side with sub", "dev@example.test", at.Add(2*time.Hour))
	r.Write("sub/README.md", "# sub\n\nMore.\n")
	r.Commit("document sub", "dev@example.test", at.Add(3*time.Hour))
	r.Write("sub/README.md", "# sub\n\nEven more.\n")
	r.Commit("document sub again", "dev@example.test", at.Add(4*time.Hour))

	repo, err := gitrepo.Open(ctx, r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	dates, err := repo.Dates(ctx, []string{"sub/README.md", "b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if dates.Added["sub/README.md"] != at.Add(3*time.Hour).Unix() || dates.Changed["sub/README.md"] != at.Add(4*time.Hour).Unix() {
		t.Fatalf("sub/README.md dates = %+v", dates)
	}
	if dates.Added["b.txt"] != at.Add(time.Hour).Unix() {
		t.Fatalf("b.txt dates = %+v", dates)
	}
}
