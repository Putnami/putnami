package sdd

import (
	"fmt"
	"path"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// The feature-coverage gate of `validate-workspace` (ADR 0003).
//
// Every project of the workspace answers one question: which product outcome
// does its code serve? It answers with a feature authored in its own
// putnami.features.json, a spec under its own specs/, or a reviewed
// featureAuthority in its putnami.json: an owner that names an authored
// feature, none with the reason the project has no feature, dependents when a
// project that depends on it declares a feature or hosts a spec, or a provider
// that names a member that passes on its own answer. A project that gives no
// answer fails, unless the committed debt list names it.
//
// The debt list, options.sdd.featureCoverage.debt in putnami.workspace.json,
// is exact. An entry whose project now answers fails until it is removed, and
// an entry that names no project fails, so paying a debt always shrinks the
// list. Adding an entry is an edit of putnami.workspace.json that a reviewer
// sees: the step reads no history to refuse it.
//
// A workspace that declares no options.sdd.featureCoverage has not adopted the
// gate. The step passes with a note, as an absent recipe index does.
//
// The step judges every member the job wire carries and reads each member's
// putnami.json from that wire. It reads the manifests and specs through the
// candidate cut, so the task is cached on the input `git:**`. A key on the
// cut also holds the workspace membership, configs included, so declaring a
// featureAuthority moves it.

// FeatureCoverageOption is the key of options.sdd that adopts the gate.
const FeatureCoverageOption = "featureCoverage"

// featureCoverageDebtMember is the one member of options.sdd.featureCoverage.
const featureCoverageDebtMember = "debt"

// Feature-coverage diagnostic codes.
const (
	// ErrorCodeUncoveredProject reports a project that carries no feature, no
	// spec and no resolvable featureAuthority, and that the debt list does not
	// name.
	ErrorCodeUncoveredProject = "sdd.uncovered_project"
	// ErrorCodeStaleCoverageDebt reports a debt entry whose project is covered.
	ErrorCodeStaleCoverageDebt = "sdd.stale_coverage_debt"
	// ErrorCodeUnknownCoverageDebt reports a debt entry that names no project.
	ErrorCodeUnknownCoverageDebt = "sdd.unknown_coverage_debt"
	// ErrorCodeInvalidFeatureCoverage reports an options.sdd.featureCoverage
	// block this step cannot read.
	ErrorCodeInvalidFeatureCoverage = "sdd.invalid_feature_coverage"
)

// coverageKind is how one project answers the coverage question.
type coverageKind int

const (
	// coverageNone is a project that gives no answer.
	coverageNone coverageKind = iota
	// coverageFeatureOrSpec is a project whose own root declares a feature or
	// hosts a spec.
	coverageFeatureOrSpec
	// coverageAuthority is a project whose reviewed featureAuthority resolves.
	coverageAuthority
)

// FeatureCoverageReport is the typed data of one feature-coverage judgment.
type FeatureCoverageReport struct {
	// Adopted reports that the workspace declares options.sdd.featureCoverage.
	Adopted bool `json:"adopted"`
	// Projects counts the workspace members judged.
	Projects int `json:"projects"`
	// FeatureOrSpec counts the members whose own root declares a feature or
	// hosts a spec.
	FeatureOrSpec int `json:"featureOrSpec"`
	// Authority counts the members that a resolvable featureAuthority covers.
	Authority int `json:"authority"`
	// Debt lists the committed debt entries, in file order.
	Debt []string `json:"debt"`
	// Uncovered lists the members that give no answer, sorted, whether the
	// debt list names them or not.
	Uncovered []string `json:"uncovered"`
	// Note explains a run that enforced nothing: an unadopted gate.
	Note string `json:"note,omitempty"`
	// Diagnostics carries every finding.
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`
}

// BuildFeatureCoverageResult judges every workspace member and checks the
// committed debt list. workspaceOptions are the committed workspace option
// blocks the job context carries.
func BuildFeatureCoverageResult(ws *workspace.Workspace, workspaceOptions map[string]map[string]any) (FeatureCoverageReport, error) {
	report := FeatureCoverageReport{Debt: []string{}, Uncovered: []string{}}
	debt, adopted, findings := featureCoverageDebt(workspaceOptions["sdd"])
	if len(findings) > 0 {
		report.Diagnostics = findings
		return report, WithResultData(protocolcli.Classify(
			fmt.Errorf("feature coverage: options.sdd.%s is invalid", FeatureCoverageOption),
			protocolcli.ErrInvalidConfig), report)
	}
	report.Adopted = adopted
	report.Debt = append(report.Debt, debt...)

	reader, err := featureengine.NewTaskReader(ws.Root)
	if err != nil {
		return report, fmt.Errorf("feature coverage: %w", err)
	}
	// A manifest or a spec that fails to parse covers nothing here; the
	// project-scoped validate steps report the document itself.
	manifests := featureengine.DiscoverAuthoredManifestsWith(ws, reader)
	specs := featureengine.DiscoverSpecsWith(ws, nil, reader)
	authored := make(map[string]bool)
	for _, id := range featureproto.AuthoredFeatureIDs(manifests.Manifests) {
		authored[id] = true
	}
	resolver := newCoverageResolver(ws, coveredRoots(manifests.Manifests, specs.Specs), authored)

	listed := make(map[string]bool, len(debt))
	for _, id := range debt {
		listed[id] = true
	}
	kinds := make(map[string]coverageKind, len(ws.Projects))
	for _, project := range sortedMembers(ws) {
		report.Projects++
		answer := resolver.resolve(project)
		kind := answer.kind
		kinds[project.ID] = kind
		switch kind {
		case coverageFeatureOrSpec:
			report.FeatureOrSpec++
			continue
		case coverageAuthority:
			report.Authority++
			continue
		}
		report.Uncovered = append(report.Uncovered, project.ID)
		if adopted && !listed[project.ID] {
			report.Diagnostics = append(report.Diagnostics, uncoveredProjectDiagnostic(project, answer.unresolved))
		}
	}
	if !adopted {
		report.Note = fmt.Sprintf("not adopted (no options.sdd.%s), %d project(s) carry no feature, spec or featureAuthority",
			FeatureCoverageOption, len(report.Uncovered))
		return report, nil
	}

	for i, id := range debt {
		field := fmt.Sprintf("%s#options.sdd.%s.%s[%d]", wsproto.WorkspaceConfigFilename,
			FeatureCoverageOption, featureCoverageDebtMember, i)
		kind, member := kinds[id]
		switch {
		case !member:
			report.Diagnostics = append(report.Diagnostics, diag.Errorf(ErrorCodeUnknownCoverageDebt, field,
				"%s names no workspace project; remove the entry", id))
		case kind == coverageFeatureOrSpec:
			report.Diagnostics = append(report.Diagnostics, diag.Errorf(ErrorCodeStaleCoverageDebt, field,
				"%s now declares a feature or hosts a spec at its root; remove the entry", id))
		case kind == coverageAuthority:
			report.Diagnostics = append(report.Diagnostics, diag.Errorf(ErrorCodeStaleCoverageDebt, field,
				"%s now declares a resolvable featureAuthority; remove the entry", id))
		}
	}
	sortDiagnostics(report.Diagnostics)
	if diag.HasErrors(report.Diagnostics) {
		return report, WithResultData(protocolcli.Classify(
			fmt.Errorf("feature coverage: %d finding(s)", len(report.Diagnostics)),
			protocolcli.ErrInvalidConfig), report)
	}
	return report, nil
}

// featureCoverageDebt strictly decodes options.sdd.featureCoverage. A block
// without the member has not adopted the gate. Anything else this step cannot
// read is a finding, never a fallback: the list decides which projects fail.
func featureCoverageDebt(block map[string]any) ([]string, bool, []diag.Diagnostic) {
	value, found := block[FeatureCoverageOption]
	if !found {
		return nil, false, nil
	}
	field := wsproto.WorkspaceConfigFilename + "#options.sdd." + FeatureCoverageOption
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidFeatureCoverage, field,
			"must be an object whose one member is %s", featureCoverageDebtMember)}
	}
	members := make([]string, 0, len(object))
	for member := range object {
		members = append(members, member)
	}
	sort.Strings(members)
	for _, member := range members {
		if member != featureCoverageDebtMember {
			return nil, false, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidFeatureCoverage, field,
				"has the unknown member %q; its one member is %s", member, featureCoverageDebtMember)}
		}
	}
	field += "." + featureCoverageDebtMember
	raw, found := object[featureCoverageDebtMember]
	if !found {
		return nil, false, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidFeatureCoverage, field,
			"is required: list the ids of the projects that carry no feature, spec or featureAuthority yet, or [] when every project is covered")}
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, false, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidFeatureCoverage, field,
			"must be an array of project ids such as \"/go/framework/cache\"")}
	}
	debt := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	var findings []diag.Diagnostic
	for i, entry := range list {
		entryField := fmt.Sprintf("%s[%d]", field, i)
		id, ok := entry.(string)
		switch {
		case !ok || !strings.HasPrefix(id, "/") || id == "/" || strings.TrimSpace(id) != id:
			findings = append(findings, diag.Errorf(ErrorCodeInvalidFeatureCoverage, entryField,
				"is %s; an entry is a project id such as \"/go/framework/cache\"", describeDebtEntry(entry)))
		case seen[id]:
			findings = append(findings, diag.Errorf(ErrorCodeInvalidFeatureCoverage, entryField,
				"lists %s twice", id))
		default:
			seen[id] = true
			debt = append(debt, id)
		}
	}
	if len(findings) > 0 {
		return nil, false, findings
	}
	return debt, true, nil
}

// describeDebtEntry names a debt entry in the words of the JSON document.
func describeDebtEntry(entry any) string {
	switch value := entry.(type) {
	case string:
		return fmt.Sprintf("%q", value)
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	default:
		return "a number"
	}
}

// coveredRoots marks the roots that carry their own link to product intent: a
// valid authored manifest that declares at least one feature, or a spec under
// the root's specs/. Membership is per ROOT, never per module, so a package
// family that shares one feature is not pushed into minting an artificial
// feature per technical module: the check reports the gap, it does not
// prescribe the shape of the fix.
func coveredRoots(manifests []featureproto.ManifestSource, specs []featureengine.DiscoveredSpec) map[string]bool {
	covered := make(map[string]bool)
	for _, source := range manifests {
		if source.Manifest != nil && len(source.Manifest.Features) > 0 {
			covered[featureengine.ManifestRoot(source.Path)] = true
		}
	}
	for _, discovered := range specs {
		covered[discovered.Root] = true
	}
	return covered
}

// coverageAnswer is how one project answers the coverage question.
type coverageAnswer struct {
	kind coverageKind
	// unresolved says why a declared featureAuthority covers nothing, in words
	// that follow "project <id>"; empty when the project declares none or it
	// resolves.
	unresolved string
}

// coverageResolver answers the coverage question for the members of one
// workspace. covered marks the roots that declare a feature or host a spec,
// and authored holds every authored feature id. A reviewed featureAuthority
// answers the question, so it covers the project. An answer the resolver
// cannot resolve covers nothing and says why, so the caller can name it: an
// unresolvable answer is worse than none, or the field becomes the cheapest
// way to silence the check.
type coverageResolver struct {
	covered  map[string]bool
	authored map[string]bool
	// members indexes the workspace members by id, and dependents holds the
	// reversed direct edges of the resolved dependency graph.
	members    map[string]*workspace.Project
	dependents map[string][]string
	answers    map[string]coverageAnswer
	// resolving holds the projects on the current provider chain, so a cycle
	// resolves to no answer instead of recursing forever.
	resolving map[string]bool
}

func newCoverageResolver(ws *workspace.Workspace, covered, authored map[string]bool) *coverageResolver {
	members := sortedMembers(ws)
	resolver := &coverageResolver{
		covered:    covered,
		authored:   authored,
		members:    make(map[string]*workspace.Project, len(members)),
		dependents: make(map[string][]string),
		answers:    make(map[string]coverageAnswer),
		resolving:  make(map[string]bool),
	}
	for _, project := range members {
		resolver.members[project.ID] = project
		for _, dependency := range ws.Graph.DependenciesOf(project.ID) {
			resolver.dependents[dependency] = append(resolver.dependents[dependency], project.ID)
		}
	}
	return resolver
}

// resolve answers the coverage question for one project.
func (resolver *coverageResolver) resolve(project *workspace.Project) coverageAnswer {
	if answer, done := resolver.answers[project.ID]; done {
		return answer
	}
	answer := resolver.answer(project)
	resolver.answers[project.ID] = answer
	return answer
}

func (resolver *coverageResolver) answer(project *workspace.Project) coverageAnswer {
	if resolver.covered[project.Path] {
		return coverageAnswer{kind: coverageFeatureOrSpec}
	}
	authority := projectFeatureAuthority(project)
	switch {
	case authority == nil:
		return coverageAnswer{kind: coverageNone}
	case authority.None != "":
		return coverageAnswer{kind: coverageAuthority}
	case authority.Owner != "":
		if resolver.authored[authority.Owner] {
			return coverageAnswer{kind: coverageAuthority}
		}
		return coverageAnswer{kind: coverageNone, unresolved: fmt.Sprintf(
			"names featureAuthority.owner %q, which is no authored feature", authority.Owner)}
	case authority.Dependents:
		if resolver.dependentCarriesFeatureOrSpec(project.ID) {
			return coverageAnswer{kind: coverageAuthority}
		}
		return coverageAnswer{kind: coverageNone, unresolved: "declares featureAuthority.dependents, " +
			"but no project that depends on it, directly or not, declares a feature or hosts a spec"}
	case authority.Provider != "":
		return resolver.provider(project, authority.Provider)
	}
	return coverageAnswer{kind: coverageNone}
}

// provider resolves a featureAuthority.provider answer: the provider must be
// a member that passes on its own answer. A debt entry is no answer.
func (resolver *coverageResolver) provider(project *workspace.Project, id string) coverageAnswer {
	member, found := resolver.members[id]
	if !found {
		return coverageAnswer{kind: coverageNone, unresolved: fmt.Sprintf(
			"names featureAuthority.provider %q, which is no workspace project", id)}
	}
	uncovered := coverageAnswer{kind: coverageNone, unresolved: fmt.Sprintf(
		"names featureAuthority.provider %q, which carries no feature, no spec and no resolvable featureAuthority", id)}
	if resolver.resolving[member.ID] || member.ID == project.ID {
		return uncovered
	}
	resolver.resolving[project.ID] = true
	defer delete(resolver.resolving, project.ID)
	if resolver.resolve(member).kind == coverageNone {
		return uncovered
	}
	return coverageAnswer{kind: coverageAuthority}
}

// dependentCarriesFeatureOrSpec walks the dependency graph backwards from id
// and reports whether a project that depends on it, directly or not, declares
// a feature or hosts a spec at its own root. A dependent's own
// featureAuthority does not count: it carries no requirement either.
func (resolver *coverageResolver) dependentCarriesFeatureOrSpec(id string) bool {
	seen := map[string]bool{id: true}
	queue := []string{id}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, dependent := range resolver.dependents[current] {
			if seen[dependent] {
				continue
			}
			seen[dependent] = true
			if member := resolver.members[dependent]; member != nil && resolver.covered[member.Path] {
				return true
			}
			queue = append(queue, dependent)
		}
	}
	return false
}

func uncoveredProjectDiagnostic(project *workspace.Project, unresolved string) diag.Diagnostic {
	field := path.Join(project.Path, wsproto.ConfigFilename) + "#featureAuthority"
	if unresolved != "" {
		return diag.Errorf(ErrorCodeUncoveredProject, field,
			"project %s %s; give an answer that resolves, or list the project in options.sdd.%s.%s",
			project.ID, unresolved, FeatureCoverageOption, featureCoverageDebtMember)
	}
	return diag.Errorf(ErrorCodeUncoveredProject, field,
		"project %s carries no feature of its own, no spec under specs/ and no featureAuthority; add one, or list the project in options.sdd.%s.%s",
		project.ID, FeatureCoverageOption, featureCoverageDebtMember)
}

// sortedMembers returns the workspace members in id order, so the report and
// its findings do not depend on the order of the wire.
func sortedMembers(ws *workspace.Workspace) []*workspace.Project {
	members := make([]*workspace.Project, 0, len(ws.Projects))
	for _, project := range ws.Projects {
		if project != nil {
			members = append(members, project)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })
	return members
}
