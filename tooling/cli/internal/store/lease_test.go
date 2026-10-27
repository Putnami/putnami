package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const leaseTestKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func configureFastLease(s *LocalStore, ttl time.Duration) {
	s.leaseTTL = ttl
	s.leaseHeartbeat = ttl / 4
	s.leasePoll = 5 * time.Millisecond
}

// The waiter sees the winner's publish. The test is not about expiry, so both
// store handles read a frozen lease clock: on a loaded host the winner's
// heartbeat can run later than a short TTL allows, and the waiter would then
// rightly report an expired lease (seen on a Windows VM under the full suite).
// The frozen clock also keeps the waiter's budget from running out, so the
// waiter returns only on the publish, or on the expiry release writes.
func TestLease_OneWinnerPublishesForWaiter(t *testing.T) {
	root := t.TempDir()
	winnerStore := NewLocalStore(root)
	waiterStore := NewLocalStore(root)
	configureFastLease(winnerStore, 500*time.Millisecond)
	configureFastLease(waiterStore, 500*time.Millisecond)
	frozen := time.Now()
	winnerStore.leaseClock = func() time.Time { return frozen }
	waiterStore.leaseClock = func() time.Time { return frozen }

	winner, release := winnerStore.TryClaim(leaseTestKey, time.Second)
	if !winner {
		t.Fatal("first claimant did not win")
	}
	t.Cleanup(release)

	if won, loserRelease := waiterStore.TryClaim(leaseTestKey, time.Second); won {
		loserRelease()
		t.Fatal("second claimant won while the first lease was live")
	}

	waited := make(chan error, 1)
	go func() { waited <- waiterStore.WaitForPublish(leaseTestKey, 2*time.Second) }()

	if err := winnerStore.Put(leaseTestKey, leaseStatusEntry()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	release()

	if err := <-waited; err != nil {
		t.Fatalf("wait for publish: %v", err)
	}
}

// The heartbeat keeps ownership past the expiry the claim stamped. Both store
// handles read clocks the test sets, so the proof does not depend on when the
// scheduler runs the heartbeat.
func TestLease_HeartbeatKeepsOwnershipLive(t *testing.T) {
	const ttl = 500 * time.Millisecond
	root := t.TempDir()
	owner := NewLocalStore(root)
	contender := NewLocalStore(root)
	configureFastLease(owner, ttl)
	configureFastLease(contender, ttl)
	start := time.Now()
	var ownerNow atomic.Int64
	ownerNow.Store(start.UnixNano())
	owner.leaseClock = func() time.Time { return time.Unix(0, ownerNow.Load()) }
	// The contender reads an instant after the claim's expiry: without a
	// heartbeat, the lease is expired for it.
	afterClaimExpiry := start.Add(ttl + 100*time.Millisecond)
	contender.leaseClock = func() time.Time { return afterClaimExpiry }

	winner, release := owner.TryClaim(leaseTestKey, time.Second)
	if !winner {
		t.Fatal("first claimant did not win")
	}
	defer release()

	// The owner's clock moves inside the claim's TTL, so its next heartbeat
	// extends the lease one TTL past that instant.
	beforeClaimExpiry := start.Add(ttl - 100*time.Millisecond)
	ownerNow.Store(beforeClaimExpiry.UnixNano())
	extended := beforeClaimExpiry.Add(ttl).UnixNano()
	path := owner.leasePath(leaseTestKey)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, record, err := owner.leaseSnapshot(path, nil)
		if err != nil {
			t.Fatalf("read lease: %v", err)
		}
		if record.ExpiresAtNano == extended {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no heartbeat extended the lease: it expires at %d, want %d", record.ExpiresAtNano, extended)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if won, contenderRelease := contender.TryClaim(leaseTestKey, time.Second); won {
		contenderRelease()
		t.Fatal("contender stole a heartbeating lease")
	}

	release()
	if won, takeoverRelease := contender.TryClaim(leaseTestKey, time.Second); !won {
		t.Fatal("released lease was not immediately claimable")
	} else {
		takeoverRelease()
	}
}

func TestLease_KnownCheapWorkBypassesCoalescing(t *testing.T) {
	root := t.TempDir()
	a := NewLocalStore(root)
	b := NewLocalStore(root)

	floor := 200 * time.Millisecond
	if CoalescingFloor != floor {
		t.Fatalf("CoalescingFloor = %s, want %s", CoalescingFloor, floor)
	}
	if WorthCoalescing(floor - time.Millisecond) {
		t.Fatal("known sub-floor work must not be coalesced")
	}
	if !WorthCoalescing(floor) {
		t.Fatal("work at the coalescing floor should be coalesced")
	}
	if !WorthCoalescing(0) {
		t.Fatal("unknown cold-key cost should remain eligible for coalescing")
	}

	// Bypassing returns both callers as independent winners and creates no
	// coordination sidecar at all.
	winnerA, releaseA := a.TryClaim(leaseTestKey, floor-time.Millisecond)
	winnerB, releaseB := b.TryClaim(leaseTestKey, floor-time.Millisecond)
	releaseA()
	releaseB()
	if !winnerA || !winnerB {
		t.Fatalf("cheap claim winners = (%t, %t), want both true", winnerA, winnerB)
	}
	if _, err := os.Stat(filepath.Join(root, leaseDirName)); !os.IsNotExist(err) {
		t.Fatalf("cheap claims created lease state: %v", err)
	}
}

func TestLease_WaitTimeoutFallsBack(t *testing.T) {
	root := t.TempDir()
	owner := NewLocalStore(root)
	waiter := NewLocalStore(root)
	configureFastLease(owner, time.Second)
	configureFastLease(waiter, time.Second)

	winner, release := owner.TryClaim(leaseTestKey, time.Second)
	if !winner {
		t.Fatal("first claimant did not win")
	}
	defer release()

	start := time.Now()
	err := waiter.WaitForPublish(leaseTestKey, 50*time.Millisecond)
	if !errors.Is(err, ErrLeaseWaitTimeout) {
		t.Fatalf("WaitForPublish error = %v, want ErrLeaseWaitTimeout", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond || elapsed > time.Second {
		t.Fatalf("wait timeout elapsed = %s, want a bounded ~50ms wait", elapsed)
	}
}

func TestLease_CacheManagerWaitHonorsContextCancellation(t *testing.T) {
	root := t.TempDir()
	owner := NewLocalStore(root)
	waiter := NewCacheManager(NewLocalStore(root))
	configureFastLease(owner, time.Second)
	configureFastLease(waiter.store, time.Second)

	winner, release := owner.TryClaim(leaseTestKey, time.Second)
	if !winner {
		t.Fatal("first claimant did not win")
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := waiter.WaitForPublish(ctx, leaseTestKey, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitForPublish error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("canceled cache-manager wait took %s", elapsed)
	}
}

func TestLease_GCPreservesLiveAndReapsExpiredSidecar(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	configureFastLease(s, 2*time.Second)

	winner, release := s.TryClaim(leaseTestKey, time.Second)
	if !winner {
		t.Fatal("first claimant did not win")
	}
	leasePath := s.leasePath(leaseTestKey)
	leaseInfo, err := os.Stat(leasePath)
	if err != nil {
		t.Fatalf("stat active lease: %v", err)
	}

	entryKey := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := s.Put(entryKey, leaseStatusEntry()); err != nil {
		t.Fatalf("seed entry: %v", err)
	}
	expectedBytes := blobMetaBytes(s.blobDir(entryKey)) + dirBytes(filepath.Join(root, "cas"))

	res, err := RunGC([]string{root}, GCOptions{
		MaxBytes: 1,
		Grace:    0,
		Now:      time.Now(),
	})
	if err != nil {
		t.Fatalf("RunGC with active lease: %v", err)
	}
	if res.ScannedBytes != expectedBytes {
		t.Fatalf("ScannedBytes = %d, want %d (lease's %d bytes excluded)", res.ScannedBytes, expectedBytes, leaseInfo.Size())
	}
	if _, err := os.Stat(leasePath); err != nil {
		t.Fatalf("GC reaped an unexpired lease: %v", err)
	}

	release()
	if _, err := RunGC([]string{root}, GCOptions{
		MaxBytes: 1 << 20,
		Grace:    0,
		Now:      time.Now().Add(time.Second),
	}); err != nil {
		t.Fatalf("RunGC after release: %v", err)
	}
	if _, err := os.Stat(leasePath); !os.IsNotExist(err) {
		t.Fatalf("expired lease was not reaped: %v", err)
	}
}

func leaseStatusEntry() *Entry {
	return &Entry{
		Result:   &EntryResult{Status: "success"},
		Metadata: &EntryMetadata{Extension: "test", Task: "build", Project: "project"},
	}
}
