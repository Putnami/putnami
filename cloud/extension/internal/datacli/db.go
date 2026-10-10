// Package datacli holds the data-domain command implementations for the
// @putnami/cloud CLI extension: the `cloud db` command family.
//
// `db grant request|list|revoke` drives the ephemeral database-access surface
// the control-plane api exposes — the workspace self-grant
// route group /v1/workspaces/{workspace}/databases/{database}/grants — through
// data-api's generated Go client (ResolveWorkspaceContext + CallWithSession), so
// a member opens, lists, and revokes their own time-boxed grants with a
// workspace-scoped bearer.
//
// `db connect` opens a localhost TCP listener bridged over a WebSocket to the
// db-gateway, so psql and GUI tools connect with zero GCP credentials.
//
// Secret-handling invariant: the one-time login password lives on
// exactly one DTO, GrantCredentials, returned once from the open-grant POST.
// This CLI prints it to stdout EXACTLY ONCE with a "shown once — not stored"
// warning and NEVER writes the credential, DSN, or password to disk (no cache
// file, no config, no temp file). Every read surface (list) uses GrantView,
// which has no secret field by construction.
package datacli

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	dataapiclient "go.putnami.dev/cloud/clients/data-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// DB dispatches the `putnami cloud db <sub>` family. It self-parses the raw
// args the same way the registries/oci families do (the aggregator forwards
// args verbatim), so the subcommand and its positionals are read here.
func DB(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	pos := dbPositionals(args)
	sub := ""
	if len(pos) > 0 {
		sub = pos[0]
	}
	switch sub {
	case "", "help":
		return dbHelp(params, ioctx)
	case "list":
		return dbList(params, workspaceRoot, env, ioctx)
	case "info":
		return dbInfo(params, pos[1:], workspaceRoot, env, ioctx)
	case "grant":
		return dbGrant(params, pos[1:], workspaceRoot, env, ioctx)
	case "connect":
		return dbConnect(params, workspaceRoot, env, ioctx)
	case "status":
		return dbStatus(params, args, workspaceRoot, env, ioctx)
	default:
		return clicore.NewError("unknown db subcommand: "+sub+`; expected "status [<db>]", "list", "info <db>", "grant request|list|revoke", or "connect"`, clicore.ExitUsage)
	}
}

// dbGrant dispatches the `db grant <request|list|revoke>` verbs.
func dbGrant(params map[string]any, rest []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	action := ""
	if len(rest) > 0 {
		action = rest[0]
	}
	switch action {
	case "", "help":
		return dbHelp(params, ioctx)
	case "request":
		return dbGrantRequest(params, workspaceRoot, env, ioctx)
	case "list":
		return dbGrantList(params, workspaceRoot, env, ioctx)
	case "revoke":
		return dbGrantRevoke(params, rest[1:], workspaceRoot, env, ioctx)
	default:
		return clicore.NewError("unknown db grant subcommand: "+action+`; expected "request", "list", or "revoke <id>"`, clicore.ExitUsage)
	}
}

func dbHelp(params map[string]any, ioctx clicore.IO) error {
	commands := []map[string]string{
		{"command": "cloud db status [<db>] [--strict]", "description": "compare the databases with the workloads putnami.ci.json selects; with <db>, its bindings, grant and, with an active grant, size, tables and connections"},
		{"command": "cloud db list", "description": "list the databases in this workspace (engine, schema count, your active grant) — no grant needed"},
		{"command": "cloud db info <db>", "description": "show high-level facts about a database, plus size/table/connection metrics when you hold an active grant"},
		{"command": "cloud db grant request --database <db> --level read|write|maintain --reason <why> [--ttl <seconds>]", "description": "open a time-boxed access grant and print the one-time connection string once (never stored)"},
		{"command": "cloud db grant list --database <db>", "description": "list your own access grants on a database (never shows secrets)"},
		{"command": "cloud db grant revoke <id> --database <db>", "description": "revoke one of your own grants and tear down its login immediately"},
		{"command": "cloud db connect --database <db> [--port <port>]", "description": "open a localhost listener bridged to the db-gateway so psql/GUIs connect with zero GCP credentials"},
		{"command": "cloud db migrations publish [<project>]", "description": "publish a project's migration bundle (putnami publish does this for you)"},
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(map[string]any{"commands": commands}, params, ioctx, "")
		return nil
	}
	ioctx.Stdout("@putnami/cloud db commands:")
	for _, c := range commands {
		ioctx.Stdout(fmt.Sprintf("  putnami %-90s %s", c["command"], c["description"]))
	}
	return nil
}

// requiredDatabase reads the mandatory --database flag.
func requiredDatabase(params map[string]any) (string, error) {
	database := strings.TrimSpace(clicore.StringParam(params, "database", "db"))
	if database == "" {
		return "", clicore.NewError("cloud db requires --database <name>", clicore.ExitUsage)
	}
	return database, nil
}

// dbGrantRequest POSTs an open-grant request to the workspace self-grant surface
// and prints the returned connection payload plus one-time password EXACTLY
// ONCE. The credential flows only to ioctx.Stdout — it is never written to disk.
func dbGrantRequest(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	database, err := requiredDatabase(params)
	if err != nil {
		return err
	}
	level := clicore.FirstString(clicore.StringParam(params, "level"), "read")
	reason := strings.TrimSpace(clicore.StringParam(params, "reason"))
	if reason == "" {
		return clicore.NewError("cloud db grant request requires --reason <why> (grants are auditable and the reason is mandatory)", clicore.ExitUsage)
	}
	ttl := 0
	if v := clicore.NumberParam(params, "ttl", "ttl-seconds", "ttlSeconds"); v != nil {
		ttl = int(*v)
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	api, err := dataClient(ctx)
	if err != nil {
		return err
	}
	grantRequest := dataapiclient.DatabaseGrantRequest{Level: &level, Reason: &reason}
	if ttl != 0 {
		ttlSeconds := int64(ttl)
		grantRequest.TtlSeconds = &ttlSeconds
	}
	target := ctx.WorkspaceURL("/databases/" + clicore.URLPathEscape(database) + "/grants")
	invalid := "invalid grant response from " + target
	result, err := clicore.CallWithSession(context.Background(), ctx, func(callCtx context.Context) (*dataapiclient.CreateV1WorkspacesDatabasesGrantsResult, error) {
		return api.CreateV1WorkspacesDatabasesGrants(callCtx, dataapiclient.CreateV1WorkspacesDatabasesGrantsInput{
			Path: dataapiclient.CreateV1WorkspacesDatabasesGrantsPath{Workspace: ctx.WorkspaceID, Database: database},
			Body: grantRequest,
		})
	})
	if err != nil {
		return dataCallError(target, invalid, err)
	}
	if result.Body == nil {
		return clicore.NewError(invalid, clicore.ExitAPI)
	}
	return emitGrantCredentials(grantCredentialsFrom(*result.Body), params, ioctx, bridgePort(params, env))
}

// emitGrantCredentials writes the one-time credential to stdout exactly once and
// never to disk. In structured mode it renders the grantCredentials envelope
// (which carries the password, on stdout only); in human mode it prints the DSN
// — the single place the password appears — under a "shown once" warning.
func emitGrantCredentials(cred grantCredentials, params map[string]any, ioctx clicore.IO, port int) error {
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(cred, params, ioctx, "")
		return nil
	}
	dsn := localDSN(cred.Username, cred.Password, port, cred.Database)
	ioctx.Stdout("Access grant opened.")
	ioctx.Stdout("  grant id: " + cred.GrantID)
	ioctx.Stdout("  database: " + cred.Database)
	ioctx.Stdout("  level:    " + cred.Level)
	if cred.ExpiresAt != nil {
		ioctx.Stdout("  expires:  " + cred.ExpiresAt.UTC().Format(time.RFC3339))
	}
	ioctx.Stdout("")
	ioctx.Stdout("Connection string (shown once — not stored; re-request if lost):")
	ioctx.Stdout("  " + dsn)
	ioctx.Stdout("")
	ioctx.Stdout("Start the local bridge in another terminal, then connect with the string above:")
	ioctx.Stdout("  putnami cloud db connect --database " + cred.Database)
	return nil
}

// localDSN builds the postgres:// connection string a client uses against the
// local `db connect` bridge. Username and password are URL-escaped via
// url.UserPassword so a password with reserved characters round-trips.
func localDSN(username, password string, port int, database string) string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(username, password),
		Host:   "127.0.0.1:" + strconv.Itoa(port),
		Path:   "/" + database,
	}
	return u.String()
}

// dbGrantList GETs the caller's grants on a database and renders a secret-free
// table (GrantView carries no password field).
func dbGrantList(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	database, err := requiredDatabase(params)
	if err != nil {
		return err
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	api, err := dataClient(ctx)
	if err != nil {
		return err
	}
	target := ctx.WorkspaceURL("/databases/" + clicore.URLPathEscape(database) + "/grants")
	invalid := "invalid grant list response from " + target
	grants, err := clicore.CallWithSession(context.Background(), ctx, func(callCtx context.Context) (*dataapiclient.GrantList, error) {
		return api.GetV1WorkspacesDatabasesGrants(callCtx, dataapiclient.GetV1WorkspacesDatabasesGrantsInput{
			Path: dataapiclient.GetV1WorkspacesDatabasesGrantsPath{Workspace: ctx.WorkspaceID, Database: database},
		})
	})
	if err != nil {
		return dataCallError(target, invalid, err)
	}
	list := grantListFrom(grants)
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(list, params, ioctx, "")
		return nil
	}
	renderGrantTable(database, list.Grants, ioctx)
	return nil
}

// renderGrantTable prints the grants as an aligned, secret-free table.
func renderGrantTable(database string, grants []grantView, ioctx clicore.IO) {
	if len(grants) == 0 {
		ioctx.Stdout("No access grants on " + database + ".")
		return
	}
	ioctx.Stdout(fmt.Sprintf("%-26s  %-8s  %-9s  %-20s  %s", "ID", "LEVEL", "STATUS", "EXPIRES", "REASON"))
	for _, g := range grants {
		expires := "-"
		if g.ExpiresAt != nil {
			expires = g.ExpiresAt.UTC().Format(time.RFC3339)
		}
		ioctx.Stdout(fmt.Sprintf("%-26s  %-8s  %-9s  %-20s  %s", g.ID, g.Level, g.Status, expires, g.Reason))
	}
}

// dbList GETs the workspace database inventory and renders a secret-free table.
// The inventory endpoint is grant-free (PermDatabaseRead, no active-grant gate),
// so this discovery command works with no open grant.
func dbList(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	inv, err := fetchInventory(ctx)
	if err != nil {
		return err
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(inv, params, ioctx, "")
		return nil
	}
	renderDatabaseTable(inv.Databases, ioctx)
	return nil
}

// fetchInventory GETs the grant-free workspace database inventory. Every field on
// databaseInventory is secret-free by construction (ActiveGrant is a grantView),
// so no caller of this can echo a credential. Shared by `db list` and `db info`.
func fetchInventory(ctx *clicore.WorkspaceContext) (databaseInventory, error) {
	api, err := dataClient(ctx)
	if err != nil {
		return databaseInventory{}, err
	}
	target := ctx.WorkspaceURL("/databases")
	invalid := "invalid database inventory response from " + target
	inventory, err := clicore.CallWithSession(invocationContext(ctx), ctx, func(callCtx context.Context) (*dataapiclient.DatabaseInventory, error) {
		return api.GetV1WorkspacesDatabases(callCtx, dataapiclient.GetV1WorkspacesDatabasesInput{
			Path: dataapiclient.GetV1WorkspacesDatabasesPath{Workspace: ctx.WorkspaceID},
		})
	})
	if err != nil {
		return databaseInventory{}, dataCallError(target, invalid, err)
	}
	return databaseInventoryFrom(inventory), nil
}

// renderDatabaseTable prints the inventory as an aligned, secret-free table:
// DATABASE, ENGINE, SCHEMAS (count), and the caller's ACTIVE GRANT status (or -).
func renderDatabaseTable(databases []databaseInventoryItem, ioctx clicore.IO) {
	if len(databases) == 0 {
		ioctx.Stdout("No databases in this workspace.")
		return
	}
	ioctx.Stdout(fmt.Sprintf("%-24s  %-12s  %-8s  %s", "DATABASE", "ENGINE", "SCHEMAS", "ACTIVE GRANT"))
	for _, d := range databases {
		engine := clicore.FirstString(d.Engine, "-")
		grant := "-"
		if d.ActiveGrant != nil {
			grant = d.ActiveGrant.Status
		}
		ioctx.Stdout(fmt.Sprintf("%-24s  %-12s  %-8d  %s", d.Database, engine, len(d.Schemas), grant))
	}
}

// dbGrantRevoke DELETEs one of the caller's grants by id.
func dbGrantRevoke(params map[string]any, rest []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	id := ""
	if len(rest) > 0 {
		id = strings.TrimSpace(rest[0])
	}
	if id == "" {
		return clicore.NewError("cloud db grant revoke requires a grant id (e.g. putnami cloud db grant revoke grant_ab12 --database <db>)", clicore.ExitUsage)
	}
	database, err := requiredDatabase(params)
	if err != nil {
		return err
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	api, err := dataClient(ctx)
	if err != nil {
		return err
	}
	target := ctx.WorkspaceURL("/databases/" + clicore.URLPathEscape(database) + "/grants/" + clicore.URLPathEscape(id))
	if _, err := clicore.CallWithSession(context.Background(), ctx, func(callCtx context.Context) (*dataapiclient.DeleteV1WorkspacesDatabasesGrantsResult, error) {
		return api.DeleteV1WorkspacesDatabasesGrants(callCtx, dataapiclient.DeleteV1WorkspacesDatabasesGrantsInput{
			Path: dataapiclient.DeleteV1WorkspacesDatabasesGrantsPath{Workspace: ctx.WorkspaceID, Database: database, Id: id},
		})
	}); err != nil {
		return dataCallError(target, "invalid grant revoke response from "+target, err)
	}
	clicore.WriteResult(map[string]any{"revoked": true, "id": id, "database": database}, params, ioctx,
		fmt.Sprintf("Revoked grant %s on %s.", id, database))
	return nil
}

// dbPositionals extracts the non-flag positional args from the raw arg vector,
// skipping the parent CLI's --putnamiContext token and any value consumed by a
// non-boolean string flag. It mirrors the registries/oci self-parse so the db
// family reads its subcommand + id positionally without a flag parser.
func dbPositionals(args []string) []string {
	out := []string{}
	skipNext := false
	for i, raw := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if raw == "--putnamiContext" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(raw, "--") {
			name := strings.TrimPrefix(raw, "--")
			if strings.Contains(name, "=") || strings.HasPrefix(name, "no-") || clicore.IsBooleanFlag(name) {
				continue
			}
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				skipNext = true
			}
			continue
		}
		out = append(out, raw)
	}
	return out
}
