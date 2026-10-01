package lifecycle

import (
	"io"

	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/output"
)

// installDisplay owns the small amount of chrome around the composed install.
// Non-interactive default mode stays silent until the durable action list;
// interactive mode rewrites one preparation line between phases.
type installDisplay struct {
	out         io.Writer
	display     LifecycleDisplay
	phaseActive bool
}

func newInstallDisplay(env LifecycleEnv) *installDisplay {
	return &installDisplay{out: env.out(), display: env.Display}
}

func (d *installDisplay) phase(name string) {
	if d == nil || !d.display.Interactive || d.display.Verbose || d.display.Debug || d.display.Quiet ||
		!output.ShouldUseLiveRenderer(d.out) {
		return
	}
	iox.Fprintf(d.out, "\r\033[2K⠋ Initializing workspace · %s", name)
	d.phaseActive = true
}

func (d *installDisplay) clear() {
	if d == nil || !d.phaseActive {
		return
	}
	iox.Fprint(d.out, "\r\033[2K")
	d.phaseActive = false
}

func (d *installDisplay) finish(actions []LifecycleAction) {
	d.clear()
	if d == nil || d.display.Quiet {
		return
	}
	for _, action := range actions {
		iox.Fprintf(d.out, "  ✓ %s\n", action.Description)
	}
}
