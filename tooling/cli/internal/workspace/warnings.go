package workspace

import (
	"io"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// WarningReport reports the warnings Load recorded on a workspace. Report
// writes each one as a "putnami: warning: <message>" line and keeps it, so a
// structured command also carries it in its result envelope. Every command that
// reads the workspace reports its warnings through a WarningReport, so the same
// state reads the same on every surface.
type WarningReport struct {
	w        io.Writer
	reported []Warning
}

// NewWarningReport returns a report that writes to w. A nil w writes nothing
// and still keeps every warning: a caller passes nil for the quiet mode.
func NewWarningReport(w io.Writer) *WarningReport {
	return &WarningReport{w: w}
}

// Report writes and keeps each warning Load recorded on ws, in order. A nil
// report or a nil ws does nothing.
func (r *WarningReport) Report(ws *Workspace) {
	if r == nil || ws == nil {
		return
	}
	for _, warning := range ws.Warnings {
		if r.w != nil {
			iox.Fprintf(r.w, "putnami: warning: %s\n", warning.Message)
		}
		r.reported = append(r.reported, warning)
	}
}

// Keep keeps warnings, in order, and writes nothing: their producer already
// wrote them, or was quiet. A caller whose engine run loaded the workspace
// hands the run's warnings to the report of its command this way. A nil report
// does nothing.
func (r *WarningReport) Keep(warnings []Warning) {
	if r == nil {
		return
	}
	r.reported = append(r.reported, warnings...)
}

// Warnings returns every reported warning, in order. A nil report has none.
func (r *WarningReport) Warnings() []Warning {
	if r == nil {
		return nil
	}
	return r.reported
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
