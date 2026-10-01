// Invocation-scoped private scratch and the non-secret crash-recovery lease.
//
// A declared output with `scope: "invocation"` does not live in a staging root
// and is never captured (protocols/extension/task_contract.go). It lives HERE:
// a private per-invocation tree the orchestrator creates before the producing
// task runs and destroys when the invocation ends. A `sensitive` one is
// additionally mode 0600, because the whole point of the flag is that the value
// never leaves the process tree that needs it.
//
// Two facts make this a store concern rather than a scheduler one:
//
//   - The tree outlives the goroutine that made it. A SIGKILL cannot run a
//     finalizer, so the only thing that can recover an orphan is a LATER
//     process finding a durable record on disk. That record — the invocation
//     LEASE — is a store artifact with the same "written by one process, read by
//     another" contract as every other file in this package.
//   - Liveness is decided the way this package already decides it everywhere
//     else: an advisory whole-file lock (internal/flock). A lock the owner holds
//     for its whole life is released by the kernel when the owner dies, however
//     it died, which is a stronger statement than any heartbeat could make.
//
// The lease is NON-SECRET by construction: its type has exactly five members —
// invocation id, pid, provider, action digest, creation time — and none of them
// can carry a credential. That is deliberate. A lease is copied onto external
// resources as a label so a provider can find its own orphans, and a label is a
// surface a secret may never reach (doc/07-lifecycles.md).
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/ownerperm"
	"go.putnami.dev/tooling/cli/internal/flock"
)

const (
	// invocationsDirName holds one subdirectory per live (or orphaned)
	// invocation, under the workspace's .putnami directory. It is per-worktree
	// and never the machine-global CAS: nothing in it is content-addressed, and
	// two worktrees must not be able to reap each other's scratch.
	invocationsDirName = "invocations"
	// invocationArtifactsDirName is the subdirectory an invocation-scoped
	// declared output resolves against — the `artifactRoot` of the job context's
	// invocation member.
	invocationArtifactsDirName = "artifacts"
	// invocationLeaseFileName is the durable non-secret ownership record.
	invocationLeaseFileName = "lease.json"
	// invocationOwnerLockName is the file whose advisory lock the owning process
	// holds for the invocation's whole life. Its release is what tells a later
	// process that the owner is gone.
	invocationOwnerLockName = "owner.lock"
	// InvocationLeaseVersion is the lease record's format version. A record that
	// does not carry it is not a lease this build wrote and is left alone.
	InvocationLeaseVersion = 1

	// invocationDirMode keeps the whole private tree owner-only.
	invocationDirMode = fs.FileMode(0o700)
	// invocationSensitiveMode is the mode a sensitive artifact is created with
	// and re-asserted at, after the producing task wrote it.
	invocationSensitiveMode = fs.FileMode(0o600)
)

// ErrInvocationArtifactMissing reports that a declared invocation-scoped output
// is absent after its producer succeeded — the task and its declaration
// disagree. Callers map it onto the typed `sensitive.artifact_missing` failure.
var ErrInvocationArtifactMissing = errors.New("declared invocation-scoped output is missing")

// InvocationLease is the durable, NON-SECRET record of one invocation's
// ownership of external resources.
//
// Every member is a locator or a timestamp. There is deliberately no free-form
// member: a lease is copied onto container labels, resource names and provider
// filters, and a map a caller could stuff a DSN into would make "credentials
// never appear in leases, labels, names, filters, logs, or errors" unprovable.
type InvocationLease struct {
	// Version is InvocationLeaseVersion.
	Version int `json:"version"`
	// ID is the opaque, non-secret invocation handle. It is the same value the
	// job context's `invocation.id` carries and the value a provider stamps on
	// the resources it provisions.
	ID string `json:"invocationId"`
	// PID is the owning process. It is a fast pre-check only: the authoritative
	// liveness signal is the owner lock, because a pid can be reused.
	PID int `json:"pid"`
	// Provider names the extension that provisions and tears the resource down.
	Provider string `json:"provider"`
	// ActionDigest is the producing action's cache identity — derived from the
	// producer's declared inputs, never from anything it produced. It is what a
	// downstream consumer's cache key folds in, so a reader can tell which
	// action a leaked resource belongs to without reading the resource.
	ActionDigest string `json:"actionDigest"`
	// CreatedAt is RFC3339 UTC.
	CreatedAt string `json:"createdAt"`
}

// InvocationScratch is one invocation's private artifact tree plus the lease
// that outlives it.
type InvocationScratch struct {
	root         string
	artifactRoot string
	lease        InvocationLease
	owner        *flock.Lock
}

// InvocationsRoot is the directory holding every invocation scratch of a
// workspace.
func InvocationsRoot(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, putnamiDir, invocationsDirName)
}

// NewInvocationScratch creates a private artifact tree for one finalizes
// relation and publishes its lease.
//
// The order is load-bearing: the owner lock is taken BEFORE the lease is
// written, so a concurrent reaper can never observe a lease whose owner lock is
// free and conclude the invocation is an orphan. provider and actionDigest are
// recorded verbatim; both are non-secret by contract.
func NewInvocationScratch(workspaceRoot, provider, actionDigest string) (*InvocationScratch, error) {
	id, err := newInvocationID()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(InvocationsRoot(workspaceRoot), id)
	artifactRoot := filepath.Join(root, invocationArtifactsDirName)
	if err := os.MkdirAll(artifactRoot, invocationDirMode); err != nil {
		return nil, fmt.Errorf("create invocation scratch: %w", err)
	}
	// MkdirAll honors the process umask, and the parent directories it created
	// on the way may predate this call. Assert the mode explicitly so a
	// permissive umask cannot widen a private tree. On Windows the mode bits
	// restrict nobody: ownerperm also gives both directories an access list
	// that admits only the current user and passes on to what they will hold.
	for _, dir := range []string{root, artifactRoot} {
		if err := ownerperm.Restrict(dir, invocationDirMode); err != nil {
			return nil, fmt.Errorf("restrict invocation scratch: %w", err)
		}
	}

	owner, err := flock.Acquire(filepath.Join(root, invocationOwnerLockName), true, true)
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("claim invocation scratch: %w", err)
	}

	scratch := &InvocationScratch{
		root:         root,
		artifactRoot: artifactRoot,
		owner:        owner,
		lease: InvocationLease{
			Version:      InvocationLeaseVersion,
			ID:           id,
			PID:          os.Getpid(),
			Provider:     provider,
			ActionDigest: actionDigest,
			CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		},
	}
	if err := writeInvocationLease(root, scratch.lease); err != nil {
		_ = owner.Release()
		_ = os.RemoveAll(root)
		return nil, err
	}
	return scratch, nil
}

// ID is the non-secret invocation handle.
func (s *InvocationScratch) ID() string {
	if s == nil {
		return ""
	}
	return s.lease.ID
}

// ArtifactRoot is the absolute private directory invocation-scoped declared
// output paths resolve against.
func (s *InvocationScratch) ArtifactRoot() string {
	if s == nil {
		return ""
	}
	return s.artifactRoot
}

// Lease returns a copy of the invocation's non-secret lease.
func (s *InvocationScratch) Lease() InvocationLease {
	if s == nil {
		return InvocationLease{}
	}
	return s.lease
}

// Resolve turns a declared invocation-scoped output path into its absolute
// location, rejecting anything that would escape the private tree.
func (s *InvocationScratch) Resolve(rel string) (string, error) {
	if s == nil {
		return "", errors.New("no invocation scratch")
	}
	clean, err := cleanInvocationRel(rel)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.artifactRoot, clean), nil
}

// Prepare creates the parent directory of a declared invocation-scoped output
// and, for a sensitive one, pre-creates the file at mode 0600.
//
// Pre-creating is what makes the mode hold for the common provider: open(2) and
// os.WriteFile apply their permission argument only when they CREATE the file,
// so a task that writes 0644 into an existing 0600 file leaves it 0600. A task
// that writes through a temporary file and renames defeats that, which is why
// Harden re-asserts the mode afterwards. Both are needed; neither is sufficient.
func (s *InvocationScratch) Prepare(rel string, sensitive bool) (string, error) {
	path, err := s.Resolve(rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), invocationDirMode); err != nil {
		return "", fmt.Errorf("create invocation artifact directory: %w", err)
	}
	if !sensitive {
		return path, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, invocationSensitiveMode)
	if err != nil {
		return "", fmt.Errorf("reserve sensitive invocation artifact: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("reserve sensitive invocation artifact: %w", err)
	}
	if err := ownerperm.Restrict(path, invocationSensitiveMode); err != nil {
		return "", fmt.Errorf("restrict sensitive invocation artifact: %w", err)
	}
	return path, nil
}

// Verify reports whether a declared invocation-scoped output exists after its
// producer ran.
//
// A missing artifact returns ErrInvocationArtifactMissing so the caller can
// raise the typed `sensitive.artifact_missing` failure rather than letting the
// consumers run against a path to nothing.
func (s *InvocationScratch) Verify(rel string) error {
	path, err := s.Resolve(rel)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrInvocationArtifactMissing, rel)
	} else if err != nil {
		return err
	}
	return nil
}

// Harden re-asserts owner-only permissions on a SENSITIVE declared
// invocation-scoped output after its producer ran.
//
// A file a task moved in from elsewhere keeps the access list it had there on
// Windows, so Harden replaces it as it re-asserts the mode.
//
// It is deliberately not applied to a non-sensitive invocation-scoped output:
// 0600 strips the execute bit, and an ordinary runtime artifact may legitimately
// be a socket directory or an executable helper. Privacy for those comes from
// the 0700 tree they live in.
func (s *InvocationScratch) Harden(rel string) error {
	path, err := s.Resolve(rel)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrInvocationArtifactMissing, rel)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ownerperm.Restrict(path, invocationSensitiveMode)
	}
	return filepath.WalkDir(path, func(entry string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return ownerperm.Restrict(entry, invocationDirMode)
		}
		return ownerperm.Restrict(entry, invocationSensitiveMode)
	})
}

// Discard destroys the private tree and releases the owner lock. It is
// idempotent: a finalizer that already ran, a second call from the run's
// end-of-life sweep, and a reaper that got there first all end in the same
// state.
func (s *InvocationScratch) Discard() error {
	if s == nil {
		return nil
	}
	if s.owner == nil {
		return os.RemoveAll(s.root)
	}
	owner := s.owner
	s.owner = nil
	// The owner lock is released before its file goes: Windows before
	// version 1809 cannot remove a directory that holds an open file.
	return flock.RemoveDir(s.root, invocationOwnerLockName, func() error {
		_ = owner.Release()
		return nil
	})
}

// ReapOrphanInvocations removes every invocation scratch in the workspace whose
// lease has no live owner, and returns their leases in creation order.
//
// It is called BEFORE provisioning, not on a timer, which is what makes "the
// next invocation after a kill recovers the orphan without waiting for a GC
// interval" true. The returned leases are the recovery signal's payload and are
// what a provider matches its own orphaned external resources against.
//
// An entry is an orphan only when BOTH liveness signals say so: its owner lock
// is free AND its pid is not running. The conjunction is deliberately biased
// towards leaving state alone — a pid that has been recycled by an unrelated
// process delays a reap by one run, whereas reaping a live invocation would
// delete the artifacts a running build is reading.
func ReapOrphanInvocations(workspaceRoot string) []InvocationLease {
	root := InvocationsRoot(workspaceRoot)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var reaped []InvocationLease
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		lease, ok := readInvocationLease(dir)
		if !ok {
			// Not a lease this build wrote (a partially created tree, a record
			// from a future format). Leaving it is the conservative direction:
			// deleting state whose ownership cannot be established is exactly
			// what the two-signal rule above exists to avoid.
			continue
		}
		if lease.PID == os.Getpid() || !invocationOwnerGone(dir, lease) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			continue
		}
		reaped = append(reaped, lease)
	}
	sort.Slice(reaped, func(i, j int) bool {
		if reaped[i].CreatedAt != reaped[j].CreatedAt {
			return reaped[i].CreatedAt < reaped[j].CreatedAt
		}
		return reaped[i].ID < reaped[j].ID
	})
	return reaped
}

// invocationOwnerGone reports whether nothing is alive that owns dir.
func invocationOwnerGone(dir string, lease InvocationLease) bool {
	if ProcessAlive(lease.PID) {
		return false
	}
	lock, err := flock.Acquire(filepath.Join(dir, invocationOwnerLockName), true, true)
	if err != nil {
		return false // busy (a live owner) or unreadable: leave it alone
	}
	_ = lock.Release()
	return true
}

func readInvocationLease(dir string) (InvocationLease, bool) {
	data, err := os.ReadFile(filepath.Join(dir, invocationLeaseFileName))
	if err != nil {
		return InvocationLease{}, false
	}
	var lease InvocationLease
	if json.Unmarshal(data, &lease) != nil {
		return InvocationLease{}, false
	}
	if lease.Version != InvocationLeaseVersion || lease.ID == "" || lease.PID <= 0 {
		return InvocationLease{}, false
	}
	return lease, true
}

// writeInvocationLease publishes the record atomically: a reaper must never see
// a half-written lease and decide it is unowned.
func writeInvocationLease(dir string, lease InvocationLease) error {
	data, err := json.Marshal(lease)
	if err != nil {
		return fmt.Errorf("marshal invocation lease: %w", err)
	}
	data = append(data, '\n')
	tmp := filepath.Join(dir, invocationLeaseFileName+".tmp")
	if err := os.WriteFile(tmp, data, invocationSensitiveMode); err != nil {
		return fmt.Errorf("write invocation lease: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, invocationLeaseFileName)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish invocation lease: %w", err)
	}
	return nil
}

// newInvocationID returns the opaque, non-secret invocation handle. It is
// random rather than derived so two concurrent invocations of the same action
// never collide on a private tree; it is not a credential and nothing may treat
// it as one.
func newInvocationID() (string, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generate invocation id: %w", err)
	}
	return "inv-" + hex.EncodeToString(nonce[:]), nil
}

// cleanInvocationRel is the declared-path rule applied to the invocation
// scratch: relative, cleaned, slash-separated, no escape above the root, never
// the root itself. It mirrors protocols/extension's NormalizeRelativePath
// without importing the manifest vocabulary into the store.
func cleanInvocationRel(rel string) (string, error) {
	trimmed := strings.TrimSpace(rel)
	if trimmed == "" {
		return "", errors.New("invocation artifact path is empty")
	}
	if strings.ContainsAny(trimmed, "\\") {
		return "", fmt.Errorf("invocation artifact path %q is not slash-separated", rel)
	}
	if strings.HasPrefix(trimmed, "/") || filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("invocation artifact path %q must be relative", rel)
	}
	clean := filepath.ToSlash(filepath.Clean(trimmed))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invocation artifact path %q escapes the invocation scratch", rel)
	}
	return filepath.FromSlash(clean), nil
}
