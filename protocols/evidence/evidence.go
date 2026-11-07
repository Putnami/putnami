// Package evidence defines the change evidence document: what one change in a
// repository that keeps its own CI touches, with its tests, its coverage and
// its dependency changes.
//
// The observe extension writes the document from what the CI already produced
// (the git diff, JUnit and coverage reports, lockfiles); a reporter uploads it.
// Three rules shape every type here.
//
// The document is self-reported. It carries no verdict and no trust claim:
// whatever produced it, a reader treats its facts as reported, not proven.
//
// The document is consistent. Every component it names exists, a component's
// impact follows from the changed files and the dependency edges, and every
// count adds up. ValidateDocument refuses a document that contradicts itself.
//
// The document is bounded. Every list has a cap, so a reader can accept it
// without trusting its producer's size.
package evidence

import (
	"strings"

	"go.putnami.dev/protocol/codehost"
	diag "go.putnami.dev/protocol/diagnostic"
)

// ProtocolVersion is the version of the document. A reader refuses any other
// value.
const ProtocolVersion = 1

// Code hosts: where the repository lives. The codehost protocol owns the
// slugs; the document validates against them.
const (
	ProviderGitHub  = codehost.GitHub
	ProviderGitLab  = codehost.GitLab
	ProviderGeneric = codehost.Generic
)

// Providers is the closed code-host vocabulary, codehost.Slugs.
var Providers = codehost.Slugs()

// CI systems: what ran the job that produced the document.
const (
	CISystemGitHubActions      = "github-actions"
	CISystemGitLabCI           = "gitlab-ci"
	CISystemJenkins            = "jenkins"
	CISystemCircleCI           = "circleci"
	CISystemBuildkite          = "buildkite"
	CISystemAzurePipelines     = "azure-pipelines"
	CISystemBitbucketPipelines = "bitbucket-pipelines"
	CISystemTeamCity           = "teamcity"
	CISystemGeneric            = "generic"
)

// CISystems is the closed CI-system vocabulary.
var CISystems = []string{
	CISystemGitHubActions, CISystemGitLabCI, CISystemJenkins, CISystemCircleCI, CISystemBuildkite,
	CISystemAzurePipelines, CISystemBitbucketPipelines, CISystemTeamCity, CISystemGeneric,
}

// CIMarker is the variable a CI sets in every job. Value "" matches any
// non-empty value; another value matches without case.
type CIMarker struct {
	System   string
	Variable string
	Value    string
}

// CIMarkers recognize a CI system, in order: the first that matches wins.
var CIMarkers = []CIMarker{
	{CISystemGitHubActions, "GITHUB_ACTIONS", "true"},
	{CISystemGitLabCI, "GITLAB_CI", "true"},
	{CISystemJenkins, "JENKINS_URL", ""},
	{CISystemCircleCI, "CIRCLECI", "true"},
	{CISystemBuildkite, "BUILDKITE", "true"},
	{CISystemAzurePipelines, "TF_BUILD", "true"},
	{CISystemBitbucketPipelines, "BITBUCKET_BUILD_NUMBER", ""},
	{CISystemTeamCity, "TEAMCITY_VERSION", ""},
}

// DetectCISystem names the CI that env describes: the system of the first
// marker that matches, else generic when CI is true or 1. ok is false when
// the environment is no CI. env returns "" for an unset variable.
func DetectCISystem(env func(string) string) (system string, ok bool) {
	for _, marker := range CIMarkers {
		value := strings.TrimSpace(env(marker.Variable))
		if value == "" {
			continue
		}
		if marker.Value == "" || strings.EqualFold(value, marker.Value) {
			return marker.System, true
		}
	}
	switch strings.ToLower(strings.TrimSpace(env("CI"))) {
	case "true", "1":
		return CISystemGeneric, true
	}
	return "", false
}

// File statuses, as git reports them.
const (
	FileAdded    = "added"
	FileModified = "modified"
	FileDeleted  = "deleted"
	FileRenamed  = "renamed"
)

// FileStatuses is the closed file-status vocabulary.
var FileStatuses = []string{FileAdded, FileModified, FileDeleted, FileRenamed}

// Component sources, from the most to the least authoritative inference, then a
// hand declaration, which always wins.
const (
	// SourceOrchestrator is a monorepo orchestrator's graph export (Nx, Turborepo, Bazel).
	SourceOrchestrator = "orchestrator"
	// SourceWorkspace is a package-manager workspace (npm, pnpm, yarn, go.work, Composer path repositories).
	SourceWorkspace = "workspace"
	// SourceManifest is an isolated manifest (package.json, composer.json, go.mod).
	SourceManifest = "manifest"
	// SourceFallback is the repository itself, when nothing else applies.
	SourceFallback = "fallback"
	// SourceFallbackCodeowners is a directory a CODEOWNERS pattern names, when no detector finds a component.
	SourceFallbackCodeowners = "fallback-codeowners"
	// SourceFallbackDirectories is a top-level directory that holds a source file, when no detector finds a component and no CODEOWNERS names a directory.
	SourceFallbackDirectories = "fallback-directories"
	// SourceDeclared is a putnami.json the team wrote.
	SourceDeclared = "declared"
)

// ComponentSources is the closed component-source vocabulary.
var ComponentSources = []string{SourceOrchestrator, SourceWorkspace, SourceManifest, SourceFallback, SourceFallbackCodeowners, SourceFallbackDirectories, SourceDeclared}

// Component impacts.
const (
	// ImpactChanged means a changed file belongs to the component.
	ImpactChanged = "changed"
	// ImpactDependent means the component is not changed and reaches a changed
	// component through its dependency edges.
	ImpactDependent = "dependent"
	// ImpactUnaffected means neither.
	ImpactUnaffected = "unaffected"
)

// Impacts is the closed impact vocabulary.
var Impacts = []string{ImpactChanged, ImpactDependent, ImpactUnaffected}

// Dependency change kinds.
const (
	DependencyAdded      = "added"
	DependencyRemoved    = "removed"
	DependencyUpgraded   = "upgraded"
	DependencyDowngraded = "downgraded"
	// DependencyChanged is a version change that is neither an upgrade nor a
	// downgrade, such as a git reference or a non-semantic version.
	DependencyChanged = "changed"
)

// DependencyChanges is the closed dependency-change vocabulary.
var DependencyChanges = []string{DependencyAdded, DependencyRemoved, DependencyUpgraded, DependencyDowngraded, DependencyChanged}

// Software bill of materials (SBOM) formats.
const (
	SBOMCycloneDX = "cyclonedx"
	SBOMSPDX      = "spdx"
)

// SBOMFormats is the closed SBOM-format vocabulary.
var SBOMFormats = []string{SBOMCycloneDX, SBOMSPDX}

// Vulnerability severities, as CycloneDX names them. An SBOM that states no
// severity gives SeverityUnknown.
const (
	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityMedium   = "medium"
	SeverityLow      = "low"
	SeverityInfo     = "info"
	SeverityNone     = "none"
	SeverityUnknown  = "unknown"
)

// Severities is the closed severity vocabulary, from the most to the least
// severe, unknown last.
var Severities = []string{SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityInfo, SeverityNone, SeverityUnknown}

// Decision record statuses, as MADR names them. A record that states no
// status the producer recognizes gives DecisionUnknown.
const (
	DecisionProposed   = "proposed"
	DecisionAccepted   = "accepted"
	DecisionRejected   = "rejected"
	DecisionDeprecated = "deprecated"
	DecisionSuperseded = "superseded"
	DecisionUnknown    = "unknown"
)

// DecisionStatuses is the closed decision-status vocabulary.
var DecisionStatuses = []string{DecisionProposed, DecisionAccepted, DecisionRejected, DecisionDeprecated, DecisionSuperseded, DecisionUnknown}

// RootComponent is the id of a component that is the whole repository.
const RootComponent = "."

// Bounds. A larger member is refused with ErrorCodeTooLarge.
const (
	MaxChangedFiles            = 20000
	MaxComponents              = 5000
	MaxDependencyChanges       = 10000
	MaxChangedLineCoverageFile = 20000
	MaxDiagnostics             = 500
	MaxFailingCases            = 50
	MaxUncoveredLines          = 200
	MaxDecisions               = 1000
	// MaxDecisionScope and MaxGovernedFiles cap one decision.
	MaxDecisionScope = 100
	MaxGovernedFiles = 1000
	// MaxLicenses and MaxVulnerabilities cap one dependency state.
	MaxLicenses        = 20
	MaxVulnerabilities = 200
	// MaxEdges caps the dependency edges of all components together.
	MaxEdges = 100000
	// MaxStringBytes caps every string member. A producer truncates a longer
	// message before it writes the document.
	MaxStringBytes = 4096
)

// Document is one change, as one CI job observed it. A reader identifies it by
// its subject's sha with its CI system, run, job and attempt.
type Document struct {
	// ProtocolVersion is always ProtocolVersion.
	ProtocolVersion int `json:"protocolVersion"`
	// Subject names the change.
	Subject Subject `json:"subject"`
	// CI names the job that produced the document.
	CI CI `json:"ci"`
	// Producer names the program that wrote the document.
	Producer Producer `json:"producer"`
	// GeneratedAt is when the document was written, RFC 3339 with a zone.
	GeneratedAt string `json:"generatedAt"`
	// ChangedFiles is the diff between the base and the commit. Empty when no
	// base was resolved.
	ChangedFiles []ChangedFile `json:"changedFiles"`
	// Components is every component the producer found.
	Components []Component `json:"components"`
	// Tests is one entry per component whose test reports were read.
	Tests []ComponentTests `json:"tests,omitempty"`
	// Coverage is one entry per component whose coverage was read.
	Coverage []ComponentCoverage `json:"coverage,omitempty"`
	// ChangedLineCoverage is the coverage of the lines the change added or
	// modified. Absent when no coverage or no base was available.
	ChangedLineCoverage *ChangedLineCoverage `json:"changedLineCoverage,omitempty"`
	// DependencyChanges is the difference of the lockfiles between the base and
	// the commit.
	DependencyChanges []DependencyChange `json:"dependencyChanges,omitempty"`
	// SBOM names the software bills of materials the dependency states were
	// read from. Absent when the producer read none.
	SBOM *SBOM `json:"sbom,omitempty"`
	// Decisions are the decision records the change concerns: those whose
	// scope matches a changed file and those the change adds, modifies,
	// renames or deletes. Absent when the producer found none.
	Decisions []Decision `json:"decisions,omitempty"`
	// Diagnostics are the producer's findings: a missing or malformed report, an
	// unresolved base. They never make the document invalid.
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`
}

// Subject names the change: the repository and the commits.
type Subject struct {
	// Provider is the code host: github, gitlab or generic.
	Provider string `json:"provider"`
	// Host is the code host's hostname. Required for github and gitlab.
	Host string `json:"host,omitempty"`
	// ProjectID is the code host's numeric id of the repository, the key that
	// survives a rename or a transfer. Required for github and gitlab.
	ProjectID string `json:"projectId,omitempty"`
	// Repository is the repository's path or name, for display.
	Repository string `json:"repository"`
	// SHA is the commit the document describes.
	SHA string `json:"sha"`
	// BaseSHA is the commit the change is measured against. Absent when no base
	// was resolved.
	BaseSHA string `json:"baseSha,omitempty"`
	// Branch is the branch the commit was built on.
	Branch string `json:"branch,omitempty"`
	// ChangeRequest is the merge request or pull request, when the job built one.
	ChangeRequest *ChangeRequest `json:"changeRequest,omitempty"`
}

// ChangeRequest is a merge request (GitLab) or a pull request (GitHub).
type ChangeRequest struct {
	// Number is the code host's number of the change request.
	Number int `json:"number"`
}

// CI names the job that produced the document.
type CI struct {
	// System is one of CISystems.
	System string `json:"system"`
	// RunID is the CI system's id of the run or pipeline.
	RunID string `json:"runId,omitempty"`
	// JobID is the CI system's id of the job inside the run, so two jobs of one
	// run (a matrix) are two documents.
	JobID string `json:"jobId,omitempty"`
	// Attempt is the run's attempt number, from 1, when the CI system re-runs a
	// run under the same id.
	Attempt *int `json:"attempt,omitempty"`
	// URL is the run's page, https and without credentials.
	URL string `json:"url,omitempty"`
}

// Producer names the program that wrote the document.
type Producer struct {
	// Name is the producer's package or program name.
	Name string `json:"name"`
	// Version is the producer's version.
	Version string `json:"version"`
}

// ChangedFile is one file of the diff.
type ChangedFile struct {
	// Path is the file's path at the commit, or its last path when deleted.
	Path string `json:"path"`
	// Status is added, modified, deleted or renamed.
	Status string `json:"status"`
	// PreviousPath is the path at the base. Required for renamed, refused otherwise.
	PreviousPath string `json:"previousPath,omitempty"`
	// Component is the id of the component the file belongs to, when one does.
	Component string `json:"component,omitempty"`
}

// Component is one unit of the repository: a package, a module, a project.
type Component struct {
	// ID is the component's directory relative to the repository root, or "."
	// for the whole repository.
	ID string `json:"id"`
	// Name is the component's own name (package name, module path, project name).
	Name string `json:"name,omitempty"`
	// Ecosystem is an open lowercase slug: node, composer, go, nx, ...
	Ecosystem string `json:"ecosystem"`
	// Source says how the component was found.
	Source string `json:"source"`
	// Impact is changed, dependent or unaffected.
	Impact string `json:"impact"`
	// DependsOn lists the ids of the components this one depends on.
	DependsOn []string `json:"dependsOn,omitempty"`
}

// ComponentTests is the test outcome of one component.
type ComponentTests struct {
	// Component is the id of the component the tests belong to.
	Component string `json:"component"`
	// Passed counts the passed test cases.
	Passed int `json:"passed"`
	// Failed counts the failed test cases, errors included.
	Failed int `json:"failed"`
	// Skipped counts the skipped test cases.
	Skipped int `json:"skipped"`
	// DurationMs is the summed duration of the test cases, in milliseconds.
	DurationMs int64 `json:"durationMs,omitempty"`
	// FailingCases lists failing cases, at most Failed and at most MaxFailingCases.
	FailingCases []FailingCase `json:"failingCases,omitempty"`
}

// FailingCase is one failing test case.
type FailingCase struct {
	// Name is the test case name.
	Name string `json:"name"`
	// Suite is the suite or class that holds the case.
	Suite string `json:"suite,omitempty"`
	// File is the test file, relative to the repository root, when the report names it.
	File string `json:"file,omitempty"`
	// Message is the failure message, truncated to MaxStringBytes.
	Message string `json:"message,omitempty"`
}

// ComponentCoverage is the line coverage of one component.
type ComponentCoverage struct {
	// Component is the id of the component the coverage belongs to.
	Component string `json:"component"`
	// CoveredLines counts the instrumented lines the tests executed.
	CoveredLines int `json:"coveredLines"`
	// TotalLines counts the instrumented lines.
	TotalLines int `json:"totalLines"`
}

// ChangedLineCoverage is the coverage of the lines the change added or
// modified that the coverage reports instrument.
type ChangedLineCoverage struct {
	// CoveredLines is the sum of the files' covered changed lines.
	CoveredLines int `json:"coveredLines"`
	// TotalLines is the sum of the files' instrumented changed lines.
	TotalLines int `json:"totalLines"`
	// Files is one entry per changed file the coverage instruments.
	Files []ChangedLineCoverageFile `json:"files"`
}

// ChangedLineCoverageFile is the changed-line coverage of one file.
type ChangedLineCoverageFile struct {
	// Path is a changed file that still exists, relative to the repository root.
	Path string `json:"path"`
	// CoveredLines counts the file's changed lines the tests executed.
	CoveredLines int `json:"coveredLines"`
	// TotalLines counts the file's instrumented changed lines.
	TotalLines int `json:"totalLines"`
	// UncoveredLines lists the uncovered changed line numbers, increasing,
	// capped at MaxUncoveredLines.
	UncoveredLines []int `json:"uncoveredLines,omitempty"`
}

// DependencyChange is one package whose locked version changed.
type DependencyChange struct {
	// Ecosystem is an open lowercase slug: npm, composer, go, ...
	Ecosystem string `json:"ecosystem"`
	// Lockfile is the lockfile's path relative to the repository root.
	Lockfile string `json:"lockfile"`
	// Name is the package name.
	Name string `json:"name"`
	// Change is added, removed, upgraded, downgraded or changed.
	Change string `json:"change"`
	// From is the version at the base. Absent for added.
	From string `json:"from,omitempty"`
	// To is the version at the commit. Absent for removed.
	To string `json:"to,omitempty"`
	// Component is the id of the component whose lockfile it is, when one does.
	Component string `json:"component,omitempty"`
	// Before is the package at the base, as the base SBOM lists it at the
	// From versions. Absent for an added package, without a base SBOM, or when
	// the base SBOM does not list it.
	Before *DependencyState `json:"before,omitempty"`
	// After is the package at the commit, as the head SBOM lists it at the To
	// versions. Absent for a removed package, without a head SBOM, or when the
	// head SBOM does not list it.
	After *DependencyState `json:"after,omitempty"`
}

// SBOM names the software bills of materials a producer read. The CI wrote
// them; the producer queries no vulnerability database.
type SBOM struct {
	// Head is the document of the commit. Absent when none was read.
	Head *SBOMDocument `json:"head,omitempty"`
	// Base is the document of the base. Absent when none was read: the base
	// state of every dependency is then unknown.
	Base *SBOMDocument `json:"base,omitempty"`
}

// SBOMDocument is one software bill of materials.
type SBOMDocument struct {
	// Format is cyclonedx or spdx.
	Format string `json:"format"`
	// SpecVersion is the format's version the document states, such as 1.5
	// or 2.3.
	SpecVersion string `json:"specVersion"`
}

// DependencyState is what a software bill of materials says of a package at
// one side of the change.
type DependencyState struct {
	// Licenses are the package's licenses: SPDX ids, expressions or names.
	Licenses []string `json:"licenses,omitempty"`
	// Vulnerabilities are the package's known vulnerabilities.
	Vulnerabilities []Vulnerability `json:"vulnerabilities,omitempty"`
}

// Vulnerability is one known vulnerability of a package.
type Vulnerability struct {
	// ID is the advisory's identifier: CVE-..., GHSA-..., GO-...
	ID string `json:"id"`
	// Severity is critical, high, medium, low, info, none or unknown.
	Severity string `json:"severity"`
}

// Decision is one decision record the change concerns. The producer reads the
// record and matches its scope; the document reports the result.
type Decision struct {
	// Path is the record's path at the commit, or its last path when deleted.
	Path string `json:"path"`
	// Title is the record's title.
	Title string `json:"title"`
	// Status is proposed, accepted, rejected, deprecated, superseded or
	// unknown.
	Status string `json:"status"`
	// Date is the date the record states, YYYY-MM-DD. Absent when it states
	// none.
	Date string `json:"date,omitempty"`
	// Scope lists the path patterns the record governs, repository paths
	// whose segments may hold *, ** and ?.
	Scope []string `json:"scope,omitempty"`
	// Governs lists the changed files the scope matches.
	Governs []string `json:"governs,omitempty"`
	// Change is the record's own file status when the change adds, modifies,
	// renames or deletes it.
	Change string `json:"change,omitempty"`
}
