package infraagg

import (
	"fmt"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/infra"
	pctx "go.putnami.dev/sdk/extension/context"
)

// TestAggregate_ConcurrentReadWriteStress hammers the generator sync path while
// framework producers rewrite their ephemeral sidecars and the aggregator reads
// the committed infra/requirements.json underneath it.
//
// The move to extension-owned aggregation makes this MORE load-bearing, not
// less: the aggregators are now ordinary scheduled tasks, so several of them
// run in parallel worker goroutines (and, across projects, in parallel
// processes) while the generators of other projects are still writing. The
// contract is unchanged — infra.WriteSidecar and SyncGeneratedRequirements
// write atomically (temp + rename), and so does this package — so a reader
// always observes a complete file and never a torn write. Each sidecar here has
// a single writer that varies its content continuously; because the bytes
// genuinely change between reads, a non-atomic write is caught even though the
// real pipelines emit idempotent bytes.
//
// Assertions:
//   - Aggregate never panics under concurrent writes;
//   - every call returns with no error-severity diagnostics — a torn read would
//     surface as an infra.parse_error from the strict per-project parser;
//   - every emitted aggregated manifest strict-parses, so no torn write
//     propagates into the workload artifact.
func TestAggregate_ConcurrentReadWriteStress(t *testing.T) {
	root := t.TempDir()

	const (
		libCount = 4
		appCount = 4
	)
	// Each library exposes these producer slugs; one writer goroutine owns each
	// (library, slug) file so writers never contend on a shared temp path —
	// this isolates the test to the reader-vs-writer window.
	slugs := []string{"database", "storage"}

	libPaths := make([]string, libCount)
	for i := range libPaths {
		libPaths[i] = fmt.Sprintf("lib%d", i)
	}
	// Each workload depends on every library and aggregates to its own
	// .gen/requirements.json, so no two reader goroutines share a write target.
	contexts := make([]*pctx.Context, appCount)
	for j := range contexts {
		appPath := fmt.Sprintf("app%d", j)
		contexts[j] = closureContext(root, appPath, appPath, "application", libPaths...)
	}

	// Per-reader iteration count; ~1000 aggregations total across the workloads.
	perReader := 250
	if testing.Short() {
		perReader = 40
	}

	// Seed every sidecar and committed requirements file before any reader runs
	// so the aggregator observes real contributions from the first iteration
	// (independent of writer scheduling) and the positive assertion below is
	// meaningful.
	for i, libPath := range libPaths {
		libRoot := filepath.Join(root, libPath)
		for _, slug := range slugs {
			if err := infra.WriteSidecar(libRoot, slug, stressManifest(slug, i, 0)); err != nil {
				t.Fatalf("seed WriteSidecar(%s, %q): %v", libRoot, slug, err)
			}
		}
		if diags, err := infra.SyncGeneratedRequirements(libRoot); err != nil || diag.HasErrors(diags) {
			t.Fatalf("seed SyncGeneratedRequirements(%s): err=%v diags=%v", libRoot, err, diags)
		}
	}

	var stop atomic.Bool

	// Writers: continuously rewrite their owned sidecar with always-valid but
	// varying content until the readers finish.
	var writers sync.WaitGroup
	for i, libPath := range libPaths {
		libRoot := filepath.Join(root, libPath)
		for _, slug := range slugs {
			writers.Add(1)
			go func(libRoot, slug string, libIdx int) {
				defer writers.Done()
				for n := 0; !stop.Load(); n++ {
					if err := infra.WriteSidecar(libRoot, slug, stressManifest(slug, libIdx, n)); err != nil {
						t.Errorf("WriteSidecar(%s, %q): %v", libRoot, slug, err)
						return
					}
					if diags, err := infra.SyncGeneratedRequirements(libRoot); err != nil || diag.HasErrors(diags) {
						t.Errorf("SyncGeneratedRequirements(%s): err=%v diags=%v", libRoot, err, diags)
						return
					}
				}
			}(libRoot, slug, i)
		}
	}

	// Readers: aggregate concurrently with the churning writers.
	var readers sync.WaitGroup
	for j := range contexts {
		readers.Add(1)
		go func(ctx *pctx.Context) {
			defer readers.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Aggregate(%s) panicked: %v\n%s", ctx.Project.Name, r, debug.Stack())
				}
			}()
			for n := 0; n < perReader; n++ {
				if result := Aggregate(ctx, Options{}); result.Err != nil || diag.HasErrors(result.Diagnostics) {
					t.Errorf("Aggregate(%s) iteration %d failed a write or surfaced error diagnostics (torn read?): %v, %v",
						ctx.Project.Name, n, result.Err, result.Diagnostics)
					return
				}
			}
		}(contexts[j])
	}

	readers.Wait()
	stop.Store(true)
	writers.Wait()

	if t.Failed() {
		return
	}
	// Every workload's final manifest must be a complete, strict-valid artifact
	// AND must carry every library's database and storage contribution —
	// proving the aggregator actually read the committed requirements synced
	// from churning sidecars rather than emitting only runtime defaults.
	for j := range contexts {
		m := readAggregated(t, root, contexts[j].Project.Path)
		assertAllContributions(t, m, libCount)
	}
}

// stressManifest returns an always-valid per-project manifest whose content
// varies with the writer's iteration n, so the on-disk sidecar genuinely
// changes (in length and bytes) between aggregator reads. Each (library, slug)
// owns a distinct resource identity so contributions union cleanly across the
// workload's dependency closure and never trigger a merge conflict.
func stressManifest(slug string, libIdx, n int) infra.PerProjectManifest {
	switch slug {
	case "database":
		schemas := make([]string, 0, n%4+1)
		for k := 0; k <= n%4; k++ {
			schemas = append(schemas, fmt.Sprintf("schema_%d", k))
		}
		return infra.PerProjectManifest{
			Databases: []infra.Database{{
				Name:    fmt.Sprintf("db_%d", libIdx),
				Engine:  infra.EnginePostgres,
				Schemas: schemas,
			}},
		}
	default: // "storage"
		retentions := []string{"7d", "30d", "90d", "365d"}
		return infra.PerProjectManifest{
			Storage: []infra.StorageBucket{{
				Name:      fmt.Sprintf("store_%d", libIdx),
				Retention: retentions[n%len(retentions)],
			}},
		}
	}
}

// assertAllContributions verifies the aggregated manifest carries every
// library's database and storage requirement, each attributed to the committed
// generated requirements manifest. A database/storage entry can only come from
// infra/requirements.json (the runtime defaults add neither), so this fails if
// any contribution was dropped or if only the runtime block survived.
func assertAllContributions(t *testing.T, m *infra.AggregatedManifest, libCount int) {
	t.Helper()
	dbs := make(map[string]infra.AggregatedDatabase, len(m.Databases))
	for _, db := range m.Databases {
		dbs[db.Name] = db
	}
	stores := make(map[string]infra.AggregatedStorage, len(m.Storage))
	for _, s := range m.Storage {
		stores[s.Name] = s
	}
	if len(dbs) != libCount || len(stores) != libCount {
		t.Errorf("workload %s: got %d databases / %d storage, want %d each",
			m.Workload, len(dbs), len(stores), libCount)
	}
	for i := 0; i < libCount; i++ {
		dbName := fmt.Sprintf("db_%d", i)
		if db, ok := dbs[dbName]; !ok {
			t.Errorf("workload %s: missing database %q", m.Workload, dbName)
		} else if !hasContributor(db.Sources, infra.GeneratedRequirementsContributor) {
			t.Errorf("workload %s: database %q not attributed to committed generated requirements: %+v",
				m.Workload, dbName, db.Sources)
		}
		storeName := fmt.Sprintf("store_%d", i)
		if st, ok := stores[storeName]; !ok {
			t.Errorf("workload %s: missing storage %q", m.Workload, storeName)
		} else if !hasContributor(st.Sources, infra.GeneratedRequirementsContributor) {
			t.Errorf("workload %s: storage %q not attributed to committed generated requirements: %+v",
				m.Workload, storeName, st.Sources)
		}
	}
}

func hasContributor(sources []infra.Source, c infra.ContributorID) bool {
	for _, s := range sources {
		if s.Contributor == c {
			return true
		}
	}
	return false
}
