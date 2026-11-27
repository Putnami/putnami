package workspace

import (
	"io"
	"slices"
	"sync"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// WarningReport reports the warnings Load, and the plan of a run, recorded on
// a workspace. Report writes each one as a "putnami: warning: <message>" line
// and keeps it, so a structured command also carries it in its result
// envelope. Every command that
// reads the workspace reports its warnings through a WarningReport, so the same
// state reads the same on every surface.
type WarningReport struct {
	w io.Writer
	// mu guards reported: a run may add a warning from a worker while it
	// renders (Add).
	mu       sync.Mutex
	reported []Warning
}

// NewWarningReport returns a report that writes to w. A nil w writes nothing
// and still keeps every warning: a caller passes nil for the quiet mode.
func NewWarningReport(w io.Writer) *WarningReport {
	return &WarningReport{w: w}
}

// Report writes and keeps each warning recorded on ws, in order, except a
// warning the report already holds: it holds each warning once, so a run that
// reports after its load and again after its plan writes each warning once. A nil report or
// a nil ws does nothing.
func (r *WarningReport) Report(ws *Workspace) {
	if r == nil || ws == nil {
		return
	}
	for _, warning := range ws.Warnings {
		if !r.add(warning) {
			continue
		}
		if r.w != nil {
			iox.Fprintf(r.w, "putnami: warning: %s\n", warning.Message)
		}
	}
}

// Add writes and keeps warning, a warning the run raises after its load,
// unless the report already holds it. A nil report does nothing.
func (r *WarningReport) Add(warning Warning) {
	if r == nil || !r.add(warning) {
		return
	}
	if r.w != nil {
		iox.Fprintf(r.w, "putnami: warning: %s\n", warning.Message)
	}
}

// add keeps warning unless the report already holds it, and reports whether it
// kept it.
func (r *WarningReport) add(warning Warning) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if slices.Contains(r.reported, warning) {
		return false
	}
	r.reported = append(r.reported, warning)
	return true
}

// Warnings returns every reported warning, in order. A nil report has none.
func (r *WarningReport) Warnings() []Warning {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reported)
}

// Envelope returns every reported warning in its result envelope shape, in
// order, or nil when the report holds none: the envelope's workspaceWarnings
// member is absent then, never an empty array.
func (r *WarningReport) Envelope() []protocolcli.WorkspaceWarning {
	reported := r.Warnings()
	if len(reported) == 0 {
		return nil
	}
	warnings := make([]protocolcli.WorkspaceWarning, len(reported))
	for i, warning := range reported {
		warnings[i] = protocolcli.WorkspaceWarning{Code: string(warning.Code), Message: warning.Message}
	}
	return warnings
}
