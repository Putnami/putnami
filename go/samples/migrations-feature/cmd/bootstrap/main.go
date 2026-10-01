// bootstrap is the escape-hatch entry point for scenarios where the
// framework's lifecycle Migrate phase isn't enough — usually because
// privileged SQL has to run around the schema-apply step.
//
// Real production shape: a Cloud Run / Kubernetes Job that bootstraps a
// shared application role, applies schema migrations, then transfers
// ownership of the new tables to that role. None of those operations
// belong inside a migration file (they touch GRANT / ALTER OWNER, which
// have to be re-run idempotently), but the schema step in the middle is
// just the feature's normal MigrationContributor source.
//
// This sample shows the pattern with database.ApplyToPool — the same
// SQLSource the iam plugin returns from MigrationSources, applied
// directly against a pool the caller owns, with pre/post SQL bracketing
// the apply step.
//
// Prefer cmd/server (lifecycle) + cmd/migrate (migratecli) for the
// common case. Reach for this pattern only when the privileged
// bracketing is unavoidable.
package main

import (
	"context"
	"fmt"
	"os"

	"go.putnami.dev/examples/migrations-feature/iam"

	"go.putnami.dev/database"
)

func main() {
	os.Exit(run())
}

func run() int {
	// PUTNAMI_DESCRIBE is the framework's build-time describe pass: it
	// runs every binary in the workspace to harvest metadata without
	// hitting external systems. The bootstrap binary's whole job is the
	// runtime SQL — there is nothing useful to describe — so exit
	// cleanly and let the describe phase complete.
	if os.Getenv("PUTNAMI_DESCRIBE") != "" {
		return 0
	}

	dsn := os.Getenv("DATABASE_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_DSN is required (postgres://user:pass@host/db)")
		return 1
	}

	ctx := context.Background()
	pool, err := database.NewPool(ctx, database.PoolConfig{DSN: dsn})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pool: %v\n", err)
		return 1
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, preBootstrapSQL); err != nil {
		fmt.Fprintf(os.Stderr, "pre-bootstrap: %v\n", err)
		return 2
	}

	applied, err := database.ApplyToPool(ctx, pool, iam.Source())
	if err != nil {
		fmt.Fprintf(os.Stderr, "apply: %v\n", err)
		return 2
	}
	fmt.Fprintf(os.Stdout, "applied %d migrations\n", len(applied))

	if _, err := pool.Exec(ctx, postBootstrapSQL); err != nil {
		fmt.Fprintf(os.Stderr, "post-bootstrap: %v\n", err)
		return 2
	}
	return 0
}

const preBootstrapSQL = `
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'iam_app') THEN
        CREATE ROLE iam_app NOLOGIN;
    END IF;
END$$;

DO $$
BEGIN
    EXECUTE format('GRANT iam_app TO %I', current_user);
END$$;
`

const postBootstrapSQL = `
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN
        SELECT tablename FROM pg_tables
        WHERE schemaname = 'public' AND tableowner <> 'iam_app'
    LOOP
        EXECUTE format('ALTER TABLE public.%I OWNER TO iam_app', r.tablename);
    END LOOP;
END$$;
`
