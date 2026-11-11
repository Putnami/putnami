package ci

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// A verdict policy file states what each verdict gap and each declaration
// change costs a change. A repository places one at its root, in a scope
// directory or in a project directory. The nearest file that names a key
// decides; inside that key, the maturity of the feature concerned can pick
// the value. The policy decides what blocks a change; it never changes the
// verdict, whose gaps and mark stay what the run found.
const (
	// VerdictPolicyFile is the name of a verdict policy file.
	VerdictPolicyFile = "putnami.verdict-policy.json"
	// VerdictPolicySchemaURL is the published identifier of its schema.
	VerdictPolicySchemaURL = "https://putnami.dev/schemas/putnami-verdict-policy.json"
	// MaxVerdictPolicyBytes bounds the file ParseVerdictPolicy reads.
	MaxVerdictPolicyBytes = 64 << 10
)

// What a gap costs a change.
const (
	// PolicyInform: the gap is listed and never blocks the change.
	PolicyInform = "inform"
	// PolicyAccept: the gap blocks the change until a person accepts it.
	PolicyAccept = "accept"
	// PolicyBlock: the gap blocks the change, and nobody can accept it.
	PolicyBlock = "block"
)

// When a declaration change waits for an approval.
const (
	// PolicyAlways: every change of the declaration waits for an approval.
	PolicyAlways = "always"
	// PolicyWhenWeakening: a change waits for an approval when it weakens
	// what the declaration promises. A producer that could not judge the
	// change counts it as weakening.
	PolicyWhenWeakening = "when-weakening"
	// PolicyNever: no change of the declaration waits for an approval.
	PolicyNever = "never"
)

// PolicyLostProof is a gap key of the policy only: an unproven-requirement
// gap whose requirement the change's base proved. The verdict lists it as
// GapUnprovenRequirement.
const PolicyLostProof = "lost-proof"

// verdictPolicyMaturities is the closed maturity order of a feature, from
// low to high. It is the order of go.putnami.dev/protocol/features, which a
// test holds it to.
var verdictPolicyMaturities = []string{
	"modeled", "coded", "wired", "default-on", "live-verified", "design-partner-proven", "ga",
}

// The ranks of the values, from the cheapest to the strictest.
var (
	verdictPolicyGapValues         = []string{PolicyInform, PolicyAccept, PolicyBlock}
	verdictPolicyDeclarationValues = []string{PolicyNever, PolicyWhenWeakening, PolicyAlways}
)

// The keys a policy file can set, and those that can take a maturity map.
var (
	verdictPolicyGapKeys = []string{
		GapNoRequirement, GapUnknownDeclarations, GapUnknownImpact, GapUnknownRequirements,
		GapUnprovenRequirement, PolicyLostProof,
	}
	// verdictPolicyRootGapKeys belong to the change, not to a project: only
	// the root file sets them.
	verdictPolicyRootGapKeys            = []string{GapUnknownDeclarations, GapUnknownImpact}
	verdictPolicyFeatureGapKeys         = []string{GapUnprovenRequirement, PolicyLostProof}
	verdictPolicyFeatureDeclarationKeys = []string{DeclarationBinding, DeclarationSpec}
)

//go:embed schemas/putnami-verdict-policy.json
var verdictPolicySchemaBytes []byte

// VerdictPolicySchema returns a defensive copy of the verdict policy JSON
// Schema.
func VerdictPolicySchema() []byte { return append([]byte(nil), verdictPolicySchemaBytes...) }

// VerdictPolicyMaturities returns a fresh copy of the maturity order a
// policy reads, from low to high.
func VerdictPolicyMaturities() []string { return slices.Clone(verdictPolicyMaturities) }

// VerdictPolicy is one verdict policy file.
type VerdictPolicy struct {
	// Schema is VerdictPolicySchemaURL, when the file names its schema.
	Schema string `json:"$schema,omitempty"`
	// Declarations maps a declaration kind to when its changes wait for an
	// approval.
	Declarations map[string]VerdictPolicyValue `json:"declarations,omitempty"`
	// Gaps maps a gap kind, or PolicyLostProof, to what the gap costs.
	Gaps map[string]VerdictPolicyValue `json:"gaps,omitempty"`
}

// VerdictPolicyValue is one value, or one value per feature maturity.
type VerdictPolicyValue struct {
	// Value applies to every feature. It is empty when ByMaturity is set.
	Value string
	// ByMaturity maps a maturity to the value that applies from that
	// maturity on: the highest key a feature's maturity reaches applies. It
	// holds the lowest maturity, modeled, so it covers every feature.
	ByMaturity map[string]string
}

// UnmarshalJSON reads a string, or an object from maturity to string.
func (v *VerdictPolicyValue) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	switch {
	case len(trimmed) > 0 && trimmed[0] == '"':
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return err
		}
		*v = VerdictPolicyValue{Value: value}
		return nil
	case len(trimmed) > 0 && trimmed[0] == '{':
		var byMaturity map[string]string
		if err := json.Unmarshal(trimmed, &byMaturity); err != nil {
			return fmt.Errorf("a value by maturity maps a maturity to a string: %w", err)
		}
		*v = VerdictPolicyValue{ByMaturity: byMaturity}
		return nil
	}
	return fmt.Errorf("a policy value is a string or an object by maturity")
}

// MarshalJSON writes the string, or the object by maturity.
func (v VerdictPolicyValue) MarshalJSON() ([]byte, error) {
	if v.ByMaturity != nil {
		return json.Marshal(v.ByMaturity)
	}
	return json.Marshal(v.Value)
}

// ParseVerdictPolicy strictly decodes the verdict policy file at path, a
// workspace-relative slash path, and validates it. It refuses what
// ParseVerdictPolicyWithDiagnostics reports as an error.
func ParseVerdictPolicy(path string, data []byte) (VerdictPolicy, error) {
	policy, diagnostics := ParseVerdictPolicyWithDiagnostics(path, data)
	if diag.HasErrors(diagnostics) {
		return VerdictPolicy{}, &ValidationError{Diagnostics: diagnostics}
	}
	return policy, nil
}

// ParseVerdictPolicyWithDiagnostics strictly decodes the verdict policy file
// at path and validates it. It refuses a file above MaxVerdictPolicyBytes, a
// duplicate object key, an unknown member, trailing data, and every defect
// ValidateVerdictPolicy reports.
func ParseVerdictPolicyWithDiagnostics(path string, data []byte) (VerdictPolicy, []diag.Diagnostic) {
	if len(data) > MaxVerdictPolicyBytes {
		return VerdictPolicy{}, []diag.Diagnostic{diag.Errorf(
			"ci.verdict_policy_too_large", "(document)", "verdict policy is %d bytes; maximum is %d", len(data), MaxVerdictPolicyBytes,
		)}
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return VerdictPolicy{}, []diag.Diagnostic{diag.Errorf("ci.duplicate_key", "(document)", "%v", err)}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var policy VerdictPolicy
	if err := decoder.Decode(&policy); err != nil {
		return VerdictPolicy{}, []diag.Diagnostic{diag.Errorf("ci.invalid_json", "(document)", "%v", err)}
	}
	if err := requireJSONEOF(decoder); err != nil {
		return VerdictPolicy{}, []diag.Diagnostic{diag.Errorf("ci.trailing_json", "(document)", "%v", err)}
	}
	diagnostics := ValidateVerdictPolicy(path, policy)
	if diag.HasErrors(diagnostics) {
		return VerdictPolicy{}, diagnostics
	}
	return policy, diagnostics
}

// ValidateVerdictPolicy reports every defect of the policy file at path:
//
//  1. a path that is not a workspace-relative slash path to a file named
//     VerdictPolicyFile;
//  2. a $schema other than VerdictPolicySchemaURL;
//  3. a declaration kind that is unknown or is DeclarationPolicy, and a gap
//     key that is unknown or is GapUnapprovedDeclaration;
//  4. a gap key of the change (unknown-impact, unknown-declarations) outside
//     the root file;
//  5. a value outside the key's vocabulary;
//  6. a map by maturity on a key that concerns no feature, an unknown
//     maturity, and a map that does not hold modeled.
//
// It does not check that the directory is the root, a scope or a project:
// the workspace knows its scopes and projects, the protocol does not.
func ValidateVerdictPolicy(filePath string, policy VerdictPolicy) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	add := func(d diag.Diagnostic) { diagnostics = append(diagnostics, d) }
	root := filePath == VerdictPolicyFile
	if !root && !validVerdictPolicyPath(filePath) {
		add(diag.Errorf("ci.verdict_policy_path", "(document)", "a verdict policy is a workspace-relative path to a file named %s", VerdictPolicyFile))
	}
	if policy.Schema != "" && policy.Schema != VerdictPolicySchemaURL {
		add(diag.Errorf("ci.verdict_policy_schema", "$schema", "must be %s", VerdictPolicySchemaURL))
	}
	for _, kind := range sortedMapKeys(policy.Declarations) {
		field := "declarations." + kind
		switch {
		case kind == DeclarationPolicy:
			add(diag.Errorf("ci.verdict_policy_kind", field, "a change to a verdict policy always waits for an approval; no policy sets it"))
			continue
		case !slices.Contains(verdictDeclarationKinds, kind):
			add(diag.Errorf("ci.verdict_policy_kind", field, "unknown declaration kind %q", kind))
			continue
		}
		validateVerdictPolicyValue(add, field, policy.Declarations[kind], verdictPolicyDeclarationValues,
			slices.Contains(verdictPolicyFeatureDeclarationKeys, kind))
	}
	for _, kind := range sortedMapKeys(policy.Gaps) {
		field := "gaps." + kind
		switch {
		case kind == GapUnapprovedDeclaration:
			add(diag.Errorf("ci.verdict_policy_kind", field, "a declaration change is set under declarations, by its kind"))
			continue
		case !slices.Contains(verdictPolicyGapKeys, kind):
			add(diag.Errorf("ci.verdict_policy_kind", field, "unknown gap kind %q", kind))
			continue
		case !root && slices.Contains(verdictPolicyRootGapKeys, kind):
			add(diag.Errorf("ci.verdict_policy_root_only", field, "%s concerns the whole change: only the root %s sets it", kind, VerdictPolicyFile))
			continue
		}
		validateVerdictPolicyValue(add, field, policy.Gaps[kind], verdictPolicyGapValues,
			slices.Contains(verdictPolicyFeatureGapKeys, kind))
	}
	return diagnostics
}

func validateVerdictPolicyValue(add func(diag.Diagnostic), field string, value VerdictPolicyValue, vocabulary []string, byFeature bool) {
	if value.ByMaturity == nil {
		if !slices.Contains(vocabulary, value.Value) {
			add(diag.Errorf("ci.verdict_policy_value", field, "must be one of %s", strings.Join(vocabulary, ", ")))
		}
		return
	}
	if !byFeature {
		add(diag.Errorf("ci.verdict_policy_maturity", field, "concerns no feature: it takes one value, not a value by maturity"))
		return
	}
	if _, ok := value.ByMaturity[verdictPolicyMaturities[0]]; !ok {
		add(diag.Errorf("ci.verdict_policy_maturity", field, "a value by maturity holds %s, so it covers every feature", verdictPolicyMaturities[0]))
	}
	for _, maturity := range sortedMapKeys(value.ByMaturity) {
		if !slices.Contains(verdictPolicyMaturities, maturity) {
			add(diag.Errorf("ci.verdict_policy_maturity", field+"."+maturity, "unknown maturity; one of %s", strings.Join(verdictPolicyMaturities, ", ")))
			continue
		}
		if !slices.Contains(vocabulary, value.ByMaturity[maturity]) {
			add(diag.Errorf("ci.verdict_policy_value", field+"."+maturity, "must be one of %s", strings.Join(vocabulary, ", ")))
		}
	}
}

// validVerdictPolicyPath accepts a clean workspace-relative slash path whose
// base name is VerdictPolicyFile.
func validVerdictPolicyPath(filePath string) bool {
	if filePath == "" || strings.HasPrefix(filePath, "/") || strings.Contains(filePath, `\`) {
		return false
	}
	if path.Clean(filePath) != filePath || containsDotSegment(filePath) {
		return false
	}
	return path.Base(filePath) == VerdictPolicyFile
}

// ProjectVerdictPolicyPaths returns the paths of the policy files that may
// govern the project, nearest first: one per directory from the project's
// directory up to the workspace root. projectDir is the project's
// workspace-relative directory, "" or "/" for the project at the root. It is
// not the project's ID: an ID leaves out grouping folders such as "(web)",
// and the files sit in the directories on disk. A nested project also reads
// the file of the project that holds it.
func ProjectVerdictPolicyPaths(projectDir string) []string {
	return verdictPolicyPathsFrom(strings.Trim(projectDir, "/"))
}

// DeclarationVerdictPolicyPaths returns the paths of the policy files that
// may govern the declaration at the workspace-relative path, nearest first:
// one per directory from the declaration's directory up to the root.
func DeclarationVerdictPolicyPaths(declarationPath string) []string {
	dir := path.Dir(strings.Trim(declarationPath, "/"))
	if dir == "." {
		dir = ""
	}
	return verdictPolicyPathsFrom(dir)
}

func verdictPolicyPathsFrom(dir string) []string {
	var paths []string
	for dir != "" && dir != "." {
		paths = append(paths, dir+"/"+VerdictPolicyFile)
		dir = path.Dir(dir)
	}
	return append(paths, VerdictPolicyFile)
}

// VerdictPolicyFileAt is one policy file of a chain.
type VerdictPolicyFileAt struct {
	// Path is the file's workspace-relative path.
	Path string
	// Policy is the parsed file.
	Policy VerdictPolicy
}

// VerdictPolicyChain is the policy files that govern one project or one
// declaration, nearest first, in the order ProjectVerdictPolicyPaths or
// DeclarationVerdictPolicyPaths returns. A file that does not exist is not
// in the chain.
type VerdictPolicyChain []VerdictPolicyFileAt

// Gap returns what a gap of kind costs, for a feature of the given maturity,
// and the path of the file that decides it. kind is a gap kind or
// PolicyLostProof. maturity is the feature's maturity, or "" for a gap that
// concerns no feature. ok is false when no file of the chain names the
// kind: the caller then applies its own default. A key of the change
// (unknown-impact, unknown-declarations) is read from the root file only.
func (c VerdictPolicyChain) Gap(kind, maturity string) (value, decidedBy string, ok bool) {
	for _, file := range c {
		if slices.Contains(verdictPolicyRootGapKeys, kind) && file.Path != VerdictPolicyFile {
			continue
		}
		if entry, found := file.Policy.Gaps[kind]; found {
			return entry.resolve(maturity, verdictPolicyGapValues), file.Path, true
		}
	}
	return "", "", false
}

// Declaration returns when a change to a declaration of kind waits for an
// approval, for the highest maturity of the features the declaration
// declares, and the path of the file that decides it. maturity is "" for a
// declaration that concerns no feature. ok is false when no file of the
// chain names the kind. A DeclarationPolicy change always waits:
// Declaration answers PolicyAlways for it, decided by no file.
func (c VerdictPolicyChain) Declaration(kind, maturity string) (value, decidedBy string, ok bool) {
	if kind == DeclarationPolicy {
		return PolicyAlways, "", true
	}
	for _, file := range c {
		if entry, found := file.Policy.Declarations[kind]; found {
			return entry.resolve(maturity, verdictPolicyDeclarationValues), file.Path, true
		}
	}
	return "", "", false
}

// resolve picks the value for a feature of the given maturity: the value of
// the highest maturity key the feature reaches. An unknown maturity, "" for
// a map, takes the strictest value of the map: a policy fails closed.
func (v VerdictPolicyValue) resolve(maturity string, ranks []string) string {
	if v.ByMaturity == nil {
		return v.Value
	}
	rank := slices.Index(verdictPolicyMaturities, maturity)
	if rank < 0 {
		strictest := ""
		for _, value := range v.ByMaturity {
			if slices.Index(ranks, value) > slices.Index(ranks, strictest) {
				strictest = value
			}
		}
		return strictest
	}
	for index := rank; index >= 0; index-- {
		if value, found := v.ByMaturity[verdictPolicyMaturities[index]]; found {
			return value
		}
	}
	return ""
}

// VerdictPolicyLoosens reports whether head, a new version of the policy
// file base, can make a change cost less: a key it removes, a value that
// costs less at some maturity, or a key it adds below the strictest value
// at some maturity, since that key now overrides the files above it.
func VerdictPolicyLoosens(base, head VerdictPolicy) bool {
	return policyEntriesLoosen(base.Declarations, head.Declarations, verdictPolicyDeclarationValues) ||
		policyEntriesLoosen(base.Gaps, head.Gaps, verdictPolicyGapValues)
}

func policyEntriesLoosen(base, head map[string]VerdictPolicyValue, ranks []string) bool {
	for key, before := range base {
		after, kept := head[key]
		if !kept {
			return true
		}
		for _, maturity := range verdictPolicyMaturities {
			if slices.Index(ranks, after.resolve(maturity, ranks)) < slices.Index(ranks, before.resolve(maturity, ranks)) {
				return true
			}
		}
	}
	strictest := ranks[len(ranks)-1]
	for key, added := range head {
		if _, existed := base[key]; existed {
			continue
		}
		for _, maturity := range verdictPolicyMaturities {
			if added.resolve(maturity, ranks) != strictest {
				return true
			}
		}
	}
	return false
}
