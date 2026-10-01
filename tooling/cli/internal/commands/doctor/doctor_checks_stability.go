package doctor

import (
	"encoding/json"
	"fmt"
	"strings"

	capabilities "go.putnami.dev/protocol/capabilities"
	doctor "go.putnami.dev/protocol/doctor"
)

// Committed generated artifacts the stability check inspects, in a fixed order
// so a project's findings are deterministic. schema/config.json is deliberately
// absent from the version class: it is a declared identity, not inherited state.
var doctorGeneratedArtifacts = []string{
	doctorCapabilitiesPath,
	doctorOpenAPIPath,
	doctorHTTPRoutesPath,
	doctorConfigSchemaPath,
	doctorConfigJSONSchemaPath,
}

// doctorSourceBindingPrefix is the source-v1 binding scheme. A binding hashes
// every file of a project, so a committed artifact carrying one changes on every
// source commit — the same instability class one layer down (an earlier change
// removed it from the capability manifest; this keeps it out).
const doctorSourceBindingPrefix = "source-v1:sha256:"

// checkCommittedManifestStability is the single-project stability contract:
// a committed generated artifact must be a function of its OWN
// project's declared inputs, and of nothing else in the workspace.
//
// Three content classes break that, and each is detectable from the committed
// bytes alone — no build, no network, no runtime state:
//
//   - a WORKSPACE VERSION string, so one `putnami.workspace.json` bump re-stamps
//     every committed spec in the repo;
//   - a CLOSURE ENUMERATION — package entries the manifest cannot justify from
//     its own contributions — so adding a dependency edge anywhere re-stamps
//     every workload's manifest; and
//   - a SOURCE BINDING, so any source commit re-stamps the artifact.
//
// The failure they produce is not a wrong value but a wrong OWNER: a gate for
// project A dirties project B's tracked file, and CI reports it against
// whichever job noticed ("worktree mutated"). Naming the artifact and the field
// here is what turns that into an attributable finding.
//
// Findings are advisory (never blocking, see severityFor): the artifact is
// correct, it is only coupled to state it should not be, and the remediation is
// a regeneration rather than a production-safety fix. Each is waivable per
// project, and per field within a project.
func checkCommittedManifestStability(p doctorProject) []doctor.Finding {
	var findings []doctor.Finding
	findings = append(findings, checkCommittedClosureEnumeration(p)...)
	findings = append(findings, checkCommittedSourceBindings(p)...)
	return findings
}

// checkCommittedClosureEnumeration flags package entries in a committed
// capability manifest that fall outside the manifest's own capability surface —
// i.e. entries it cannot justify from its own contributions. Those entries are
// the workload's reachable dependency closure, which is workspace state.
//
// The surface rule lives in the protocol (capabilities.CapabilitySurfaceV2) and
// is what canonical emission enforces, so doctor reports exactly the subset a
// regeneration would drop rather than re-deriving a second opinion. A manifest
// that does not strict-parse is left to checkCapabilities, which owns that
// diagnostic.
func checkCommittedClosureEnumeration(p doctorProject) []doctor.Finding {
	data, ok := readCommitted(p.absDir, doctorCapabilitiesPath)
	if !ok {
		return nil
	}
	document, _ := capabilities.ParseManifestDocument(data)
	if document == nil || document.V2 == nil {
		return nil
	}
	surface := capabilities.CapabilitySurfaceV2(document.V2)
	// Canonical emission migrates historical packageVersions into packages before
	// applying the surface projection. Doctor must inspect the same logical set,
	// otherwise an old manifest can carry an unrelated closure under
	// packageVersions and pass this check even though regeneration would drop it.
	packages := append([]capabilities.PackageV2(nil), document.V2.Packages...)
	for _, entry := range document.V2.PackageVersions {
		packages = append(packages, capabilities.PackageV2{
			Identity:   entry.Identity,
			Package:    entry.Package,
			Provenance: entry.Provenance,
		})
	}
	var outside []string
	for _, entry := range packages {
		if !capabilities.PackageWithinSurfaceV2(surface, entry) {
			outside = append(outside, entry.Package)
		}
	}
	if len(outside) == 0 {
		return nil
	}
	return []doctor.Finding{p.finding(
		doctor.CheckCommittedManifestStability, false,
		doctorCapabilitiesPath, "packages",
		fmt.Sprintf("committed manifest enumerates %d package(s) that contribute nothing to it (%s); that is the dependency closure, so an edit to any of them re-stamps this tracked file",
			len(outside), summarizeNames(outside)))}
}

// checkCommittedSourceBindings flags a committed generated artifact carrying a
// source-v1 binding. A binding digests every file of a project, so the artifact
// re-stamps on every commit to that project — the same volatility an earlier
// change removed from the capability manifest, kept out of every committed artifact here.
//
// The scan is a substring match on the scheme prefix rather than a per-artifact
// parse: the class is defined by the value, the artifacts have different shapes,
// and the prefix is a reserved scheme that appears in no other content.
func checkCommittedSourceBindings(p doctorProject) []doctor.Finding {
	var findings []doctor.Finding
	for _, rel := range doctorGeneratedArtifacts {
		data, ok := readCommitted(p.absDir, rel)
		if !ok || !strings.Contains(string(data), doctorSourceBindingPrefix) {
			continue
		}
		findings = append(findings, p.finding(
			doctor.CheckCommittedManifestStability, false,
			rel, "sourceBinding",
			"committed artifact embeds a source-v1 binding, which digests the whole project; it re-stamps this tracked file on every commit"))
	}
	return findings
}

// checkSchemaCommitRegime warns when a project commits generated schema
// artifacts without declaring `options.generate.schema`.
//
// The option exists and is read by the extension SDK, but it is implicit
// default-true: today a project tracks generated schemas because nobody decided
// otherwise. Whether a generated file is reviewed like source or lives in the
// gitignored .gen/ tree is a per-project decision with real consequences — a
// tracked artifact can be dirtied by a build and fail an unrelated gate — so it
// should be authored, not inherited. Applications are recommended to declare
// `false` (apps never commit schema/capabilities.json); libraries whose
// consumers read the artifact declare `true`.
//
// The finding is advisory and per project. It fires only when the project
// actually commits such an artifact, so a project with nothing tracked is never
// asked to declare anything.
func checkSchemaCommitRegime(p doctorProject) []doctor.Finding {
	if p.generateSchemaDeclared() {
		return nil
	}
	var committed []string
	for _, rel := range doctorGeneratedArtifacts {
		if _, ok := readCommitted(p.absDir, rel); ok {
			committed = append(committed, rel)
		}
	}
	if len(committed) == 0 {
		return nil
	}
	return []doctor.Finding{p.finding(
		doctor.CheckUndeclaredSchemaCommit, false,
		doctorProjectConfigPath, "options.generate.schema",
		fmt.Sprintf("project commits %d generated schema artifact(s) (%s) without declaring options.generate.schema; the commit regime is an implicit default rather than a reviewed decision",
			len(committed), summarizeNames(committed)))}
}

// projectOptionsBlock is the slice of a project's own putnami.json the stability
// checks read: the declared version and the option blocks that can declare a
// version or the commit regime. It is a committed file, so reading it keeps the
// report reproducible from the working tree alone.
type projectOptionsBlock struct {
	Version string `json:"version"`
	Options struct {
		Generate struct {
			Schema *bool `json:"schema"`
		} `json:"generate"`
		OpenAPI struct {
			Version string `json:"version"`
		} `json:"openapi"`
	} `json:"options"`
}

// projectConfig reads and decodes the project's own putnami.json. A missing or
// malformed file yields the zero value: the checks then see "declares nothing",
// which is the conservative reading (a malformed project config is reported by
// workspace load, not by doctor).
func (p doctorProject) projectConfig() projectOptionsBlock {
	var cfg projectOptionsBlock
	data, ok := readCommitted(p.absDir, doctorProjectConfigPath)
	if !ok {
		return cfg
	}
	if json.Unmarshal(data, &cfg) != nil {
		return projectOptionsBlock{}
	}
	return cfg
}

// generateSchemaDeclared reports whether the project authored
// options.generate.schema — either value counts, since the check is about the
// decision being explicit, not about which way it went.
func (p doctorProject) generateSchemaDeclared() bool {
	return p.projectConfig().Options.Generate.Schema != nil
}

// summarizeNames renders at most three names for a finding message, so the
// message stays readable (and stable) when a manifest enumerates thirty
// packages. Input order is the artifact's own canonical order.
func summarizeNames(names []string) string {
	const max = 3
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, … +%d more", strings.Join(names[:max], ", "), len(names)-max)
}
