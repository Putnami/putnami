package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/flock"
)

const (
	leaseDirName          = "leases"
	leaseFileSuffix       = ".lease"
	defaultLeaseTTL       = 30 * time.Second
	defaultLeaseHeartbeat = 10 * time.Second
	defaultLeasePoll      = 25 * time.Millisecond
	leaseWaitCancelPoll   = 100 * time.Millisecond
	leaseRecordVersion    = 1
)

var (
	// ErrLeaseExpired means the current owner released its lease without
	// publishing, or its heartbeat TTL elapsed. Callers may try to claim again;
	// exactly one waiter will become the replacement owner.
	ErrLeaseExpired = errors.New("store lease expired before publish")

	// ErrLeaseWaitTimeout means the caller's wait budget elapsed while another
	// owner still held a live lease. Coalescing is an optimization, so callers
	// should fall back to computing locally.
	ErrLeaseWaitTimeout = errors.New("timed out waiting for leased publish")

	// This keyed registry orders the lease holders of one process by path, so
	// two LocalStore handles that share a root never rely on how two handles of
	// one process contend for the same file lock.
	leaseProcessLocks = struct {
		sync.Mutex
		paths map[string]*leaseProcessLock
	}{paths: make(map[string]*leaseProcessLock)}
)

type leaseProcessLock struct {
	mu   sync.Mutex
	refs int
}

// leaseRecord is the small on-disk ownership heartbeat. ExpiresAtNano is a wall
// clock because independently-running processes cannot share Go's monotonic
// clock reading; all participants are on the same machine-global store.
type leaseRecord struct {
	Version       int    `json:"version"`
	Key           string `json:"key"`
	Owner         string `json:"owner"`
	ExpiresAtNano int64  `json:"expiresAt"`
}

// WorthCoalescing applies the same 200ms minimum-duration floor used by the
// remote-cache break-even policy. A known-cheap operation bypasses all lease
// I/O and computes independently. A non-positive estimate means "unknown" and
// remains eligible: a brand-new key in a fresh worktree is the primary cold-
// start stampede this primitive exists to prevent.
func WorthCoalescing(estimatedCost time.Duration) bool {
	if estimatedCost <= 0 {
		return true
	}
	floor := time.Duration(cache.DefaultBreakEven.MinDurationMs) * time.Millisecond
	return estimatedCost >= floor
}

// TryClaim delegates cache-miss ownership to the machine-global local store.
// Keeping this on CacheManager lets production execution coordinate without
// reaching through to the store implementation.
func (cm *CacheManager) TryClaim(key string, estimatedCost time.Duration) (winner bool, release func()) {
	return cm.store.TryClaim(key, estimatedCost)
}

// WaitForPublish waits for a sibling process to publish key while remaining
// responsive to scheduler cancellation. LocalStore's public primitive owns the
// lease-state semantics; the short wait slices only add context cancellation
// without spawning a goroutine that could outlive the job.
func (cm *CacheManager) WaitForPublish(ctx context.Context, key string, timeout time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if timeout <= 0 {
		return cm.store.WaitForPublish(key, timeout)
	}

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ErrLeaseWaitTimeout
		}
		wait := min(remaining, leaseWaitCancelPoll)
		err := cm.store.WaitForPublish(key, wait)
		if !errors.Is(err, ErrLeaseWaitTimeout) {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// TryClaim attempts to own the computation that will publish key. Exactly one
// process wins while a lease is live. A winner receives an idempotent release
// function and a heartbeat keeps its ownership alive until release. A loser
// should call WaitForPublish, then look the entry up normally. estimatedCost is
// optional for callers without a prediction; when supplied, only its first
// value is used to apply the break-even floor.
//
// Any filesystem/locking failure and any known cost below the break-even floor
// return winner=true with a no-op release. Leasing must never make a build less
// reliable than computing the key independently.
func (s *LocalStore) TryClaim(key string, estimatedCost ...time.Duration) (winner bool, release func()) {
	cost := time.Duration(0)
	if len(estimatedCost) > 0 {
		cost = estimatedCost[0]
	}
	return s.tryClaim(key, cost, func() bool { return s.entryPublished(key) })
}

// tryClaim is the target-agnostic form used by other store content-addressed
// paths (for example CAS blob fetches). published must be safe to call while the
// store's shared lock is held.
func (s *LocalStore) tryClaim(key string, estimatedCost time.Duration, published func() bool) (bool, func()) {
	noop := func() {}
	if !WorthCoalescing(estimatedCost) {
		return true, noop
	}

	path := s.leasePath(key)
	owner := newLeaseOwner()
	winner, err := s.claimLease(path, key, owner, published)
	if err != nil {
		return true, noop // graceful fallback: compute without coalescing
	}
	if !winner {
		return false, noop
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go s.heartbeatLease(path, owner, stop, done)

	var once sync.Once
	release := func() {
		once.Do(func() {
			close(stop)
			<-done
			s.expireLease(path, owner)
		})
	}
	return true, release
}

// WaitForPublish blocks until key has been atomically published, the current
// owner expires/releases, or timeout elapses. On ErrLeaseExpired callers should
// call TryClaim again before computing, so one waiter takes over rather than all
// waiters recomputing together. On ErrLeaseWaitTimeout callers may compute
// directly as a bounded-wait reliability fallback.
func (s *LocalStore) WaitForPublish(key string, timeout time.Duration) error {
	return s.waitForPublish(key, timeout, func() bool { return s.entryPublished(key) })
}

// waitForPublish is the target-agnostic form used by other store paths.
func (s *LocalStore) waitForPublish(key string, timeout time.Duration, published func() bool) error {
	path := s.leasePath(key)
	deadline := s.leaseNow().Add(timeout)

	for {
		isPublished, rec, err := s.leaseSnapshot(path, published)
		if isPublished {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read store lease: %w", err)
		}

		now := s.leaseNow()
		if rec.Owner == "" || rec.ExpiresAtNano <= now.UnixNano() {
			return ErrLeaseExpired
		}
		if timeout <= 0 || !deadline.After(now) {
			return ErrLeaseWaitTimeout
		}

		wait := s.leasePollDuration()
		if untilExpiry := time.Unix(0, rec.ExpiresAtNano).Sub(now); untilExpiry < wait {
			wait = untilExpiry
		}
		if remaining := deadline.Sub(now); remaining < wait {
			wait = remaining
		}
		if wait <= 0 {
			continue
		}
		timer := time.NewTimer(wait)
		<-timer.C
	}
}

// claimLease serializes the check-and-set with a per-key flock. The store-wide
// shared lock is always acquired first; GC takes it exclusively before deleting
// expired lease files, so no process can retain a descriptor for an unlinked
// lock inode and accidentally form a second lock domain.
func (s *LocalStore) claimLease(path, key, owner string, published func() bool) (bool, error) {
	releaseStore := s.lockShared()
	defer releaseStore()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	releaseLease, err := acquireLeaseFileLock(path)
	if err != nil {
		return false, err
	}
	defer releaseLease()

	// A previous owner may have published and released between the caller's
	// cache miss and this claim attempt. Treat that as a lost claim so the caller
	// waits (which returns immediately) and consumes the existing entry.
	if published != nil && published() {
		return false, nil
	}

	now := s.leaseNow()
	if rec, ok := readLeaseRecord(path); ok && rec.Owner != "" && rec.ExpiresAtNano > now.UnixNano() {
		return false, nil
	}

	rec := leaseRecord{
		Version:       leaseRecordVersion,
		Key:           key,
		Owner:         owner,
		ExpiresAtNano: now.Add(s.leaseTTLDuration()).UnixNano(),
	}
	if err := writeLeaseRecord(path, rec); err != nil {
		return false, err
	}
	return true, nil
}

func (s *LocalStore) heartbeatLease(path, owner string, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(s.leaseHeartbeatDuration())
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			owned, err := s.extendLease(path, owner)
			if err == nil && !owned {
				return // expired, released, reaped, or taken over
			}
			// A transient lock/filesystem error is allowed to retry. If it persists,
			// the TTL expires and a waiter safely takes over.
		}
	}
}

func (s *LocalStore) extendLease(path, owner string) (bool, error) {
	releaseStore := s.lockShared()
	defer releaseStore()

	releaseLease, err := acquireLeaseFileLock(path)
	if err != nil {
		return false, err
	}
	defer releaseLease()

	rec, ok := readLeaseRecord(path)
	now := s.leaseNow()
	if !ok || rec.Owner != owner || rec.ExpiresAtNano <= now.UnixNano() {
		return false, nil
	}
	rec.ExpiresAtNano = now.Add(s.leaseTTLDuration()).UnixNano()
	if err := writeLeaseRecord(path, rec); err != nil {
		return false, err
	}
	return true, nil
}

// expireLease marks ownership released in place instead of unlinking the file.
// Removing a flock file while another process has it open can create two inode-
// backed lock domains. GC later removes expired records under the store-wide
// exclusive lock, when no lease operation can have the file open.
func (s *LocalStore) expireLease(path, owner string) {
	releaseStore := s.lockShared()
	defer releaseStore()

	releaseLease, err := acquireLeaseFileLock(path)
	if err != nil {
		return
	}
	defer releaseLease()

	rec, ok := readLeaseRecord(path)
	if !ok || rec.Owner != owner {
		return // ownership already expired and was taken over
	}
	rec.ExpiresAtNano = s.leaseNow().UnixNano()
	_ = writeLeaseRecord(path, rec)
}

// leaseSnapshot reads a consistent publish+lease state under both locks. The
// per-key flock excludes a heartbeat's in-place record rewrite; the shared
// store lock excludes GC from unlinking the lease inode during the read.
func (s *LocalStore) leaseSnapshot(path string, published func() bool) (bool, leaseRecord, error) {
	releaseStore := s.lockShared()
	defer releaseStore()

	if published != nil && published() {
		return true, leaseRecord{}, nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, leaseRecord{}, nil
		}
		return false, leaseRecord{}, err
	}
	releaseLease, err := acquireLeaseFileLock(path)
	if err != nil {
		return false, leaseRecord{}, err
	}
	defer releaseLease()

	// Recheck after taking the per-key lock: the owner publishes before it
	// expires the record, and either event may have happened while we waited.
	if published != nil && published() {
		return true, leaseRecord{}, nil
	}
	rec, ok := readLeaseRecord(path)
	if !ok {
		return false, leaseRecord{}, nil // malformed/crash-torn means unowned
	}
	return false, rec, nil
}

func (s *LocalStore) entryPublished(key string) bool {
	fi, err := os.Stat(filepath.Join(s.blobDir(key), "result.json"))
	return err == nil && fi.Mode().IsRegular()
}

func (s *LocalStore) leasePath(key string) string {
	// Hash the caller's namespace+key so digests containing ':' and any future
	// opaque keys cannot escape the lease tree or create platform-specific names.
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:])
	return filepath.Join(s.root, leaseDirName, name[:2], name+leaseFileSuffix)
}

func (s *LocalStore) leaseTTLDuration() time.Duration {
	if s.leaseTTL > 0 {
		return s.leaseTTL
	}
	return defaultLeaseTTL
}

func (s *LocalStore) leaseHeartbeatDuration() time.Duration {
	ttl := s.leaseTTLDuration()
	if s.leaseHeartbeat > 0 && s.leaseHeartbeat < ttl {
		return s.leaseHeartbeat
	}
	if ttl != defaultLeaseTTL {
		return max(time.Millisecond, ttl/3)
	}
	return defaultLeaseHeartbeat
}

// leaseNow is the clock the lease code reads: record stamps, expiry checks and
// the deadline of a wait.
func (s *LocalStore) leaseNow() time.Time {
	if s.leaseClock != nil {
		return s.leaseClock()
	}
	return time.Now()
}

func (s *LocalStore) leasePollDuration() time.Duration {
	if s.leasePoll > 0 {
		return s.leasePoll
	}
	return defaultLeasePoll
}

func newLeaseOwner() string {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err == nil {
		return fmt.Sprintf("%d-%x", os.Getpid(), nonce[:])
	}
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}

func acquireLeaseFileLock(path string) (func(), error) {
	releaseProcess := acquireProcessLeaseLock(path)
	l, err := flock.Acquire(path, true, false)
	if err != nil {
		releaseProcess()
		return nil, err
	}
	return func() {
		_ = l.Release()
		releaseProcess()
	}, nil
}

func acquireProcessLeaseLock(path string) func() {
	leaseProcessLocks.Lock()
	l := leaseProcessLocks.paths[path]
	if l == nil {
		l = &leaseProcessLock{}
		leaseProcessLocks.paths[path] = l
	}
	l.refs++
	leaseProcessLocks.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		leaseProcessLocks.Lock()
		l.refs--
		if l.refs == 0 {
			delete(leaseProcessLocks.paths, path)
		}
		leaseProcessLocks.Unlock()
	}
}

func readLeaseRecord(path string) (leaseRecord, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return leaseRecord{}, false
	}
	var rec leaseRecord
	if json.Unmarshal(data, &rec) != nil || rec.Version != leaseRecordVersion || rec.Owner == "" {
		return leaseRecord{}, false
	}
	return rec, true
}

// writeLeaseRecord rewrites the already-flocked inode in place. Every caller
// holds the per-key flock, so readers never observe the truncate/write window.
func writeLeaseRecord(path string, rec leaseRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func hasLeaseSidecars(root string) bool {
	found := false
	_ = filepath.Walk(filepath.Join(root, leaseDirName), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(info.Name(), leaseFileSuffix) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// reapExpiredLeases removes stale/released sidecars. The caller must hold the
// store-wide exclusive lock. Active records are deliberately left untouched;
// lease bytes are never included in GC's store accounting.
func reapExpiredLeases(root string, now time.Time) {
	leaseRoot := filepath.Join(root, leaseDirName)
	_ = filepath.Walk(leaseRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(info.Name(), leaseFileSuffix) {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil // unreadable is conservatively treated as possibly active
		}
		var rec leaseRecord
		valid := json.Unmarshal(data, &rec) == nil && rec.Version == leaseRecordVersion && rec.Owner != ""
		if !valid || rec.ExpiresAtNano <= now.UnixNano() {
			_ = os.Remove(path)
		}
		return nil
	})
	removeEmptyPrefixDirs(leaseRoot)
	if entries, err := os.ReadDir(leaseRoot); err == nil && len(entries) == 0 {
		_ = os.Remove(leaseRoot)
	}
}
