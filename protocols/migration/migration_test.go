package migration

import (
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestDefaultContract(t *testing.T) {
	c := DefaultContract()

	if c.Version != ProtocolVersion {
		t.Fatalf("got version %d, want %d", c.Version, ProtocolVersion)
	}
	if c.StateStore.Schema != StateSchema {
		t.Fatalf("got schema %q, want %q", c.StateStore.Schema, StateSchema)
	}
	if c.StateStore.Table != StateTable {
		t.Fatalf("got table %q, want %q", c.StateStore.Table, StateTable)
	}
	if !c.Startup.RunBeforeReady || !c.Startup.FailStartupOnError {
		t.Fatal("default startup contract must block readiness and fail startup on migration errors")
	}
	if !c.Reliability.DedicatedStateSchema || !c.Reliability.TransactionalApply || !c.Reliability.TransactionalRollback {
		t.Fatal("default reliability contract must require dedicated schema and transactional apply/rollback")
	}
	if !c.Reliability.SupportsRollback {
		t.Fatal("default reliability contract must advertise SupportsRollback")
	}
	if c.Reliability.DriftPolicy != DriftReject {
		t.Fatalf("got drift policy %q, want %q", c.Reliability.DriftPolicy, DriftReject)
	}
	if c.Reliability.LockStrategy != LockAdvisory {
		t.Fatalf("got lock strategy %q, want %q", c.Reliability.LockStrategy, LockAdvisory)
	}
}

func TestQualifiedStateTable(t *testing.T) {
	if got, want := QualifiedStateTable(), "migration.migrations"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCanonicalID(t *testing.T) {
	if got, want := CanonicalID("", "20260101000000-init"), "default:20260101000000-init"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, want := CanonicalID("auth", "20260101000000-init"), "auth:20260101000000-init"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestValidateDefinition(t *testing.T) {
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	downHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	def := Definition{
		Datasource: "default",
		Name:       "20260101000000-init",
		Hash:       hash,
		DownHash:   downHash,
		Reversible: true,
		SourceKind: SourceSQLFile,
	}

	diags := ValidateDefinition(&def)
	if diag.HasErrors(diags) {
		t.Fatalf("expected valid definition, got diagnostics: %v", diags)
	}
}

func TestValidateDefinition_NamespaceRoundTrip(t *testing.T) {
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	def := Definition{
		Datasource: "default",
		Namespace:  "iam",
		Name:       "iam/20260101000000-init",
		Hash:       hash,
		SourceKind: SourceSQLFile,
	}

	diags := ValidateDefinition(&def)
	if diag.HasErrors(diags) {
		t.Fatalf("expected valid definition with namespace, got diagnostics: %v", diags)
	}

	normalized := NormalizeDefinition(def)
	if normalized.Namespace != "iam" {
		t.Fatalf("Namespace must survive normalization, got %q", normalized.Namespace)
	}
}

func TestValidateDefinition_Invalid(t *testing.T) {
	def := Definition{
		Name:       "",
		Hash:       "not-a-hash",
		Reversible: true,
		SourceKind: "unknown",
	}

	diags := ValidateDefinition(&def)
	if !diag.HasErrors(diags) {
		t.Fatal("expected validation errors")
	}
}

func TestValidateDefinitions_Duplicate(t *testing.T) {
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	defs := []Definition{
		{Name: "20260101000000-init", Hash: hash, SourceKind: SourceSQLFile},
		{Name: "20260101000000-init", Hash: hash, SourceKind: SourceRegistry},
	}

	diags := ValidateDefinitions(defs)
	if !diag.HasErrors(diags) {
		t.Fatal("expected duplicate definition error")
	}
}

func TestValidErrorCodes(t *testing.T) {
	required := []string{
		ErrorCodeInvalidDefinition,
		ErrorCodeDuplicateDefinition,
		ErrorCodeInvalidHash,
		ErrorCodeDriftDetected,
		ErrorCodeApplyFailed,
		ErrorCodeRollbackFailed,
		ErrorCodeLockFailed,
		ErrorCodeStateStoreFailed,
		ErrorCodeStartupBlocked,
	}

	for _, code := range required {
		if !ValidErrorCodes[code] {
			t.Fatalf("missing error code %q", code)
		}
	}
}
