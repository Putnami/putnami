package migration

import (
	"go.putnami.dev/errors"
	protocolmigration "go.putnami.dev/protocol/migration"
)

// Error codes for the transversal migration package. Where a code maps to
// the cross-language protocol taxonomy in
// go.putnami.dev/protocol/migration, we reuse the canonical string so
// observability tooling can correlate failures across runtimes.
const (
	// CodeInvalidSource is returned when a Source returned from a
	// Contributor is structurally invalid (empty namespace, empty kind).
	CodeInvalidSource errors.Code = "migration.invalid_source"
	// CodeDuplicateRunner is returned when two Runners try to register
	// for the same Kind.
	CodeDuplicateRunner errors.Code = "migration.duplicate_runner"
	// CodeUnknownKind is returned at Apply/Status/Rollback/Verify time
	// when a Source's Kind has no registered Runner.
	CodeUnknownKind errors.Code = "migration.unknown_kind"
	// CodeInfraEmit is returned when the framework-generated infra
	// requirements scratch fragment cannot be written during the generate phase.
	CodeInfraEmit errors.Code = "migration.infra_emit"

	// CodeApplyFailed mirrors protocolmigration.ErrorCodeApplyFailed.
	CodeApplyFailed errors.Code = errors.Code(protocolmigration.ErrorCodeApplyFailed)
	// CodeRollbackFailed mirrors protocolmigration.ErrorCodeRollbackFailed.
	CodeRollbackFailed errors.Code = errors.Code(protocolmigration.ErrorCodeRollbackFailed)
	// CodeDriftDetected mirrors protocolmigration.ErrorCodeDriftDetected.
	CodeDriftDetected errors.Code = errors.Code(protocolmigration.ErrorCodeDriftDetected)
)
