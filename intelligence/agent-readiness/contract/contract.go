// Package contract is the single definition of what the agent-readiness
// collector sends and what the report renders. The CLI fills a Payload;
// intelligence-api accepts it with papi.Type[Payload]() and answers a Report.
// The public JSON Schemas under schema/ are extracted from intelligence-api's
// OpenAPI components, which are generated from these types.
//
// The contract is additive-only once released. Framework decoders refuse an
// unknown field, so intelligence-api deploys before any collector that sends
// a new field, and a collector never sends a field its MethodVersion does not
// declare. The method is documented in https://putnami.dev/agent-readiness/method.
package contract

import "time"

// SchemaVersion is the version of the public payload and report schemas.
const SchemaVersion = 1

// MethodVersion identifies the marker set collected for the service.
const MethodVersion = "0.4"

// Level is the autonomy an area supports, defined by what the human stops
// doing: L1 Assist, L2 Supervised, L3 Delegated, L4 Exception-based.
type Level string

// The four levels, from least to most autonomous.
const (
	LevelAssist         Level = "L1"
	LevelSupervised     Level = "L2"
	LevelDelegated      Level = "L3"
	LevelExceptionBased Level = "L4"
)

// Step is one step of the loop an agent runs on every task.
type Step string

// The four steps, in loop order.
const (
	StepUnderstand Step = "understand"
	StepBound      Step = "bound"
	StepVerify     Step = "verify"
	StepRecover    Step = "recover"
)

// MarkerState is what the collector observed for a marker, before any
// threshold: absent (L1), exists (L2), or enforced by a tool (L3). Only the
// server decides L4, from the marker's 90-day Value.
type MarkerState string

// The three states a collector can observe.
const (
	MarkerAbsent   MarkerState = "absent"
	MarkerExists   MarkerState = "exists"
	MarkerEnforced MarkerState = "enforced"
)

// AreaSource says whether an area comes from a workspace manifest or from the
// collector's directory inference.
type AreaSource string

// The two area sources.
const (
	AreaFromManifest AreaSource = "manifest"
	AreaInferred     AreaSource = "inferred"
)

// AreaRole says whether an area holds code or supports it. A test tree and a
// documentation site have no public entry point to protect, so the method
// exempts them from the boundary markers.
type AreaRole string

// The three area roles. A payload without a role, such as every method 0.1
// payload, describes a code area.
const (
	AreaCode  AreaRole = "code"
	AreaTests AreaRole = "tests"
	AreaDocs  AreaRole = "docs"
)

// Payload is what the collector sends. It carries counts, area names and
// repository-relative paths only: no file contents, no author identities and
// no absolute paths.
type Payload struct {
	Meta      PayloadMeta `json:"meta" validate:"required"`
	Inventory Inventory   `json:"inventory" validate:"required"`
	// Areas partition the repository. Their ShareOfChange values need not sum
	// to 1: a commit that touches two areas counts in both.
	Areas   []Area   `json:"areas" validate:"required"`
	Markers []Marker `json:"markers" validate:"required"`
	// AgentProbe is reserved for the opt-in agent probe. Collectors send null
	// until the method declares it.
	AgentProbe *AgentProbe `json:"agentProbe" description:"Reserved for the opt-in agent probe. Always null in methods 0.1 to 0.3."`
}

// PayloadMeta identifies the collector run without identifying the machine or
// the person that ran it.
type PayloadMeta struct {
	CollectorVersion string    `json:"collectorVersion" validate:"required,minlen=1,maxlen=64"`
	MethodVersion    string    `json:"methodVersion" validate:"required,pattern=^[0-9]+\\.[0-9]+$"`
	CollectedAt      time.Time `json:"collectedAt" validate:"required"`
	// RepoFingerprint is the lowercase hex SHA-256 of the lexically smallest
	// root commit id. It recognizes the same repository across runs and
	// clones without sending its name or remote.
	RepoFingerprint string `json:"repoFingerprint" validate:"required,pattern=^[0-9a-f]{64}$" description:"Lowercase hex SHA-256 of the lexically smallest root commit id."`
	// HeadCommit is the full id of the commit the collector read.
	HeadCommit string `json:"headCommit" validate:"required,pattern=^([0-9a-f]{40}|[0-9a-f]{64})$"`
}

// Inventory describes the product the repository builds. Every list is
// sorted by name and may be empty.
type Inventory struct {
	Languages        []LanguageShare   `json:"languages" validate:"required"`
	Frameworks       []Tool            `json:"frameworks" validate:"required"`
	PackageManagers  []Tool            `json:"packageManagers" validate:"required"`
	MonorepoTools    []Tool            `json:"monorepoTools" validate:"required"`
	CIProviders      []Tool            `json:"ciProviders" validate:"required"`
	IaC              []Tool            `json:"iac" validate:"required"`
	Containers       []Tool            `json:"containers" validate:"required"`
	Databases        []Tool            `json:"databases" validate:"required"`
	TestFrameworks   []Tool            `json:"testFrameworks" validate:"required"`
	AgentTools       []Tool            `json:"agentTools" validate:"required"`
	InstructionFiles []InstructionFile `json:"instructionFiles" validate:"required"`
	// Files and Lines count tracked text files and their lines at HeadCommit.
	Files int `json:"files" validate:"required,min=0"`
	Lines int `json:"lines" validate:"required,min=0"`
	// RepoAgeDays is the age of the oldest root commit on CollectedAt.
	RepoAgeDays int `json:"repoAgeDays" validate:"required,min=0"`
	// CommitsTotal counts commits reachable from HeadCommit.
	CommitsTotal int `json:"commitsTotal" validate:"required,min=0"`
	// Commits90d counts commits reachable from HeadCommit in the 90 days
	// before CollectedAt. It is the commit velocity.
	Commits90d int `json:"commits90d" validate:"required,min=0"`
	// ActiveContributors90d counts distinct author emails in those commits.
	ActiveContributors90d int `json:"activeContributors90d" validate:"required,min=0"`
	// AgentCommits90d counts the commits of Commits90d that credit an agent
	// tool, in a Co-Authored-By trailer or as their author. Method 0.3
	// declares it; a 0.1 or 0.2 payload never sends it.
	AgentCommits90d *int `json:"agentCommits90d,omitempty" validate:"min=0" description:"Commits of the last 90 days that credit an agent tool in a Co-Authored-By trailer or as author. Declared by method 0.3."`
	// AgentCommitsReverted90d counts those of AgentCommits90d that a later
	// commit of the window reverted.
	AgentCommitsReverted90d *int `json:"agentCommitsReverted90d,omitempty" validate:"min=0" description:"Agent commits of the last 90 days that a later commit of the window reverted. Declared by method 0.3."`
}

// LanguageShare is one language and the tracked lines written in it.
type LanguageShare struct {
	Name  string `json:"name" validate:"required,minlen=1,maxlen=64"`
	Files int    `json:"files" validate:"required,min=0"`
	Lines int    `json:"lines" validate:"required,min=0"`
}

// Tool is a detected framework, package manager, provider or agent tool.
// Version is omitted when the repository does not pin one.
type Tool struct {
	Name    string `json:"name" validate:"required,minlen=1,maxlen=64"`
	Version string `json:"version,omitempty" validate:"maxlen=64"`
}

// InstructionFile is an agent instruction file such as AGENTS.md or
// CLAUDE.md. Only its location, size and age leave the machine.
type InstructionFile struct {
	Path  string `json:"path" validate:"required,minlen=1,maxlen=512,pattern=^[^/\\\\~\\s][^\\\\]*$"`
	Bytes int    `json:"bytes" validate:"required,min=0"`
	// AgeDays is the number of days since the file last changed.
	AgeDays int `json:"ageDays" validate:"required,min=0"`
}

// Area is one part of the repository an agent can work in: a workspace
// member from a manifest, or a directory the collector inferred. Name is
// unique within a payload: markers, report areas and findings refer to an
// area by name.
type Area struct {
	Name   string     `json:"name" validate:"required,minlen=1,maxlen=128"`
	Path   string     `json:"path" validate:"required,minlen=1,maxlen=512,pattern=^(\\.|[^/\\\\~\\s][^\\\\]*)$" description:"Repository-relative, slash-separated path; \".\" for the root."`
	Source AreaSource `json:"source" validate:"required,oneof=manifest|inferred"`
	// Role says whether the area holds code, a test tree or a documentation
	// site. Method 0.2 declares it; an absent role reads as code.
	Role AreaRole `json:"role,omitempty" validate:"oneof=code|tests|docs" description:"code, tests (a test tree) or docs (a documentation site). Declared by method 0.2; absent reads as code."`
	// Language is the area's language with the most lines, empty when the
	// area holds no recognized source file.
	Language   string `json:"language,omitempty" validate:"maxlen=64"`
	Files      int    `json:"files" validate:"required,min=0"`
	Lines      int    `json:"lines" validate:"required,min=0"`
	Commits90d int    `json:"commits90d" validate:"required,min=0"`
	// ShareOfChange is Commits90d over Inventory.Commits90d, from 0 to 1.
	ShareOfChange float64 `json:"shareOfChange" validate:"required,min=0,max=1"`
	// Authors90d are per-run pseudonyms of the area's authors in the last 90
	// days: the first 16 hex digits of SHA-256(salt + ":" + lowercase author
	// email), where salt is 32 random bytes the collector draws once per run
	// and never sends. The same author gets the same pseudonym in every area
	// of one payload, so the server counts distinct authors, and no one can
	// test a guessed email against it.
	Authors90d []string `json:"authors90d" validate:"required" description:"First 16 hex digits of SHA-256(salt + \":\" + lowercase author email), one per distinct author; the salt is random per run and never sent."`
}

// Marker is one deterministic observation. A marker with an empty Area
// applies to every area that does not carry the same marker id itself.
type Marker struct {
	ID   string `json:"id" validate:"required,maxlen=64,pattern=^(understand|bound|verify|recover)\\.[a-z0-9-]+$" description:"Marker id defined on the method page."`
	Area string `json:"area,omitempty" validate:"maxlen=128" description:"Area name; empty for a repository-wide marker."`
	// State is what the collector observed. The server alone applies the
	// thresholds that turn State and Value into a level.
	State MarkerState `json:"state" validate:"required,oneof=absent|exists|enforced"`
	// Value is the marker's 90-day measurement in the unit the method page
	// declares for its id, or null when the marker has none for this area.
	Value *float64 `json:"value" description:"The marker's 90-day measurement in the unit the method page declares; null when not measured."`
	// EnforcedDays is how many days the treatment that makes the marker
	// enforced has been in place, for a marker whose Value measures something
	// else: L4 needs the treatment to hold over a full 90-day window. Method
	// 0.2 sends it for bound.cross-area-changes; method 0.3 also for
	// understand.instructions, understand.area-docs, bound.declared-areas,
	// recover.ownership and recover.small-changes.
	EnforcedDays *float64 `json:"enforcedDays,omitempty" description:"Days the treatment that enforces the marker has been in place, for a marker whose value measures something else. Declared by method 0.2 for bound.cross-area-changes and by method 0.3 for every marker whose L4 needs it."`
	Evidence     Evidence `json:"evidence" validate:"required"`
}

// Evidence lets anyone reproduce a marker on their own checkout.
type Evidence struct {
	// Command reproduces the observation from the repository root.
	Command string `json:"command" validate:"required,minlen=1,maxlen=300,pattern=^[^\\r\\n]+$"`
	// Sample lists up to five repository-relative locations that support the
	// observation, each a path with an optional ":line". Never a file's
	// contents.
	Sample []string `json:"sample" validate:"required" description:"Up to five repository-relative locations, each a path with an optional :line. Never file contents."`
}

// AgentProbe is reserved for the opt-in agent probe (method 0.3 or later).
type AgentProbe struct{}

// Submission is what intelligence-api answers to a submitted payload: the
// verdict the terminal prints and the report link. It is frozen. A generated
// client refuses a response member it does not know, so a field added here
// would break every installed CLI; new report content goes to Report, which
// only the report page reads.
type Submission struct {
	Token         string `json:"token" validate:"required,minlen=6,maxlen=32,pattern=^[a-z0-9]+$"`
	URL           string `json:"url" validate:"required,url"`
	MethodVersion string `json:"methodVersion" validate:"required,pattern=^[0-9]+\\.[0-9]+$"`
	Level         Level  `json:"level" validate:"required,oneof=L1|L2|L3|L4"`
	// ShareBlocked is Report.ShareBlocked, the share the terminal prints.
	ShareBlocked float64 `json:"shareBlocked" validate:"required,min=0,max=1"`
}

// Report is what the report page renders for a submitted payload. The page is
// deployed with intelligence-api, so Report may grow additively.
type Report struct {
	Token string `json:"token" validate:"required,minlen=6,maxlen=32,pattern=^[a-z0-9]+$"`
	// URL is the report page, opened after GitHub sign-in.
	URL           string `json:"url" validate:"required,url"`
	MethodVersion string `json:"methodVersion" validate:"required,pattern=^[0-9]+\\.[0-9]+$"`
	// CreatedAt is when the server scored the payload.
	CreatedAt time.Time `json:"createdAt" validate:"required"`
	// HeadCommit and CollectedAt repeat the payload's meta: the commit the
	// report describes and when it was read.
	HeadCommit  string    `json:"headCommit" validate:"required,pattern=^([0-9a-f]{40}|[0-9a-f]{64})$"`
	CollectedAt time.Time `json:"collectedAt" validate:"required"`
	// Level is the highest level whose areas carry at least half of the
	// recent change.
	Level Level `json:"level" validate:"required,oneof=L1|L2|L3|L4"`
	// ShareBlocked is the share of recent change, from 0 to 1, that lands in
	// areas below L3, where an agent cannot yet work alone.
	ShareBlocked float64 `json:"shareBlocked" validate:"required,min=0,max=1"`
	// Steps is the repository's level per step, weighted like Level.
	Steps     StepLevels   `json:"steps" validate:"required"`
	Areas     []ReportArea `json:"areas" validate:"required"`
	Findings  []Finding    `json:"findings" validate:"required" description:"At most three findings, one per area, from areas with at least 5% of the change, ranked by share of change times the gap to the goal."`
	Inventory Inventory    `json:"inventory" validate:"required"`
	Goal      *Goal        `json:"goal" description:"The three goal answers; null until the signed-in owner answers them."`
	Claim     Claim        `json:"claim" validate:"required"`
}

// PublicReportSummary is what anyone holding a report link may read without
// signing in: the verdict, the level ladder and a few counts. It carries no
// path, no area name and no finding text, so a leaked link shows how ready a
// repository is and nothing about its layout. Reading it never claims the
// report, and the owner's goal answers stay out of it.
type PublicReportSummary struct {
	Token         string `json:"token" validate:"required,minlen=6,maxlen=32,pattern=^[a-z0-9]+$"`
	MethodVersion string `json:"methodVersion" validate:"required,pattern=^[0-9]+\\.[0-9]+$"`
	// CreatedAt is when the server scored the payload.
	CreatedAt time.Time `json:"createdAt" validate:"required"`
	// HeadCommit is the commit the report describes, and CollectedAt when it
	// was read.
	HeadCommit  string    `json:"headCommit" validate:"required,pattern=^([0-9a-f]{40}|[0-9a-f]{64})$"`
	CollectedAt time.Time `json:"collectedAt" validate:"required"`
	// Level, ShareBlocked and Steps are Report's: the verdict sentence and
	// the ladder are rendered from them.
	Level        Level      `json:"level" validate:"required,oneof=L1|L2|L3|L4"`
	ShareBlocked float64    `json:"shareBlocked" validate:"required,min=0,max=1"`
	Steps        StepLevels `json:"steps" validate:"required"`
	// FindingCount is how many findings the full report lists.
	FindingCount int `json:"findingCount" validate:"required,min=0" description:"How many findings the full report lists; their text is for the signed-in owner."`
	// CommitsTotal, Commits90d and RepoAgeDays repeat the inventory counts.
	CommitsTotal int `json:"commitsTotal" validate:"required,min=0"`
	Commits90d   int `json:"commits90d" validate:"required,min=0"`
	RepoAgeDays  int `json:"repoAgeDays" validate:"required,min=0"`
}

// StepLevels holds one level per step of the loop.
type StepLevels struct {
	Understand Level `json:"understand" validate:"required,oneof=L1|L2|L3|L4"`
	Bound      Level `json:"bound" validate:"required,oneof=L1|L2|L3|L4"`
	Verify     Level `json:"verify" validate:"required,oneof=L1|L2|L3|L4"`
	Recover    Level `json:"recover" validate:"required,oneof=L1|L2|L3|L4"`
}

// ReportArea is one area's level. An area supports the level of its weakest
// step.
type ReportArea struct {
	Name          string     `json:"name" validate:"required,minlen=1,maxlen=128"`
	Path          string     `json:"path" validate:"required,minlen=1,maxlen=512"`
	Language      string     `json:"language,omitempty" validate:"maxlen=64"`
	ShareOfCode   float64    `json:"shareOfCode" validate:"required,min=0,max=1"`
	ShareOfChange float64    `json:"shareOfChange" validate:"required,min=0,max=1"`
	Level         Level      `json:"level" validate:"required,oneof=L1|L2|L3|L4"`
	Steps         StepLevels `json:"steps" validate:"required"`
}

// Finding is one of the gaps that block the next level, with the evidence
// that shows it and the change that closes it.
type Finding struct {
	ID       string `json:"id" validate:"required,minlen=1,maxlen=64"`
	MarkerID string `json:"markerId" validate:"required,maxlen=64,pattern=^(understand|bound|verify|recover)\\.[a-z0-9-]+$"`
	Title    string `json:"title" validate:"required,minlen=1,maxlen=200"`
	Step     Step   `json:"step" validate:"required,oneof=understand|bound|verify|recover"`
	Area     string `json:"area" validate:"required,minlen=1,maxlen=128"`
	// Evidence lists what was observed, each item with the command that
	// reproduces it, and what was inferred, each with the rule applied.
	Evidence []FindingEvidence `json:"evidence" validate:"required"`
	Impact   string            `json:"impact" validate:"required,minlen=1,maxlen=600"`
	Fix      string            `json:"fix" validate:"required,minlen=1,maxlen=600"`
	// UnlockShare is the share of recent change, from 0 to 1, the fix would
	// move to the next level.
	UnlockShare float64 `json:"unlockShare" validate:"required,min=0,max=1"`
}

// EvidenceKind separates what the collector observed from what the method
// inferred.
type EvidenceKind string

// The two evidence kinds.
const (
	EvidenceObserved EvidenceKind = "observed"
	EvidenceInferred EvidenceKind = "inferred"
)

// FindingEvidence is one line of a finding's evidence.
type FindingEvidence struct {
	Kind EvidenceKind `json:"kind" validate:"required,oneof=observed|inferred"`
	Text string       `json:"text" validate:"required,minlen=1,maxlen=400"`
	// Command reproduces an observed fact; empty for an inference.
	Command string `json:"command,omitempty" validate:"maxlen=300"`
	// Rule names the published rule behind an inference; empty for an
	// observation.
	Rule string `json:"rule,omitempty" validate:"maxlen=120"`
}

// Goal holds the three answers the signed-in owner gives on the report page.
type Goal struct {
	// Want is what an agent should do without a human.
	Want Level `json:"want" validate:"required,oneof=L1|L2|L3|L4"`
	// Allow is what agents do in this codebase today; "none" when nothing.
	Allow string `json:"allow" validate:"required,oneof=none|L1|L2|L3|L4"`
	// Agents are the agent tools the team uses, as lowercase slugs such as
	// "claude-code", "cursor", "codex", "github-copilot" or "other".
	Agents []string `json:"agents" validate:"required"`
}

// ClaimState says whether a signed-in visitor owns the report yet.
type ClaimState string

// The two claim states.
const (
	ClaimUnclaimed ClaimState = "unclaimed"
	ClaimClaimed   ClaimState = "claimed"
)

// Claim is the report's ownership. The first signed-in visitor claims it.
type Claim struct {
	State     ClaimState `json:"state" validate:"required,oneof=unclaimed|claimed"`
	ClaimedAt *time.Time `json:"claimedAt" description:"When the first signed-in visitor claimed the report; null while unclaimed."`
}
