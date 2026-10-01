package features

import (
	"fmt"
	"path"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// The committed rollout ratchet of the executable-spec gate.
//
// specs.baseline.json records the enforced floor: which projects have
// committed to options.sdd.verification.specs = "enforce", and which
// executable requirements each one covers. Each enforced project commits its
// own file in its own directory (ProjectSpecsBaseline, protocol version 2):
// the directory names the project and the file's presence states enforce, so
// the file holds only the covered requirements, and two changes to two
// projects never edit the same file. The floor is the union of those files
// (SpecsBaseline). Version 1 is that union written as one workspace-root
// file; readers still accept it there so a workspace can move off it.
// The file is the reviewed policy artifact the epic requires — an enforced
// project may not regress to report/off, and may not lose
// executable-requirement coverage, without an explicit change to its file in
// the same diff. Growth is always legal without touching it: executable
// requirements may increase freely, and the baseline is raised deliberately,
// never by side effect.
//
// The comparison here is pure and worktree-only on both sides: the caller
// derives the CURRENT floor from the committed manifests and options it
// already loaded, and no git history enters the judgment — which is what
// keeps the consuming validation task honestly cacheable (the same reasoning
// as the architecture engine's worktree-only DAG verdict).

const (
	// SpecsBaselineFilename is the file name of a committed enforce floor, in
	// the directory of the project it records.
	SpecsBaselineFilename = "specs.baseline.json"
	// SpecsBaselineProtocolVersion selects the wire contract of a project's
	// baseline, the file every writer emits.
	SpecsBaselineProtocolVersion = 2
	// WorkspaceSpecsBaselineProtocolVersion selects the wire contract of the
	// workspace floor: every enforced project in one document. It is the
	// in-memory shape of the floor, and the older workspace-root file.
	WorkspaceSpecsBaselineProtocolVersion = 1
	// SpecsBaselineSchemaURL is the published JSON schema for the file.
	SpecsBaselineSchemaURL = "https://putnami.dev/schemas/putnami-specs-baseline.json"
	// specsBaselineMaxProjects and specsBaselineMaxRequirements bound one
	// committed baseline.
	specsBaselineMaxProjects     = 4096
	specsBaselineMaxRequirements = 4096
)

// ProjectSpecsBaseline is one enforced project's committed floor. It names no
// project: the directory it sits in does.
type ProjectSpecsBaseline struct {
	// Schema identifies the JSON Schema used to author the baseline.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the exact baseline wire contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// ExecutableRequirements lists the covered requirements as sorted
	// `feature#requirement` identities.
	ExecutableRequirements []string `json:"executableRequirements"`
}

// SpecsBaseline is the enforce floor of the spec gate across a workspace.
type SpecsBaseline struct {
	// Schema identifies the JSON Schema used to author the baseline.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the exact baseline wire contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Projects lists the enforced projects, sorted by project ID.
	Projects []SpecsBaselineProject `json:"projects"`
}

// SpecsBaselineProject is one enforced project's recorded coverage.
type SpecsBaselineProject struct {
	// Project is the workspace project ID committed to enforce.
	Project string `json:"project"`
	// Mode is exactly "enforce" in v1: the baseline records the floor the
	// ratchet protects, and a report or off project has nothing to protect.
	Mode VerificationMode `json:"mode"`
	// ExecutableRequirements lists the covered requirements as sorted
	// `feature#requirement` identities.
	ExecutableRequirements []string `json:"executableRequirements"`
}

// SpecsBaselinePath returns the workspace-relative location of the baseline
// that records the project at the slash-separated projectPath; "" names the
// workspace root.
func SpecsBaselinePath(projectPath string) string {
	projectPath = strings.Trim(path.Clean("/"+projectPath), "/")
	if projectPath == "" {
		return SpecsBaselineFilename
	}
	return projectPath + "/" + SpecsBaselineFilename
}

// SpecsBaselineRequirementIdentity joins one feature ID and one requirement
// ID with the separator neither grammar admits. It is the single composer of
// the identities a baseline records; validation splits on the same separator
// and judges each half with the manifest wire's own canonical pattern, so
// the ID grammar has exactly one definition to evolve.
func SpecsBaselineRequirementIdentity(feature, requirement string) string {
	return feature + "#" + requirement
}

// validSpecsBaselineRequirement checks one recorded identity against the
// canonical grammars (featureIDPattern, segmentIDPattern in validate.go).
func validSpecsBaselineRequirement(identity string) bool {
	feature, requirement, found := strings.Cut(identity, "#")
	return found && featureIDPattern.MatchString(feature) && segmentIDPattern.MatchString(requirement)
}

// ParseProjectSpecsBaseline strictly parses one project's committed baseline.
func ParseProjectSpecsBaseline(data []byte) (*ProjectSpecsBaseline, []diag.Diagnostic) {
	if err := requireExactProtocolVersion(data, SpecsBaselineProtocolVersion); err != nil {
		return nil, []diag.Diagnostic{versionOrParseDiagnostic(err)}
	}
	var baseline ProjectSpecsBaseline
	if err := decodeStrictJSON(data, &baseline); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	if err := rejectExplicitNulls(data); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	return &baseline, nil
}

// ParseAndValidateProjectSpecsBaseline performs strict parsing followed by
// local validation.
func ParseAndValidateProjectSpecsBaseline(data []byte) (*ProjectSpecsBaseline, []diag.Diagnostic) {
	baseline, diagnostics := ParseProjectSpecsBaseline(data)
	if baseline == nil {
		return nil, diagnostics
	}
	diagnostics = append(diagnostics, ValidateProjectSpecsBaseline(baseline)...)
	sortDiagnostics(diagnostics)
	if diag.HasErrors(diagnostics) {
		return nil, diagnostics
	}
	return baseline, diagnostics
}

// IsProjectSpecsBaseline reports whether data declares the project baseline
// version, so a reader of the workspace root can tell a root project's file
// from the older workspace-wide one before parsing either strictly.
func IsProjectSpecsBaseline(data []byte) bool {
	return requireExactProtocolVersion(data, SpecsBaselineProtocolVersion) == nil
}

// ValidateProjectSpecsBaseline validates one project's baseline in isolation.
func ValidateProjectSpecsBaseline(input *ProjectSpecsBaseline) []diag.Diagnostic {
	if input == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "specs baseline is nil")}
	}
	var diagnostics []diag.Diagnostic
	if input.ProtocolVersion != SpecsBaselineProtocolVersion {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported (want %d)", input.ProtocolVersion, SpecsBaselineProtocolVersion))
	}
	diagnostics = append(diagnostics, validateBaselineRequirements("", input.ExecutableRequirements)...)
	sortDiagnostics(diagnostics)
	return diagnostics
}

// CanonicalProjectSpecsBaseline returns a deterministically ordered copy with
// the published schema URL stamped, so an authored file round-trips to one
// byte form.
func CanonicalProjectSpecsBaseline(input *ProjectSpecsBaseline) *ProjectSpecsBaseline {
	if input == nil {
		return nil
	}
	out := *input
	out.Schema = SpecsBaselineSchemaURL
	// The copy stays non-nil even when empty: an enforced project with no
	// executable requirement yet is a legal floor, and a nil slice would
	// marshal as the explicit null the strict reader refuses.
	out.ExecutableRequirements = append(make([]string, 0, len(input.ExecutableRequirements)), input.ExecutableRequirements...)
	sort.Strings(out.ExecutableRequirements)
	return &out
}

// MarshalProjectSpecsBaseline encodes the canonical wire form of one
// project's baseline.
func MarshalProjectSpecsBaseline(baseline *ProjectSpecsBaseline) ([]byte, error) {
	return marshalCanonical(CanonicalProjectSpecsBaseline(baseline))
}

// ParseSpecsBaseline strictly parses one workspace floor document.
func ParseSpecsBaseline(data []byte) (*SpecsBaseline, []diag.Diagnostic) {
	if err := requireExactProtocolVersion(data, WorkspaceSpecsBaselineProtocolVersion); err != nil {
		return nil, []diag.Diagnostic{versionOrParseDiagnostic(err)}
	}
	var baseline SpecsBaseline
	if err := decodeStrictJSON(data, &baseline); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	if err := rejectExplicitNulls(data); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	return &baseline, nil
}

// ParseAndValidateSpecsBaseline performs strict parsing followed by local
// validation.
func ParseAndValidateSpecsBaseline(data []byte) (*SpecsBaseline, []diag.Diagnostic) {
	baseline, diagnostics := ParseSpecsBaseline(data)
	if baseline == nil {
		return nil, diagnostics
	}
	diagnostics = append(diagnostics, ValidateSpecsBaseline(baseline)...)
	sortDiagnostics(diagnostics)
	if diag.HasErrors(diagnostics) {
		return nil, diagnostics
	}
	return baseline, diagnostics
}

// ValidateSpecsBaseline validates one workspace floor in isolation.
func ValidateSpecsBaseline(input *SpecsBaseline) []diag.Diagnostic {
	if input == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "specs baseline is nil")}
	}
	var diagnostics []diag.Diagnostic
	if input.ProtocolVersion != WorkspaceSpecsBaselineProtocolVersion {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported (want %d)", input.ProtocolVersion, WorkspaceSpecsBaselineProtocolVersion))
	}
	if input.Projects == nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeParseError, "projects", "projects is required and must be an array"))
	}
	if len(input.Projects) > specsBaselineMaxProjects {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeSensitiveContent, "projects",
			"baseline exceeds the bounded project limit of %d", specsBaselineMaxProjects))
		sortDiagnostics(diagnostics)
		return diagnostics
	}
	seenProjects := make(map[string]bool, len(input.Projects))
	for i, project := range input.Projects {
		field := fmt.Sprintf("projects[%d]", i)
		if strings.TrimSpace(project.Project) == "" {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".project", "baseline project is required"))
		}
		if seenProjects[project.Project] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateSpec, field+".project",
				"project %q is duplicated", project.Project))
		}
		seenProjects[project.Project] = true
		if project.Mode != VerificationModeEnforce {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field+".mode",
				"baseline mode must be %q: the ratchet records the enforced floor, and %q protects nothing",
				VerificationModeEnforce, project.Mode))
		}
		diagnostics = append(diagnostics, validateBaselineRequirements(field+".", project.ExecutableRequirements)...)
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

// validateBaselineRequirements checks one recorded requirement list; prefix
// anchors its fields inside the enclosing document.
func validateBaselineRequirements(prefix string, requirements []string) []diag.Diagnostic {
	field := prefix + "executableRequirements"
	if requirements == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, field,
			"executableRequirements is required and must be an array")}
	}
	if len(requirements) > specsBaselineMaxRequirements {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeSensitiveContent, field,
			"baseline exceeds the bounded requirement limit of %d", specsBaselineMaxRequirements)}
	}
	var diagnostics []diag.Diagnostic
	seen := make(map[string]bool, len(requirements))
	for j, requirement := range requirements {
		requirementField := fmt.Sprintf("%s[%d]", field, j)
		if !validSpecsBaselineRequirement(requirement) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRequirement, requirementField,
				"requirement identity %q is not a canonical feature#requirement pair", requirement))
		}
		if seen[requirement] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRequirement, requirementField,
				"requirement identity %q is duplicated", requirement))
		}
		seen[requirement] = true
	}
	return diagnostics
}

// CanonicalSpecsBaseline returns a deterministically ordered copy: projects
// sorted by ID, requirement identities sorted, and the published schema URL
// stamped so an authored file round-trips to one byte form.
func CanonicalSpecsBaseline(input *SpecsBaseline) *SpecsBaseline {
	if input == nil {
		return nil
	}
	out := *input
	out.Schema = SpecsBaselineSchemaURL
	out.Projects = append(make([]SpecsBaselineProject, 0, len(input.Projects)), input.Projects...)
	for i := range out.Projects {
		// The copy stays non-nil even when empty: an enforced project with no
		// executable requirement yet is a legal floor entry, and a nil slice
		// would marshal as the explicit null the strict reader refuses.
		out.Projects[i].ExecutableRequirements = append(make([]string, 0, len(out.Projects[i].ExecutableRequirements)),
			out.Projects[i].ExecutableRequirements...)
		sort.Strings(out.Projects[i].ExecutableRequirements)
	}
	sort.Slice(out.Projects, func(i, j int) bool { return out.Projects[i].Project < out.Projects[j].Project })
	return &out
}

// MarshalSpecsBaseline encodes the canonical wire form.
func MarshalSpecsBaseline(baseline *SpecsBaseline) ([]byte, error) {
	return marshalCanonical(CanonicalSpecsBaseline(baseline))
}

// SpecsRatchetSummary is the deterministic accounting of one comparison.
type SpecsRatchetSummary struct {
	// BaselineProjects counts the enforced projects the committed floor names.
	BaselineProjects int `json:"baselineProjects"`
	// RegressedProjects counts baseline projects no longer enforced.
	RegressedProjects int `json:"regressedProjects"`
	// LostRequirements counts baseline requirements no longer executable in a
	// still-enforced project.
	LostRequirements int `json:"lostRequirements"`
	// GrownProjects and GrownRequirements count current enforcement the
	// baseline does not record yet — always legal, never a finding, and the
	// caller may surface them as a nudge to raise the floor.
	GrownProjects     int `json:"grownProjects"`
	GrownRequirements int `json:"grownRequirements"`
}

// CompareSpecsBaseline judges the CURRENT enforced coverage against the
// committed floor. Both sides use the baseline wire; the caller derives
// current from the committed manifests and options. The rule is shrink-only:
// a baseline project that regressed out of enforce, or lost a recorded
// executable requirement while still enforced, is an error — weakening the
// gate is a reviewed policy change that must edit the committed baseline in
// the same diff. Growth produces no finding.
func CompareSpecsBaseline(baseline, current *SpecsBaseline) ([]diag.Diagnostic, SpecsRatchetSummary) {
	var diagnostics []diag.Diagnostic
	summary := SpecsRatchetSummary{}
	currentByID := make(map[string]SpecsBaselineProject)
	if current != nil {
		for _, project := range current.Projects {
			currentByID[project.Project] = project
		}
	}
	baselineByID := make(map[string]bool)
	if baseline != nil {
		summary.BaselineProjects = len(baseline.Projects)
		for _, recorded := range baseline.Projects {
			baselineByID[recorded.Project] = true
			live, enforced := currentByID[recorded.Project]
			if !enforced {
				summary.RegressedProjects++
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeRatchetRegression, recorded.Project,
					"project %s regressed out of enforce; weakening the spec gate is a reviewed policy change — update its %s in the same change",
					recorded.Project, SpecsBaselineFilename))
				continue
			}
			liveSet := make(map[string]bool, len(live.ExecutableRequirements))
			for _, requirement := range live.ExecutableRequirements {
				liveSet[requirement] = true
			}
			covered := 0
			for _, requirement := range recorded.ExecutableRequirements {
				if !liveSet[requirement] {
					summary.LostRequirements++
					diagnostics = append(diagnostics, diag.Errorf(ErrorCodeRatchetRegression, recorded.Project,
						"project %s lost executable requirement coverage (%s); shrinking the gate is a reviewed policy change — update its %s in the same change",
						recorded.Project, requirement, SpecsBaselineFilename))
					continue
				}
				covered++
			}
			// Growth is the live coverage beyond what the floor records.
			if grown := len(live.ExecutableRequirements) - covered; grown > 0 {
				summary.GrownRequirements += grown
			}
		}
	}
	for id := range currentByID {
		if !baselineByID[id] {
			summary.GrownProjects++
		}
	}
	sortDiagnostics(diagnostics)
	return diagnostics, summary
}
