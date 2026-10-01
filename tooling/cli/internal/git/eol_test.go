package git

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestCRLFCheckouts_ListsTextFilesRewrittenWithCRLF commits LF files, then
// rewrites one working-tree copy with CRLF endings, as core.autocrlf=true does
// on checkout. Only that file is reported; a file committed with CRLF is the
// committed content, not a checkout conversion, and stays out.
func TestCRLFCheckouts_ListsTextFilesRewrittenWithCRLF(t *testing.T) {
	dir := initGitRepo(t)
	runGit(t, dir, "config", "core.autocrlf", "false")
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("app/main.go", "package main\n\nfunc main() {}\n")
	write("app/notes.txt", "one\ntwo\n")
	write("committed-crlf.txt", "one\r\ntwo\r\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "files")

	write("app/main.go", "package main\r\n\r\nfunc main() {}\r\n")

	files, err := CRLFCheckouts(dir)
	if err != nil {
		t.Fatalf("CRLFCheckouts: %v", err)
	}
	if !slices.Equal(files, []string{"app/main.go"}) {
		t.Fatalf("CRLFCheckouts = %v, want [app/main.go]", files)
	}

	// From a subdirectory, paths are relative to it.
	files, err = CRLFCheckouts(filepath.Join(dir, "app"))
	if err != nil {
		t.Fatalf("CRLFCheckouts(app): %v", err)
	}
	if !slices.Equal(files, []string{"main.go"}) {
		t.Fatalf("CRLFCheckouts(app) = %v, want [main.go]", files)
	}
}

// TestCRLFCheckouts_LeavesOutFilesTheAttributesCheckOutWithCRLF commits files
// whose attributes set eol=crlf, as a workspace does for .bat and .cmd scripts.
// Git checks them out with CRLF on every platform, so they are not a
// conversion to report; a file the attributes leave alone still is.
func TestCRLFCheckouts_LeavesOutFilesTheAttributesCheckOutWithCRLF(t *testing.T) {
	dir := initGitRepo(t)
	runGit(t, dir, "config", "core.autocrlf", "false")
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitattributes", "*.bat text eol=crlf\n*.cmd text=auto eol=crlf\n")
	write("run.bat", "@echo off\necho run\n")
	write("run.cmd", "@echo off\necho run\n")
	write("notes.txt", "one\ntwo\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "files")

	// Git applies eol=crlf on checkout; write what it writes.
	write("run.bat", "@echo off\r\necho run\r\n")
	write("run.cmd", "@echo off\r\necho run\r\n")
	write("notes.txt", "one\r\ntwo\r\n")

	files, err := CRLFCheckouts(dir)
	if err != nil {
		t.Fatalf("CRLFCheckouts: %v", err)
	}
	if !slices.Equal(files, []string{"notes.txt"}) {
		t.Fatalf("CRLFCheckouts = %v, want [notes.txt]", files)
	}
}

func TestCRLFCheckouts_FailsOutsideAGitCheckout(t *testing.T) {
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(t.TempDir()))
	if _, err := CRLFCheckouts(t.TempDir()); err == nil {
		t.Fatal("CRLFCheckouts outside a Git checkout must fail")
	}
}

func TestConfigValue_DistinguishesUnsetFromSet(t *testing.T) {
	dir := initGitRepo(t)
	if _, set, err := ConfigValue(dir, "putnami.test-unset"); err != nil || set {
		t.Fatalf("unset key: set=%v err=%v, want set=false err=nil", set, err)
	}
	runGit(t, dir, "config", "core.autocrlf", "true")
	value, set, err := ConfigValue(dir, "core.autocrlf")
	if err != nil || !set || value != "true" {
		t.Fatalf("core.autocrlf = %q set=%v err=%v, want \"true\" set", value, set, err)
	}
}

func TestConfigBool(t *testing.T) {
	for value, want := range map[string]bool{
		"true": true, "Yes": true, "on": true, "1": true,
		"false": false, "input": false, "": false, "0": false,
	} {
		if got := ConfigBool(value); got != want {
			t.Errorf("ConfigBool(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestHasLFPolicy_ReadsTheAttributesGitApplies(t *testing.T) {
	dir := initGitRepo(t)
	if ok, err := HasLFPolicy(dir, "README.md"); err != nil || ok {
		t.Fatalf("without .gitattributes: ok=%v err=%v, want false", ok, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte(LFPolicyAttributes+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := HasLFPolicy(dir, "README.md"); err != nil || !ok {
		t.Fatalf("with the LF policy: ok=%v err=%v, want true", ok, err)
	}
}
