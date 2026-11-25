package gittest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// RepoFile and RepoContent are the one file the commit of Repo holds.
const (
	RepoFile    = "README.md"
	RepoContent = "# Test"
)

// RepoMessage is the message of the one commit of Repo.
const RepoMessage = "initial"

// repo is the repository Repo copies, built once per test binary.
var repo template

// Repo returns a new directory that holds a repository with one commit on
// DefaultBranch: RepoFile with RepoContent, committed as RepoMessage by Name
// and Email. Its configuration names the same identity and signs no commit.
//
// The test binary builds that repository once, under Env, and keeps its files
// in memory; each call writes a copy into a directory of t.TempDir. A test
// changes its copy freely and may run in parallel with the others. Every copy
// holds the same commit, with the same date. The index of a copy keeps the
// file stat data of the build, so a command that refreshes the index, such as
// status or diff, sees a clean worktree, while plumbing that does not, such as
// diff-files or diff-index, reports RepoFile until the test runs
// update-index --refresh.
//
// The first call builds the repository with the environment of its test; a
// build that fails fails every test of the binary that asks for a copy. A test
// must therefore not call Repo after t.Setenv("PATH", ...) or another change
// to the environment git reads: when it is the first caller, that environment
// builds the repository every other test copies.
func Repo(t testing.TB) string {
	t.Helper()
	return repo.copy(t, buildRepo)
}

// buildRepo builds the repository Repo copies in dir.
func buildRepo(dir string) error {
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", Email},
		{"config", "user.name", Name},
		{"config", "commit.gpgsign", "false"},
		{"checkout", "-b", DefaultBranch},
	} {
		if err := runIn(dir, args...); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, RepoFile), []byte(RepoContent), 0o644); err != nil {
		return err
	}
	if err := runIn(dir, "add", "."); err != nil {
		return err
	}
	return runIn(dir, "commit", "-m", RepoMessage)
}

// runIn runs git in dir with Env and reports a failure with git's output.
func runIn(dir string, args ...string) error {
	if out, err := Command(dir, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("git %v: %w\n%s", args, err, out)
	}
	return nil
}

// template holds the files of a directory a build function wrote once.
type template struct {
	once  sync.Once
	files []templateFile
	err   error
}

// templateFile is one directory or file of a template, by its slash path
// relative to the template's root.
type templateFile struct {
	path string
	mode fs.FileMode
	data []byte
}

// copy builds the template on its first call, then writes its files into a
// new directory of t.TempDir and returns that directory. A build that failed
// fails every test that asks for a copy.
func (tpl *template) copy(t testing.TB, build func(dir string) error) string {
	t.Helper()
	tpl.once.Do(func() {
		// A build that panics or exits its goroutine leaves this error.
		tpl.err = errors.New("the build did not finish")
		tpl.files, tpl.err = snapshot(build)
	})
	if tpl.err != nil {
		t.Fatalf("build the gittest template: %v", tpl.err)
	}
	dir := t.TempDir()
	if err := tpl.write(dir); err != nil {
		t.Fatalf("copy the gittest template: %v", err)
	}
	return dir
}

// snapshot runs build in a temporary directory, reads what it wrote and
// removes the directory.
func snapshot(build func(dir string) error) ([]templateFile, error) {
	dir, err := os.MkdirTemp("", "gittest-template-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := build(dir); err != nil {
		return nil, err
	}
	var files []templateFile
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		file := templateFile{path: filepath.ToSlash(rel), mode: info.Mode()}
		switch {
		case info.Mode().IsDir():
		case info.Mode().IsRegular():
			if file.data, err = os.ReadFile(path); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: a template holds only directories and regular files", file.path)
		}
		files = append(files, file)
		return nil
	})
	return files, err
}

// write writes the template's files into dir. A directory comes before the
// files it holds, because snapshot walks in lexical order.
func (tpl *template) write(dir string) error {
	for _, file := range tpl.files {
		path := filepath.Join(dir, filepath.FromSlash(file.path))
		if file.mode.IsDir() {
			if err := os.Mkdir(path, file.mode.Perm()|0o700); err != nil {
				return err
			}
			continue
		}
		if err := os.WriteFile(path, file.data, file.mode.Perm()); err != nil {
			return err
		}
	}
	return nil
}
