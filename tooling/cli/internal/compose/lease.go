package compose

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"go.putnami.dev/sdk/extension/dbtestenv"
	"go.putnami.dev/tooling/cli/internal/flock"
)

// LeaseVersion is the lease.json format this CLI writes and reaps.
const LeaseVersion = 1

const (
	leaseFileName = "lease.json"
	ownerLockName = "owner.lock"
	// reapKillDelay is how long a reaped process group has to exit after
	// SIGTERM before it receives SIGKILL.
	reapKillDelay = 2 * time.Second
	// reapDropTimeout bounds dropping one orphaned database.
	reapDropTimeout = 30 * time.Second
	// groupPollInterval is the cadence of the liveness checks while a signaled
	// group exits.
	groupPollInterval = 50 * time.Millisecond
	// groupReleaseWait is how long a returned serve step's process group has
	// to empty before it stays on record as a survivor.
	groupReleaseWait = time.Second
)

// Lease is the record a running composition keeps under
// .putnami/compose/<id>/lease.json so that the next invocation can release what
// an ungraceful death left behind. It names resources only: process groups,
// database names, the provisioner digest and proxy ports — never a credential.
type Lease struct {
	Version   int    `json:"version"`
	ID        string `json:"id"`
	PID       int    `json:"pid"`
	CreatedAt string `json:"createdAt"`
	Target    string `json:"target"`
	PGIDs     []int  `json:"pgids"`
	// Groups carry the identity of the recorded process groups: the start time
	// of the process that led each one when it was recorded. A pgid without an
	// entry (a lease written before the field existed, or a leader whose start
	// time could not be read) has no identity and is never signaled.
	Groups            []ProcessGroup `json:"groups,omitempty"`
	Databases         []string       `json:"databases"`
	ProvisionerDigest string         `json:"provisionerDigest"`
	ProxyPorts        []int          `json:"proxyPorts"`
}

// ProcessGroup is a recorded process group and the start time of its leader.
type ProcessGroup struct {
	PGID        int    `json:"pgid"`
	LeaderStart string `json:"leaderStart"`
}

// recordGroup records pgid, with its leader's start time when it is known.
func (l *Lease) recordGroup(pgid int, leaderStart string) {
	if !slices.Contains(l.PGIDs, pgid) {
		l.PGIDs = append(l.PGIDs, pgid)
	}
	l.Groups = slices.DeleteFunc(l.Groups, func(g ProcessGroup) bool { return g.PGID == pgid })
	if leaderStart != "" {
		l.Groups = append(l.Groups, ProcessGroup{PGID: pgid, LeaderStart: leaderStart})
	}
}

// forgetGroup removes pgid from the record.
func (l *Lease) forgetGroup(pgid int) {
	l.keepGroups(func(candidate int) bool { return candidate != pgid })
}

// keepGroups keeps the recorded groups keep accepts and drops the others.
func (l *Lease) keepGroups(keep func(pgid int) bool) {
	l.PGIDs = slices.DeleteFunc(l.PGIDs, func(pgid int) bool { return !keep(pgid) })
	l.Groups = slices.DeleteFunc(l.Groups, func(g ProcessGroup) bool { return !keep(g.PGID) })
	if l.PGIDs == nil {
		l.PGIDs = []int{}
	}
}

// leaderStart is the recorded start time of pgid's leader, or "" when the
// lease holds no identity for it.
func (l Lease) leaderStart(pgid int) string {
	for _, group := range l.Groups {
		if group.PGID == pgid {
			return group.LeaderStart
		}
	}
	return ""
}

// ReapedLease is an orphaned composition the reaper released, with what it
// could not release.
type ReapedLease struct {
	Lease
	// Leftovers name the resources that survived the reap. The lease is
	// removed either way: a reap is attempted once and reported, rather than
	// retried by every later invocation.
	Leftovers []string `json:"leftovers,omitempty"`
}

// Root is the directory composition leases live under.
func Root(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".putnami", "compose")
}

// newCompositionID returns 16 lowercase hex characters from crypto/rand.
func newCompositionID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate composition id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// leaseHandle is the live side of a lease: the held owner lock and the record
// it rewrites as resources are acquired.
type leaseHandle struct {
	dir   string
	lock  *flock.Lock
	mu    sync.Mutex
	lease Lease
}

// createLease creates the lease directory, takes its owner lock and publishes
// the first record. The lock is held for the composition's lifetime; the
// descriptor is close-on-exec, so a member process can never keep an orphaned
// lease looking owned.
func createLease(workspaceRoot, target string) (*leaseHandle, error) {
	id, err := newCompositionID()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(Root(workspaceRoot), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create composition lease: %w", err)
	}
	lock, err := flock.Acquire(filepath.Join(dir, ownerLockName), true, true)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("lock composition lease: %w", err)
	}
	handle := &leaseHandle{dir: dir, lock: lock, lease: Lease{
		Version:    LeaseVersion,
		ID:         id,
		PID:        os.Getpid(),
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		Target:     target,
		PGIDs:      []int{},
		Databases:  []string{},
		ProxyPorts: []int{},
	}}
	if err := writeLease(dir, handle.lease); err != nil {
		_ = lock.Release()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return handle, nil
}

// update applies change to the record and republishes it atomically. Callers
// record a resource BEFORE acquiring it, so a death in between leaves a
// reaper something to release rather than something to miss.
func (h *leaseHandle) update(change func(*Lease)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	change(&h.lease)
	return writeLease(h.dir, h.lease)
}

func (h *leaseHandle) snapshot() Lease {
	h.mu.Lock()
	defer h.mu.Unlock()
	lease := h.lease
	lease.PGIDs = append([]int{}, h.lease.PGIDs...)
	lease.Groups = append([]ProcessGroup(nil), h.lease.Groups...)
	lease.Databases = append([]string{}, h.lease.Databases...)
	lease.ProxyPorts = append([]int{}, h.lease.ProxyPorts...)
	return lease
}

// release removes the lease directory and drops the owner lock. The lock
// file goes after the release: Windows before version 1809 cannot remove a
// directory that holds an open file.
func (h *leaseHandle) release() error {
	return flock.RemoveDir(h.dir, ownerLockName, func() error {
		_ = h.lock.Release()
		return nil
	})
}

func writeLease(dir string, lease Lease) error {
	data, err := json.Marshal(lease)
	if err != nil {
		return fmt.Errorf("encode composition lease: %w", err)
	}
	data = append(data, '\n')
	tmp := filepath.Join(dir, leaseFileName+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write composition lease: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, leaseFileName)); err != nil {
		return fmt.Errorf("publish composition lease: %w", err)
	}
	return nil
}

func readLease(dir string) (Lease, bool) {
	data, err := os.ReadFile(filepath.Join(dir, leaseFileName))
	if err != nil {
		return Lease{}, false
	}
	var lease Lease
	if json.Unmarshal(data, &lease) != nil {
		return Lease{}, false
	}
	if lease.Version != LeaseVersion || lease.ID == "" || lease.PID <= 0 || lease.ID != filepath.Base(dir) {
		return Lease{}, false
	}
	return lease, true
}

// reapDeps are the effects ReapOrphans acts through, replaceable in tests.
type reapDeps struct {
	isolator  func() (dbtestenv.DatabaseIsolator, bool)
	killDelay time.Duration
}

func defaultReapDeps() reapDeps {
	return reapDeps{
		isolator: func() (dbtestenv.DatabaseIsolator, bool) {
			isolator, ok := dbtestenv.SelectProvider().(dbtestenv.DatabaseIsolator)
			return isolator, ok
		},
		killDelay: reapKillDelay,
	}
}

// ReapOrphans releases every composition whose owner is gone: its lock is free
// AND its pid is dead. For each, it sends SIGTERM to the recorded process
// groups it confirms (see stopProcessGroups), SIGKILL to the ones still running
// after two seconds, drops the recorded databases, and removes the lease
// directory. It returns the reaped
// leases in creation order.
//
// It follows store.ReapOrphanInvocations, with one difference that is the
// reason it exists: a composition owns resources outside the directory, so the
// lock is held for the whole reap, and a second concurrent reaper skips the
// entry instead of racing it.
func ReapOrphans(workspaceRoot string) []ReapedLease {
	return reapOrphans(workspaceRoot, defaultReapDeps())
}

func reapOrphans(workspaceRoot string, deps reapDeps) []ReapedLease {
	entries, err := os.ReadDir(Root(workspaceRoot))
	if err != nil {
		return nil
	}
	var reaped []ReapedLease
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(Root(workspaceRoot), entry.Name())
		lease, ok := readLease(dir)
		if !ok || lease.PID == os.Getpid() || processAlive(lease.PID) {
			// Unreadable (being created, or a future format) or owned: leave it.
			continue
		}
		lock, err := flock.Acquire(filepath.Join(dir, ownerLockName), true, true)
		if err != nil {
			continue // a live owner, or another reaper, holds it
		}
		result := ReapedLease{Lease: lease}
		result.Leftovers = append(result.Leftovers, stopProcessGroups(lease, deps.killDelay)...)
		result.Leftovers = append(result.Leftovers, dropLeaseDatabases(lease, deps)...)
		_ = flock.RemoveDir(dir, ownerLockName, lock.Release)
		reaped = append(reaped, result)
	}
	sort.Slice(reaped, func(i, j int) bool {
		if reaped[i].CreatedAt != reaped[j].CreatedAt {
			return reaped[i].CreatedAt < reaped[j].CreatedAt
		}
		return reaped[i].ID < reaped[j].ID
	})
	return reaped
}

// stopProcessGroups terminates the lease's process groups that it confirms are
// still the ones the composition started, escalates to SIGKILL after delay
// (confirming again first), and names every group it left running.
//
// A process group id is handed to another process once its group is empty, so
// a signal to a recorded id is only sent while the recorded leader still leads
// the group. A group whose identity cannot be confirmed — no start time on
// record, or a leader that is gone while the group still runs — is named and
// never signaled. An id whose leader is a different process names a group
// that is not this composition's: it is neither signaled nor reported.
func stopProcessGroups(lease Lease, delay time.Duration) []string {
	var leftovers []string
	var running []int
	for _, pgid := range lease.PGIDs {
		switch identifyGroup(pgid, lease.leaderStart(pgid)) {
		case groupOwned:
			_ = terminateProcessGroup(pgid)
			running = append(running, pgid)
		case groupUnverified:
			leftovers = append(leftovers, unverifiedGroup(pgid))
		}
	}
	running = waitForGroups(running, delay)
	var killed []int
	for _, pgid := range running {
		switch identifyGroup(pgid, lease.leaderStart(pgid)) {
		case groupOwned:
			_ = killProcessGroup(pgid)
			killed = append(killed, pgid)
		case groupUnverified:
			leftovers = append(leftovers, unverifiedGroup(pgid))
		}
	}
	for _, pgid := range waitForGroups(killed, time.Second) {
		leftovers = append(leftovers, fmt.Sprintf("process group %d", pgid))
	}
	return leftovers
}

// unverifiedGroup names a running group the reaper did not signal.
func unverifiedGroup(pgid int) string {
	return fmt.Sprintf("process group %d (not signaled: its leader is gone or was not recorded, so the id is not confirmed to be this composition's)", pgid)
}

// groupIdentity is what a recorded process group id names now.
type groupIdentity int

const (
	// groupGone: no process runs in the group, or the id now names a group
	// led by another process, which the kernel only allows once the recorded
	// group was empty.
	groupGone groupIdentity = iota
	// groupOwned: the recorded leader still leads the running group.
	groupOwned
	// groupUnverified: the group runs, but nothing confirms it is the
	// recorded one.
	groupUnverified
)

// identifyGroup compares a running group's leader with the start time
// recorded for it. A pid, and so a group id, is never reused while a process
// or a non-empty group holds it; a leader alive with the recorded start time is
// therefore the recorded process, leading the recorded group.
func identifyGroup(pgid int, leaderStart string) groupIdentity {
	if !processGroupAlive(pgid) {
		return groupGone
	}
	if leaderStart == "" {
		return groupUnverified
	}
	start, ok := processStartTime(pgid)
	switch {
	case !ok:
		return groupUnverified
	case start != leaderStart:
		return groupGone
	default:
		return groupOwned
	}
}

// waitForGroups polls until every group exited or delay elapsed, and returns
// the groups still running.
func waitForGroups(pgids []int, delay time.Duration) []int {
	deadline := time.Now().Add(delay)
	for {
		alive := pgids[:0:0]
		for _, pgid := range pgids {
			if processGroupAlive(pgid) {
				alive = append(alive, pgid)
			}
		}
		if len(alive) == 0 || !time.Now().Before(deadline) {
			return alive
		}
		pgids = alive
		time.Sleep(groupPollInterval)
	}
}

// dropLeaseDatabases drops the lease's databases through the provider that
// created them, and names every one it could not drop.
func dropLeaseDatabases(lease Lease, deps reapDeps) []string {
	if len(lease.Databases) == 0 {
		return nil
	}
	var isolator dbtestenv.DatabaseIsolator
	ok := false
	if deps.isolator != nil {
		isolator, ok = deps.isolator()
	}
	var leftovers []string
	for _, name := range lease.Databases {
		if !ok || lease.ProvisionerDigest == "" {
			leftovers = append(leftovers, "database "+name+" (no isolating provider to drop it)")
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), reapDropTimeout)
		err := isolator.DropDatabase(ctx, lease.ProvisionerDigest, name)
		cancel()
		if err != nil {
			leftovers = append(leftovers, "database "+name+" ("+err.Error()+")")
		}
	}
	return leftovers
}
