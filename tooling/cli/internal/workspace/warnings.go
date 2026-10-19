package workspace

import (
	"io"

	"go.putnami.dev/tooling/cli/internal/iox"
)

// WriteWarnings writes each warning Load recorded on ws to w, one
// "putnami: warning: <message>" line each. A nil ws or a nil w writes nothing:
// a caller passes nil for the quiet mode. Every command that reads the
// workspace reports its warnings through this one function, so the same state
// reads the same on every surface.
func WriteWarnings(w io.Writer, ws *Workspace) {
	if ws == nil || w == nil {
		return
	}
	for _, warning := range ws.Warnings {
		iox.Fprintf(w, "putnami: warning: %s\n", warning)
	}
}
