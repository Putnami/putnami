package testprovider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A per-suite database is named <base>_t_<suffix>, where the suffix is the
// creation time (Unix seconds, 8 lowercase hex digits) followed by 8 random hex
// digits. Postgres records no creation time for a database, so the name carries
// it: that is what lets a later Provision tell an abandoned database from one a
// live suite is still using. The TypeScript provider (@putnami/database) writes
// and reads the same format, so each reclaims what the other left behind.
const (
	suffixTimeLen = 8
	suffixRandLen = 8
	suffixLen     = suffixTimeLen + suffixRandLen
)

// orphanAge is how old an isolated database with no open connection must be
// before a later Provision drops it. A suite whose test process is killed (a
// gate timeout, a SIGKILL, a `go test -timeout` panic) never runs its Cleanup,
// so its database outlives it. The bound is six times `go test`'s default
// 10-minute timeout: a live suite can hold no connection for a moment (the pool
// releases idle connections), but no live suite is that old.
const orphanAge = time.Hour

// maxReclaimPerProvision caps how many orphans one Provision drops. Every DROP
// DATABASE forces a cluster-wide checkpoint, so the cap keeps one suite's setup
// bounded; the next Provision continues where this one stopped.
const maxReclaimPerProvision = 16

// teardownTimeout bounds a per-suite DROP. The teardown never inherits the
// provisioning context's cancellation: a test that provisions with t.Context()
// has that context canceled before its Cleanup runs, and a DROP issued on it
// fails at once, leaving the database behind.
const teardownTimeout = 2 * time.Minute

// isolatedSuffix is the default per-suite suffix: the creation time followed by
// random hex. It panics when crypto/rand fails rather than emit a predictable
// identifier.
func isolatedSuffix(now time.Time) string {
	var buf [suffixRandLen / 2]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("testprovider: read random: %v", err))
	}
	return fmt.Sprintf("%0*x", suffixTimeLen, uint32(now.Unix())) + hex.EncodeToString(buf[:]) //nolint:gosec // Unix seconds fit 32 bits until 2106
}

// isolatedPrefix is the part of every per-suite database name that precedes the
// suffix for one base: pgIdent clamps the base so the tail survives, and the
// clamp depends only on the tail's length.
func isolatedPrefix(base string) string {
	ident := pgIdent(base, "t", strings.Repeat("0", suffixLen))
	return ident[:len(ident)-suffixLen]
}

// createdAt reads the creation time a per-suite database name carries. It
// reports false for any name that is not <prefix><16 lowercase hex digits>,
// which includes every name written before the suffix carried a time: those
// cannot be aged, so they are never reclaimed.
func createdAt(name, prefix string) (time.Time, bool) {
	suffix, found := strings.CutPrefix(name, prefix)
	if !found || len(suffix) != suffixLen {
		return time.Time{}, false
	}
	for _, r := range suffix {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return time.Time{}, false
		}
	}
	secs, err := strconv.ParseUint(suffix[:suffixTimeLen], 16, 32)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(int64(secs), 0), true
}

// staleOrphans selects, oldest first and at most maxReclaimPerProvision, the
// idle databases old enough to reclaim. idle lists databases that had no open
// connection when the server was asked.
func staleOrphans(idle []string, prefix string, now time.Time) []string {
	type orphan struct {
		name    string
		created time.Time
	}
	var stale []orphan
	for _, name := range idle {
		created, ok := createdAt(name, prefix)
		if ok && now.Sub(created) >= orphanAge {
			stale = append(stale, orphan{name: name, created: created})
		}
	}
	sort.Slice(stale, func(i, j int) bool {
		if !stale[i].created.Equal(stale[j].created) {
			return stale[i].created.Before(stale[j].created)
		}
		return stale[i].name < stale[j].name
	})
	if len(stale) > maxReclaimPerProvision {
		stale = stale[:maxReclaimPerProvision]
	}
	out := make([]string, len(stale))
	for i, o := range stale {
		out[i] = o.name
	}
	return out
}

// orphanServer is what reclaiming needs from the server. pgOrphanServer is the
// Postgres implementation; tests substitute a fake.
type orphanServer interface {
	// tryLock takes a session advisory lock without waiting and reports whether
	// it got it, so concurrent suites never reclaim the same base at once.
	tryLock(ctx context.Context, key int64) (bool, error)
	unlock(ctx context.Context, key int64) error
	// idleDatabases lists the databases whose name starts with prefix and that
	// have no open connection.
	idleDatabases(ctx context.Context, prefix string) ([]string, error)
	// dropIdle drops a database only if nothing is connected to it.
	dropIdle(ctx context.Context, name string) error
}

// reclaimOrphans drops the per-suite databases of one base that a killed test
// process left behind. It is best-effort: it never fails the Provision that
// runs it, and it returns the names it dropped.
//
// A live suite's database is never dropped. Three conditions protect it: the
// database must have no open connection, it must be older than orphanAge, and
// the DROP carries no FORCE, so a suite that connects between the listing and
// the DROP makes the DROP fail instead of losing its database.
func reclaimOrphans(ctx context.Context, srv orphanServer, prefix string, now time.Time) []string {
	key := lockKey("reclaim:" + prefix)
	locked, err := srv.tryLock(ctx, key)
	if err != nil || !locked {
		return nil
	}
	defer func() {
		_ = srv.unlock(ctx, key) //nolint:errcheck // best-effort unlock; the session ends on release regardless
	}()
	idle, err := srv.idleDatabases(ctx, prefix)
	if err != nil {
		return nil
	}
	var dropped []string
	for _, name := range staleOrphans(idle, prefix, now) {
		if srv.dropIdle(ctx, name) == nil {
			dropped = append(dropped, name)
		}
	}
	return dropped
}

// pgOrphanServer runs the reclaim statements on one acquired connection, which
// holds the advisory lock for the whole pass.
type pgOrphanServer struct{ conn *pgxpool.Conn }

func (s pgOrphanServer) tryLock(ctx context.Context, key int64) (bool, error) {
	var locked bool
	err := s.conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&locked)
	return locked, err
}

func (s pgOrphanServer) unlock(ctx context.Context, key int64) error {
	_, err := s.conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", key)
	return err
}

func (s pgOrphanServer) idleDatabases(ctx context.Context, prefix string) ([]string, error) {
	rows, err := s.conn.Query(ctx, `SELECT d.datname FROM pg_database d
WHERE left(d.datname, length($1::text)) = $1::text
  AND NOT d.datistemplate
  AND NOT EXISTS (SELECT 1 FROM pg_stat_activity a WHERE a.datname = d.datname)`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func (s pgOrphanServer) dropIdle(ctx context.Context, name string) error {
	_, err := s.conn.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name))
	return err
}
