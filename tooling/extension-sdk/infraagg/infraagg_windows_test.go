//go:build windows

package infraagg

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	diag "go.putnami.dev/protocol/diagnostic"
)

// heldByAnotherHandle reports whether err is what a removal answers while
// another handle holds the file: ERROR_ACCESS_DENIED or
// ERROR_SHARING_VIOLATION.
func heldByAnotherHandle(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

// closeAfter closes reader 100 ms from now, well inside the retry budget, and
// returns a channel that is closed once the reader closed.
func closeAfter(reader *os.File) <-chan struct{} {
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		time.Sleep(100 * time.Millisecond)
		_ = reader.Close()
	}()
	return closed
}

// requireRefused checks a test's precondition: the operation the test tried
// while a handle held the file was refused with access denied or a sharing
// violation, the refusal the code under test waits out. When the operation went
// through, this host cannot show that refusal, so the test skips and says why
// instead of passing without proving anything. Any other error fails the test.
// release closes the holder before the test stops.
func requireRefused(t *testing.T, what string, err error, release func()) {
	t.Helper()
	if heldByAnotherHandle(err) {
		return
	}
	release()
	if err == nil {
		t.Skipf("precondition: %s went through while another handle held the file, so this host cannot show the refusal the test waits out. "+
			"A read goes through an exclusive hold under an elevated token with SeBackupPrivilege, because Go opens a file read-only "+
			"with FILE_FLAG_BACKUP_SEMANTICS; run the test under a non-elevated token", what)
	}
	t.Fatalf("precondition: %s = %v, want access denied or a sharing violation", what, err)
}

// holdForRemoval opens path the way every Go reader does, without sharing
// delete access, and requires that a plain removal of the held file fails, so
// the test proves the retry and not a host that lets the removal through.
func holdForRemoval(t *testing.T, path string) *os.File {
	t.Helper()
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	requireRefused(t, "removing an open file", os.Remove(path), func() { _ = reader.Close() })
	return reader
}

// holdForRename opens path the way every Go reader does and requires that a
// plain rename of another file over the held one fails, so the test proves the
// retry and not a host that lets the rename through.
func holdForRename(t *testing.T, path string) *os.File {
	t.Helper()
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, []byte("probe\n"), 0o600); err != nil {
		_ = reader.Close()
		t.Fatal(err)
	}
	requireRefused(t, "a rename over an open file", os.Rename(probe, path), func() { _ = reader.Close() })
	return reader
}

// A deployer reading the aggregated manifest holds it open without sharing
// delete access. writeFileAtomic waits for that reader instead of failing
// the rename with access denied.
func TestWriteFileAtomicWaitsForAReaderOfTheManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requirements.json")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	closed := closeAfter(holdForRename(t, path))
	err := writeFileAtomic(path, []byte("new\n"))
	<-closed
	if err != nil {
		t.Fatalf("writeFileAtomic after the reader closed: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new\n" {
		t.Fatalf("the manifest holds %q, %v; want the new bytes", got, err)
	}
}

// Removing a stale aggregated manifest waits for its reader the same way.
func TestRemoveAggregatedManifestWaitsForAReaderOfTheManifest(t *testing.T) {
	root := t.TempDir()
	path := AggregatedManifestPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	closed := closeAfter(holdForRemoval(t, path))
	diags := RemoveAggregatedManifest(root)
	<-closed
	if len(diags) != 0 {
		t.Fatalf("RemoveAggregatedManifest after the reader closed: %v", diags)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the manifest survived RemoveAggregatedManifest: %v", err)
	}
}

// A developer-authored runtime replaces the defaults sidecar, and the removal
// of the stale sidecar waits for its reader.
func TestAggregateWaitsForAReaderOfTheStaleRuntimeDefaults(t *testing.T) {
	root := t.TempDir()
	ctx := closureContext(root, "app", "app", "application")
	writeProjectFile(t, root, "app", ".gen/infra/runtime.json",
		`{"scaling":{"max":1,"concurrency":500},"ingress":{"public":false}}`)
	writeProjectFile(t, root, "app", "infra/runtime.json",
		`{"ingress":{"domain":"api.example.com","public":true},"scaling":{"max":50,"concurrency":80}}`)

	defaultsPath := filepath.Join(root, "app", ".gen", "infra", "runtime.json")
	closed := closeAfter(holdForRemoval(t, defaultsPath))
	result := Aggregate(ctx, Options{})
	<-closed
	if result.Err != nil || diag.HasErrors(result.Diagnostics) {
		t.Fatalf("Aggregate after the reader closed: %v, %v", result.Err, result.Diagnostics)
	}
	if _, err := os.Stat(defaultsPath); !os.IsNotExist(err) {
		t.Fatalf("the stale runtime defaults survived: %v", err)
	}
}
