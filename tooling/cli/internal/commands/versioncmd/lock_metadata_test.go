package versioncmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// A CLI pin that names the running binary's version, with or without a
// leading "v", is stamped with that binary's output protocol version. A lock
// below format version 3 records no protocol version, so the stamp writes it
// at version 3.
func TestRefreshLockMetadataStampsTheCLIProtocolOfTheRunningVersion(t *testing.T) {
	const cliVersion = "1.2.3"
	ws := t.TempDir()
	lf := &lockfile.LockFile{
		Version:    lockfile.FormatVersionV2,
		Extensions: map[string]lockfile.LockEntry{},
		Templates:  map[string]lockfile.LockEntry{},
	}
	lf.SetCLI(lockfile.LockEntry{Version: cliVersion})
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}

	changed, err := RefreshLockMetadataWithResult(context.Background(), ws, "v"+cliVersion)
	if err != nil || !changed {
		t.Fatalf("RefreshLockMetadataWithResult = %v, %v; want the stamp written", changed, err)
	}
	got, err := lockfile.ReadLockFile(ws)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != lockfile.FormatVersionV3 {
		t.Fatalf("lock version = %d, want 3", got.Version)
	}
	if cli, _ := got.GetCLI(); cli.ProtocolVersion != protocolcli.ResultProtocolVersion {
		t.Fatalf("cli.protocolVersion = %d, want %d", cli.ProtocolVersion, protocolcli.ResultProtocolVersion)
	}
}

// A lock whose canonical bytes already match the refresh is not rewritten:
// the file keeps its inode, and the refresh reports no change.
func TestRefreshLockMetadataWithResultSkipsByteIdenticalLockWrite(t *testing.T) {
	ws := t.TempDir()
	lf := lockfile.NewLockFile()
	lf.SetCLI(lockfile.LockEntry{Version: "1.2.3", ProtocolVersion: protocolcli.ResultProtocolVersion})
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ws, lockfile.LockFilename)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := RefreshLockMetadataWithResult(context.Background(), ws, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("byte-identical lock refresh reported a change")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("byte-identical lock refresh replaced the lock inode")
	}
}

// A source workspace pins no published CLI. The refresh leaves that sentinel
// exactly as committed: stamping a protocolVersion would claim an output
// protocol for bytes that do not exist, and rewriting the entry would erase
// the sentinel. Either way the next `putnami install` would dirty a committed
// lock.
func TestRefreshLockMetadataLeavesTheSourceWorkspaceSentinelIntact(t *testing.T) {
	ws := t.TempDir()
	lf := &lockfile.LockFile{
		Version:    lockfile.FormatVersionV3,
		Extensions: map[string]lockfile.LockEntry{},
		Templates:  map[string]lockfile.LockEntry{},
	}
	lf.SetCLI(lockfile.LockEntry{Source: lockfile.SourceWorkspace})
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(ws, lockfile.LockFilename))
	if err != nil {
		t.Fatal(err)
	}

	// The running version is not empty, so the guard on the sentinel, not an
	// empty pinned version, is what prevents the stamp.
	changed, err := RefreshLockMetadataWithResult(context.Background(), ws, "9.9.9")
	if err != nil {
		t.Fatalf("RefreshLockMetadataWithResult: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(ws, lockfile.LockFilename))
	if err != nil {
		t.Fatal(err)
	}
	if changed || !bytes.Equal(before, after) {
		t.Fatalf("the refresh rewrote a committed sentinel lock (changed %v):\nbefore:\n%s\nafter:\n%s", changed, before, after)
	}
	got, err := lockfile.ReadLockFile(ws)
	if err != nil {
		t.Fatal(err)
	}
	if cli, ok := got.GetCLI(); !ok || !cli.IsWorkspaceSource() || cli.ProtocolVersion != 0 || cli.Version != "" {
		t.Fatalf("cli entry = %+v, %v; want the source-workspace sentinel without a protocol version", cli, ok)
	}
}
