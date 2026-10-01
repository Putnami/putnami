// The contract compiler.
//
// ContractsGenerate validates a project's authored contract manifest
// (schema/contracts.json — the canonical IR, the single authored source, the
// same opt-in model clientgen uses for an OpenAPI input) and lowers it into the
// deterministic committed artifacts: the re-canonicalized IR, the Go type twin
// (protocols/contracts.EmitGo), and the JSON Schema over the DTO vocabulary
// (protocols/contracts.MarshalJSONSchema). Generation is atomic: every artifact
// is computed in memory before any file is touched, so a validation or emit
// failure never overwrites a good committed artifact.
//
// ContractsCheck re-runs that generation without writing and diffs
// the result against the committed artifacts (drift → exit 2), then runs the
// net-new semantic-compatibility pass: it loads the git-committed prior IR and
// classifies renamed or removed enum values, scopes, claims, and grants as
// breaking changes (→ exit 2). The machine-readable report travels in the
// structured Result envelope's data.
//
// The TypeScript twin is intentionally not emitted here: it is a pure
// deterministic function of the canonical IR (application/src/contracts
// emitTypeScript, golden-pinned), so verifying the IR is fresh transitively
// guarantees the TS types are current. Emitting the TS bytes requires a
// non-hermetic bun subprocess that would undermine this command's determinism
// and atomicity, so the on-disk TS emission is left to the language-native bun
// path (the same Go-task/bun-task split clientgen uses).
package sdd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	protocolcli "go.putnami.dev/protocol/cli"
	contracts "go.putnami.dev/protocol/contracts"
	diag "go.putnami.dev/protocol/diagnostic"
)

// Committed and staged contract artifact paths, all project-relative. The
// authored source and the canonical IR are the same file: `generate`
// re-canonicalizes the authored manifest in place, so drift on the IR means the
// committed source is not in canonical form.
const (
	// ContractsSourcePath is the authored manifest a project commits to opt in;
	// it is the protocol's committed IR path (schema/contracts.json).
	ContractsSourcePath = contracts.CommittedPath
	// contractsGoPath is the generated Go type twin.
	contractsGoPath = "schema/contracts.gen.go"
	// contractsSchemaPath is the generated JSON Schema over the DTO vocabulary.
	contractsSchemaPath = "schema/contracts.schema.json"
	// contractsMarkdownPath is generated reference documentation over the
	// canonical contract vocabulary.
	contractsMarkdownPath = "schema/contracts.md"
)

// Check outcome values (also the CheckReport.Outcome field).
const (
	OutcomeClean    = "clean"
	OutcomeDrift    = "drift"
	OutcomeBreaking = "breaking"
)

// Compatibility change categories and kinds.
const (
	compatEnumValue = "enumValue"
	compatScope     = "scope"
	compatClaim     = "claim"
	compatGrant     = "grant"

	compatRemoved = "removed"
	compatRenamed = "renamed"
)

// GenerateReport is the machine-readable result of `contracts generate`.
type GenerateReport struct {
	// Project is the workspace-relative project path the manifest belongs to.
	Project string `json:"project"`
	// Manifest is the contract identity from the manifest name field.
	Manifest string `json:"manifest"`
	// Artifacts lists the project-relative paths written, in generation order.
	Artifacts []string `json:"artifacts"`
}

// CheckReport is the machine-readable result of `contracts check`.
type CheckReport struct {
	// Project is the workspace-relative project path the manifest belongs to.
	Project string `json:"project"`
	// Manifest is the contract identity from the manifest name field.
	Manifest string `json:"manifest"`
	// Outcome is OutcomeClean, OutcomeDrift, or OutcomeBreaking. Breaking takes
	// precedence over drift when both are present.
	Outcome string `json:"outcome"`
	// Drift lists artifacts whose committed bytes differ from a fresh
	// generation, in generation order.
	Drift []DriftArtifact `json:"drift,omitempty"`
	// BreakingChanges lists backwards-incompatible changes from the committed
	// prior IR to the current IR, in prior source order.
	BreakingChanges []CompatChange `json:"breakingChanges,omitempty"`
}

// DriftArtifact records one committed artifact that no longer matches a fresh
// generation from the current manifest.
type DriftArtifact struct {
	// Path is the project-relative artifact path.
	Path string `json:"path"`
	// Reason is "stale" (bytes differ) or "missing" (never generated).
	Reason string `json:"reason"`
}

// CompatChange is one classified backwards-incompatible change between the
// committed prior IR and the current IR.
type CompatChange struct {
	// Category is compatEnumValue, compatScope, compatClaim, or compatGrant.
	Category string `json:"category"`
	// Kind is compatRemoved (identity gone from current — covers a rename of the
	// identity) or compatRenamed (an enum value's wire representation changed).
	Kind string `json:"kind"`
	// Name is the affected identity, e.g. "Currency.USD" or "payments:write".
	Name string `json:"name"`
	// Detail is a human-facing explanation of the change.
	Detail string `json:"detail,omitempty"`
}

// PriorLoader returns the committed prior manifest bytes for the semantic
// compatibility pass, or (nil, nil) when there is no prior (a new contract, or
// no VCS history). An error aborts the check.
type PriorLoader func() ([]byte, error)

// genArtifact is one generated committed artifact: its project-relative path
// and the exact bytes a fresh generation produces. It is the single generation
// unit shared by generate (which writes it) and check (which compares it), so
// the two can never disagree about what "current" means.
type genArtifact struct {
	path  string
	bytes []byte
}

// canonicalIR renders m in the canonical wire form: json.MarshalIndent with
// two-space indentation plus a trailing newline. This is byte-identical to the
// form protocols/contracts pins in its determinism golden and the TS twin
// reproduces (see protocols/contracts/determinism_test.go).
func canonicalIR(m *contracts.Manifest) ([]byte, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// generateArtifacts lowers m into the full, deterministic set of committed
// artifacts. It is the one generation path both generate and check call.
func generateArtifacts(m *contracts.Manifest) ([]genArtifact, error) {
	ir, err := canonicalIR(m)
	if err != nil {
		return nil, fmt.Errorf("canonicalize contract IR: %w", err)
	}
	goSrc, err := contracts.EmitGo(m)
	if err != nil {
		return nil, fmt.Errorf("emit Go types: %w", err)
	}
	schema, err := contracts.MarshalJSONSchema(m)
	if err != nil {
		return nil, fmt.Errorf("render JSON Schema: %w", err)
	}
	return []genArtifact{
		{path: ContractsSourcePath, bytes: ir},
		{path: contractsGoPath, bytes: []byte(goSrc)},
		{path: contractsSchemaPath, bytes: schema},
		{path: contractsMarkdownPath, bytes: contracts.RenderMarkdown(m)},
	}, nil
}

// loadManifest reads and validates the authored manifest at projectDir. A
// missing manifest and a failed validation both return an exit-2 error so the
// caller never proceeds to write on a bad input.
func loadManifest(projectDir string) (*contracts.Manifest, error) {
	srcPath := filepath.Join(projectDir, ContractsSourcePath)
	data, err := os.ReadFile(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, protocolcli.Classify(
				fmt.Errorf("no contract manifest at %s; author it to opt in to the contract compiler", ContractsSourcePath),
				protocolcli.ErrNotFound)
		}
		return nil, fmt.Errorf("read contract manifest: %w", err)
	}
	m, diags := contracts.ParseAndValidateManifest(data)
	if m == nil || diag.HasErrors(diags) {
		return nil, invalidManifestError(diags)
	}
	return m, nil
}

// invalidManifestError classifies a validation-failure diagnostic set as an
// exit-2 configuration error, summarizing the first finding.
func invalidManifestError(diags []diag.Diagnostic) error {
	errs := diag.Errors(diags)
	msg := "contract manifest is invalid"
	if len(errs) > 0 {
		msg = fmt.Sprintf("contract manifest is invalid: %s", errs[0].String())
	}
	return protocolcli.Classify(errors.New(msg), protocolcli.ErrInvalidConfig)
}

// ContractsGenerate validates the authored manifest at projectDir and emits the
// committed artifacts, staging each under .gen/schema/ before atomically
// promoting it into the tracked schema/ tree. All artifact bytes are computed
// before any write, so a validation or emit failure leaves the committed
// artifacts untouched. label is the project identity recorded in the report.
func ContractsGenerate(projectDir, label string) (GenerateReport, error) {
	m, err := loadManifest(projectDir)
	if err != nil {
		return GenerateReport{}, err
	}
	arts, err := generateArtifacts(m)
	if err != nil {
		return GenerateReport{}, err
	}

	// Stage under .gen/schema/ (the ephemeral, gitignored tree the codegen
	// committer promotes from); a partial staging tree is harmless.
	for _, a := range arts {
		staged := filepath.Join(projectDir, contracts.EmitDir, filepath.Base(a.path))
		if err := atomicWriteFile(staged, a.bytes); err != nil {
			return GenerateReport{}, err
		}
	}

	// Promote into the tracked schema/ tree as a set: stage every artifact's
	// bytes to a temp file first — where a write error (ENOSPC, EACCES) surfaces
	// — then rename them all back-to-back, so the common failure mode can no
	// longer leave the committed set half-updated. (A rename failing mid-loop is
	// the only remaining partial-update window; same-dir rename(2) after a
	// successful stage effectively fails only under catastrophic conditions.)
	temps := make([]string, 0, len(arts))
	defer func() {
		for _, t := range temps {
			os.Remove(t) // a no-op once the temp has been renamed into place
		}
	}()
	for _, a := range arts {
		tmp, err := stageTemp(filepath.Join(projectDir, a.path), a.bytes)
		if err != nil {
			return GenerateReport{}, err
		}
		temps = append(temps, tmp)
	}
	paths := make([]string, 0, len(arts))
	for i, a := range arts {
		if err := commitTemp(temps[i], filepath.Join(projectDir, a.path)); err != nil {
			return GenerateReport{}, err
		}
		paths = append(paths, a.path)
	}
	return GenerateReport{Project: label, Manifest: m.Name, Artifacts: paths}, nil
}

// ContractsCheck regenerates the artifacts in memory and diffs them against the
// committed bytes (drift), then runs the semantic-compatibility pass against the
// prior IR that loadPrior yields (breaking changes). It writes nothing. The
// returned error is nil on a clean result and an exit-2 configuration error on
// drift or a breaking change, with the CheckReport attached for the structured
// failure envelope. label is the project identity recorded in the report.
func ContractsCheck(projectDir, label string, loadPrior PriorLoader) (CheckReport, error) {
	current, err := loadManifest(projectDir)
	if err != nil {
		return CheckReport{}, err
	}
	arts, err := generateArtifacts(current)
	if err != nil {
		return CheckReport{}, err
	}

	drift, err := detectDrift(projectDir, arts)
	if err != nil {
		return CheckReport{}, err
	}

	breaking, err := detectBreaking(loadPrior, current)
	if err != nil {
		return CheckReport{}, err
	}

	report := CheckReport{
		Project:         label,
		Manifest:        current.Name,
		Outcome:         outcomeFor(drift, breaking),
		Drift:           drift,
		BreakingChanges: breaking,
	}
	return report, outcomeError(report)
}

// detectDrift compares each freshly generated artifact against its committed
// bytes, in generation order. A missing committed artifact is drift.
func detectDrift(projectDir string, arts []genArtifact) ([]DriftArtifact, error) {
	var drift []DriftArtifact
	for _, a := range arts {
		committed, err := os.ReadFile(filepath.Join(projectDir, a.path))
		if err != nil {
			if os.IsNotExist(err) {
				drift = append(drift, DriftArtifact{Path: a.path, Reason: "missing"})
				continue
			}
			return nil, fmt.Errorf("read committed artifact %s: %w", a.path, err)
		}
		if !bytes.Equal(committed, a.bytes) {
			drift = append(drift, DriftArtifact{Path: a.path, Reason: "stale"})
		}
	}
	return drift, nil
}

// detectBreaking loads the prior IR and classifies breaking changes against
// current. No prior (a new contract or no VCS history) yields no changes; a
// prior that fails to parse is skipped rather than fatal, since it predates the
// current shape and cannot be meaningfully compared.
func detectBreaking(loadPrior PriorLoader, current *contracts.Manifest) ([]CompatChange, error) {
	if loadPrior == nil {
		return nil, nil
	}
	priorBytes, err := loadPrior()
	if err != nil {
		return nil, err
	}
	if len(priorBytes) == 0 {
		return nil, nil
	}
	prior, _ := contracts.ParseManifest(priorBytes)
	if prior == nil {
		return nil, nil
	}
	return CompareManifests(prior, current), nil
}

// CompareManifests classifies the backwards-incompatible changes from prior to
// current: an enum value, scope, claim, or grant that is present in prior but
// gone from current is a breaking removal (a rename manifests as the old
// identity disappearing), and an enum value whose wire representation changed
// under the same constant name is a breaking rename. Additions are compatible
// and are not reported. Output is deterministic: prior is walked in source
// order and current is consulted only through lookup maps.
//
// This slice classifies only removals/renames of the four identity vocabularies.
// Modification-in-place — a claim narrowing from optional to required or changing
// its type, or a grant repointing its capability — is also backwards-incompatible
// but is not yet detected here; that coverage is deferred to the
// identity-vocabulary adoption slice.
func CompareManifests(prior, current *contracts.Manifest) []CompatChange {
	if prior == nil || current == nil {
		return nil
	}
	var changes []CompatChange

	// Enum values: keyed by enum name then value (constant) name.
	curEnums := make(map[string]map[string]string, len(current.Enums))
	for _, e := range current.Enums {
		vals := make(map[string]string, len(e.Values))
		for _, v := range e.Values {
			vals[v.Name] = v.Value
		}
		curEnums[e.Name] = vals
	}
	for _, e := range prior.Enums {
		curVals, enumPresent := curEnums[e.Name]
		for _, v := range e.Values {
			id := e.Name + "." + v.Name
			if !enumPresent {
				changes = append(changes, CompatChange{
					Category: compatEnumValue, Kind: compatRemoved, Name: id,
					Detail: fmt.Sprintf("enum %q was removed or renamed", e.Name),
				})
				continue
			}
			curWire, valuePresent := curVals[v.Name]
			if !valuePresent {
				changes = append(changes, CompatChange{
					Category: compatEnumValue, Kind: compatRemoved, Name: id,
					Detail: fmt.Sprintf("enum value %q was removed or renamed", id),
				})
				continue
			}
			if curWire != v.Value {
				changes = append(changes, CompatChange{
					Category: compatEnumValue, Kind: compatRenamed, Name: id,
					Detail: fmt.Sprintf("enum value %q wire value changed from %q to %q", id, v.Value, curWire),
				})
			}
		}
	}

	// Scopes, claims, grants: keyed by name; a missing prior identity is a
	// breaking removal (or rename).
	scopeName := func(s contracts.Scope) string { return s.Name }
	claimName := func(c contracts.Claim) string { return c.Name }
	grantName := func(g contracts.Grant) string { return g.Name }
	changes = appendRemovals(changes, compatScope, names(prior.Scopes, scopeName), nameSet(current.Scopes, scopeName))
	changes = appendRemovals(changes, compatClaim, names(prior.Claims, claimName), nameSet(current.Claims, claimName))
	changes = appendRemovals(changes, compatGrant, names(prior.Grants, grantName), nameSet(current.Grants, grantName))
	return changes
}

// appendRemovals reports each prior identity (in order) that is absent from
// current as a breaking removal in the given category.
func appendRemovals(changes []CompatChange, category string, priorNames []string, current map[string]bool) []CompatChange {
	for _, name := range priorNames {
		if !current[name] {
			changes = append(changes, CompatChange{
				Category: category, Kind: compatRemoved, Name: name,
				Detail: fmt.Sprintf("%s %q was removed or renamed", category, name),
			})
		}
	}
	return changes
}

// names projects items to their identity strings, preserving order.
func names[T any](items []T, name func(T) string) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, name(it))
	}
	return out
}

// nameSet is the set of identity strings in items, for presence lookups.
func nameSet[T any](items []T, name func(T) string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, it := range items {
		out[name(it)] = true
	}
	return out
}

// outcomeFor derives the report outcome. A breaking change is the strongest
// signal and takes precedence over drift; both still return exit 2.
func outcomeFor(drift []DriftArtifact, breaking []CompatChange) string {
	switch {
	case len(breaking) > 0:
		return OutcomeBreaking
	case len(drift) > 0:
		return OutcomeDrift
	default:
		return OutcomeClean
	}
}

// outcomeError maps a non-clean report to an exit-2 configuration error with
// the report attached for the structured failure envelope. A clean report is a
// nil error (exit 0).
func outcomeError(r CheckReport) error {
	switch r.Outcome {
	case OutcomeBreaking:
		err := protocolcli.Classify(
			fmt.Errorf("contracts check found %d breaking change(s) since the committed contract", len(r.BreakingChanges)),
			protocolcli.ErrInvalidConfig)
		return WithResultData(err, r)
	case OutcomeDrift:
		err := protocolcli.Classify(
			fmt.Errorf("contracts check found %d drifted artifact(s); run `putnami contracts generate`", len(r.Drift)),
			protocolcli.ErrInvalidConfig)
		return WithResultData(err, r)
	default:
		return nil
	}
}

// GitPriorManifest returns a PriorLoader that reads the committed manifest at
// HEAD via `git show`. When git runs and reports the ref or path is absent (a
// path not at HEAD, a repo with no commits, or not a git repository) it yields
// (nil, nil) so the check treats it as "no prior" and reports no breaking
// changes. But when git cannot be run at all (binary missing, not executable) it
// returns the error, since silently treating an unrunnable baseline as "no
// breaking changes" would let a breaking change pass the CI gate unseen.
func GitPriorManifest(repoRoot, projectRel string) PriorLoader {
	return func() ([]byte, error) {
		rel := filepath.ToSlash(filepath.Join(projectRel, ContractsSourcePath))
		out, err := exec.Command("git", "-C", repoRoot, "show", "HEAD:"+rel).Output()
		if err != nil {
			// git ran and exited non-zero: the ref/path is absent or this is not
			// a git repo — a legitimate "no baseline", not a failure.
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				return nil, nil
			}
			// git could not be run at all: we cannot establish a baseline, so
			// surface it rather than passing a check that may hide breaking
			// changes.
			return nil, fmt.Errorf("load prior contract via git: %w", err)
		}
		return out, nil
	}
}

// atomicWriteFile writes data to path via a same-directory temp file and
// rename(2), so a reader never observes a partially written artifact and a
// failed write never truncates the existing good file.
//
// Copied from `shared.AtomicWriteFile`. Only the staging tree uses it — the
// tracked tree is promoted through stageTemp/commitTemp below, which splits the
// same two steps so a multi-artifact generation cannot be half-applied.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create artifact directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".contracts-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp artifact in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the temp has been renamed into place
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp artifact: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod temp artifact: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("promote artifact to %s: %w", path, err)
	}
	return nil
}

// stageTemp writes data to a same-directory temp file next to path and returns
// the temp file's name, ready for commitTemp to rename into place. Splitting the
// write from the rename lets a caller stage every artifact first — where a write
// error (ENOSPC, EACCES) surfaces — before promoting any, so a multi-artifact
// generation cannot be left half-applied by a mid-write failure. On any error
// the temp file is removed before returning.
func stageTemp(path string, data []byte) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create artifact directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".contracts-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp artifact in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("write temp artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("close temp artifact: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("chmod temp artifact: %w", err)
	}
	return tmpName, nil
}

// commitTemp renames a staged temp file over path via rename(2), so a reader
// never observes a partially written artifact and a failed write never truncates
// the existing good file.
func commitTemp(tmpName, path string) error {
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("promote artifact to %s: %w", path, err)
	}
	return nil
}
