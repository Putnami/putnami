// Package output provides rendering implementations for job execution output.
// Renderers consume job events and produce user-facing output in various formats.
package output

import (
	"io"
	"maps"
	"os"

	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// CoverageReplay is one prior validation measurement made available to the
// human renderer. It is display-only: it never enters JobResult, the canonical
// reduction, task parameters, or cache keys.
type CoverageReplay = workspace_state.ValidationCoverage

// Config holds output rendering configuration.
type Config struct {
	Output    string // "text", "json", "jsonl", "cloud-logging", "" (auto)
	Command   string // the CLI command(s) invoked, for the --output=json envelope
	Verbose   bool
	Debug     bool
	Quiet     bool
	NoColor   bool
	ServeMode bool // enable real-time log tailing for serve commands
	// CoverageReplay is keyed by stable project ID. The live renderer consults
	// it only when the current run produced no coverage for that project.
	CoverageReplay map[string]CoverageReplay

	// Out and Err are the streams the selected renderer writes to. Nil means
	// os.Stdout / os.Stderr, which is every terminal-shaped caller.
	//
	// They let a caller choose the DESTINATION without changing which renderer
	// this constructor selects. The first-use bootstrap runs an implicit
	// `putnami install` whose job rendering must land on stderr under
	// --output=json|jsonl; that used to be achieved by swapping the
	// process-global os.Stdout for the duration of the install.
	Out io.Writer
	Err io.Writer
}

// out returns the renderer's stdout stream: the caller's when it supplied one,
// the process's otherwise.
func (c Config) out() io.Writer {
	if c.Out != nil {
		return c.Out
	}
	return os.Stdout
}

// err returns the renderer's stderr stream: the caller's when it supplied one,
// the process's otherwise.
func (c Config) err() io.Writer {
	if c.Err != nil {
		return c.Err
	}
	return os.Stderr
}

// StructuredOutput reports whether the given --output selector produces a
// machine-readable stream on stdout ("json", "jsonl", or "cloud-logging"). Commands
// that stream human-readable progress must gate it on !StructuredOutput so
// the stdout stream stays a single, uncorrupted terminal object that an agent
// can parse.
//
// It reflects the explicit --output selector only. The auto-detected JSONL
// fallback for non-TTY runs (see autoDetectRenderer) is deliberately excluded
// so this matches the gating long-running commands already rely on.
func StructuredOutput(output string) bool {
	switch output {
	case "json", "jsonl", "cloud-logging":
		return true
	}
	return false
}

// NewRenderer creates the appropriate renderer based on config and environment.
func NewRenderer(cfg Config) jobs.Renderer {
	SetColorsEnabled(!cfg.NoColor)

	// Both stdout machine wires are the version-2 documents, and only those:
	// they shipped behind an opt-in, then became the default, and the v1
	// renderers and the branch that chose between them are gone. There is no
	// environment variable left to select a contract here — see
	// internal/machine.
	switch cfg.Output {
	case "json":
		if machineOutputMode(cfg) == protocolcli.MachineOutputModeNormal {
			return newMachineV2Renderer(cfg.out(), cfg.Command, false)
		}
		return newMachineV2RendererMode(cfg.out(), cfg.Command, false, machineOutputMode(cfg))
	case "jsonl":
		if machineOutputMode(cfg) == protocolcli.MachineOutputModeNormal {
			return newMachineV2Renderer(cfg.out(), cfg.Command, true)
		}
		return newMachineV2RendererMode(cfg.out(), cfg.Command, true, machineOutputMode(cfg))
	case "cloud-logging":
		return NewCloudLoggingRenderer(cfg.out())
	default:
		return autoDetectRenderer(cfg)
	}
}

// NewLifecycleRenderer renders a nested workspace reconciliation without
// claiming stdout's machine protocol. Default human mode uses the live project
// table when possible and removes it on completion; non-interactive mode keeps
// only failure diagnostics. Verbose/debug preserve the ordinary detailed text
// stream. Quiet suppresses completed actions in the outer lifecycle while this
// renderer retains warnings and failures.
func NewLifecycleRenderer(cfg Config, interactive bool) jobs.Renderer {
	terminalOutput := !cfg.NoColor && ShouldUseLiveRenderer(cfg.err())
	SetColorsEnabled(terminalOutput)
	if !cfg.Quiet && cfg.Debug {
		return NewTextRenderer(cfg.out(), cfg.err(), TextRendererConfig{Debug: true, Verbose: true, ServeMode: cfg.ServeMode})
	}
	if !cfg.Quiet && cfg.Verbose {
		return NewTextRenderer(cfg.out(), cfg.err(), TextRendererConfig{Verbose: true, ServeMode: cfg.ServeMode})
	}
	if interactive && !cfg.Quiet && terminalOutput {
		r := NewLiveRenderer(cfg.out(), cfg.err())
		r.lifecycleMode = true
		return r
	}
	return NewTextRenderer(cfg.out(), cfg.err(), TextRendererConfig{Lifecycle: true, ServeMode: cfg.ServeMode})
}

func machineOutputMode(cfg Config) string {
	if cfg.Verbose || cfg.Debug {
		return protocolcli.MachineOutputModeVerbose
	}
	return protocolcli.MachineOutputModeNormal
}

// autoDetectRenderer selects a renderer based on environment:
// - K_SERVICE env var → cloud-logging
// - TTY with live support → live renderer (spinners, progress bars, colors)
// - TTY without live support → text renderer
// - Otherwise → jsonl
func autoDetectRenderer(cfg Config) jobs.Renderer {
	out, errOut := cfg.out(), cfg.err()

	if os.Getenv("K_SERVICE") != "" {
		return NewCloudLoggingRenderer(out)
	}

	if cfg.Quiet {
		return NewTextRenderer(out, errOut, TextRendererConfig{Quiet: true})
	}

	if cfg.Debug {
		return NewTextRenderer(out, errOut, TextRendererConfig{Debug: true, Verbose: true, ServeMode: cfg.ServeMode})
	}

	if cfg.Verbose {
		return NewTextRenderer(out, errOut, TextRendererConfig{Verbose: true, ServeMode: cfg.ServeMode})
	}

	// Use live renderer when TTY supports it (no CI, not dumb terminal)
	if ShouldUseLiveRenderer(errOut) && !cfg.NoColor {
		r := NewLiveRenderer(out, errOut)
		r.coverageReplay = maps.Clone(cfg.CoverageReplay)
		r.serveMode = cfg.ServeMode
		return r
	}

	return NewTextRenderer(out, errOut, TextRendererConfig{ServeMode: cfg.ServeMode})
}
