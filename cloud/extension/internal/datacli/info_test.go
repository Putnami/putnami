package datacli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// TestDBInfoWithGrantShowsFactsAndMetrics: with an active grant the inventory
// annotates the database, so `db info` prints the facts AND runs the read-only
// rollup query, printing size/table/connection metrics — never a secret.
func TestDBInfoWithGrantShowsFactsAndMetrics(t *testing.T) {
	fake := &dbFakeServer{activeGrant: true}
	ioctx, _, _ := newDBTestIO(t, fake)
	var out []string
	ioctx.Stdout = func(s string) { out = append(out, s) }

	if err := runDB(ioctx, []string{"info", testDatabase}); err != nil {
		t.Fatalf("db info: %v", err)
	}
	joined := strings.Join(out, "\n")
	for _, want := range []string{
		"Database: " + testDatabase, "postgres", "proj:region:inst", "public, app",
		"read/active", "12 MB", "tables:      7", "est. rows:   1234", "connections: 3",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("info output missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, oneTimePassword) {
		t.Fatalf("info output leaked a secret:\n%s", joined)
	}
	// Two authed calls: the inventory GET then the metrics query POST.
	if len(fake.requests) != 2 {
		t.Fatalf("requests = %+v, want 2 (inventory GET + query POST)", fake.requests)
	}
	inv, query := fake.requests[0], fake.requests[1]
	if inv.Method != http.MethodGet || query.Method != http.MethodPost {
		t.Fatalf("methods = %s, %s, want GET, POST", inv.Method, query.Method)
	}
	wantQueryPath := "/v1/workspaces/" + testWorkspace + "/databases/" + testDatabase + "/query"
	if query.Path != wantQueryPath {
		t.Fatalf("query path = %q, want %q", query.Path, wantQueryPath)
	}
	if stmt := clicore.StringValue(query.Body["statement"]); !strings.Contains(stmt, "pg_database_size") {
		t.Fatalf("query statement = %q, want the metrics rollup", stmt)
	}
}

// TestDBInfoWithoutGrantShowsHintNoQuery: with no active grant `db info` prints
// the facts plus the one-line hint and NEVER hits the grant-gated query route.
func TestDBInfoWithoutGrantShowsHintNoQuery(t *testing.T) {
	fake := &dbFakeServer{} // no active grant
	ioctx, _, _ := newDBTestIO(t, fake)
	var out []string
	ioctx.Stdout = func(s string) { out = append(out, s) }

	if err := runDB(ioctx, []string{"info", testDatabase}); err != nil {
		t.Fatalf("db info (no grant): %v", err)
	}
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "Database: "+testDatabase) || !strings.Contains(joined, "grant:    none") {
		t.Fatalf("info output missing facts/none-grant line:\n%s", joined)
	}
	if !strings.Contains(joined, "open a grant with 'cloud db grant request'") {
		t.Fatalf("info output missing the metrics hint:\n%s", joined)
	}
	for _, forbidden := range []string{"12 MB", "connections:"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("info output showed metrics without a grant (%q):\n%s", forbidden, joined)
		}
	}
	// Only the inventory GET — the query route must not be called without a grant.
	if len(fake.requests) != 1 || fake.requests[0].Method != http.MethodGet {
		t.Fatalf("requests = %+v, want exactly 1 inventory GET", fake.requests)
	}
}

// TestDBInfoLapsedGrantDegradesToHint: the inventory can still show a grant as
// active after its TTL lapses (it does not re-check expiry), but the query route
// 403s such a grant. `db info` must then print the facts + hint, not surface the
// 403.
func TestDBInfoLapsedGrantDegradesToHint(t *testing.T) {
	fake := &dbFakeServer{activeGrant: true, queryStatus: http.StatusForbidden}
	ioctx, _, _ := newDBTestIO(t, fake)
	var out []string
	ioctx.Stdout = func(s string) { out = append(out, s) }

	if err := runDB(ioctx, []string{"info", testDatabase}); err != nil {
		t.Fatalf("db info with a lapsed grant must not fail: %v", err)
	}
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "Database: "+testDatabase) {
		t.Fatalf("info output missing facts:\n%s", joined)
	}
	if !strings.Contains(joined, "open a grant with 'cloud db grant request'") {
		t.Fatalf("info output missing the metrics hint after a 403:\n%s", joined)
	}
	for _, forbidden := range []string{"12 MB", "connections:"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("metrics leaked despite the grant-gate 403 (%q):\n%s", forbidden, joined)
		}
	}
	// The inventory GET plus the (doomed) query POST were both attempted.
	if len(fake.requests) != 2 {
		t.Fatalf("requests = %+v, want inventory GET + query POST", fake.requests)
	}
}

// TestDBInfoMetricsServerErrorPropagates: a genuine query failure (500) is NOT
// swallowed as a lapsed grant — it surfaces as an API error.
func TestDBInfoMetricsServerErrorPropagates(t *testing.T) {
	fake := &dbFakeServer{activeGrant: true, queryStatus: http.StatusInternalServerError}
	ioctx, _, _ := newDBTestIO(t, fake)
	err := runDB(ioctx, []string{"info", testDatabase})
	if err == nil || clicore.ExitCode(err) != clicore.ExitAPI {
		t.Fatalf("query 500 = %v (exit %d), want an ExitAPI error", err, clicore.ExitCode(err))
	}
}

// TestDBInfoNotFound: a database absent from the inventory is a clean usage error,
// not a crash or an empty render.
func TestDBInfoNotFound(t *testing.T) {
	fake := &dbFakeServer{}
	ioctx, _, _ := newDBTestIO(t, fake)
	err := runDB(ioctx, []string{"info", "ghostdb"})
	if err == nil || clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("unknown database = %v (exit %d), want ExitUsage", err, clicore.ExitCode(err))
	}
	if !strings.Contains(err.Error(), "ghostdb") {
		t.Fatalf("error %q should name the missing database", err.Error())
	}
	// The inventory was fetched but no query was attempted for a missing database.
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %+v, want 1 inventory GET", fake.requests)
	}
}

// TestDBInfoRequiresDatabase: no positional and no --database is a usage error
// before any network call.
func TestDBInfoRequiresDatabase(t *testing.T) {
	fake := &dbFakeServer{}
	ioctx, _, _ := newDBTestIO(t, fake)
	err := runDB(ioctx, []string{"info"})
	if err == nil || clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("missing database = %v, want ExitUsage", err)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("requests = %+v, want none", fake.requests)
	}
}

// TestDBInfoJSONCombinesFactsAndMetrics: --json emits a single secret-free object
// carrying both the inventory facts and the metrics.
func TestDBInfoJSONCombinesFactsAndMetrics(t *testing.T) {
	fake := &dbFakeServer{activeGrant: true}
	ioctx, _, _ := newDBTestIO(t, fake)
	var out []string
	ioctx.Stdout = func(s string) { out = append(out, s) }

	if err := runDB(ioctx, []string{"info", testDatabase, "--output", "json"}); err != nil {
		t.Fatalf("db info --json: %v", err)
	}
	var envelope struct {
		Data struct {
			Database string `json:"database"`
			Engine   string `json:"engine"`
			Metrics  *struct {
				Size        string `json:"size"`
				Tables      int    `json:"tables"`
				LiveRows    int    `json:"live_rows"`
				Connections int    `json:"connections"`
			} `json:"metrics"`
			ActiveGrant *struct {
				Status string `json:"status"`
			} `json:"active_grant"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.Join(out, "\n")), &envelope); err != nil {
		t.Fatalf("json output not decodable: %v\n%s", err, strings.Join(out, "\n"))
	}
	payload := envelope.Data
	if payload.Database != testDatabase || payload.Engine != "postgres" {
		t.Fatalf("json facts = %+v, want %s/postgres", payload, testDatabase)
	}
	if payload.Metrics == nil || payload.Metrics.Size != "12 MB" || payload.Metrics.Tables != 7 || payload.Metrics.Connections != 3 {
		t.Fatalf("json metrics = %+v, want size=12 MB tables=7 connections=3", payload.Metrics)
	}
	if payload.ActiveGrant == nil || payload.ActiveGrant.Status != "active" {
		t.Fatalf("json active_grant = %+v, want status active", payload.ActiveGrant)
	}
}

// TestDBInfoAcceptsDatabaseFlag: `db info --database <db>` works as an alternative
// to the positional form.
func TestDBInfoAcceptsDatabaseFlag(t *testing.T) {
	fake := &dbFakeServer{}
	ioctx, _, _ := newDBTestIO(t, fake)
	var out []string
	ioctx.Stdout = func(s string) { out = append(out, s) }
	if err := runDB(ioctx, []string{"info", "--database", testDatabase}); err != nil {
		t.Fatalf("db info --database: %v", err)
	}
	if !strings.Contains(strings.Join(out, "\n"), "Database: "+testDatabase) {
		t.Fatalf("info --database output missing facts:\n%s", strings.Join(out, "\n"))
	}
}
