package repositorychecks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// workspaceManifest is the file that marks the repository root.
const workspaceManifest = "putnami.workspace.json"

// repositoryRoot walks up from the test's working directory, the project root,
// to the directory holding the workspace manifest.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, workspaceManifest)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no %s above the test working directory", workspaceManifest)
		}
		dir = parent
	}
}

// cliModuleRoot returns the root of the CLI module whose committed documents
// these checks read.
func cliModuleRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(repositoryRoot(t), filepath.FromSlash(cliModuleDir))
}

// projectRoot returns this project's own root.
func projectRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(repositoryRoot(t), filepath.FromSlash(ProjectDir))
}

// readRepositoryFile returns the contents of a repository-relative file.
func readRepositoryFile(t *testing.T, repoPath string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), filepath.FromSlash(repoPath)))
	if err != nil {
		t.Fatalf("read %s: %v", repoPath, err)
	}
	return string(data)
}

// specFragmentDirEnv names the directory the test job collects spec-check
// observations from. The fragment written below is the observation shape of
// the features protocol's spectest package: this module imports nothing from
// the repository, so it writes the same document itself.
const specFragmentDirEnv = "PUTNAMI_SPEC_FRAGMENTS"

type specFragment struct {
	Feature     string `json:"feature"`
	Requirement string `json:"requirement"`
	Check       string `json:"check"`
	Status      string `json:"status"`
	File        string `json:"file"`
	Symbol      string `json:"symbol"`
}

// proves records that the calling test proves one check of a requirement of a
// feature. The test's outcome, read when it ends, is the observation's status:
// passed, failed or skipped. Without a collection directory it records
// nothing.
func proves(t *testing.T, feature, requirement, check string) {
	t.Helper()
	directory := os.Getenv(specFragmentDirEnv)
	if directory == "" {
		return
	}
	_, file, _, _ := runtime.Caller(1)
	symbol := t.Name()
	if index := strings.IndexByte(symbol, '/'); index >= 0 {
		symbol = symbol[:index]
	}
	fragment := specFragment{Feature: feature, Requirement: requirement, Check: check, File: file, Symbol: symbol}
	t.Cleanup(func() {
		switch {
		case t.Failed():
			fragment.Status = "failed"
		case t.Skipped():
			fragment.Status = "skipped"
		default:
			fragment.Status = "passed"
		}
		writeSpecFragment(directory, fragment)
	})
}

// writeSpecFragment publishes one fragment atomically under the digest of its
// bytes. A write failure is silent: a missing observation resolves as missing
// evidence, the fail-closed direction, and a full disk must not turn a green
// test red from inside its cleanup.
func writeSpecFragment(directory string, fragment specFragment) {
	encoded, err := json.Marshal(fragment)
	if err != nil {
		return
	}
	sum := sha256.Sum256(encoded)
	final := filepath.Join(directory, hex.EncodeToString(sum[:16])+".json")
	staging, err := os.CreateTemp(directory, ".fragment-*.tmp")
	if err != nil {
		return
	}
	name := staging.Name()
	_, writeErr := staging.Write(encoded)
	closeErr := staging.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(name)
		return
	}
	if err := os.Rename(name, final); err != nil {
		_ = os.Remove(name)
	}
}

// requireShell skips a test that runs a shell script where no POSIX shell is.
func requireShell(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
}
