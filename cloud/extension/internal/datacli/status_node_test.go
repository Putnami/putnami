package datacli

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// writeCIEnvs writes a putnami.ci.json whose prod environment selects paths,
// and a putnami.json for each path named in names.
func writeCIEnvs(t *testing.T, workspaceRoot string, paths []string, names map[string]string) {
	t.Helper()
	document := map[string]any{"envs": map[string]any{"prod": map[string]any{"workloads": []map[string]any{{"select": paths}}}}}
	raw, _ := json.Marshal(document)
	if err := os.WriteFile(filepath.Join(workspaceRoot, "putnami.ci.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	for path, name := range names {
		dir := filepath.Join(workspaceRoot, filepath.FromSlash(path))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "putnami.json"), []byte(`{"name":"`+name+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func readyBinding() map[string]any {
	return map[string]any{"project_name": "p", "environment": "prod", "logical_name": "main", "database_name": testDatabase, "ready": true}
}

func runDBStatus(t *testing.T, fake *dbFakeServer, prepare func(workspaceRoot string), args ...string) (string, error) {
	t.Helper()
	ioctx, _, workspaceRoot := newDBTestIO(t, fake)
	if prepare != nil {
		prepare(workspaceRoot)
	}
	var out []string
	ioctx.Stdout = func(line string) { out = append(out, line) }
	err := runDB(ioctx, append([]string{"status"}, args...))
	return strings.Join(out, "\n"), err
}

func TestDBStatusNodeCountsTheInventory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		empty bool
		want  string
	}{
		{name: "one database", want: "1 database"},
		{name: "no database", empty: true, want: "no database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &dbFakeServer{overrideEmptyInventory: tc.empty}
			ioctx, _, workspaceRoot := newDBTestIO(t, fake)
			node := DBStatusNode(map[string]any{}, workspaceRoot, ioctx.Env, ioctx)
			if node.State != clicore.StatusOK || node.Detail != tc.want || node.Fix != "" || len(node.Metrics) != 2 {
				t.Fatalf("node = %+v, want ok %q", node, tc.want)
			}
		})
	}
}

func TestDBStatusNodeIsUnknownWithoutAWorkspace(t *testing.T) {
	ioctx, _, _ := newDBTestIO(t, &dbFakeServer{})
	ioctx.Env = maps.Clone(ioctx.Env)
	delete(ioctx.Env, "PUTNAMI_WORKSPACE_ROOT")
	node := DBStatusNode(map[string]any{}, t.TempDir(), ioctx.Env, ioctx)
	if node.State != clicore.StatusUnknown || node.Detail == "" {
		t.Fatalf("node = %+v, want unknown with the reason", node)
	}
}

func TestDBStatusComparesDatabasesWithDeclaredBindings(t *testing.T) {
	fake := &dbFakeServer{activeGrant: true, bindings: map[string][]map[string]any{
		"svc/api prod":    {readyBinding()},
		"svc/worker prod": {{"logical_name": "cache", "ready": false}},
	}}
	out, err := runDBStatus(t, fake, func(root string) {
		writeCIEnvs(t, root, []string{"svc/api", "svc/worker"}, nil)
	})
	if err != nil {
		t.Fatalf("degraded status failed without --strict: %v", err)
	}
	want := []string{
		"db  degraded  1 database, 1 of 2 bindings ready",
		"",
		"  METRIC               VALUE         WINDOW",
		"  databases            1             now",
		"  schemas              2             now",
		"  bindings ready       1 of 2 (50%)  now",
		"  databases not bound  0             now",
		"",
		"  STATE     CHECK  DETAIL",
		"  ok        mydb   postgres, 2 schemas, bound by svc/api, your read grant active",
		"  degraded  cache  declared by svc/worker in prod, no database yet",
		"                   fix: putnami cloud env doctor prod",
	}
	if out != strings.Join(want, "\n") {
		t.Fatalf("output =\n%s\nwant\n%s", out, strings.Join(want, "\n"))
	}
	if _, err := runDBStatus(t, fake, func(root string) {
		writeCIEnvs(t, root, []string{"svc/api", "svc/worker"}, nil)
	}, "--strict"); clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("--strict on a degraded status = %v, want exit 1", err)
	}
}

func TestDBStatusReadsALedgerStillKeyedByName(t *testing.T) {
	fake := &dbFakeServer{bindings: map[string][]map[string]any{
		"api prod": {readyBinding()},
	}}
	out, err := runDBStatus(t, fake, func(root string) {
		writeCIEnvs(t, root, []string{"svc/api"}, map[string]string{"svc/api": "api"})
	})
	if err != nil || !strings.Contains(out, "  ok     mydb   postgres, 2 schemas, bound by svc/api") {
		t.Fatalf("err = %v, output =\n%s", err, out)
	}
}

func TestDBStatusFlagsADatabaseNoDeclaredWorkloadBinds(t *testing.T) {
	fake := &dbFakeServer{}
	out, err := runDBStatus(t, fake, func(root string) { writeCIEnvs(t, root, []string{"svc/worker"}, nil) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"db  degraded  1 database, 0 bindings ready, 1 not bound by a selected workload",
		"  degraded  mydb   postgres, 2 schemas, no workload putnami.ci.json selects binds it",
		"databases not bound  1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "fix:") {
		t.Errorf("a database no workload binds names no fix, since no command clears it:\n%s", out)
	}
	// A read that failed keeps the database unjudged and says what was not read.
	fake.bindingStatus = map[string]int{"svc/worker prod": http.StatusServiceUnavailable}
	out, _ = runDBStatus(t, fake, func(root string) { writeCIEnvs(t, root, []string{"svc/worker"}, nil) })
	if !strings.Contains(out, "  ok       mydb        postgres, 2 schemas\n") || !strings.Contains(out, "  unknown  svc/worker  request failed for") {
		t.Fatalf("output =\n%s", out)
	}
}

func TestDBStatusReportsAnUnreadableCIFile(t *testing.T) {
	out, err := runDBStatus(t, &dbFakeServer{}, func(root string) {
		if err := os.WriteFile(filepath.Join(root, "putnami.ci.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil || !strings.Contains(out, "  unknown  bindings  putnami.ci.json does not parse") {
		t.Fatalf("err = %v, output =\n%s", err, out)
	}
}

func TestDBStatusNamedDatabaseWithAGrantReadsItsMetrics(t *testing.T) {
	fake := &dbFakeServer{activeGrant: true, bindings: map[string][]map[string]any{"svc/api prod": {readyBinding()}}}
	out, err := runDBStatus(t, fake, func(root string) { writeCIEnvs(t, root, []string{"svc/api"}, nil) }, testDatabase)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"db mydb  ok  postgres, 2 schemas, bound by svc/api, your read grant active",
		"",
		"  METRIC       VALUE   WINDOW",
		"  size         12 MiB  now",
		"  tables       7       now",
		"  connections  3       now",
		"",
		"  STATE  CHECK    DETAIL",
		"  ok     schemas  2 schemas: public, app",
		"  ok     svc/api  binds it as main in prod",
		"  ok     grant    your read grant is active until 2026-05-19T10:00:00Z",
	}
	if out != strings.Join(want, "\n") {
		t.Fatalf("output =\n%s\nwant\n%s", out, strings.Join(want, "\n"))
	}
}

func TestDBStatusNamedDatabaseWithoutAGrantNamesTheGrantRequest(t *testing.T) {
	fake := &dbFakeServer{}
	out, err := runDBStatus(t, fake, nil, testDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "  ok     metrics  size needs an active grant: putnami cloud db grant request --database mydb --level read --reason <why>") {
		t.Fatalf("output =\n%s", out)
	}
	out, err = runDBStatus(t, fake, nil, testDatabase, "--json")
	var envelope struct {
		Data clicore.StatusNode `json:"data"`
	}
	if err != nil || json.Unmarshal([]byte(out), &envelope) != nil || envelope.Data.ID != "db.mydb" || envelope.Data.Children[len(envelope.Data.Children)-1].ID != "db.mydb.metrics" {
		t.Fatalf("err = %v, json =\n%s", err, out)
	}
	for _, request := range fake.requests {
		if strings.HasSuffix(request.Path, "/query") || strings.HasSuffix(request.Path, "/grants") {
			t.Fatalf("status without a grant called %s %s", request.Method, request.Path)
		}
	}
	if _, err := runDBStatus(t, fake, nil, "nope"); clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("unknown database = %v, want a usage error", err)
	}
	if _, err := runDBStatus(t, fake, nil, "a", "b"); clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("two databases = %v, want a usage error", err)
	}
}

func TestDBStatusNamedDatabaseKeepsTheNodeWhenTheQueryFails(t *testing.T) {
	fake := &dbFakeServer{activeGrant: true, queryStatus: http.StatusInternalServerError}
	out, err := runDBStatus(t, fake, nil, testDatabase)
	if err != nil || !strings.Contains(out, "db mydb  unknown") || !strings.Contains(out, "  unknown  metrics  request failed for") {
		t.Fatalf("err = %v, output =\n%s", err, out)
	}
	// A grant the query route no longer honors reads as no grant.
	fake.queryStatus = http.StatusForbidden
	out, err = runDBStatus(t, fake, nil, testDatabase)
	if err != nil || !strings.Contains(out, "size needs an active grant") {
		t.Fatalf("err = %v, output =\n%s", err, out)
	}
}

func TestDBStatusLeavesWorkloadsPastTheBudgetUnread(t *testing.T) {
	paths := make([]string, 0, dbStatusCallBudget+2)
	names := map[string]string{}
	for index := range dbStatusCallBudget + 2 {
		path := fmt.Sprintf("svc/w%02d", index)
		paths = append(paths, path)
		names[path] = fmt.Sprintf("w%02d", index)
	}
	for _, tc := range []struct {
		name     string
		bindings map[string][]map[string]any
		want     string
	}{
		// Every database is bound after the path reads, so no name read runs.
		{"bound", map[string][]map[string]any{"svc/w00 prod": {readyBinding()}}, "2 workloads past"},
		// mydb is unbound, so each workload with a name needs a name read, and
		// the budget has none left.
		{"unbound", nil, fmt.Sprintf("%d workloads past", dbStatusCallBudget+2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &dbFakeServer{bindings: tc.bindings}
			out, err := runDBStatus(t, fake, func(root string) { writeCIEnvs(t, root, paths, names) })
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "  unknown  not read  "+tc.want+fmt.Sprintf(" the %d-call budget of a status", dbStatusCallBudget)) {
				t.Fatalf("output =\n%s", out)
			}
			calls := 0
			for _, request := range fake.requests {
				if strings.HasSuffix(request.Path, "/database-bindings") {
					calls++
				}
			}
			if calls != dbStatusCallBudget {
				t.Fatalf("binding calls = %d, want %d", calls, dbStatusCallBudget)
			}
		})
	}
}

func TestDBStatusFromFoldsEachState(t *testing.T) {
	databases := []DBDatabase{{Name: "a", Engine: "postgres"}, {Name: "b", Engine: "postgres", Schemas: []string{"public"}}}
	node := DBStatusFrom(DBStatusInput{Databases: databases, Declared: true, Reads: []DBBindingRead{
		{Environment: "prod", Workload: "x", Bindings: []DBBinding{{Environment: "prod", Workload: "x", Logical: "main", Database: "a", Ready: true}}},
		{Environment: "prod", Workload: "y", Bindings: []DBBinding{{Environment: "prod", Workload: "y", Logical: "main", Database: "a", Ready: false}}},
		{Environment: "prod", Workload: "z", Bindings: []DBBinding{{Environment: "prod", Workload: "z", Logical: "main", Database: "b", Ready: true}}},
	}})
	if node.State != clicore.StatusDegraded || node.Detail != "2 databases, 2 of 3 bindings ready" {
		t.Fatalf("node = %+v", node)
	}
	if a := node.Children[0]; a.Detail != "postgres, 0 schemas, bound by 2 workloads, 1 of 2 bindings not ready" || a.Fix != "putnami cloud env doctor prod" {
		t.Fatalf("a = %+v", a)
	}
	unread := DBStatusFrom(DBStatusInput{Declared: true, Unread: []string{"prod svc/x"}})
	if unread.State != clicore.StatusUnknown || unread.Children[0].ID != "db.unread" {
		t.Fatalf("unread = %+v", unread)
	}
	failed := DBDatabaseStatusFrom(DBStatusInput{Databases: databases}, "a", nil, errors.New("catalog query timed out"))
	if failed.State != clicore.StatusUnknown || failed.Children[len(failed.Children)-1].Detail != "catalog query timed out" {
		t.Fatalf("failed = %+v", failed)
	}
}
