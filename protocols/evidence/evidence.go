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

import diag "go.putnami.dev/protocol/diagnostic"

// ProtocolVersion is the version of the document. A reader refuses any other
// value.
const ProtocolVersion = 1

// Code hosts: where the repository lives.
const (
	ProviderGitHub  = "github"
	ProviderGitLab  = "gitlab"
	ProviderGeneric = "generic"
)

// Providers is the closed code-host vocabulary.
var Providers = []string{ProviderGitHub, ProviderGitLab, ProviderGeneric}

// CI systems: what ran the job that produced the document.
const (
	CISystemGitHubActions = "github-actions"
	CISystemGitLabCI      = "gitlab-ci"
	CISystemGeneric       = "generic"
)

// CISystems is the closed CI-system vocabulary.
var CISystems = []string{CISystemGitHubActions, CISystemGitLabCI, CISystemGeneric}

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
	// SourceFallback is the repository itself, or a top-level directory, when nothing else applies.
	SourceFallback = "fallback"
	// SourceDeclared is a putnami.json the team wrote.
	SourceDeclared = "declared"
)

// ComponentSources is the closed component-source vocabulary.
var ComponentSources = []string{SourceOrchestrator, SourceWorkspace, SourceManifest, SourceFallback, SourceDeclared}

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
	// System is github-actions, gitlab-ci or generic.
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
}
