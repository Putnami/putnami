package datacli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	dataapiclient "go.putnami.dev/cloud/clients/data-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// `putnami cloud db status [<db>]` compares the databases the
// workspace has with the databases its declared workloads bind. The databases
// come from the Data inventory; the bindings come from the datasource ledger,
// read once per workload each environment of putnami.ci.json selects. It only
// reads, and it never opens a grant: the size of a database shows only when
// the caller already holds one.

const (
	// dbStatusParallelism bounds the binding reads one status has in flight.
	dbStatusParallelism = 8
	// dbStatusCallBudget bounds the binding reads of one status, so
	// `putnami cloud status` answers within its budget. The workloads past it
	// become one unknown check.
	dbStatusCallBudget = 240
)

// DBDatabase is one database of the workspace inventory. GrantLevel is the
// level of the caller's active grant on it, empty without one.
type DBDatabase struct {
	Name       string
	Engine     string
	Schemas    []string
	GrantLevel string
	GrantUntil *time.Time
}

// DBBinding is one datasource a workload declares in one environment, and the
// database the ledger resolved it to. Database is empty until it is ready.
type DBBinding struct {
	Environment string
	Workload    string
	Logical     string
	Database    string
	Ready       bool
}

// DBBindingRead is what the ledger answered for one workload in one
// environment: its bindings, or why they could not be read.
type DBBindingRead struct {
	Environment string
	Workload    string
	Bindings    []DBBinding
	Err         error
}

// DBStatusInput is what a db status folds. Declared is false when
// putnami.ci.json selects no workload, so no binding was read; DeclareErr is
// why putnami.ci.json could not be read. Unread names the "<env> <workload>"
// pairs left unread because the status reached its call budget.
type DBStatusInput struct {
	Databases  []DBDatabase
	Declared   bool
	DeclareErr error
	Reads      []DBBindingRead
	Unread     []string
}

// DBMetrics is what the read-only catalog query of a database answers.
type DBMetrics struct {
	SizeBytes   int64
	Tables      int
	Connections int
}

// DBStatusNode is the db line of `putnami cloud status`: every database, the
// workloads that bind it, and the bindings that are not ready.
func DBStatusNode(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) clicore.StatusNode {
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return clicore.UnknownStatus("db", "db", err, "")
	}
	input, err := readDBStatus(ctx, workspaceRoot)
	if err != nil {
		return clicore.UnknownStatus("db", "db", err, "")
	}
	return DBStatusFrom(input)
}

// dbStatus serves `putnami cloud db status [<db>]`.
func dbStatus(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	names := clicore.Positionals(args)
	if len(names) > 0 && names[0] == "status" {
		names = names[1:]
	}
	if len(names) > 1 {
		return clicore.NewError("cloud db status takes at most one database", clicore.ExitUsage)
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	input, err := readDBStatus(ctx, workspaceRoot)
	if clicore.ExitCode(err) == clicore.ExitAuth {
		return err
	}
	if len(names) == 0 {
		if err != nil {
			return clicore.WriteStatus(params, ioctx, clicore.UnknownStatus("db", "db", err, ""))
		}
		return clicore.WriteStatus(params, ioctx, DBStatusFrom(input))
	}
	name := names[0]
	if err != nil {
		return clicore.WriteStatus(params, ioctx, clicore.UnknownStatus("db."+name, "db "+name, err, ""))
	}
	database, found := findDBDatabase(input.Databases, name)
	if !found {
		return clicore.NewError(fmt.Sprintf("database %q is not in this workspace's inventory; run 'putnami cloud db status' to see the databases", name), clicore.ExitUsage)
	}
	var metrics *DBMetrics
	var metricsErr error
	if database.GrantLevel != "" {
		metrics, metricsErr = readDBMetrics(ctx, name)
	}
	return clicore.WriteStatus(params, ioctx, DBDatabaseStatusFrom(input, name, metrics, metricsErr))
}

// readDBStatus reads the inventory, then the bindings of every workload
// putnami.ci.json selects. Only an inventory failure is an error; a binding
// read that fails stays in its DBBindingRead.
func readDBStatus(ctx *clicore.WorkspaceContext, workspaceRoot string) (DBStatusInput, error) {
	inventory, err := fetchInventory(ctx)
	if err != nil {
		return DBStatusInput{}, err
	}
	input := DBStatusInput{Databases: dbDatabasesFrom(inventory.Databases)}
	workloads, err := declaredDBWorkloads(workspaceRoot)
	if err != nil {
		input.DeclareErr = err
		return input, nil
	}
	if len(workloads) == 0 {
		return input, nil
	}
	input.Declared = true
	input.Reads, input.Unread = readDBBindings(ctx, workloads, input.Databases)
	return input, nil
}

// dbWorkload is one workload one environment of putnami.ci.json selects. Name
// is its putnami.json name when it differs from its path: a ledger row not yet
// moved to the workload path is still keyed by that name.
type dbWorkload struct {
	Environment string
	Path        string
	Name        string
}

// declaredDBWorkloads lists the exact workload paths each environment of
// putnami.ci.json selects, in environment then path order. No file, or no
// envs, is no workload.
func declaredDBWorkloads(workspaceRoot string) ([]dbWorkload, error) {
	data, err := os.ReadFile(filepath.Join(workspaceRoot, "putnami.ci.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("putnami.ci.json could not be read: %w", err)
	}
	var document struct {
		Envs map[string]struct {
			Workloads []struct {
				Select json.RawMessage `json:"select"`
			} `json:"workloads"`
		} `json:"envs"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("putnami.ci.json does not parse: %w", err)
	}
	var out []dbWorkload
	for environment, declared := range document.Envs {
		seen := map[string]bool{}
		for _, rule := range declared.Workloads {
			for _, path := range dbSelectors(rule.Select) {
				if path = strings.Trim(strings.TrimSpace(path), "/"); path == "" || seen[path] || strings.ContainsAny(path, "*?[") {
					continue
				}
				seen[path] = true
				workload := dbWorkload{Environment: environment, Path: path}
				if name, nameErr := clicore.ProjectNameAt(filepath.Join(workspaceRoot, filepath.FromSlash(path))); nameErr == nil && name != "" && name != path {
					workload.Name = name
				}
				out = append(out, workload)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Environment != out[j].Environment {
			return out[i].Environment < out[j].Environment
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// dbSelectors reads a select value: one selector or a list of them.
func dbSelectors(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(raw, &many)
	return many
}

// readDBBindings reads each workload's bindings by its path, then, for a
// workload with none, by its name, within dbStatusCallBudget calls. The name
// reads run only while a database of the inventory is still unbound: a ledger
// row keyed by name is an older, resolved one. It answers the reads in
// workload order and the "<env> <path>" pairs left unread.
func readDBBindings(ctx *clicore.WorkspaceContext, workloads []dbWorkload, databases []DBDatabase) ([]DBBindingRead, []string) {
	ctx = dbParallelContext(ctx)
	read := workloads
	var unread []string
	if len(read) > dbStatusCallBudget {
		read = workloads[:dbStatusCallBudget]
		for _, workload := range workloads[dbStatusCallBudget:] {
			unread = append(unread, workload.Environment+" "+workload.Path)
		}
	}
	reads := make([]DBBindingRead, len(read))
	runDBBounded(len(read), func(index int) {
		reads[index] = readDBBinding(ctx, read[index].Environment, read[index].Path)
	})
	if everyDatabaseBound(databases, reads) {
		return reads, unread
	}
	budget := dbStatusCallBudget - len(read)
	var fallback []int
	for index, workload := range read {
		if workload.Name == "" || reads[index].Err != nil || len(reads[index].Bindings) > 0 {
			continue
		}
		if budget == 0 {
			unread = append(unread, workload.Environment+" "+workload.Path)
			reads[index].Err = errDBUnread
			continue
		}
		budget--
		fallback = append(fallback, index)
	}
	runDBBounded(len(fallback), func(at int) {
		index := fallback[at]
		byName := readDBBinding(ctx, read[index].Environment, read[index].Name)
		byName.Workload = read[index].Path
		for i := range byName.Bindings {
			byName.Bindings[i].Workload = read[index].Path
		}
		reads[index] = byName
	})
	kept := reads[:0]
	for _, entry := range reads {
		if !errors.Is(entry.Err, errDBUnread) {
			kept = append(kept, entry)
		}
	}
	return kept, unread
}

// dbParallelContext copies ctx for the parallel binding reads, with the 401
// re-mint disabled: CallWithSession then never writes the bearer another read
// is using. The token was minted when the command started.
func dbParallelContext(ctx *clicore.WorkspaceContext) *clicore.WorkspaceContext {
	copied := *ctx
	copied.RefreshAuth = nil
	return &copied
}

// everyDatabaseBound reports whether each database of the inventory has a
// binding among reads.
func everyDatabaseBound(databases []DBDatabase, reads []DBBindingRead) bool {
	bound := map[string]bool{}
	for _, read := range reads {
		for _, binding := range read.Bindings {
			bound[binding.Database] = true
		}
	}
	for _, database := range databases {
		if !bound[database.Name] {
			return false
		}
	}
	return true
}

// errDBUnread marks a workload the call budget left unread; it is reported in
// DBStatusInput.Unread, not as a failed read.
var errDBUnread = errors.New("past the call budget")

// readDBBinding asks the ledger which databases one project binds in one
// environment.
func readDBBinding(ctx *clicore.WorkspaceContext, environment, project string) DBBindingRead {
	read := DBBindingRead{Environment: environment, Workload: project}
	api, err := dataClient(ctx)
	if err != nil {
		read.Err = err
		return read
	}
	target := ctx.WorkspaceURL("/database-bindings")
	answer, err := clicore.CallWithSession(invocationContext(ctx), ctx, func(callCtx context.Context) (*dataapiclient.ProjectDatabaseBindingList, error) {
		return api.GetV1WorkspacesDatabaseBindings(callCtx, dataapiclient.GetV1WorkspacesDatabaseBindingsInput{
			Path:  dataapiclient.GetV1WorkspacesDatabaseBindingsPath{Workspace: ctx.WorkspaceID},
			Query: dataapiclient.GetV1WorkspacesDatabaseBindingsQuery{Environment: environment, Project: project},
		})
	})
	if err != nil {
		read.Err = dataCallError(target, "invalid database binding response from "+target, err)
		return read
	}
	for _, binding := range clicore.Deref(clicore.Deref(answer).Bindings) {
		read.Bindings = append(read.Bindings, DBBinding{
			Environment: environment,
			Workload:    project,
			Logical:     clicore.Deref(binding.LogicalName),
			Database:    clicore.Deref(binding.DatabaseName),
			Ready:       clicore.Deref(binding.Ready),
		})
	}
	return read
}

// readDBMetrics runs the catalog query of `db info` as the caller's grant. A
// grant the query route no longer honors answers no metrics and no error.
func readDBMetrics(ctx *clicore.WorkspaceContext, database string) (*DBMetrics, error) {
	metrics, err := fetchDatabaseMetrics(ctx, database)
	if errors.Is(err, errGrantNotInForce) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &DBMetrics{SizeBytes: metrics.SizeBytes, Tables: metrics.Tables, Connections: metrics.Connections}, nil
}

// runDBBounded calls read once per index, with at most dbStatusParallelism
// calls in flight.
func runDBBounded(count int, read func(index int)) {
	slots := make(chan struct{}, dbStatusParallelism)
	var wg sync.WaitGroup
	for index := range count {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			read(index)
		}()
	}
	wg.Wait()
}

func dbDatabasesFrom(items []databaseInventoryItem) []DBDatabase {
	out := make([]DBDatabase, 0, len(items))
	for _, item := range items {
		database := DBDatabase{Name: item.Database, Engine: item.Engine, Schemas: item.Schemas}
		if grant := item.ActiveGrant; grant != nil {
			database.GrantLevel, database.GrantUntil = grant.Level, grant.ExpiresAt
		}
		out = append(out, database)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func findDBDatabase(databases []DBDatabase, name string) (DBDatabase, bool) {
	for _, database := range databases {
		if database.Name == name {
			return database, true
		}
	}
	return DBDatabase{}, false
}

// dbBindingTally groups the bindings the reads answered.
type dbBindingTally struct {
	ready, total int
	// byDatabase lists the bindings resolved to each database.
	byDatabase map[string][]DBBinding
	// pending lists the bindings with no database yet.
	pending []DBBinding
	failed  []DBBindingRead
}

func tallyDBBindings(reads []DBBindingRead) dbBindingTally {
	tally := dbBindingTally{byDatabase: map[string][]DBBinding{}}
	for _, read := range reads {
		if read.Err != nil {
			tally.failed = append(tally.failed, read)
			continue
		}
		for _, binding := range read.Bindings {
			tally.total++
			if binding.Ready {
				tally.ready++
			}
			if binding.Database == "" {
				tally.pending = append(tally.pending, binding)
				continue
			}
			tally.byDatabase[binding.Database] = append(tally.byDatabase[binding.Database], binding)
		}
	}
	return tally
}

// complete reports whether every declared workload was read, so a database no
// binding names is known to be bound by no declared workload.
func (in DBStatusInput) complete(tally dbBindingTally) bool {
	return in.Declared && in.DeclareErr == nil && len(in.Unread) == 0 && len(tally.failed) == 0
}

// DBStatusFrom folds the inventory and the binding reads into the db status of
// the whole workspace: one check per database, one per binding that has no
// database yet, and one per read that failed. The state is the worst of the
// checks. A binding that is not ready degrades the status rather than fail it:
// the ledger says the database is not attached yet, not that the workload is
// down.
func DBStatusFrom(in DBStatusInput) clicore.StatusNode {
	node := clicore.StatusNode{ID: "db", Title: "db"}
	tally := tallyDBBindings(in.Reads)
	schemas, unbound := 0, 0
	for _, database := range in.Databases {
		schemas += len(database.Schemas)
		if len(tally.byDatabase[database.Name]) == 0 && in.complete(tally) {
			unbound++
		}
		node.Children = append(node.Children, dbDatabaseCheck("db."+database.Name, database.Name, database, in, tally))
	}
	for _, binding := range tally.pending {
		node.Children = append(node.Children, dbPendingCheck("db", binding))
	}
	node.Children = append(node.Children, dbReadChecks("db", in, tally)...)
	node.Metrics = []clicore.StatusMetric{
		clicore.CountMetric("databases", "databases", len(in.Databases), clicore.MetricUsage),
		clicore.CountMetric("schemas", "schemas", schemas, clicore.MetricUsage),
	}
	if in.Declared {
		node.Metrics = append(node.Metrics,
			clicore.CountMetric("bindings_ready", "bindings ready", tally.ready, clicore.MetricHealth).Of(float64(tally.total)),
			clicore.CountMetric("databases_unbound", "databases not bound", unbound, clicore.MetricHealth))
	}
	node.State = clicore.WorstStatus(node.ChildStates()...)
	node.Detail = countDB(len(in.Databases), "database", "databases")
	switch {
	case len(in.Databases) == 0 && tally.total == 0:
		node.Detail = "no database"
	case in.Declared && tally.ready < tally.total:
		node.Detail += fmt.Sprintf(", %d of %d bindings ready", tally.ready, tally.total)
	case in.Declared:
		node.Detail += ", " + countDB(tally.total, "binding", "bindings") + " ready"
	}
	if unbound > 0 {
		node.Detail += fmt.Sprintf(", %d not bound by a selected workload", unbound)
	}
	return node
}

// DBDatabaseStatusFrom folds one database into the status
// `putnami cloud db status <db>` prints: its schemas, the workloads that bind
// it, the caller's grant, and, with that grant, its size, tables and
// connections. metricsErr is why the catalog query failed.
func DBDatabaseStatusFrom(in DBStatusInput, name string, metrics *DBMetrics, metricsErr error) clicore.StatusNode {
	database, _ := findDBDatabase(in.Databases, name)
	tally := tallyDBBindings(in.Reads)
	id := "db." + name
	summary := dbDatabaseCheck(id, "db "+name, database, in, tally)
	node := clicore.StatusNode{ID: id, Title: "db " + name, Detail: summary.Detail}
	schemas := clicore.StatusNode{ID: id + ".schemas", Title: "schemas", State: clicore.StatusOK, Detail: "no schema"}
	if len(database.Schemas) > 0 {
		schemas.Detail = countDB(len(database.Schemas), "schema", "schemas") + ": " + strings.Join(database.Schemas, ", ")
	}
	node.Children = append(node.Children, schemas)
	for _, binding := range tally.byDatabase[name] {
		check := clicore.StatusNode{
			ID: id + "." + binding.Environment + "." + binding.Workload, Title: binding.Workload, State: clicore.StatusOK,
			Detail: fmt.Sprintf("binds it as %s in %s", binding.Logical, binding.Environment),
		}
		if !binding.Ready {
			check.State, check.Detail = clicore.StatusDegraded, check.Detail+", not ready"
			check.Fix = dbEnvFix(binding.Environment)
		}
		node.Children = append(node.Children, check)
	}
	if len(tally.byDatabase[name]) == 0 && in.complete(tally) {
		node.Children = append(node.Children, clicore.StatusNode{
			ID: id + ".bindings", Title: "bindings", State: clicore.StatusDegraded,
			Detail: "no workload putnami.ci.json selects binds it",
		})
	}
	node.Children = append(node.Children, dbReadChecks(id, in, tally)...)
	grant := clicore.StatusNode{ID: id + ".grant", Title: "grant", State: clicore.StatusOK, Detail: "you hold no active grant"}
	if database.GrantLevel != "" {
		grant.Detail = "your " + database.GrantLevel + " grant is active"
		if database.GrantUntil != nil {
			grant.Detail += " until " + database.GrantUntil.UTC().Format(time.RFC3339)
		}
	}
	node.Children = append(node.Children, grant)
	switch {
	case metricsErr != nil:
		node.Children = append(node.Children, clicore.UnknownStatus(id+".metrics", "metrics", metricsErr, ""))
	case metrics != nil:
		node.Metrics = []clicore.StatusMetric{
			{ID: "size_bytes", Title: "size", Value: float64(metrics.SizeBytes), Unit: clicore.UnitBytes, Kind: clicore.MetricUsage},
			clicore.CountMetric("tables", "tables", metrics.Tables, clicore.MetricUsage),
			clicore.CountMetric("connections", "connections", metrics.Connections, clicore.MetricHealth),
		}
	default:
		node.Children = append(node.Children, clicore.StatusNode{
			ID: id + ".metrics", Title: "metrics", State: clicore.StatusOK,
			Detail: "size needs an active grant: putnami cloud db grant request --database " + name + " --level read --reason <why>",
		})
	}
	node.State = clicore.WorstStatus(node.ChildStates()...)
	return node
}

// dbEnvFix names the check that says why a binding is not ready: env doctor
// prints the fix for each continuous-delivery prerequisite of the environment.
func dbEnvFix(environment string) string {
	return "putnami cloud env doctor " + environment
}

// dbDatabaseCheck is the check of one database: its engine and schemas, the
// workloads that bind it, and the caller's grant.
func dbDatabaseCheck(id, title string, database DBDatabase, in DBStatusInput, tally dbBindingTally) clicore.StatusNode {
	check := clicore.StatusNode{ID: id, Title: title, State: clicore.StatusOK}
	parts := []string{clicore.FirstString(database.Engine, "unknown engine"), countDB(len(database.Schemas), "schema", "schemas")}
	bindings := tally.byDatabase[database.Name]
	workloads := map[string]bool{}
	notReady := 0
	var environment string
	for _, binding := range bindings {
		workloads[binding.Workload] = true
		if !binding.Ready {
			notReady++
			environment = binding.Environment
		}
	}
	switch {
	case len(workloads) == 1:
		parts = append(parts, "bound by "+bindings[0].Workload)
	case len(workloads) > 1:
		parts = append(parts, fmt.Sprintf("bound by %d workloads", len(workloads)))
	case in.complete(tally):
		// No command clears it: the workload that uses it is missing from
		// putnami.ci.json, or the database outlived its workload.
		parts = append(parts, "no workload putnami.ci.json selects binds it")
		check.State = clicore.StatusDegraded
	}
	if notReady > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d bindings not ready", notReady, len(bindings)))
		check.State, check.Fix = clicore.StatusDegraded, dbEnvFix(environment)
	}
	if database.GrantLevel != "" {
		parts = append(parts, "your "+database.GrantLevel+" grant active")
	}
	check.Detail = strings.Join(parts, ", ")
	return check
}

// dbPendingCheck is the check of a binding the ledger has not resolved to a
// database yet.
func dbPendingCheck(parentID string, binding DBBinding) clicore.StatusNode {
	return clicore.StatusNode{
		ID:     parentID + "." + binding.Environment + "." + binding.Workload + "." + binding.Logical,
		Title:  binding.Logical,
		State:  clicore.StatusDegraded,
		Detail: fmt.Sprintf("declared by %s in %s, no database yet", binding.Workload, binding.Environment),
		Fix:    dbEnvFix(binding.Environment),
	}
}

// dbReadChecks are the unknown checks of what could not be read:
// putnami.ci.json, a workload's bindings, the workloads past the budget.
func dbReadChecks(parentID string, in DBStatusInput, tally dbBindingTally) []clicore.StatusNode {
	var out []clicore.StatusNode
	if in.DeclareErr != nil {
		out = append(out, clicore.UnknownStatus(parentID+".bindings", "bindings", in.DeclareErr, ""))
	}
	for _, read := range tally.failed {
		out = append(out, clicore.UnknownStatus(parentID+"."+read.Environment+"."+read.Workload, read.Workload, read.Err, ""))
	}
	if len(in.Unread) > 0 {
		out = append(out, clicore.StatusNode{
			ID: parentID + ".unread", Title: "not read", State: clicore.StatusUnknown,
			Detail: fmt.Sprintf("%s past the %d-call budget of a status", countDB(len(in.Unread), "workload", "workloads"), dbStatusCallBudget),
		})
	}
	return out
}

// invocationContext is the context a db call derives from: the invocation's,
// so a caller such as `putnami cloud status` can cancel it.
func invocationContext(ctx *clicore.WorkspaceContext) context.Context {
	if ctx.IO.Context != nil {
		return ctx.IO.Context
	}
	return context.Background()
}

func countDB(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
