// Package migration defines the shared migration protocol used by
// framework runners, startup hooks, and automation that needs one
// deterministic migration contract across languages.
package migration

// ProtocolVersion is the current migration protocol version.
const ProtocolVersion = 1

// Canonical migration state store.
const (
	DefaultDatasource = "default"
	StateSchema       = "migration"
	StateTable        = "migrations"
)

// SourceKind identifies where a migration definition came from.
type SourceKind string

// SourceKind values represent language-specific authoring sources.
const (
	SourceSQLFile   SourceKind = "sql-file"
	SourceRegistry  SourceKind = "registry"
	SourceGenerated SourceKind = "generated"
)

// ValidSourceKinds is the set of allowed source kind values.
var ValidSourceKinds = map[SourceKind]bool{
	SourceSQLFile:   true,
	SourceRegistry:  true,
	SourceGenerated: true,
}

// Operation identifies the migration operation being performed.
type Operation string

// Operation values classify runner behavior.
const (
	OperationApply    Operation = "apply"
	OperationRollback Operation = "rollback"
	OperationValidate Operation = "validate"
	OperationStatus   Operation = "status"
)

// DriftPolicy defines how a runner handles already-applied migrations whose
// normalized contents no longer match the stored hash.
type DriftPolicy string

// DriftPolicy values define the canonical Putnami behavior.
const (
	DriftReject DriftPolicy = "reject"
)

// LockStrategy defines how runners coordinate concurrent migration execution.
type LockStrategy string

// LockStrategy values identify supported mutual exclusion strategies.
const (
	LockAdvisory LockStrategy = "advisory"
)

// StartupContract defines how migration execution interacts with application startup.
type StartupContract struct {
	RunBeforeReady     bool      `json:"runBeforeReady"`
	FailStartupOnError bool      `json:"failStartupOnError"`
	Operation          Operation `json:"operation"`
}

// ReliabilityContract defines the guarantees every compliant runner should provide.
type ReliabilityContract struct {
	DedicatedStateSchema  bool `json:"dedicatedStateSchema"`
	TransactionalApply    bool `json:"transactionalApply"`
	TransactionalRollback bool `json:"transactionalRollback"`
	RecordFailures        bool `json:"recordFailures"`
	PersistRollbackHash   bool `json:"persistRollbackHash"`
	// SupportsRollback advertises that the runner accepts down SQL at apply
	// time, persists it alongside the up hash, and exposes a rollback
	// operation that consumes it. Both Putnami runners implement rollback.
	SupportsRollback bool         `json:"supportsRollback"`
	DriftPolicy      DriftPolicy  `json:"driftPolicy"`
	LockStrategy     LockStrategy `json:"lockStrategy"`
}

// ColumnSpec describes one canonical state-table column.
type ColumnSpec struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Nullable    bool   `json:"nullable,omitempty"`
	Description string `json:"description,omitempty"`
}

// StateStoreSpec describes the canonical migration state store.
type StateStoreSpec struct {
	Schema  string       `json:"schema"`
	Table   string       `json:"table"`
	Columns []ColumnSpec `json:"columns"`
}

// Contract is the top-level migration protocol contract.
type Contract struct {
	Version     int                 `json:"version"`
	StateStore  StateStoreSpec      `json:"stateStore"`
	Startup     StartupContract     `json:"startup"`
	Reliability ReliabilityContract `json:"reliability"`
}

// Definition is the normalized logical migration definition produced by a framework.
//
// Framework-specific authoring formats are out of scope; every implementation maps
// its native migration representation into this shape before applying protocol rules.
type Definition struct {
	Datasource string `json:"datasource,omitempty"`
	// Namespace identifies the feature/plugin that owns this migration. It is
	// the runner-side ownership label, used for diagnostics and for the
	// recommended ${namespace}/${basename} naming convention. Empty for
	// rows authored before per-feature ownership was introduced.
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	OrderKey   string `json:"orderKey,omitempty"`
	Hash       string `json:"hash"`
	DownHash   string `json:"downHash,omitempty"`
	Reversible bool   `json:"reversible,omitempty"`
	// Compatible reports that the schema after this migration stays readable
	// and writable by the previous application image (expand/contract), so an
	// environment may roll back across it without running Down.
	Compatible bool       `json:"compatible,omitempty"`
	SourceKind SourceKind `json:"sourceKind,omitempty"`
}

// Record describes one row in the canonical migration state store.
type Record struct {
	ID              string `json:"id"`
	Datasource      string `json:"datasource"`
	Name            string `json:"name"`
	Hash            string `json:"hash"`
	DownHash        string `json:"downHash,omitempty"`
	ExecutedAt      string `json:"executedAt"`
	ExecutionTimeMS int    `json:"executionTimeMs"`
	Success         int    `json:"success"`
	ErrorMessage    string `json:"errorMessage,omitempty"`
}

// DefaultContract returns the canonical migration contract that Putnami frameworks
// should implement unless a later protocol version explicitly changes it.
func DefaultContract() Contract {
	return Contract{
		Version: ProtocolVersion,
		StateStore: StateStoreSpec{
			Schema: StateSchema,
			Table:  StateTable,
			Columns: []ColumnSpec{
				{Name: "id", Type: "TEXT", Description: "Canonical datasource-scoped identity: ${db_name}:${name}"},
				{Name: "db_name", Type: "TEXT", Description: "Logical datasource name"},
				{Name: "name", Type: "TEXT", Description: "Framework-visible migration name"},
				{Name: "hash", Type: "TEXT", Description: "SHA-256 hash of normalized up migration contents"},
				{Name: "executed_at", Type: "TEXT", Description: "RFC3339 UTC execution timestamp"},
				{Name: "execution_time_ms", Type: "INTEGER", Description: "Observed execution duration in milliseconds"},
				{Name: "success", Type: "INTEGER", Description: "1 for applied migrations, 0 for recorded failures"},
				{Name: "error_message", Type: "TEXT", Nullable: true, Description: "Recorded failure message"},
				{Name: "down_sql", Type: "TEXT", Nullable: true, Description: "Persisted rollback SQL when available"},
				{Name: "down_hash", Type: "TEXT", Nullable: true, Description: "SHA-256 hash of persisted rollback SQL"},
			},
		},
		Startup: StartupContract{
			RunBeforeReady:     true,
			FailStartupOnError: true,
			Operation:          OperationApply,
		},
		Reliability: ReliabilityContract{
			DedicatedStateSchema:  true,
			TransactionalApply:    true,
			TransactionalRollback: true,
			RecordFailures:        true,
			PersistRollbackHash:   true,
			SupportsRollback:      true,
			DriftPolicy:           DriftReject,
			LockStrategy:          LockAdvisory,
		},
	}
}

// QualifiedStateTable returns the canonical fully qualified migration state table name.
func QualifiedStateTable() string {
	return StateSchema + "." + StateTable
}

// CanonicalID returns the datasource-scoped migration identity used in the state store.
func CanonicalID(datasource, name string) string {
	if datasource == "" {
		datasource = DefaultDatasource
	}
	return datasource + ":" + name
}
