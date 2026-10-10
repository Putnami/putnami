// Package clicore is the shared toolkit for the @putnami/cloud CLI extension.
//
// It holds the cross-cutting infrastructure every command needs — the IO
// contract, the exit-coded error type, the control-plane HTTP client, flag and
// value accessors, the unified User-Agent, and small shared helpers — so that
// command implementations in the per-domain packages (internal/<domain>cli)
// can depend on one neutral package instead of reaching into internal/cloudcli.
// It links the framework client runtime: commands call
// Cloud providers through their generated Go clients, bound by services.go.
package clicore

import (
	"errors"

	protocolcli "go.putnami.dev/protocol/cli"
)

// Default Putnami Cloud endpoints and the workspace-relative paths of the
// credential and link files the CLI reads and writes.
const (
	DefaultAuthURL         = "https://auth.putnami.cloud"
	DefaultControlPlaneURL = "https://api.putnami.cloud"
	AuthFileRelative       = ".putnami/auth.json"
	LinkFileRelative       = ".putnami/cloud-link.json"
)

// ExitSuccess and related constants alias the shared protocol CLI exit-code
// taxonomy for existing clicore callers.
const (
	ExitSuccess = protocolcli.ExitSuccess
	ExitFailure = protocolcli.ExitFailure
	ExitUsage   = protocolcli.ExitUsage
	ExitAuth    = protocolcli.ExitAuth
	ExitAPI     = protocolcli.ExitAPI
	ExitSignal  = protocolcli.ExitSignal
)

// ExitError carries a process exit code alongside the user-facing message. It
// unwraps to the shared protocol CLI error class so ExitCode can delegate to
// protocolcli.ExitCodeForError while legacy callers can still inspect Code.
type ExitError struct {
	Message string
	Code    int
	class   error
}

func (e *ExitError) Error() string { return e.Message }

func (e *ExitError) Unwrap() error { return e.class }

// NewError builds an ExitError with the given message and exit code.
func NewError(message string, code int) error {
	return newExitError(message, code)
}

// ExitCode maps err through the shared Putnami CLI error taxonomy.
func ExitCode(err error) int {
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	return protocolcli.ExitCodeForError(err)
}

func newExitError(message string, code int) *ExitError {
	return &ExitError{Message: message, Code: code, class: classForExitCode(code)}
}

func classForExitCode(code int) error {
	switch code {
	case ExitUsage:
		return protocolcli.ErrUsage
	case ExitAuth:
		return protocolcli.ErrAuth
	case ExitAPI:
		return protocolcli.ErrAPI
	default:
		return nil
	}
}
