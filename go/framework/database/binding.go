package database

import (
	"context"
	"os"
	"sort"
	"strings"

	"go.putnami.dev/errors"
	pdb "go.putnami.dev/protocol/database"
	diag "go.putnami.dev/protocol/diagnostic"
)

// EnvBinding is the environment variable a deploy target injects to hand a
// workload the resolved physical connections for the logical datasources it
// declared: a workload that added database.NewPlugin(PluginConfig{Datasource:
// "auth"}) resolves a working, correctly-bound pool from it with no bespoke
// per-workload connection env. It is the legacy transport for the managed
// binding: deploy targets now merge the same document into the "database"
// section of the workload's resolved config (see ConfigSection), which wins
// when both are present; the env var remains the fallback until every deploy
// target stops injecting it.
//
// The value is a canonical database Binding (go.putnami.dev/protocol/database)
// encoded as JSON: a map keyed by logical datasource name, each carrying the
// engine, the owning schema, and exactly one connection strategy. The protocol
// owns the shape; this package owns reading it at startup, mirroring how
// storage reads STORAGE_BINDINGS. Bindings carry secrets and must never be
// written into infra/requirements.json.
const EnvBinding = "DATABASE_BINDINGS"

// ParseBinding strictly parses and validates the canonical database Binding the
// deployer injects via EnvBinding, surfacing the protocol's validation
// diagnostics as a single db.binding error.
func ParseBinding(data []byte) (*pdb.Binding, error) {
	b, diags := pdb.ParseAndValidateBinding(data)
	if diag.HasErrors(diags) {
		return nil, errors.Newf(CodeBinding, "invalid %s: %s", EnvBinding, formatBindingDiags(diags))
	}
	return b, nil
}

// PoolConfigFromBinding resolves the named logical datasource in b into the
// connection half of a PoolConfig: the single declared transport strategy
// (dsn / host+TCP / instance socket) plus the owning schema as SearchPath. It
// does not set pool tuning, identity, or token knobs — the caller merges those.
//
// It fails with a clear db.binding error when the datasource is absent (listing
// the datasources the binding does declare), uses an unsupported engine, or
// carries no usable connection — turning a deploy-time misconfiguration into a
// precise startup diagnostic rather than an opaque connection failure.
func PoolConfigFromBinding(b *pdb.Binding, datasource string) (PoolConfig, error) {
	return poolConfigFromBinding(b, datasource, EnvBinding)
}

// poolConfigFromBinding is PoolConfigFromBinding with the binding's transport
// named in diagnostics: EnvBinding for the env transport, configSourceLabel for
// a document carried by the resolved config section.
func poolConfigFromBinding(b *pdb.Binding, datasource, source string) (PoolConfig, error) {
	name := strings.TrimSpace(datasource)
	if name == "" {
		return PoolConfig{}, errors.Newf(CodeBinding, "a datasource name is required to resolve a database binding")
	}
	if b == nil {
		return PoolConfig{}, errors.Newf(CodeBinding, "no database binding provided for datasource %q", name)
	}
	db, ok := b.Databases[name]
	if !ok {
		return PoolConfig{}, errors.Newf(CodeBinding,
			"no binding for datasource %q; %s declares: %s", name, source, availableDatasources(b))
	}
	if db.Engine != pdb.EnginePostgres {
		return PoolConfig{}, errors.Newf(CodeBinding,
			"datasource %q uses unsupported engine %q (only postgres is supported)", name, string(db.Engine))
	}
	if db.Connection == nil {
		return PoolConfig{}, errors.Newf(CodeBinding, "datasource %q has no connection in its binding", name)
	}

	conn := db.Connection
	pc := PoolConfig{SearchPath: strings.TrimSpace(db.Schema)}
	switch {
	case strings.TrimSpace(conn.DSN) != "":
		pc.DSN = strings.TrimSpace(conn.DSN)
	case strings.TrimSpace(conn.Host) != "":
		pc.Connection = Connection{
			Host:     strings.TrimSpace(conn.Host),
			Port:     conn.Port,
			User:     conn.User,
			Password: conn.Password,
			Params:   sslParams(conn),
		}
		pc.Database = strings.TrimSpace(conn.Database)
	case strings.TrimSpace(conn.Instance) != "":
		pc.Connection = Connection{
			Instance: strings.TrimSpace(conn.Instance),
			User:     conn.User,
			Password: conn.Password,
			Params:   copyParams(conn.Params),
		}
		pc.Database = strings.TrimSpace(conn.Database)
	default:
		return PoolConfig{}, errors.Newf(CodeBinding,
			"datasource %q binding has no usable connection (need dsn, host, or instance)", name)
	}
	return pc, nil
}

// resolveDatasourceBinding resolves datasource from the managed binding a
// deploy target injected, preferring the document carried by the resolved
// config section (see ConfigSection) over the EnvBinding transport. A non-nil
// configured document is authoritative: a datasource it does not declare is a
// loud error, never a silent fall-through to env or local fallbacks — which
// makes retiring the env injection a verified no-op once every deploy target
// populates the config section. With no configured document the env transport
// behaves exactly as it always has.
func resolveDatasourceBinding(configured *pdb.Binding, datasource string) (cfg PoolConfig, ok bool, err error) {
	if configured != nil {
		pc, err := poolConfigFromBinding(configured, datasource, configSourceLabel)
		if err != nil {
			return PoolConfig{}, false, err
		}
		return pc, true, nil
	}
	return resolveBinding(datasource)
}

// HasManagedBinding reports whether a managed database binding is resolvable for
// this process — from the "database" config section (see ConfigSection) or,
// during transition, the DATABASE_BINDINGS env fallback (see EnvBinding). It
// does NOT resolve a specific datasource; it answers the whole-process "is this
// process bound to a managed DB at all" question a deploy target's composition
// uses to pick a managed postgres pool over a local/in-memory backend before
// any plugin is mounted.
//
// It is transport-agnostic: the config section wins, the env var is the fallback
// until every deploy target stops injecting it. It reuses loadConfigBinding — the
// same internal resolution the plugin applies at mount — so a present-but-
// malformed section reports true (loadConfigBinding returns an error, never a
// silent nil) and then fails loudly at mount, exactly as the plugin's own
// resolution does; the predicate can never disagree with the binding the plugin
// resolves. A section carrying no document falls through to the env presence
// check, and neither transport present is false, so local serve/test/describe
// keep their local/DSN/in-memory path unchanged.
func HasManagedBinding(ctx context.Context) bool {
	binding, err := loadConfigBinding(ctx)
	if err != nil || binding != nil {
		return true
	}
	return strings.TrimSpace(os.Getenv(EnvBinding)) != ""
}

// resolveBinding reads EnvBinding and resolves datasource into the connection
// half of a PoolConfig. It returns ok=false (and no error) when EnvBinding is
// unset or blank, so a caller can fall back to a manually configured
// connection; an error when the injected binding is malformed or does not
// declare the datasource; and the resolved config otherwise.
func resolveBinding(datasource string) (cfg PoolConfig, ok bool, err error) {
	raw := strings.TrimSpace(os.Getenv(EnvBinding))
	if raw == "" {
		return PoolConfig{}, false, nil
	}
	b, err := ParseBinding([]byte(raw))
	if err != nil {
		return PoolConfig{}, false, err
	}
	pc, err := PoolConfigFromBinding(b, datasource)
	if err != nil {
		return PoolConfig{}, false, err
	}
	return pc, true, nil
}

// sslParams maps a protocol connection's structured TCP fields onto libpq
// params: an explicit ssl flag becomes sslmode (require / disable) unless the
// deployer already set sslmode in params, which always wins. Returns a copy so
// the binding's map is never mutated.
func sslParams(conn *pdb.Connection) map[string]string {
	params := copyParams(conn.Params)
	if conn.SSL == nil {
		return params
	}
	if _, ok := lookupFold(params, "sslmode"); ok {
		return params
	}
	if params == nil {
		params = map[string]string{}
	}
	if *conn.SSL {
		params["sslmode"] = "require"
	} else {
		params["sslmode"] = "disable"
	}
	return params
}

func copyParams(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func lookupFold(m map[string]string, key string) (string, bool) {
	for k, v := range m {
		if strings.EqualFold(strings.TrimSpace(k), key) {
			return v, true
		}
	}
	return "", false
}

func availableDatasources(b *pdb.Binding) string {
	if b == nil || len(b.Databases) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(b.Databases))
	for n := range b.Databases {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func formatBindingDiags(diags []diag.Diagnostic) string {
	errs := diag.Errors(diags)
	msgs := make([]string, 0, len(errs))
	for _, d := range errs {
		msgs = append(msgs, d.Message)
	}
	return strings.Join(msgs, "; ")
}
