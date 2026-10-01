// Package database defines the Putnami canonical database protocol: the single
// cross-language contract for declaring the databases a workload needs, the
// resolved connection a deployer hands back, and the extra policy a test run
// layers on top. Go, TypeScript, and (later) Python adapters consume the same
// shapes instead of each modeling database configuration differently.
//
// Three shapes, deliberately layered so a binding is a requirement plus a
// resolved connection, and a test binding is a binding plus provisioning policy:
//
//   - RequirementManifest: a project's logical database needs, named and
//     secret-free (engine + schema per datasource). It is the rich source of
//     truth that protocol/infra projects into its thin {name, engine, schemas}
//     requirement — see RequirementManifest.Project. A requirement never carries
//     a connection, so secrets can never leak into infra/requirements.json.
//
//   - Binding: the resolved physical connection a deployer/environment injects
//     after provisioning. Framework adapters (go.putnami.dev/database,
//     @putnami/database) turn each datasource's Connection into a pgx /
//     postgres.js configuration internally; application code no longer exposes a
//     language-specific config shape.
//
//   - TestBinding: a Binding extended with test provisioning and isolation
//     policy (mode, isolation, reuse, applyMigrations). A cross-language test
//     provider reads it to provision/reuse Postgres, create an isolated
//     database/schema, apply migrations from the workload's migration bundle,
//     and return resolved bindings — for one or many named datasources, with no
//     single-DSN env-var convention.
//
// The protocol is Postgres-first: postgres is the only engine in v1 and schema
// (the owning schema / search_path) is required for it. Adding an engine or a
// new policy value is a backwards-incompatible change that bumps ProtocolVersion
// so deployers and adapters can decide how to react. See README.md for the
// protocol / provisioning / data-plane split and the migration-bundle apply
// path the test provider builds on.
package database

import "sort"

// ProtocolVersion is the current database-protocol version. It is stamped into
// every RequirementManifest, Binding, and TestBinding and pinned by the JSON
// schemas. Bumped on any backwards-incompatible change to the shapes, enums, or
// semantics; adding an optional field within an existing shape does not bump it.
const ProtocolVersion = 1

// Engine identifies a database engine. The enum is intentionally closed:
// postgres is the only implementation required in v1, and the connection and
// schema/search_path semantics are Postgres-shaped. Adding an engine requires a
// ProtocolVersion bump so adapters and deployers can decide how to react.
type Engine string

// Engine values.
const (
	EnginePostgres Engine = "postgres"
)

// Valid reports whether e is a recognized engine.
func (e Engine) Valid() bool {
	switch e {
	case EnginePostgres:
		return true
	default:
		return false
	}
}

// TestMode is the test provider's behavior when no usable binding/provider
// exists. The enum is intentionally closed.
type TestMode string

// TestMode values.
const (
	// TestModeSkip skips DB tests when no usable binding/provider exists.
	TestModeSkip TestMode = "skip"
	// TestModeRequire fails loudly when no usable binding/provider exists.
	TestModeRequire TestMode = "require"
	// TestModeAuto allows local/dev convenience provisioning (e.g. Docker) when
	// permitted.
	TestModeAuto TestMode = "auto"
)

// Valid reports whether m is a recognized test mode.
func (m TestMode) Valid() bool {
	switch m {
	case TestModeSkip, TestModeRequire, TestModeAuto:
		return true
	default:
		return false
	}
}

// Isolation is the boundary the test provider creates per datasource. The enum
// is intentionally closed.
type Isolation string

// Isolation values.
const (
	// IsolationDatabase provisions an isolated database (preferred when
	// permissions allow CREATE DATABASE).
	IsolationDatabase Isolation = "database"
	// IsolationSchema provisions an isolated schema within a shared database
	// (fallback when CREATE DATABASE is not available).
	IsolationSchema Isolation = "schema"
)

// Valid reports whether i is a recognized isolation boundary.
func (i Isolation) Valid() bool {
	switch i {
	case IsolationDatabase, IsolationSchema:
		return true
	default:
		return false
	}
}

// Reuse is the test provider's caching policy for migrated databases. The enum
// is intentionally closed.
type Reuse string

// Reuse values.
const (
	// ReuseNone provisions a fresh database/schema and replays migrations.
	ReuseNone Reuse = "none"
	// ReuseBundleTemplate may cache a migrated template database keyed by the
	// migration bundle digest, so migrations are not replayed for every suite.
	ReuseBundleTemplate Reuse = "bundle-template"
)

// Valid reports whether r is a recognized reuse policy.
func (r Reuse) Valid() bool {
	switch r {
	case ReuseNone, ReuseBundleTemplate:
		return true
	default:
		return false
	}
}

// Connection is the resolved physical coordinates for one datasource in a
// Binding. It declares exactly one transport strategy:
//
//   - DSN: a self-contained connection string. No structured field may
//     accompany it.
//   - Host (TCP): Host plus optional Port, Database, User, Password, SSL, Params.
//   - Instance (cloud/provider socket, e.g. Cloud SQL): Instance plus optional
//     Database, User, Password, Params.
//
// A Connection only ever appears in a Binding/TestBinding — never in a
// RequirementManifest — so secrets stay out of infra/requirements.json.
type Connection struct {
	// DSN is a self-contained connection string (e.g. postgres://...). Mutually
	// exclusive with every other field.
	DSN string `json:"dsn,omitempty"`
	// Host is the TCP host for the structured strategy.
	Host string `json:"host,omitempty"`
	// Port is the TCP port (structured strategy only).
	Port int `json:"port,omitempty"`
	// Database is the physical database name (structured or instance strategy).
	Database string `json:"database,omitempty"`
	// User is the login role (structured or instance strategy).
	User string `json:"user,omitempty"`
	// Password is the login secret (structured or instance strategy).
	Password string `json:"password,omitempty"` //nolint:gosec // Runtime bindings intentionally carry deployer-injected credentials.
	// SSL requests TLS for the structured (TCP) strategy. Pointer semantics let
	// the strict parser distinguish an omitted field from an explicit false,
	// which matters because DSN and instance strategies may not carry ssl at all.
	SSL *bool `json:"ssl,omitempty"`
	// Params carries extra driver options (e.g. sslmode, application_name).
	Params map[string]string `json:"params,omitempty"`
	// Instance is a cloud/provider instance identifier for the socket strategy
	// (e.g. "project:region:instance" for Cloud SQL).
	Instance string `json:"instance,omitempty"`
}

// Requirement is one datasource's logical, secret-free need in a
// RequirementManifest. It deliberately has no Connection field, so strict
// parsing rejects any attempt to smuggle connection details (and therefore
// secrets) into a requirement.
type Requirement struct {
	// Engine is the database engine the datasource needs. Required.
	Engine Engine `json:"engine"`
	// Schema is the owning schema / search_path entry. Required for postgres; no
	// implicit schema is derived from the datasource name.
	Schema string `json:"schema,omitempty"`
}

// Database is one datasource's resolved binding entry: the same engine/schema as
// the Requirement plus the physical Connection the adapter builds a driver from.
type Database struct {
	// Engine is the database engine. Required.
	Engine Engine `json:"engine"`
	// Schema is the owning schema / search_path entry. Required for postgres.
	Schema string `json:"schema,omitempty"`
	// Connection is the resolved physical connection. Required at runtime.
	Connection *Connection `json:"connection,omitempty"`
}

// RequirementManifest is a project's declared logical database needs, keyed by
// logical datasource name. It is secret-free and is the source projected into
// protocol/infra (see Project).
type RequirementManifest struct {
	Schema          string `json:"$schema,omitempty"`
	ProtocolVersion int    `json:"protocolVersion"`
	// Databases maps logical datasource name to its requirement.
	Databases map[string]Requirement `json:"databases,omitempty"`
}

// Binding is the resolved runtime connection data a deployer/environment injects
// into a workload, keyed by logical datasource name. Adapters convert each
// entry's Connection into a driver configuration internally.
type Binding struct {
	Schema          string `json:"$schema,omitempty"`
	ProtocolVersion int    `json:"protocolVersion"`
	// Databases maps logical datasource name to its resolved binding.
	Databases map[string]Database `json:"databases,omitempty"`
}

// TestBinding is a Binding extended with test provisioning and isolation policy.
// It uses the same datasource/connection shape so a multi-datasource workload
// can request migrated Postgres bindings for several datasources at once.
type TestBinding struct {
	Schema          string `json:"$schema,omitempty"`
	ProtocolVersion int    `json:"protocolVersion"`
	// Mode controls behavior when no usable binding/provider exists.
	Mode TestMode `json:"mode,omitempty"`
	// Isolation is the per-datasource isolation boundary the provider creates.
	Isolation Isolation `json:"isolation,omitempty"`
	// Reuse is the migrated-database caching policy.
	Reuse Reuse `json:"reuse,omitempty"`
	// ApplyMigrations, when true, has the provider apply and verify migrations
	// before returning bindings.
	ApplyMigrations bool `json:"applyMigrations,omitempty"`
	// KeepDatabases, when true, has the provider leave the isolated databases
	// it created in place: the per-suite teardown closes its connections but
	// skips DROP DATABASE. Set it when the server's own lifetime is the
	// cleanup — e.g. a CI server that dies with the run — where every DROP
	// forces an immediate cluster-wide checkpoint that stalls behind the whole
	// fleet's concurrent writes.
	KeepDatabases bool `json:"keepDatabases,omitempty"`
	// Databases maps logical datasource name to its resolved binding.
	Databases map[string]Database `json:"databases,omitempty"`
}

// ProjectedDatabase is the thin, secret-free database requirement that
// protocol/infra aggregates: logical name, engine, and the schemas the workload
// owns. It is the projection target of a RequirementManifest and mirrors the
// shape infra keeps (name, engine, schemas) without importing infra, so the two
// protocol modules stay decoupled until infra is wired to derive from this
// contract.
type ProjectedDatabase struct {
	Name    string
	Engine  Engine
	Schemas []string
}

// Project flattens a requirement manifest into the thin infra projection,
// sorted by datasource name for deterministic aggregation. A datasource's single
// schema becomes a one-element Schemas slice (nil when unset). Returns nil for a
// nil or empty manifest.
func (m *RequirementManifest) Project() []ProjectedDatabase {
	if m == nil || len(m.Databases) == 0 {
		return nil
	}
	names := make([]string, 0, len(m.Databases))
	for name := range m.Databases {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ProjectedDatabase, 0, len(names))
	for _, name := range names {
		r := m.Databases[name]
		var schemas []string
		if r.Schema != "" {
			schemas = []string{r.Schema}
		}
		out = append(out, ProjectedDatabase{Name: name, Engine: r.Engine, Schemas: schemas})
	}
	return out
}
