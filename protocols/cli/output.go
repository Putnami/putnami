package cli

import (
	"fmt"
	"strings"
)

// OutputMode selects how a command renders its result.
type OutputMode string

const (
	// OutputAuto defers the choice to environment detection (TTY, K_SERVICE).
	OutputAuto OutputMode = ""
	// OutputText is human-readable output (spinners/progress on a TTY).
	OutputText OutputMode = "text"
	// OutputJSON is a single aggregated JSON object, written once.
	OutputJSON OutputMode = "json"
	// OutputJSONL is a stream of line-delimited JSON events.
	OutputJSONL OutputMode = "jsonl"
	// OutputCloudLogging is structured JSON for Google Cloud Logging.
	OutputCloudLogging OutputMode = "cloud-logging"
)

// IsStructured reports whether the mode is machine-readable JSON (json or
// jsonl). Commands that emit a single result object treat both identically;
// only streaming job commands distinguish the aggregated object (json) from
// the event stream (jsonl).
func (m OutputMode) IsStructured() bool {
	return m == OutputJSON || m == OutputJSONL
}

// Valid reports whether m is a known output mode. OutputAuto (empty) is valid.
func (m OutputMode) Valid() bool {
	switch m {
	case OutputAuto, OutputText, OutputJSON, OutputJSONL, OutputCloudLogging:
		return true
	}
	return false
}

// ResolveOutputMode combines the raw --output value with the --json boolean
// shorthand into a single OutputMode.
//
// --json is an alias for --output=json. Passing --json together with an
// explicit --output that selects a different mode is a conflict and returns an
// error; --json with --output=json (or without --output) is accepted. An
// unrecognized --output value is a usage error.
func ResolveOutputMode(rawOutput string, jsonFlag bool) (OutputMode, error) {
	mode := OutputMode(strings.TrimSpace(rawOutput))
	if !mode.Valid() {
		return OutputAuto, fmt.Errorf("unknown output mode %q (want text, json, jsonl, or cloud-logging)", rawOutput)
	}
	if jsonFlag {
		if mode != OutputAuto && mode != OutputJSON {
			return OutputAuto, fmt.Errorf("--json conflicts with --output=%s", mode)
		}
		return OutputJSON, nil
	}
	return mode, nil
}
