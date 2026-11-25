//go:build linux || darwin

package scheduler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// countedRuntime publishes, in a new artifact store, an entry whose runtime
// answers the handshake with version and appends a line to the returned
// counter file each time it starts.
func countedRuntime(t *testing.T, version string) (store *artifactstore.Store, executable, counter string) {
	t.Helper()
	store = artifactstore.New(t.TempDir())
	counter = filepath.Join(t.TempDir(), "starts")
	const digest = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	dir, err := store.Admit(digest, func(stageDir string) error {
		mustWriteRuntimeFile(t, filepath.Join(stageDir, "runtime", "ext"), runtimeAnswering(counter, version), 0o755)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, filepath.Join(dir, "runtime", "ext"), counter
}

// runtimeAnswering is a shell runtime that records its start in counter and
// answers runtime-info as @putnami/test at version.
func runtimeAnswering(counter, version string) string {
	return fmt.Sprintf("#!/bin/sh\necho start >> %q\nprintf '%%s\\n' '{\"extension\":\"@putnami/test\",\"version\":%q,\"platform\":%q,\"cliContract\":%d,\"runtimeProtocol\":%d,\"runtimeABI\":%d}'\n",
		counter, version, runtime.GOOS+"/"+runtime.GOARCH, protocolcli.CurrentContract,
		runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
}

func starts(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "start")
}

func TestARecordedHandshakeStartsNoRuntime(t *testing.T) {
	store, executable, counter := countedRuntime(t, "1.2.3")
	ext := &extension.ExtensionDescription{Name: "@putnami/test", Version: "1.2.3"}
	for range 3 {
		if err := verifyRuntimeHandshake(context.Background(), store, executable, ext, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := starts(t, counter); got != 1 {
		t.Fatalf("three loads of one entry started the runtime %d times, want 1", got)
	}
	spectest.Proves(t, "cli/job-planning-execution", "unchanged-planning-reads-records",
		"a-recorded-handshake-starts-no-runtime")
}

func TestAChangedRuntimeIsHandshakenAgain(t *testing.T) {
	store, executable, counter := countedRuntime(t, "1.2.3")
	ext := &extension.ExtensionDescription{Name: "@putnami/test", Version: "1.2.3"}
	if err := verifyRuntimeHandshake(context.Background(), store, executable, ext, nil); err != nil {
		t.Fatal(err)
	}

	// The record states another version than the run expects: the runtime is
	// asked, and its answer is refused.
	other := &extension.ExtensionDescription{Name: "@putnami/test", Version: "1.2.4"}
	if err := verifyRuntimeHandshake(context.Background(), store, executable, other, nil); err == nil {
		t.Fatal("a record of version 1.2.3 admitted a run that expects 1.2.4")
	}
	if got := starts(t, counter); got != 2 {
		t.Fatalf("a record of another identity left %d starts, want 2", got)
	}

	// The executable is rewritten in place with the same size and
	// modification time: its file identity moved, so it is asked again.
	info, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	mustWriteRuntimeFile(t, executable, runtimeAnswering(counter, "1.2.4"), 0o755)
	if err := os.Chtimes(executable, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := verifyRuntimeHandshake(context.Background(), store, executable, ext, nil); err == nil {
		t.Fatal("a rewritten runtime answering 1.2.4 was admitted from the record of 1.2.3")
	}
	if got := starts(t, counter); got != 3 {
		t.Fatalf("a rewritten runtime left %d starts, want 3", got)
	}
	// The failed handshake recorded nothing: the next load asks again.
	if err := verifyRuntimeHandshake(context.Background(), store, executable, other, nil); err != nil {
		t.Fatal(err)
	}
	if err := verifyRuntimeHandshake(context.Background(), store, executable, other, nil); err != nil {
		t.Fatal(err)
	}
	if got := starts(t, counter); got != 4 {
		t.Fatalf("after a failed handshake, two loads left %d starts, want 4", got)
	}
	spectest.Proves(t, "cli/job-planning-execution", "unchanged-planning-reads-records",
		"a-changed-runtime-is-handshaken-again")
}

func TestARuntimeOutsideTheStoreIsAlwaysHandshaken(t *testing.T) {
	store, _, _ := countedRuntime(t, "1.2.3")
	counter := filepath.Join(t.TempDir(), "starts")
	executable := filepath.Join(t.TempDir(), "ext")
	mustWriteRuntimeFile(t, executable, runtimeAnswering(counter, "1.2.3"), 0o755)
	ext := &extension.ExtensionDescription{Name: "@putnami/test", Version: "1.2.3"}
	for range 2 {
		if err := verifyRuntimeHandshake(context.Background(), store, executable, ext, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := starts(t, counter); got != 2 {
		t.Fatalf("two loads of a runtime outside the store started it %d times, want 2", got)
	}
}
