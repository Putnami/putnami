package database

import (
	"sort"
	"strconv"
	"strings"

	perrors "go.putnami.dev/errors"
)

// cloudSQLSocketDir is the directory Cloud Run / Cloud Functions mount the
// Cloud SQL Unix socket under: the per-instance socket lives at
// "/cloudsql/<project>:<region>:<instance>". Connecting over it pairs with
// PoolConfig.TokenFetcher when no password is supplied.
const cloudSQLSocketDir = "/cloudsql"

// reservedConnectionParams are libpq keywords the dedicated Connection fields
// own. They are filtered out of Connection.Params so a stray param cannot
// shadow or duplicate a field-derived keyword in the built DSN.
var reservedConnectionParams = map[string]struct{}{
	"host":     {},
	"hostaddr": {},
	"port":     {},
	"dbname":   {},
	"user":     {},
	"password": {},
}

// Connection describes the physical connection to a PostgreSQL server,
// decoupled from the logical datasource (name + schema) a workload declares
// in code. The data plane supplies it at bootstrap — e.g. the cloud control
// plane maps env.prod.yaml into the workload's config — so the same binary
// reaches dev, staging, and prod without changing its declared datasource.
//
// A non-empty PoolConfig.DSN overrides Connection outright (the escape hatch).
// When only a Connection is set, NewPool builds a DSN from it plus the
// declared datasource name (PoolConfig.Datasource.Name, or the legacy
// PoolConfig.Database) as the database to connect to.
type Connection struct {
	// Instance is a Cloud SQL instance connection name in
	// "project:region:instance" form. When set and Host is empty, the pool
	// connects over the Cloud SQL Unix socket at
	// "/cloudsql/<instance>" — which triggers PoolConfig.TokenFetcher when no
	// password is supplied.
	Instance string

	// Host is a TCP hostname/IP or an absolute Unix-socket directory. It
	// takes precedence over Instance when both are set.
	Host string

	// Port is the TCP port. Omitted from the DSN (pgx defaults to 5432) when
	// zero, and ignored for Unix-socket connections.
	Port int

	// User is the Postgres role. Leave empty only when PoolConfig.IdentityResolver
	// is set to resolve the runtime principal at startup.
	User string

	// Password is the Postgres password. Leave empty on a Unix socket only when
	// PoolConfig.TokenFetcher is set to provide a per-connection password.
	Password string

	// Params carries extra libpq parameters (e.g. "sslmode"). Keys that the
	// dedicated fields own (host, port, dbname, user, password, hostaddr) are
	// ignored. Remaining keys are emitted in sorted order so the built DSN is
	// deterministic.
	Params map[string]string
}

// isSet reports whether any connection field carries a value, i.e. whether
// the caller supplied a structured connection at all.
func (c Connection) isSet() bool {
	return strings.TrimSpace(c.Instance) != "" ||
		strings.TrimSpace(c.Host) != "" ||
		c.Port != 0 ||
		strings.TrimSpace(c.User) != "" ||
		c.Password != "" ||
		len(c.Params) > 0
}

// dsn renders the connection as a libpq keyword/value DSN for the given
// database. The form (rather than a postgres:// URL) is what NewPool's
// existing identity/token resolution already inspects, and it represents a
// Cloud SQL Unix socket host cleanly.
//
// User and Password are emitted only when non-empty, so a runtime-identity
// connection (no user, no password) produces a DSN that drives NewPool's
// explicit principal resolution and per-connection token fetch hooks.
func (c Connection) dsn(database string) (string, error) {
	host := strings.TrimSpace(c.Host)
	socket := false
	switch {
	case host != "":
		socket = isUnixSocket(host)
	case strings.TrimSpace(c.Instance) != "":
		host = cloudSQLSocketDir + "/" + strings.TrimSpace(c.Instance)
		socket = true
	default:
		return "", perrors.Newf(CodeConnection,
			"Connection requires Host or Instance")
	}

	db := strings.TrimSpace(database)
	if db == "" {
		return "", perrors.Newf(CodeConnection,
			"Connection requires a database name: declare PoolConfig.Datasource.Name (or set PoolConfig.Database)")
	}

	pairs := [][2]string{{"host", host}, {"dbname", db}}
	if !socket && c.Port != 0 {
		pairs = append(pairs, [2]string{"port", strconv.Itoa(c.Port)})
	}
	if u := strings.TrimSpace(c.User); u != "" {
		pairs = append(pairs, [2]string{"user", u})
	}
	if c.Password != "" {
		pairs = append(pairs, [2]string{"password", c.Password})
	}
	for _, k := range sortedParamKeys(c.Params) {
		pairs = append(pairs, [2]string{k, c.Params[k]})
	}

	parts := make([]string, len(pairs))
	for i, kv := range pairs {
		parts[i] = kv[0] + "=" + quoteDSNValue(kv[1])
	}
	return strings.Join(parts, " "), nil
}

// sortedParamKeys returns the user-supplied param keys in sorted order with
// the reserved (field-owned) keys removed, so DSN rendering is deterministic
// and a param can never shadow a dedicated field.
func sortedParamKeys(params map[string]string) []string {
	if len(params) == 0 {
		return nil
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		if _, reserved := reservedConnectionParams[strings.ToLower(strings.TrimSpace(k))]; reserved {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// quoteDSNValue renders v as a libpq keyword/value DSN value, quoting and
// escaping per libpq rules: an empty value or one containing whitespace, a
// single quote, or a backslash is wrapped in single quotes with backslashes
// and single quotes backslash-escaped.
func quoteDSNValue(v string) string {
	if v == "" {
		return "''"
	}
	if !strings.ContainsAny(v, " \t\n\r'\\") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v) + 2)
	b.WriteByte('\'')
	for _, r := range v {
		if r == '\\' || r == '\'' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

// Validate reports whether the datasource is a complete declaration. The
// uniform datasource provider requires both Name and Schema: Name is the
// logical database (and the physical database a built connection targets) and
// Schema is the real Postgres schema the workload owns, so the deployer can
// provision and migrate against a resolvable (name, schema) without parsing a
// DSN. The zero Datasource (both empty) is "not declared" — callers skip
// validation for it; a partial declaration is an authoring error.
func (d Datasource) Validate() error {
	if strings.TrimSpace(d.Name) == "" || strings.TrimSpace(d.Schema) == "" {
		return perrors.Newf(CodeDatasource,
			"datasource declaration requires both name and schema (got name=%q schema=%q)",
			d.Name, d.Schema)
	}
	return nil
}

// isZero reports whether neither Name nor Schema is set, i.e. the caller did
// not declare a datasource and the legacy PoolConfig.Database path applies.
func (d Datasource) isZero() bool {
	return strings.TrimSpace(d.Name) == "" && strings.TrimSpace(d.Schema) == ""
}
