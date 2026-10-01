package testprovider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	pdb "go.putnami.dev/protocol/database"
)

var reclaimNow = time.Unix(1_790_000_000, 0)

// stamped builds a per-suite database name created at t, the way the provider
// names one.
func stamped(prefix string, t time.Time, random string) string {
	return prefix + fmt.Sprintf("%08x", uint32(t.Unix())) + random //nolint:gosec // test timestamps fit 32 bits
}

func TestIsolatedSuffix_EncodesCreationTime(t *testing.T) {
	suffix := isolatedSuffix(reclaimNow)
	if len(suffix) != suffixLen {
		t.Fatalf("suffix %q has length %d, want %d", suffix, len(suffix), suffixLen)
	}
	prefix := isolatedPrefix("putnami_platform_test")
	created, ok := createdAt(prefix+suffix, prefix)
	if !ok || !created.Equal(reclaimNow) {
		t.Fatalf("createdAt(%q) = %v, %v; want %v, true", prefix+suffix, created, ok, reclaimNow)
	}
	if other := isolatedSuffix(reclaimNow); other == suffix {
		t.Errorf("two suffixes at the same second are equal (%q); the random half is missing", suffix)
	}
}

func TestIsolatedPrefix_IsThePlannedNamesPrefix(t *testing.T) {
	for _, base := range []string{"putnami_platform_test", "Auth-DB", strings.Repeat("x", 100)} {
		name := pgIdent(base, "t", isolatedSuffix(reclaimNow))
		prefix := isolatedPrefix(base)
		if !strings.HasPrefix(name, prefix) {
			t.Errorf("base %q: planned name %q does not start with prefix %q", base, name, prefix)
		}
		if _, ok := createdAt(name, prefix); !ok {
			t.Errorf("base %q: createdAt cannot read the planned name %q", base, name)
		}
		if len(name) > 63 {
			t.Errorf("base %q: planned name %q exceeds 63 bytes", base, name)
		}
	}
}

func TestPlanDatabases_RecordsTheReclaimPrefix(t *testing.T) {
	tb := &pdb.TestBinding{Databases: map[string]pdb.Database{
		"auth": {Engine: pdb.EnginePostgres, Schema: "iam", Connection: tcp("putnami_platform_test")},
	}}
	plans, err := planDatabases(tb, fixedSuffix("aaa"))
	if err != nil {
		t.Fatalf("planDatabases: %v", err)
	}
	if plans[0].isoPrefix != "putnami_platform_test_t_" {
		t.Errorf("isoPrefix = %q, want putnami_platform_test_t_", plans[0].isoPrefix)
	}
}

func TestCreatedAt_RejectsNamesItCannotAge(t *testing.T) {
	prefix := "base_t_"
	for _, name := range []string{
		"base_t_be4612317a",           // written before the suffix carried a time
		"base_tmpl_0123456789ab",      // a reused template
		"other_t_68b1c2d30123abcd",    // another base
		"base_t_68B1C2D30123ABCD",     // not lowercase hex
		"base_t_68b1c2d30123abcg",     // not hex
		"base_t_68b1c2d30123abcd0",    // too long
		"base",                        // the base itself
		"base_t_68b1c2d30123abc",      // too short
		"base_t_68b1c2d30123abcd_old", // trailing text
	} {
		if created, ok := createdAt(name, prefix); ok {
			t.Errorf("createdAt(%q) = %v, true; want false", name, created)
		}
	}
}

func TestStaleOrphans_SelectsOldIdleDatabasesOldestFirst(t *testing.T) {
	prefix := "base_t_"
	oldest := stamped(prefix, reclaimNow.Add(-72*time.Hour), "00000001")
	old := stamped(prefix, reclaimNow.Add(-orphanAge), "00000002")
	young := stamped(prefix, reclaimNow.Add(-orphanAge+time.Second), "00000003")
	future := stamped(prefix, reclaimNow.Add(time.Hour), "00000004")
	legacy := "base_t_be4612317a"

	got := staleOrphans([]string{young, old, legacy, future, oldest}, prefix, reclaimNow)
	if want := []string{oldest, old}; !slices.Equal(got, want) {
		t.Errorf("staleOrphans = %v, want %v", got, want)
	}
}

func TestStaleOrphans_CapsOnePass(t *testing.T) {
	prefix := "base_t_"
	idle := make([]string, 0, maxReclaimPerProvision+5)
	for i := range maxReclaimPerProvision + 5 {
		idle = append(idle, stamped(prefix, reclaimNow.Add(-time.Duration(i+2)*time.Hour), fmt.Sprintf("%08x", i)))
	}
	got := staleOrphans(idle, prefix, reclaimNow)
	if len(got) != maxReclaimPerProvision {
		t.Fatalf("staleOrphans returned %d names, want the cap %d", len(got), maxReclaimPerProvision)
	}
	if got[0] != idle[len(idle)-1] {
		t.Errorf("first reclaimed = %q, want the oldest %q", got[0], idle[len(idle)-1])
	}
}

// fakeServer models a Postgres server for reclaimOrphans: the databases that
// exist, how many connections each has open, and the advisory locks other
// sessions hold.
type fakeServer struct {
	databases map[string]int // name → open connections
	heldLocks map[int64]bool
	listErr   error
	lockErr   error
	// connectBeforeDrop simulates a suite that connects between the listing
	// and the DROP.
	connectBeforeDrop string
	dropped           []string
	unlocked          []int64
}

func (f *fakeServer) tryLock(_ context.Context, key int64) (bool, error) {
	if f.lockErr != nil {
		return false, f.lockErr
	}
	return !f.heldLocks[key], nil
}

func (f *fakeServer) unlock(_ context.Context, key int64) error {
	f.unlocked = append(f.unlocked, key)
	return nil
}

func (f *fakeServer) idleDatabases(_ context.Context, prefix string) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var idle []string
	for name, conns := range f.databases {
		if strings.HasPrefix(name, prefix) && conns == 0 {
			idle = append(idle, name)
		}
	}
	return idle, nil
}

func (f *fakeServer) dropIdle(_ context.Context, name string) error {
	if name == f.connectBeforeDrop {
		f.databases[name]++
	}
	if f.databases[name] > 0 {
		return errors.New("database is being accessed by other users")
	}
	delete(f.databases, name)
	f.dropped = append(f.dropped, name)
	return nil
}

func TestReclaimOrphans_NeverDropsALiveSuitesDatabase(t *testing.T) {
	prefix := "base_t_"
	orphan := stamped(prefix, reclaimNow.Add(-3*time.Hour), "0000000a")
	liveConnected := stamped(prefix, reclaimNow.Add(-3*time.Hour), "0000000b")
	liveIdleYoung := stamped(prefix, reclaimNow.Add(-5*time.Minute), "0000000c")
	liveConnecting := stamped(prefix, reclaimNow.Add(-3*time.Hour), "0000000d")
	srv := &fakeServer{
		databases: map[string]int{
			orphan:              0,
			liveConnected:       1, // a live suite holding a connection
			liveIdleYoung:       0, // a live suite whose pool released its idle connections
			liveConnecting:      0, // a live suite that connects while the pass runs
			"base_t_be4612317a": 0,
			"base":              3,
		},
		connectBeforeDrop: liveConnecting,
	}

	dropped := reclaimOrphans(context.Background(), srv, prefix, reclaimNow)

	if want := []string{orphan}; !slices.Equal(dropped, want) {
		t.Errorf("reclaimOrphans dropped %v, want only the orphan %v", dropped, want)
	}
	for _, live := range []string{liveConnected, liveIdleYoung, liveConnecting, "base_t_be4612317a", "base"} {
		if _, exists := srv.databases[live]; !exists {
			t.Errorf("live or unaged database %q was dropped", live)
		}
	}
	if len(srv.unlocked) != 1 {
		t.Errorf("advisory lock released %d times, want once", len(srv.unlocked))
	}
}

func TestReclaimOrphans_SkipsWhileAnotherSuiteReclaims(t *testing.T) {
	prefix := "base_t_"
	orphan := stamped(prefix, reclaimNow.Add(-3*time.Hour), "0000000a")
	srv := &fakeServer{
		databases: map[string]int{orphan: 0},
		heldLocks: map[int64]bool{lockKey("reclaim:" + prefix): true},
	}
	if dropped := reclaimOrphans(context.Background(), srv, prefix, reclaimNow); len(dropped) != 0 {
		t.Errorf("dropped %v while another session held the reclaim lock", dropped)
	}
	if len(srv.unlocked) != 0 {
		t.Error("released a lock this session never took")
	}
}

func TestReclaimOrphans_IsBestEffort(t *testing.T) {
	prefix := "base_t_"
	orphan := stamped(prefix, reclaimNow.Add(-3*time.Hour), "0000000a")
	for name, srv := range map[string]*fakeServer{
		"lock error": {databases: map[string]int{orphan: 0}, lockErr: errors.New("boom")},
		"list error": {databases: map[string]int{orphan: 0}, listErr: errors.New("boom")},
	} {
		t.Run(name, func(t *testing.T) {
			if dropped := reclaimOrphans(context.Background(), srv, prefix, reclaimNow); len(dropped) != 0 {
				t.Errorf("dropped %v after a server error", dropped)
			}
		})
	}
}

func TestTeardownContext_OutlivesACanceledProvisionContext(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "v"))
	cancel()

	ctx, release := teardownContext(parent)
	defer release()

	if err := ctx.Err(); err != nil {
		t.Fatalf("teardown context err = %v; a canceled t.Context() would leak the database", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > teardownTimeout {
		t.Errorf("teardown deadline = %v, %v; want one within %v", deadline, ok, teardownTimeout)
	}
	if ctx.Value(key{}) != "v" {
		t.Error("teardown context dropped the provisioning context's values")
	}
}
