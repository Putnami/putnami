package workspace

import (
	"fmt"
	"sort"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// The visibility check.
//
// A project declares who may import it (`visibility` in its putnami.json), and
// this is where the workspace's resolved graph is held to those declarations.
// It runs at the end of every synchronization, which is the first moment the
// graph is REAL: the providers have answered, the merged view is adopted, and
// each edge carries the manifest family it was derived from. Running it on the
// authored configuration alone would check a graph nothing builds — a declared
// `dependencies` entry with no import behind it is a boundary crossing that
// never happens, and an import a putnami.json never mentions is one that does.
//
// It answers with DIAGNOSTICS rather than with a verdict, and the mode decides
// what they mean. Only `enforce` refuses, and it refuses through the same
// typed failure a probe failure travels on, so the policy that keeps a broken
// workspace repairable (RequireProbe: every graph-dependent command fails, the
// recovery commands stay reachable) applies unchanged.

// VisibilityMode is the severity switch for the visibility check, and it is the
// vocabulary the workspace's other verification policies already use: `off`
// skips the check, `report` publishes findings as warnings without changing an
// exit code, `enforce` refuses the graph.
type VisibilityMode string

// Visibility modes.
const (
	// VisibilityModeOff does not evaluate the check at all.
	VisibilityModeOff VisibilityMode = "off"
	// VisibilityModeReport publishes findings as warnings. The run continues.
	VisibilityModeReport VisibilityMode = "report"
	// VisibilityModeEnforce refuses a graph that carries a violation.
	VisibilityModeEnforce VisibilityMode = "enforce"
)

// DefaultVisibilityMode is the mode of a workspace that declares none.
//
// It is `enforce`: a boundary a project declared holds unless the workspace
// declares `report` or `off`.
const DefaultVisibilityMode = VisibilityModeEnforce

// visibilityOptionNamespace and visibilityOptionKey locate the switch in the
// workspace document: `options.workspace.visibility`. The namespace is core's
// own — the check reads the workspace graph, which no extension owns.
const (
	visibilityOptionNamespace = "workspace"
	visibilityOptionKey       = "visibility"
)

// VisibilityDiagnosticCode is the stable machine-readable reason attached to
// every finding, so a consumer branches on it instead of matching prose.
const VisibilityDiagnosticCode = "workspace.visibility_violation"

// VisibilityModeInvalidCode is the reason attached to the warning about an
// unreadable switch. It is not a finding: it names no import.
const VisibilityModeInvalidCode = "workspace.visibility_mode_invalid"

// ValidVisibilityModes is the closed set, in severity order.
var ValidVisibilityModes = []VisibilityMode{VisibilityModeOff, VisibilityModeReport, VisibilityModeEnforce}

// Valid reports whether m is a member of the closed set.
func (m VisibilityMode) Valid() bool {
	for _, known := range ValidVisibilityModes {
		if m == known {
			return true
		}
	}
	return false
}

// ResolveVisibilityMode reads the switch from the workspace document.
//
// A document that declares no switch resolves to DefaultVisibilityMode. An
// unknown or non-string value returns DefaultVisibilityMode and an error that
// names the value, so the caller reports the typo instead of resolving it
// silently.
func ResolveVisibilityMode(cfg *wsproto.Config) (VisibilityMode, error) {
	if cfg == nil {
		return DefaultVisibilityMode, nil
	}
	raw, ok := cfg.Options[visibilityOptionNamespace][visibilityOptionKey]
	if !ok || raw == nil {
		return DefaultVisibilityMode, nil
	}
	text, ok := raw.(string)
	if !ok {
		return DefaultVisibilityMode, fmt.Errorf(
			"options.%s.%s must be a string (one of %s)",
			visibilityOptionNamespace, visibilityOptionKey, visibilityModeNames())
	}
	mode := VisibilityMode(text)
	if !mode.Valid() {
		return DefaultVisibilityMode, fmt.Errorf(
			"unknown options.%s.%s %q (want one of %s)",
			visibilityOptionNamespace, visibilityOptionKey, text, visibilityModeNames())
	}
	return mode, nil
}

func visibilityModeNames() string {
	names := make([]string, 0, len(ValidVisibilityModes))
	for _, mode := range ValidVisibilityModes {
		names = append(names, string(mode))
	}
	sort.Strings(names)
	return fmt.Sprintf("%v", names)
}

// VisibilityFindings evaluates the check over the workspace's resolved graph
// and renders one diagnostic per violated edge, in the model's own
// (importer, imported) order.
//
// The severity is the mode's: `enforce` produces errors, anything else
// warnings. `off` produces nothing — the check is not evaluated, rather than
// evaluated and muted, so a workspace that opted out pays nothing for it.
func VisibilityFindings(ws *Workspace, mode VisibilityMode) []diag.Diagnostic {
	if ws == nil || mode == VisibilityModeOff {
		return nil
	}
	violations := VisibilityViolations(ws)
	if len(violations) == 0 {
		return nil
	}
	findings := make([]diag.Diagnostic, 0, len(violations))
	for _, violation := range violations {
		message := fmt.Sprintf(
			"%s imports %s across a scope boundary: %s is in scope %s, %s is in scope %s, and %s declares "+
				"visibility %q — mark it %q, move it into the importer's scope, or drop the %s import",
			violation.Importer, violation.Imported,
			violation.Importer, ScopeLabel(violation.ImporterScope),
			violation.Imported, ScopeLabel(violation.ImportedScope),
			violation.Imported, wsproto.DefaultVisibility, wsproto.VisibilityPublic, violation.Source)
		if mode == VisibilityModeEnforce {
			findings = append(findings, diag.Errorf(VisibilityDiagnosticCode, violation.Importer, "%s", message))
			continue
		}
		findings = append(findings, diag.Warningf(VisibilityDiagnosticCode, violation.Importer, "%s", message))
	}
	return findings
}

// visibilityRefusal is the enforced verdict: the graph is not one this
// workspace permits, stated on the typed channel graph-dependent commands
// already fail on. declared tells whether the workspace document chose
// `enforce` itself or the default applied. notes are attached ahead of the
// findings and are not counted as imports.
func visibilityRefusal(findings []diag.Diagnostic, declared bool, notes ...diag.Diagnostic) *wsproto.ProbeFailure {
	origin := "the default"
	if declared {
		origin = "declared in " + wsproto.WorkspaceConfigFilename
	}
	failure := wsproto.NewProbeFailure(wsproto.ProbeFailureVisibility, "",
		"%d import(s) cross a scope boundary the imported project did not open "+
			"(options.%s.%s is %q, %s); mark each imported project %q, move it into the importer's scope, "+
			"or remove the import — declare options.%s.%s %q to see them as warnings while you fix them",
		len(findings), visibilityOptionNamespace, visibilityOptionKey, VisibilityModeEnforce, origin,
		wsproto.VisibilityPublic, visibilityOptionNamespace, visibilityOptionKey, VisibilityModeReport)
	failure.Diagnostics = append(failure.Diagnostics, notes...)
	failure.Diagnostics = append(failure.Diagnostics, findings...)
	return failure
}

// declaresVisibilityMode reports whether the workspace document declares a
// readable switch.
func declaresVisibilityMode(cfg *wsproto.Config) bool {
	if cfg == nil {
		return false
	}
	raw, ok := cfg.Options[visibilityOptionNamespace][visibilityOptionKey]
	if !ok || raw == nil {
		return false
	}
	_, err := ResolveVisibilityMode(cfg)
	return err == nil
}
