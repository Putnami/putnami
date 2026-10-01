package store

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/sdk/extension/ownerperm"
)

// invocationChildEnv makes the test binary act as a fixture PROVIDER that holds
// an invocation scratch open until it is killed. It is the only way to prove
// the SIGKILL half of the crash contract: a finalizer cannot run, so the lease
// on disk is the entire recovery mechanism.
const invocationChildEnv = "PUTNAMI_TEST_INVOCATION_HOLD"

// invocationChildHandshake names the file the child writes its scratch id into,
// so the parent kills it only once the lease is durably published.
const invocationChildHandshake = "PUTNAMI_TEST_INVOCATION_HANDSHAKE"

func TestNewInvocationScratch_PrivateTreeAndNonSecretLease(t *testing.T) {
	root := t.TempDir()
	scratch, err := NewInvocationScratch(root, "@acme/provider", "digest-abc")
	if err != nil {
		t.Fatalf("NewInvocationScratch: %v", err)
	}
	t.Cleanup(func() { _ = scratch.Discard() })

	if !strings.HasPrefix(scratch.ID(), "inv-") {
		t.Errorf("invocation id = %q, want an inv- handle", scratch.ID())
	}
	if !strings.HasPrefix(scratch.ArtifactRoot(), InvocationsRoot(root)) {
		t.Errorf("artifact root %q is outside %q", scratch.ArtifactRoot(), InvocationsRoot(root))
	}
	for _, dir := range []string{filepath.Dir(scratch.ArtifactRoot()), scratch.ArtifactRoot()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		// Windows reports no mode bits; TestInvocationScratch_PrivateOnEveryPlatform
		// checks its access lists.
		if got := info.Mode().Perm(); got != 0o700 && runtime.GOOS != "windows" {
			t.Errorf("%s mode = %04o, want 0700", dir, got)
		}
	}

	lease := scratch.Lease()
	if lease.Version != InvocationLeaseVersion || lease.ID != scratch.ID() ||
		lease.PID != os.Getpid() || lease.Provider != "@acme/provider" ||
		lease.ActionDigest != "digest-abc" || lease.CreatedAt == "" {
		t.Fatalf("lease = %+v", lease)
	}
	if _, err := time.Parse(time.RFC3339, lease.CreatedAt); err != nil {
		t.Errorf("lease createdAt %q is not RFC3339: %v", lease.CreatedAt, err)
	}
}

// TestInvocationLease_HasNoFreeFormMember pins the shape a credential could hide
// in. A lease is copied onto external resources as a label and read back by a
// provider's filter, so "credentials never appear in leases, labels, names,
// filters, logs, or errors" has to be a property of the TYPE — a map or an
// interface member would make it a property of every caller instead.
func TestInvocationLease_HasNoFreeFormMember(t *testing.T) {
	typ := reflect.TypeOf(InvocationLease{})
	wantJSON := []string{"actionDigest", "createdAt", "invocationId", "pid", "provider", "version"}

	var gotJSON []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		switch field.Type.Kind() {
		case reflect.String, reflect.Int:
		default:
			t.Errorf("lease member %s is %s; a lease carries only scalars", field.Name, field.Type.Kind())
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		gotJSON = append(gotJSON, name)
	}
	sort.Strings(gotJSON)
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Errorf("lease wire members = %v, want %v", gotJSON, wantJSON)
	}
}

func TestInvocationScratch_SensitiveArtifactStaysOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports no mode bits; TestInvocationScratch_PrivateOnEveryPlatform checks its access lists")
	}
	root := t.TempDir()
	scratch, err := NewInvocationScratch(root, "@acme/provider", "digest")
	if err != nil {
		t.Fatalf("NewInvocationScratch: %v", err)
	}
	t.Cleanup(func() { _ = scratch.Discard() })

	path, err := scratch.Prepare("database/dsn.env", true)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := permOf(t, path); got != 0o600 {
		t.Fatalf("reserved sensitive artifact mode = %04o, want 0600", got)
	}
	if got := permOf(t, filepath.Dir(path)); got != 0o700 {
		t.Fatalf("sensitive artifact directory mode = %04o, want 0700", got)
	}

	// A provider that writes through a temporary file and renames defeats the
	// reservation: the mode travels with the new inode. Harden is what closes it.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("DSN=postgres://u:p@h/db\n"), 0o644); err != nil {
		t.Fatalf("write provider temp: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename provider temp: %v", err)
	}
	if got := permOf(t, path); got != 0o644 {
		t.Fatalf("precondition: renamed artifact mode = %04o, want 0644", got)
	}
	if err := scratch.Harden("database/dsn.env"); err != nil {
		t.Fatalf("Harden: %v", err)
	}
	if got := permOf(t, path); got != 0o600 {
		t.Errorf("hardened artifact mode = %04o, want 0600", got)
	}
}

// The private tree and a sensitive artifact admit only the current user on
// every platform: through mode bits on Unix and through an access list on
// Windows, where mode bits restrict nobody. A provider that writes elsewhere
// and moves the file in brings the file's old permissions with it on both;
// Harden replaces them.
func TestInvocationScratch_PrivateOnEveryPlatform(t *testing.T) {
	root := t.TempDir()
	scratch, err := NewInvocationScratch(root, "@acme/provider", "digest")
	if err != nil {
		t.Fatalf("NewInvocationScratch: %v", err)
	}
	t.Cleanup(func() { _ = scratch.Discard() })
	ownerOnly := func(path string) bool {
		t.Helper()
		private, err := ownerperm.OwnerOnly(path)
		if err != nil {
			t.Fatalf("OwnerOnly(%s): %v", path, err)
		}
		return private
	}

	path, err := scratch.Prepare("database/dsn.env", true)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for _, private := range []string{filepath.Dir(scratch.ArtifactRoot()), scratch.ArtifactRoot(), filepath.Dir(path), path} {
		if !ownerOnly(private) {
			t.Errorf("%s admits more than the current user", private)
		}
	}

	outside := filepath.Join(t.TempDir(), "dsn.env")
	if err := os.WriteFile(outside, []byte("DSN=postgres://u:p@h/db\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(outside, path); err != nil {
		t.Fatalf("move the provider's file in: %v", err)
	}
	if ownerOnly(path) {
		t.Skip("the temporary directory admits only the current user, so a moved file proves nothing here")
	}
	if err := scratch.Harden("database/dsn.env"); err != nil {
		t.Fatalf("Harden: %v", err)
	}
	if !ownerOnly(path) {
		t.Error("a hardened sensitive artifact admits more than the current user")
	}
}

func TestInvocationScratch_VerifyReportsMissingArtifact(t *testing.T) {
	root := t.TempDir()
	scratch, err := NewInvocationScratch(root, "@acme/provider", "digest")
	if err != nil {
		t.Fatalf("NewInvocationScratch: %v", err)
	}
	t.Cleanup(func() { _ = scratch.Discard() })

	if err := scratch.Verify("database/dsn.env"); !errors.Is(err, ErrInvocationArtifactMissing) {
		t.Fatalf("Verify on absent artifact = %v, want ErrInvocationArtifactMissing", err)
	}
	if _, err := scratch.Prepare("database/dsn.env", true); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := scratch.Verify("database/dsn.env"); err != nil {
		t.Errorf("Verify on present artifact = %v", err)
	}
}

func TestInvocationScratch_ResolveRejectsEscape(t *testing.T) {
	root := t.TempDir()
	scratch, err := NewInvocationScratch(root, "@acme/provider", "digest")
	if err != nil {
		t.Fatalf("NewInvocationScratch: %v", err)
	}
	t.Cleanup(func() { _ = scratch.Discard() })

	for _, rel := range []string{"", "  ", "/etc/passwd", "../escape", "a/../../escape", ".", "a\\b"} {
		if _, err := scratch.Resolve(rel); err == nil {
			t.Errorf("Resolve(%q) accepted a path outside the invocation scratch", rel)
		}
	}
	got, err := scratch.Resolve("database/./dsn.env")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := filepath.Join(scratch.ArtifactRoot(), "database", "dsn.env"); got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}

func TestInvocationScratch_DiscardIsIdempotent(t *testing.T) {
	root := t.TempDir()
	scratch, err := NewInvocationScratch(root, "@acme/provider", "digest")
	if err != nil {
		t.Fatalf("NewInvocationScratch: %v", err)
	}
	if err := scratch.Discard(); err != nil {
		t.Fatalf("first Discard: %v", err)
	}
	if err := scratch.Discard(); err != nil {
		t.Fatalf("second Discard: %v", err)
	}
	if _, err := os.Stat(scratch.ArtifactRoot()); !os.IsNotExist(err) {
		t.Errorf("artifact root survived Discard: %v", err)
	}
}

// TestReapOrphanInvocations_SparesTheLiveOwner is the safety half of the reap
// rule: this process owns the scratch, so no amount of reaping may remove it.
func TestReapOrphanInvocations_SparesTheLiveOwner(t *testing.T) {
	root := t.TempDir()
	scratch, err := NewInvocationScratch(root, "@acme/provider", "digest")
	if err != nil {
		t.Fatalf("NewInvocationScratch: %v", err)
	}
	t.Cleanup(func() { _ = scratch.Discard() })

	if reaped := ReapOrphanInvocations(root); len(reaped) != 0 {
		t.Fatalf("reaped %d live invocations: %+v", len(reaped), reaped)
	}
	if _, err := os.Stat(scratch.ArtifactRoot()); err != nil {
		t.Errorf("live artifact root was removed: %v", err)
	}
}

// TestReapOrphanInvocations_LeavesUnrecognizedState pins the conservative
// direction: a tree whose ownership cannot be established is left alone rather
// than deleted, because "unreadable" and "abandoned" are not the same claim.
func TestReapOrphanInvocations_LeavesUnrecognizedState(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(InvocationsRoot(root), "inv-garbage")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, invocationLeaseFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write lease: %v", err)
	}
	if reaped := ReapOrphanInvocations(root); len(reaped) != 0 {
		t.Fatalf("reaped %+v, want nothing", reaped)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("unrecognized invocation tree was removed: %v", err)
	}
}

// TestReapOrphanInvocations_RecoversAfterSIGKILL is the crash conformance test.
//
// A SIGKILL cannot run a finalizer, so the contract is entirely about what a
// LATER process finds: the lease must survive the kill, and the next invocation
// must reap the orphan immediately rather than waiting for a GC interval.
func TestReapOrphanInvocations_RecoversAfterSIGKILL(t *testing.T) {
	root := t.TempDir()
	handshake := filepath.Join(t.TempDir(), "child-scratch-id")

	cmd := exec.Command(os.Args[0], "-test.run=^TestInvocationScratchHoldsUntilKilled$", "-test.timeout=60s")
	cmd.Env = append(os.Environ(),
		invocationChildEnv+"="+root,
		invocationChildHandshake+"="+handshake,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	id := waitForHandshake(t, handshake)
	dir := filepath.Join(InvocationsRoot(root), id)

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL holder: %v", err)
	}
	_ = cmd.Wait()

	// The lease persisted the kill: that is the whole recovery mechanism.
	data, err := os.ReadFile(filepath.Join(dir, invocationLeaseFileName))
	if err != nil {
		t.Fatalf("lease did not survive SIGKILL: %v", err)
	}
	var lease InvocationLease
	if err := json.Unmarshal(data, &lease); err != nil {
		t.Fatalf("unmarshal surviving lease: %v", err)
	}
	if lease.ID != id || lease.PID != cmd.Process.Pid || lease.Provider != "@acme/provider" {
		t.Fatalf("surviving lease = %+v (child pid %d)", lease, cmd.Process.Pid)
	}

	reaped := ReapOrphanInvocations(root)
	if len(reaped) != 1 || reaped[0].ID != id {
		t.Fatalf("reaped = %+v, want exactly the orphan %s", reaped, id)
	}
	if reaped[0].ActionDigest != "digest-killed" {
		t.Errorf("reaped action digest = %q, want the producing action", reaped[0].ActionDigest)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("orphan scratch survived the reap: %v", err)
	}
	if again := ReapOrphanInvocations(root); len(again) != 0 {
		t.Errorf("second reap returned %+v, want nothing (reaping is idempotent)", again)
	}
}

// TestInvocationScratchHoldsUntilKilled is the child half of the SIGKILL test.
// It creates a scratch, publishes the handshake, and blocks until the parent
// kills it — deliberately without any cleanup path.
func TestInvocationScratchHoldsUntilKilled(t *testing.T) {
	root := os.Getenv(invocationChildEnv)
	if root == "" {
		t.Skip("child-only helper; driven by TestReapOrphanInvocations_RecoversAfterSIGKILL")
	}
	scratch, err := NewInvocationScratch(root, "@acme/provider", "digest-killed")
	if err != nil {
		t.Fatalf("child NewInvocationScratch: %v", err)
	}
	if err := os.WriteFile(os.Getenv(invocationChildHandshake), []byte(scratch.ID()), 0o600); err != nil {
		t.Fatalf("child handshake: %v", err)
	}
	time.Sleep(time.Minute)
}

func waitForHandshake(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return string(data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("holder never published its invocation id at %s", path)
	return ""
}

func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}
