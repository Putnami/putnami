package database

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// TestUnitOfWork_ConcurrentQueries_Live runs the request fan-out against
// PostgreSQL, or SKIPS without a test binding: goroutines of one request read,
// list and write on one pool through its unit of work at once. Every query
// succeeds and sees the request's uncommitted writes, an Autocommit read on
// its own connection does not, and the boundary commits every write.
func TestUnitOfWork_ConcurrentQueries_Live(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "concurrent-request-queries", "concurrent-queries-take-turns-on-the-transaction")
	spectest.Proves(t, "go/persistence-transactions", "autocommit-context", "autocommit-query-skips-the-unit-of-work")
	conn := liveTestConnection(t)
	pool := liveTestPool(t, conn)
	ctx := context.Background()

	table := fmt.Sprintf("uow_concurrent_%d_%d", time.Now().UnixNano(), os.Getpid())
	if _, err := pool.Exec(ctx, `CREATE TABLE "`+table+`" (v int NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DROP TABLE IF EXISTS "`+table+`"`); err != nil {
			t.Errorf("drop table: %v", err)
		}
	})

	cc, scope := scopedUoW(t)
	defer cc.Close()
	rctx := scope.Context(ctx)
	if _, err := pool.Exec(rctx, `INSERT INTO "`+table+`" (v) VALUES (0)`); err != nil {
		t.Fatalf("request write: %v", err)
	}

	const goroutines = 24
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 3 {
			case 0:
				var n int
				if err := pool.QueryRow(rctx, `SELECT count(*) FROM "`+table+`"`).Scan(&n); err != nil {
					errs <- err
					return
				}
				if n < 1 {
					errs <- fmt.Errorf("a request read saw %d rows, want the request's own write", n)
				}
			case 1:
				rows, err := pool.Query(rctx, `SELECT v FROM "`+table+`"`)
				if err != nil {
					errs <- err
					return
				}
				for rows.Next() {
					var v int
					if err := rows.Scan(&v); err != nil {
						rows.Close()
						errs <- err
						return
					}
				}
				errs <- rows.Err()
			default:
				_, err := pool.Exec(rctx, `INSERT INTO "`+table+`" (v) VALUES ($1)`, i)
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent query of one request: %v", err)
		}
	}

	var outside int
	if err := pool.QueryRow(Autocommit(rctx), `SELECT count(*) FROM "`+table+`"`).Scan(&outside); err != nil {
		t.Fatalf("Autocommit read: %v", err)
	}
	if outside != 0 {
		t.Fatalf("an Autocommit read saw %d uncommitted rows, want 0", outside)
	}

	if err := scope.Finalize(rctx, nil); err != nil {
		t.Fatalf("the boundary failed to commit: %v", err)
	}
	var committed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM "`+table+`"`).Scan(&committed); err != nil {
		t.Fatalf("count after commit: %v", err)
	}
	if want := 1 + goroutines/3; committed != want {
		t.Fatalf("committed rows = %d, want %d", committed, want)
	}
}
