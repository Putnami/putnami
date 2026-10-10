package datacli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	dataapiclient "go.putnami.dev/cloud/clients/data-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// errGrantNotInForce marks a metrics query the query route rejected (403) because
// the caller's grant is no longer in force. The inventory annotates a database
// with the caller's grant while its status is "active" WITHOUT re-checking expiry,
// so an expired-but-not-yet-swept grant still shows as active there; the query
// route re-checks expiry (requireActiveGrant → Grant.InForce) and 403s it. `db
// info` treats that rejection as "no active grant" and degrades to the facts +
// hint rather than surfacing the 403.
var errGrantNotInForce = errors.New("database grant is no longer in force")

// databaseMetricsStatement is the single read-only catalog SELECT `db info` runs
// to compute the size/table/connection rollup. It is ONE statement (subqueries,
// no `;`) so it passes the Lane-0 single-statement, read-only query validator,
// and it runs as the caller's own u_<slug> role — one route call, one audit
// record, no new API surface. Aliases are read back by column name.
const databaseMetricsStatement = `SELECT
  pg_size_pretty(pg_database_size(current_database())) AS size,
  pg_database_size(current_database()) AS size_bytes,
  (SELECT count(*) FROM pg_stat_user_tables) AS tables,
  (SELECT coalesce(sum(n_live_tup), 0)::bigint FROM pg_stat_user_tables) AS live_rows,
  (SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()) AS connections`

// databaseMetrics is the secret-free rollup `db info` prints when the caller holds
// an active grant: on-disk size, the user-table and estimated-row counts, and the
// current connection count. Derived entirely from the read-only catalog query.
type databaseMetrics struct {
	Size string `json:"size"`
	// SizeBytes is the size `db status <db>` reports as a metric; `db info`
	// keeps printing the rounded Size, so it stays out of that JSON.
	SizeBytes   int64 `json:"-"`
	Tables      int   `json:"tables"`
	LiveRows    int   `json:"live_rows"`
	Connections int   `json:"connections"`
}

// databaseInfoPayload is the combined, secret-free object `db info --json` emits:
// the inventory facts plus, when present, the grant-gated metrics. It reuses only
// secret-free shapes (databaseInventoryItem, grantView), so it never echoes a
// credential.
type databaseInfoPayload struct {
	Database               string           `json:"database"`
	Engine                 string           `json:"engine,omitempty"`
	InstanceConnectionName string           `json:"instance_connection_name,omitempty"`
	Schemas                []string         `json:"schemas,omitempty"`
	ActiveGrant            *grantView       `json:"active_grant,omitempty"`
	Metrics                *databaseMetrics `json:"metrics,omitempty"`
}

// dbInfo prints high-level facts about one database and, when the caller holds an
// active grant on it, a basic size/table/connection rollup. It degrades cleanly:
// the facts come from the grant-free inventory; the metrics come from a read-only
// catalog query on the Lane-0 route, which requires an active grant. With no
// grant it prints the facts plus a one-line hint instead of failing.
func dbInfo(params map[string]any, rest []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	database := infoDatabase(params, rest)
	if database == "" {
		return clicore.NewError("cloud db info requires a database name (e.g. putnami cloud db info mydb)", clicore.ExitUsage)
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	inv, err := fetchInventory(ctx)
	if err != nil {
		return err
	}
	item, found := findDatabase(inv.Databases, database)
	if !found {
		return clicore.NewError(fmt.Sprintf("database %q is not in this workspace's inventory; run 'cloud db list' to see the databases you can inspect", database), clicore.ExitUsage)
	}
	// Metrics require the Lane-0 query route's active-grant gate, so only attempt
	// them when the inventory already shows the caller holds one — otherwise the
	// facts-plus-hint path below stands in. The inventory can still report a grant
	// as active briefly after its TTL lapses (it does not re-check expiry), so a
	// query rejected as no-longer-in-force degrades to the same facts + hint rather
	// than failing.
	var metrics *databaseMetrics
	if item.ActiveGrant != nil {
		m, merr := fetchDatabaseMetrics(ctx, database)
		switch {
		case merr == nil:
			metrics = m
		case errors.Is(merr, errGrantNotInForce):
			metrics = nil
		default:
			return merr
		}
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(databaseInfoView(item, metrics), params, ioctx, "")
		return nil
	}
	renderDatabaseInfo(item, metrics, ioctx)
	return nil
}

// infoDatabase reads the target database from the first positional arg, falling
// back to the --database flag so `db info mydb` and `db info --database mydb`
// both work.
func infoDatabase(params map[string]any, rest []string) string {
	if len(rest) > 0 {
		if name := strings.TrimSpace(rest[0]); name != "" {
			return name
		}
	}
	return strings.TrimSpace(clicore.StringParam(params, "database", "db"))
}

// findDatabase selects the inventory item whose database name matches name.
func findDatabase(items []databaseInventoryItem, name string) (databaseInventoryItem, bool) {
	for _, it := range items {
		if it.Database == name {
			return it, true
		}
	}
	return databaseInventoryItem{}, false
}

// fetchDatabaseMetrics runs the single read-only rollup statement through the
// Lane-0 query route and projects the (secret-free) QueryResult into metrics,
// reading each value back by its column alias.
func fetchDatabaseMetrics(ctx *clicore.WorkspaceContext, database string) (*databaseMetrics, error) {
	result, err := runReadQuery(ctx, database, databaseMetricsStatement)
	if err != nil {
		return nil, err
	}
	row := firstRow(result)
	return &databaseMetrics{
		Size:        cellByName(result, row, "size"),
		SizeBytes:   int64(atoiCell(cellByName(result, row, "size_bytes"))),
		Tables:      atoiCell(cellByName(result, row, "tables")),
		LiveRows:    atoiCell(cellByName(result, row, "live_rows")),
		Connections: atoiCell(cellByName(result, row, "connections")),
	}, nil
}

// runReadQuery POSTs one read-only statement to the Lane-0 query route and decodes
// the secret-free QueryResult. The route is gated on an active grant, so callers
// check the inventory's active_grant before invoking it.
func runReadQuery(ctx *clicore.WorkspaceContext, database, statement string) (dataapiclient.QueryResult, error) {
	api, err := dataClient(ctx)
	if err != nil {
		return dataapiclient.QueryResult{}, err
	}
	target := ctx.WorkspaceURL("/databases/" + clicore.URLPathEscape(database) + "/query")
	invalid := "invalid query response from " + target
	queryResult, err := clicore.CallWithSession(invocationContext(ctx), ctx, func(callCtx context.Context) (*dataapiclient.QueryResult, error) {
		return api.CreateV1WorkspacesDatabasesQuery(callCtx, dataapiclient.CreateV1WorkspacesDatabasesQueryInput{
			Path: dataapiclient.CreateV1WorkspacesDatabasesQueryPath{Workspace: ctx.WorkspaceID, Database: database},
			Body: dataapiclient.QueryRequest{Statement: &statement},
		})
	})
	// The route's only 403 is its active-grant gate (this call already cleared
	// workspace membership on the inventory GET): surface it as the typed
	// grant-not-in-force signal so `db info` can degrade to facts + hint.
	if clicore.ServiceStatus(err) == http.StatusForbidden {
		return dataapiclient.QueryResult{}, errGrantNotInForce
	}
	if err != nil {
		return dataapiclient.QueryResult{}, dataCallError(target, invalid, err)
	}
	return clicore.Deref(queryResult), nil
}

// firstRow returns the first result row's cells, or nil when the result is empty.
func firstRow(result dataapiclient.QueryResult) []*string {
	rows := clicore.Deref(result.Rows)
	if len(rows) == 0 {
		return nil
	}
	return clicore.Deref(rows[0].Cells)
}

// cellByName returns row's text cell under the column named name, or "" when the
// column is absent or the cell is SQL NULL.
func cellByName(result dataapiclient.QueryResult, row []*string, name string) string {
	for i, c := range clicore.Deref(result.Columns) {
		if clicore.Deref(c.Name) == name && i < len(row) && row[i] != nil {
			return *row[i]
		}
	}
	return ""
}

// atoiCell parses a text cell as an int, returning 0 for an empty or non-numeric
// value so a missing metric renders as 0 rather than erroring.
func atoiCell(s string) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return 0
}

// databaseInfoView assembles the combined --json payload from the inventory item
// and the (optional) metrics.
func databaseInfoView(item databaseInventoryItem, metrics *databaseMetrics) databaseInfoPayload {
	return databaseInfoPayload{
		Database:               item.Database,
		Engine:                 item.Engine,
		InstanceConnectionName: item.InstanceConnectionName,
		Schemas:                item.Schemas,
		ActiveGrant:            item.ActiveGrant,
		Metrics:                metrics,
	}
}

// renderDatabaseInfo prints the human view: the grant-free facts, the caller's
// active grant (if any), and the metrics — or, with no grant, a one-line hint.
func renderDatabaseInfo(item databaseInventoryItem, metrics *databaseMetrics, ioctx clicore.IO) {
	ioctx.Stdout("Database: " + item.Database)
	if item.Engine != "" {
		ioctx.Stdout("  engine:   " + item.Engine)
	}
	if item.InstanceConnectionName != "" {
		ioctx.Stdout("  instance: " + item.InstanceConnectionName)
	}
	if len(item.Schemas) > 0 {
		ioctx.Stdout("  schemas:  " + strings.Join(item.Schemas, ", "))
	} else {
		ioctx.Stdout("  schemas:  -")
	}
	if g := item.ActiveGrant; g != nil {
		expires := "-"
		if g.ExpiresAt != nil {
			expires = g.ExpiresAt.UTC().Format(time.RFC3339)
		}
		ioctx.Stdout(fmt.Sprintf("  grant:    %s/%s (expires %s)", g.Level, g.Status, expires))
	} else {
		ioctx.Stdout("  grant:    none")
	}
	if metrics != nil {
		ioctx.Stdout("")
		ioctx.Stdout("Metrics (read as your active grant role):")
		ioctx.Stdout("  size:        " + metrics.Size)
		ioctx.Stdout(fmt.Sprintf("  tables:      %d", metrics.Tables))
		ioctx.Stdout(fmt.Sprintf("  est. rows:   %d", metrics.LiveRows))
		ioctx.Stdout(fmt.Sprintf("  connections: %d", metrics.Connections))
		return
	}
	ioctx.Stdout("")
	ioctx.Stdout("open a grant with 'cloud db grant request' to see size/table/connection metrics")
}
