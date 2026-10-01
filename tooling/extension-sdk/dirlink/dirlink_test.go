package dirlink

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// targetDir makes a directory holding one file whose content names it.
func targetDir(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "name"), []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// readThrough reads the file targetDir wrote, through link.
func readThrough(t *testing.T, link string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(link, "name"))
	if err != nil {
		t.Fatalf("read through %s: %v", link, err)
	}
	return string(data)
}

func assertLink(t *testing.T, link string) {
	t.Helper()
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if !IsLink(link, info) {
		t.Fatalf("IsLink(%s) = false for mode %v", link, info.Mode())
	}
	if info.IsDir() {
		t.Fatalf("os.Lstat(%s) reports a directory, mode %v", link, info.Mode())
	}
}

func assertResolvesTo(t *testing.T, link, target string) {
	t.Helper()
	got, err := Resolve(link)
	if err != nil {
		t.Fatal(err)
	}
	want, err := Resolve(target)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Resolve(%s) = %s, want %s", link, got, want)
	}
}

func TestCreateLinksADirectory(t *testing.T) {
	dir := t.TempDir()
	target := targetDir(t, dir, "a")
	link := filepath.Join(dir, "link")
	if err := Create(target, link); err != nil {
		t.Fatal(err)
	}
	assertLink(t, link)
	if got := readThrough(t, link); got != "a" {
		t.Fatalf("read %q through the link, want %q", got, "a")
	}
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Fatalf("os.Readlink = %q, %v, want %q", got, err, target)
	}
	info, err := os.Stat(link)
	if err != nil || !info.IsDir() {
		t.Fatalf("os.Stat through the link = %v, %v, want a directory", info, err)
	}
	assertResolvesTo(t, link, target)
}

func TestCreateResolvesARelativeTargetAgainstTheLinkDirectory(t *testing.T) {
	dir := t.TempDir()
	target := targetDir(t, dir, "a")
	links := filepath.Join(dir, "links")
	if err := os.Mkdir(links, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(links, "link")
	if err := Create(filepath.Join("..", "a"), link); err != nil {
		t.Fatal(err)
	}
	if got := readThrough(t, link); got != "a" {
		t.Fatalf("read %q through the link, want %q", got, "a")
	}
	assertResolvesTo(t, link, target)
}

func TestCreateRefusesAnExistingPath(t *testing.T) {
	dir := t.TempDir()
	target := targetDir(t, dir, "a")
	link := filepath.Join(dir, "link")
	if err := Create(target, link); err != nil {
		t.Fatal(err)
	}
	if err := Create(target, link); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second Create = %v, want an error matching fs.ErrExist", err)
	}
}

func TestReplaceSwapsTheTargetAndKeepsTheOldOne(t *testing.T) {
	dir := t.TempDir()
	a := targetDir(t, dir, "a")
	b := targetDir(t, dir, "b")
	link := filepath.Join(dir, "link")
	tmp := link + ".tmp"
	if err := Replace(a, link, tmp); err != nil {
		t.Fatal(err)
	}
	if got := readThrough(t, link); got != "a" {
		t.Fatalf("read %q through the link, want %q", got, "a")
	}
	if err := Replace(b, link, tmp); err != nil {
		t.Fatal(err)
	}
	assertLink(t, link)
	if got := readThrough(t, link); got != "b" {
		t.Fatalf("read %q through the replaced link, want %q", got, "b")
	}
	if got, err := os.Readlink(link); err != nil || got != b {
		t.Fatalf("os.Readlink = %q, %v, want %q", got, err, b)
	}
	if got := readThrough(t, a); got != "a" {
		t.Fatalf("the old target holds %q, want %q", got, "a")
	}
	if _, err := os.Lstat(tmp); !os.IsNotExist(err) {
		t.Fatalf("the staging name survives the swap: %v", err)
	}
}

func TestReplaceReplacesAFile(t *testing.T) {
	dir := t.TempDir()
	target := targetDir(t, dir, "a")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(link, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Replace(target, link, link+".tmp"); err != nil {
		t.Fatal(err)
	}
	if got := readThrough(t, link); got != "a" {
		t.Fatalf("read %q through the link, want %q", got, "a")
	}
}

func TestReplaceRefusesADirectory(t *testing.T) {
	dir := t.TempDir()
	target := targetDir(t, dir, "a")
	occupied := targetDir(t, dir, "occupied")
	if err := Replace(target, occupied, occupied+".tmp"); err == nil {
		t.Fatal("Replace over a directory succeeded")
	}
	if got := readThrough(t, occupied); got != "occupied" {
		t.Fatalf("the directory holds %q after the refused replace", got)
	}
	info, err := os.Lstat(occupied)
	if err != nil || !info.IsDir() || IsLink(occupied, info) {
		t.Fatalf("the refused replace changed the directory: %v, %v", info, err)
	}
}

func TestIsLinkRejectsADirectoryAndAFile(t *testing.T) {
	dir := t.TempDir()
	target := targetDir(t, dir, "a")
	for _, path := range []string{target, filepath.Join(target, "name")} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if IsLink(path, info) {
			t.Fatalf("IsLink(%s) = true", path)
		}
	}
}

func TestRemoveDeletesOnlyTheLink(t *testing.T) {
	dir := t.TempDir()
	target := targetDir(t, dir, "a")
	for _, remove := range []func(string) error{os.Remove, os.RemoveAll} {
		link := filepath.Join(dir, "link")
		if err := Create(target, link); err != nil {
			t.Fatal(err)
		}
		if err := remove(link); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(link); !os.IsNotExist(err) {
			t.Fatalf("the link survives its removal: %v", err)
		}
		if got := readThrough(t, target); got != "a" {
			t.Fatalf("removing the link changed its target: %q", got)
		}
	}
}

func TestResolveReportsAMissingPath(t *testing.T) {
	dir := t.TempDir()
	if _, err := Resolve(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Resolve of a missing path = %v, want fs.ErrNotExist", err)
	}
	link := filepath.Join(dir, "dangling")
	if err := Create(filepath.Join(dir, "gone"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(link); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Resolve of a dangling link = %v, want fs.ErrNotExist", err)
	}
}

// Concurrent writers each stage at their own tmp name, as callers in separate
// processes do. Every replace succeeds and the link always ends on one target.
func TestConcurrentReplaceAlwaysLeavesALink(t *testing.T) {
	dir := t.TempDir()
	targets := []string{targetDir(t, dir, "a"), targetDir(t, dir, "b")}
	link := filepath.Join(dir, "link")
	const writers, rounds = 4, 20
	errs := make(chan error, writers*rounds)
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tmp := fmt.Sprintf("%s.tmp.%d", link, w)
			for r := range rounds {
				if err := Replace(targets[(w+r)%len(targets)], link, tmp); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	assertLink(t, link)
	if got := readThrough(t, link); got != "a" && got != "b" {
		t.Fatalf("read %q through the link", got)
	}
}
