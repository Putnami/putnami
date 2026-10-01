package migration

import (
	"fmt"
	"regexp"
	"sort"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Migration error codes for strict validation failures.
const (
	ErrorCodeInvalidDefinition   = "migration.invalid_definition"
	ErrorCodeDuplicateDefinition = "migration.duplicate_definition"
	ErrorCodeInvalidHash         = "migration.invalid_hash"
	ErrorCodeDriftDetected       = "migration.drift_detected"
	ErrorCodeApplyFailed         = "migration.apply_failed"
	ErrorCodeRollbackFailed      = "migration.rollback_failed"
	ErrorCodeLockFailed          = "migration.lock_failed"
	ErrorCodeStateStoreFailed    = "migration.state_store_failed"
	ErrorCodeStartupBlocked      = "migration.startup_blocked"

	// Bundle protocol error codes (migration-bundle.v1).
	ErrorCodeInvalidBundle      = "migration.invalid_bundle"
	ErrorCodeInvalidPayload     = "migration.invalid_payload"
	ErrorCodeDigestMismatch     = "migration.digest_mismatch"
	ErrorCodeDuplicateOperation = "migration.duplicate_operation"
)

// ValidErrorCodes enumerates the canonical migration error taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeInvalidDefinition:   true,
	ErrorCodeDuplicateDefinition: true,
	ErrorCodeInvalidHash:         true,
	ErrorCodeDriftDetected:       true,
	ErrorCodeApplyFailed:         true,
	ErrorCodeRollbackFailed:      true,
	ErrorCodeLockFailed:          true,
	ErrorCodeStateStoreFailed:    true,
	ErrorCodeStartupBlocked:      true,
	ErrorCodeInvalidBundle:       true,
	ErrorCodeInvalidPayload:      true,
	ErrorCodeDigestMismatch:      true,
	ErrorCodeDuplicateOperation:  true,
}

var sha256HexPattern = regexp.MustCompile("^[a-f0-9]{64}$")

// NormalizeDefinition applies canonical defaults to one definition.
func NormalizeDefinition(d Definition) Definition {
	if d.Datasource == "" {
		d.Datasource = DefaultDatasource
	}
	if d.OrderKey == "" {
		d.OrderKey = d.Name
	}
	if d.DownHash != "" {
		d.Reversible = true
	}
	if d.SourceKind == "" {
		d.SourceKind = SourceGenerated
	}
	return d
}

// NormalizeDefinitions returns a sorted, normalized copy of defs.
func NormalizeDefinitions(defs []Definition) []Definition {
	out := make([]Definition, len(defs))
	for i, d := range defs {
		out[i] = NormalizeDefinition(d)
	}

	sort.SliceStable(out, func(i, j int) bool {
		left := out[i]
		right := out[j]
		if left.Datasource != right.Datasource {
			return left.Datasource < right.Datasource
		}
		if left.OrderKey != right.OrderKey {
			return left.OrderKey < right.OrderKey
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.Hash != right.Hash {
			return left.Hash < right.Hash
		}
		return left.SourceKind < right.SourceKind
	})

	return out
}

// ValidateDefinition checks structural invariants on a normalized migration definition.
func ValidateDefinition(def *Definition) []diag.Diagnostic {
	if def == nil {
		return []diag.Diagnostic{
			diag.Errorf(ErrorCodeInvalidDefinition, "", "migration definition is nil"),
		}
	}

	d := NormalizeDefinition(*def)
	var diags []diag.Diagnostic

	if d.Name == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDefinition, "name", "migration name is required"))
	}
	if d.OrderKey == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDefinition, "orderKey", "migration orderKey is required"))
	}
	if d.Hash == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidHash, "hash", "migration hash is required"))
	} else if !sha256HexPattern.MatchString(d.Hash) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidHash, "hash", "migration hash must be a lowercase 64-character SHA-256 hex string"))
	}
	if d.DownHash != "" && !sha256HexPattern.MatchString(d.DownHash) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidHash, "downHash", "migration downHash must be a lowercase 64-character SHA-256 hex string"))
	}
	if d.Reversible && d.DownHash == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDefinition, "downHash", "reversible migrations must provide downHash"))
	}
	if !ValidSourceKinds[d.SourceKind] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDefinition, "sourceKind", "unknown sourceKind %q", d.SourceKind))
	}

	return diags
}

// ValidateDefinitions validates definitions and reports duplicates deterministically.
func ValidateDefinitions(defs []Definition) []diag.Diagnostic {
	normalized := NormalizeDefinitions(defs)
	var diags []diag.Diagnostic
	seen := make(map[string]int)

	for i := range normalized {
		d := normalized[i]
		diags = append(diags, ValidateDefinition(&d)...)

		id := CanonicalID(d.Datasource, d.Name)
		if first, ok := seen[id]; ok {
			field := fmt.Sprintf("definitions[%d].name", i)
			diags = append(diags, diag.Errorf(
				ErrorCodeDuplicateDefinition,
				field,
				"duplicate migration definition %q; first seen at normalized index %d",
				id,
				first,
			))
			continue
		}
		seen[id] = i
	}

	return diags
}
