package cli

// ResultSchemaID is the $id of the result-envelope JSON schema
// (schemas/result.json). It is kept in lock-step with the Result struct by
// the drift test.
const ResultSchemaID = "https://putnami.dev/schemas/putnami-cli-result.json"

// Result.Status values.
const (
	StatusSuccess = "success"
	StatusFailure = "failure"
)

// Result is the VERSION-1 envelope for a command's machine-readable output in
// --output=json mode. It is a READ-ONLY shape: this package can no longer
// BUILD or WRITE one.
//
// The v1 emitters are deleted, and every command
// now writes the versioned ResultV2 (see result_v2_envelope.go); a later
// cleanup deleted the leftover v1 constructor and writer with the rest of the
// bridge, because a writer for a contract nothing writes is how a second output
// contract grows back. The struct and its schema stay: documents produced by
// CLI builds published before the removal carry no protocolVersion member, and
// a consumer that must open one still needs this shape to decode it (the
// TypeScript twin keeps the matching `isResult` guard). See doc/02-result-v2.md
// § Migrating from version 1.
type Result struct {
	// Command is the full command path, e.g. "build" or "cloud status".
	Command string `json:"command"`
	// Status is StatusSuccess or StatusFailure.
	Status string `json:"status"`
	// Data is the command-specific payload (a run summary, a list, …).
	Data any `json:"data,omitempty"`
	// Error describes the failure when Status is StatusFailure.
	Error *ResultError `json:"error,omitempty"`
	// ExitCode is the process exit code the command returns.
	ExitCode int `json:"exitCode"`
}

// ResultError is the error member of a failed Result.
type ResultError struct {
	// Code is the stable error class: "usage" | "auth" | "api" | "failure".
	Code string `json:"code"`
	// Message is the human-readable failure message.
	Message string `json:"message"`
	// Next is the suggested next command or doc link the user can run to
	// recover, or empty.
	Next string `json:"next,omitempty"`
}

// NewResult and WriteResult — the v1 constructor and writer — lived here until
// the v2 cutover. They were unreachable from every emitter once the v1 emitters
// were gone, and are deleted rather than deprecated: keeping a spelling for "emit a
// versionless document" is what lets a second output contract come back one
// command at a time. New code builds NewResultV2 and writes WriteResultV2.
