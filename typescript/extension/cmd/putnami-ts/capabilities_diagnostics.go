package main

import (
	"regexp"
	"strings"

	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/errs"
)

var capabilityDiagnosticPattern = regexp.MustCompile(`\[((?:capabilities\.(?:parse_error|unknown_field|invalid_protocol_version|missing_project|invalid_name|missing_provenance|invalid_source_kind|invalid_schema_kind|invalid_discoverer_kind|invalid_infra_kind|invalid_probe|invalid_phase|invalid_capability_kind|missing_requires|missing_required_provider|duplicate_provider|conflicting_provider))|(?:http_routes\.[a-z_]+))\]\s+([^:\r\n]+):\s*([^\r\n]+)`)

// capabilityDiagnostic is one promoted validation frame: the protocol-owned
// code plus the formatted message. Kept separate from emission so the batch
// build path (which has no per-project emitter) can reconstruct the same
// diagnostics from a failed generate's error output.
type capabilityDiagnostic struct {
	Code    string
	Message string
}

// capabilityDiagnostics extracts the promoted validation frames from generator
// output. It is the pure computation shared by emitCapabilityDiagnostics (solo)
// and the batch build handlers (which return diagnostics instead of emitting).
func capabilityDiagnostics(output string) []capabilityDiagnostic {
	matches := capabilityDiagnosticPattern.FindAllStringSubmatch(output, -1)
	diags := make([]capabilityDiagnostic, 0, len(matches))
	for _, match := range matches {
		field := strings.TrimSpace(match[2])
		message := strings.TrimSpace(match[3])
		if field != "" {
			message = field + ": " + message
		}
		diags = append(diags, capabilityDiagnostic{Code: match[1], Message: message})
	}
	return diags
}

// emitCapabilityDiagnostics promotes validation frames emitted by framework
// generators into runtime diagnostic events with the protocol-owned code, so
// a failed generate is actionable in both text and JSONL CLI output.
func emitCapabilityDiagnostics(emit *jsonl.Emitter, output string) bool {
	diags := capabilityDiagnostics(output)
	for _, d := range diags {
		emit.DiagnosticWithCode("error", d.Message, "", 0, 0, d.Code)
	}
	return len(diags) > 0
}

// generateDiagnostics is the diagnostic set for a failed generate phase: the
// promoted capability/http-route frames when the error carries any, otherwise a
// single frame built from the error itself.
//
// The fallback is not cosmetic. Only errors matching capabilityDiagnosticPattern
// were ever promoted, so every other generate failure reached the jsonl consumer
// as a bare `Job FAILED` with `data: null` — the failure text existed only in the
// human-readable text renderer. That is how a missing capability manifest stayed unexplained across
// dozens of hosted CI runs. A generate failure now always carries at least one
// diagnostic, on both the solo and the batch path.
func generateDiagnostics(err error) []capabilityDiagnostic {
	if err == nil {
		return nil
	}
	if diags := capabilityDiagnostics(err.Error()); len(diags) > 0 {
		return diags
	}
	code := errs.CodeOf(err)
	if code == errs.CodeUnknown {
		code = errs.CodeGenerateFailed
	}
	return []capabilityDiagnostic{{Code: code.String(), Message: err.Error()}}
}
