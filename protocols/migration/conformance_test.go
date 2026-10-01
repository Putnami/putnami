package migration

import (
	"strings"
	"testing"
)

// TestConformance_StateStoreSchema validates that the canonical state store
// specification matches the protocol README contract exactly.
func TestConformance_StateStoreSchema(t *testing.T) {
	c := DefaultContract()

	if c.StateStore.Schema != "migration" {
		t.Fatalf("state store schema must be %q, got %q", "migration", c.StateStore.Schema)
	}
	if c.StateStore.Table != "migrations" {
		t.Fatalf("state store table must be %q, got %q", "migrations", c.StateStore.Table)
	}

	// The protocol requires exactly these columns in this order.
	required := []struct {
		Name     string
		Type     string
		Nullable bool
	}{
		{"id", "TEXT", false},
		{"db_name", "TEXT", false},
		{"name", "TEXT", false},
		{"hash", "TEXT", false},
		{"executed_at", "TEXT", false},
		{"execution_time_ms", "INTEGER", false},
		{"success", "INTEGER", false},
		{"error_message", "TEXT", true},
		{"down_sql", "TEXT", true},
		{"down_hash", "TEXT", true},
	}

	if len(c.StateStore.Columns) != len(required) {
		t.Fatalf("expected %d canonical columns, got %d", len(required), len(c.StateStore.Columns))
	}

	for i, want := range required {
		got := c.StateStore.Columns[i]
		if got.Name != want.Name {
			t.Errorf("column %d: name = %q, want %q", i, got.Name, want.Name)
		}
		if got.Type != want.Type {
			t.Errorf("column %d (%s): type = %q, want %q", i, want.Name, got.Type, want.Type)
		}
		if got.Nullable != want.Nullable {
			t.Errorf("column %d (%s): nullable = %v, want %v", i, want.Name, got.Nullable, want.Nullable)
		}
	}
}

// TestConformance_CanonicalID validates the datasource-scoped identity format.
func TestConformance_CanonicalID(t *testing.T) {
	tests := []struct {
		datasource string
		name       string
		want       string
	}{
		{"", "20260101000000-init", "default:20260101000000-init"},
		{"default", "20260101000000-init", "default:20260101000000-init"},
		{"auth", "20260101000000-init", "auth:20260101000000-init"},
		{"wealth", "20260315000000-add-accounts", "wealth:20260315000000-add-accounts"},
	}

	for _, tt := range tests {
		got := CanonicalID(tt.datasource, tt.name)
		if got != tt.want {
			t.Errorf("CanonicalID(%q, %q) = %q, want %q", tt.datasource, tt.name, got, tt.want)
		}
		// ID must contain exactly one colon separating datasource from name.
		parts := strings.SplitN(got, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			t.Errorf("CanonicalID(%q, %q) = %q does not follow ${datasource}:${name} format", tt.datasource, tt.name, got)
		}
	}
}

// TestConformance_ErrorCodes validates that all protocol error codes use the
// migration.* prefix and that the full canonical set is present.
func TestConformance_ErrorCodes(t *testing.T) {
	canonical := []string{
		"migration.invalid_definition",
		"migration.duplicate_definition",
		"migration.invalid_hash",
		"migration.drift_detected",
		"migration.apply_failed",
		"migration.rollback_failed",
		"migration.lock_failed",
		"migration.state_store_failed",
		"migration.startup_blocked",
		"migration.invalid_bundle",
		"migration.invalid_payload",
		"migration.digest_mismatch",
		"migration.duplicate_operation",
	}

	for _, code := range canonical {
		if !ValidErrorCodes[code] {
			t.Errorf("canonical error code %q missing from ValidErrorCodes", code)
		}
		if !strings.HasPrefix(code, "migration.") {
			t.Errorf("error code %q must use migration.* prefix", code)
		}
	}

	// Ensure no extra, undocumented error codes crept in.
	if len(ValidErrorCodes) != len(canonical) {
		t.Errorf("ValidErrorCodes has %d entries, want %d canonical codes", len(ValidErrorCodes), len(canonical))
	}
}

// TestConformance_StartupContract validates that the default startup contract
// enforces blocking migration execution before readiness.
func TestConformance_StartupContract(t *testing.T) {
	c := DefaultContract()

	if !c.Startup.RunBeforeReady {
		t.Error("startup contract must require RunBeforeReady")
	}
	if !c.Startup.FailStartupOnError {
		t.Error("startup contract must require FailStartupOnError")
	}
	if c.Startup.Operation != OperationApply {
		t.Errorf("startup operation must be %q, got %q", OperationApply, c.Startup.Operation)
	}
}

// TestConformance_ReliabilityContract validates that the default reliability
// contract requires the full set of protocol guarantees.
func TestConformance_ReliabilityContract(t *testing.T) {
	c := DefaultContract()

	if !c.Reliability.DedicatedStateSchema {
		t.Error("reliability contract must require DedicatedStateSchema")
	}
	if !c.Reliability.TransactionalApply {
		t.Error("reliability contract must require TransactionalApply")
	}
	if !c.Reliability.TransactionalRollback {
		t.Error("reliability contract must require TransactionalRollback")
	}
	if !c.Reliability.RecordFailures {
		t.Error("reliability contract must require RecordFailures")
	}
	if !c.Reliability.PersistRollbackHash {
		t.Error("reliability contract must require PersistRollbackHash")
	}
	if !c.Reliability.SupportsRollback {
		t.Error("reliability contract must advertise SupportsRollback")
	}
	if c.Reliability.DriftPolicy != DriftReject {
		t.Errorf("drift policy must be %q, got %q", DriftReject, c.Reliability.DriftPolicy)
	}
	if c.Reliability.LockStrategy != LockAdvisory {
		t.Errorf("lock strategy must be %q, got %q", LockAdvisory, c.Reliability.LockStrategy)
	}
}

// TestConformance_QualifiedTableName validates the fully qualified state table
// matches schema.table exactly.
func TestConformance_QualifiedTableName(t *testing.T) {
	want := "migration.migrations"
	got := QualifiedStateTable()
	if got != want {
		t.Fatalf("QualifiedStateTable() = %q, want %q", got, want)
	}
}

// TestConformance_DefinitionNormalization validates that protocol normalization
// fills canonical defaults without altering explicit values.
func TestConformance_DefinitionNormalization(t *testing.T) {
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	downHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// Minimal definition — normalization must fill defaults.
	d := NormalizeDefinition(Definition{Name: "20260101-init", Hash: hash})
	if d.Datasource != DefaultDatasource {
		t.Errorf("expected datasource %q, got %q", DefaultDatasource, d.Datasource)
	}
	if d.OrderKey != d.Name {
		t.Errorf("expected orderKey to equal name %q, got %q", d.Name, d.OrderKey)
	}
	if d.SourceKind != SourceGenerated {
		t.Errorf("expected sourceKind %q, got %q", SourceGenerated, d.SourceKind)
	}
	if d.Reversible {
		t.Error("definition without downHash must not be marked reversible")
	}

	// Definition with downHash — must be marked reversible.
	d2 := NormalizeDefinition(Definition{Name: "20260101-init", Hash: hash, DownHash: downHash})
	if !d2.Reversible {
		t.Error("definition with downHash must be marked reversible")
	}

	// Explicit values must not be overwritten.
	d3 := NormalizeDefinition(Definition{
		Datasource: "auth",
		Name:       "20260101-init",
		OrderKey:   "custom-order",
		Hash:       hash,
		SourceKind: SourceSQLFile,
	})
	if d3.Datasource != "auth" {
		t.Errorf("explicit datasource overwritten: got %q", d3.Datasource)
	}
	if d3.OrderKey != "custom-order" {
		t.Errorf("explicit orderKey overwritten: got %q", d3.OrderKey)
	}
	if d3.SourceKind != SourceSQLFile {
		t.Errorf("explicit sourceKind overwritten: got %q", d3.SourceKind)
	}
}

// TestConformance_DuplicateDetection validates that per-datasource duplicate
// detection uses canonical IDs.
func TestConformance_DuplicateDetection(t *testing.T) {
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// Same name in DIFFERENT datasources must NOT be a duplicate.
	defs := []Definition{
		{Datasource: "default", Name: "20260101-init", Hash: hash, SourceKind: SourceSQLFile},
		{Datasource: "auth", Name: "20260101-init", Hash: hash, SourceKind: SourceSQLFile},
	}
	diags := ValidateDefinitions(defs)
	for _, d := range diags {
		if d.Code == ErrorCodeDuplicateDefinition {
			t.Fatal("same name in different datasources must not be a duplicate")
		}
	}

	// Same name in SAME datasource must be a duplicate.
	defs2 := []Definition{
		{Datasource: "default", Name: "20260101-init", Hash: hash, SourceKind: SourceSQLFile},
		{Datasource: "default", Name: "20260101-init", Hash: hash, SourceKind: SourceRegistry},
	}
	diags2 := ValidateDefinitions(defs2)
	hasDup := false
	for _, d := range diags2 {
		if d.Code == ErrorCodeDuplicateDefinition {
			hasDup = true
		}
	}
	if !hasDup {
		t.Fatal("same name in same datasource must be reported as duplicate")
	}
}
