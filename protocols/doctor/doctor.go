// Package doctor defines the wire contract for `putnami doctor`: the
// deterministic report of production-readiness findings a project accumulates
// under a deployment profile, and the committed waiver file that records which
// findings a team has consciously accepted.
//
// The package is the contract only. It owns three things:
//
//   - the vocabulary — the closed Severity and Profile enums and the frozen
//     ValidCheckCodes taxonomy, each check carrying a baked, actionable
//     remediation string;
//   - the wire types — Finding, Report, Waiver, and WaiverFile, whose field
//     order is the canonical serialization order both the CLI engine and any
//     other producer must reproduce byte-for-byte; and
//   - strict parsing and structural/semantic validation (see strict.go).
//
// Everything that *evaluates* findings is out of scope and lives in the CLI
// engine: the checks themselves, profile resolution, and waiver-clock
// evaluation. In particular:
//
//	IMPORTANT: waiver EXPIRY is never evaluated here. A Waiver's Expires field
//	and a Finding's WaivedBy.Expires field are treated as opaque data: this
//	package validates only that they PARSE as an RFC 3339 date, never comparing
//	them to a clock. Expiry evaluation happens in the CLI engine with an
//	injected clock so the decision is deterministic and testable. Keeping
//	time out of this package is what makes serialization and validation here
//	pure and reproducible.
//
// The check-code taxonomy (ValidCheckCodes) and the Profile enum are consumed
// beyond this package: the evidence producers that feed a check target these
// exact check-code IDs (notably CheckEphemeralSigningKey), and callers select
// and grade by the Profile enum. Both are therefore frozen public identifiers
// rather than internal names.
//
// FREEZE POLICY. Renaming, repurposing, or removing a code — or any change to
// the Profile/Severity enums — is breaking and requires a ProtocolVersion bump.
// ADDING a code is additive and does not: an existing document stays valid
// under the new parser, so a bump would invalidate every already-committed
// doctor.waivers.json (which fails closed, blocking the gate) in exchange for
// nothing a reader can act on. The reverse direction is already handled: an
// older parser meeting a newer code rejects it as out-of-taxonomy — fail-closed,
// not silent acceptance. A code is frozen from the moment it merges. The
// decision and its rejected alternatives are recorded in
// doc/adr/0001-frozen-check-code-taxonomy.md.
//
// Field names are camelCase and field order is deliberate; the canonical
// serialization is json.MarshalIndent(v, "", "  ")+"\n".
package doctor

// ProtocolVersion is the current doctor wire-contract version: the version a
// Report and a WaiverFile stamp and the JSON schema pins. Bumped whenever a
// backwards-incompatible change to either shape, an enum, or the check-code
// taxonomy lands.
const ProtocolVersion = 1

// WaiverFilename is the committed, workspace-root file that carries accepted
// findings. It is authored by a team and reviewed like any other source; the
// CLI engine reads it to suppress waived findings and to surface waiver hygiene
// problems (expired or unknown-code waivers) as findings of their own.
const WaiverFilename = "doctor.waivers.json"

// Severity classifies how urgent a finding is. The enum is closed: adding a
// severity requires a ProtocolVersion bump so consumers can decide how to
// react. Values are listed in ascending urgency, which is also SeverityValues'
// order.
type Severity string

// Severity values, in ascending urgency.
const (
	// SeverityInfo is advisory: worth knowing, never blocking.
	SeverityInfo Severity = "info"
	// SeverityWarning flags a smell that should be addressed but does not by
	// itself make a profile non-compliant.
	SeverityWarning Severity = "warning"
	// SeverityHigh flags a production-readiness gap that should block promotion
	// unless waived.
	SeverityHigh Severity = "high"
	// SeverityCritical flags a gap that must not reach production.
	SeverityCritical Severity = "critical"
)

// ValidSeverities enumerates the canonical severities.
var ValidSeverities = map[Severity]bool{
	SeverityInfo:     true,
	SeverityWarning:  true,
	SeverityHigh:     true,
	SeverityCritical: true,
}

// SeverityValues lists the canonical severities in ascending urgency. Order is
// stable so callers can render or threshold deterministically.
var SeverityValues = []Severity{
	SeverityInfo,
	SeverityWarning,
	SeverityHigh,
	SeverityCritical,
}

// Profile names a deployment profile a report was evaluated under, and the
// profile a finding applies to. The enum is closed: adding a profile requires a
// ProtocolVersion bump. Callers outside this package select and grade by these
// exact value strings, so they are frozen once merged.
type Profile string

// Profile values, in ascending strictness.
const (
	// ProfileDev is the most permissive profile: local development, where
	// ephemeral stores and in-process helpers are expected.
	ProfileDev Profile = "dev"
	// ProfileTest is the CI/test profile: stricter than dev, still tolerant of
	// throwaway infrastructure.
	ProfileTest Profile = "test"
	// ProfileProduction is the strictest profile: durable persistence, external
	// signing keys, secure transport, and shared rate limiting are required.
	ProfileProduction Profile = "production"
)

// ValidProfiles enumerates the canonical profiles.
var ValidProfiles = map[Profile]bool{
	ProfileDev:        true,
	ProfileTest:       true,
	ProfileProduction: true,
}

// ProfileValues lists the canonical profiles in ascending strictness. Order is
// stable so callers can render or threshold deterministically.
var ProfileValues = []Profile{
	ProfileDev,
	ProfileTest,
	ProfileProduction,
}

// CheckCode is a stable identifier for one class of doctor finding. The set is
// a frozen public vocabulary (ValidCheckCodes); every code has a baked remediation
// in CheckRemediations. See the package doc's FREEZE POLICY: renaming,
// repurposing, or removing a code is breaking and requires a ProtocolVersion
// bump; adding one is additive and does not.
type CheckCode string

// CheckCode values. These IDs are frozen once merged: evidence producers target
// them (notably CheckEphemeralSigningKey), the CLI engine emits them into
// reports, and a committed doctor.waivers.json names them.
const (
	// CheckIncompleteCapability marks a capability whose required providers are
	// not all present, so the workload would start degraded.
	CheckIncompleteCapability CheckCode = "doctor.incomplete_capability"
	// CheckMissingRequiredConfig marks a configuration key the active profile
	// requires but that is unset and has no binding.
	CheckMissingRequiredConfig CheckCode = "doctor.missing_required_config"
	// CheckVolatilePersistence marks a datasource bound to an in-memory or
	// otherwise ephemeral store where the profile expects durable persistence.
	CheckVolatilePersistence CheckCode = "doctor.volatile_persistence"
	// CheckEphemeralSigningKey marks an auth signing key generated in-process
	// that would not survive a restart, invalidating issued tokens.
	CheckEphemeralSigningKey CheckCode = "doctor.ephemeral_signing_key"
	// CheckInsecureTransport marks an endpoint served over plaintext transport
	// where the profile requires TLS/HTTPS.
	CheckInsecureTransport CheckCode = "doctor.insecure_transport"
	// CheckLocalRateLimit marks an in-process rate limiter whose limits do not
	// hold across replicas.
	CheckLocalRateLimit CheckCode = "doctor.local_rate_limit"
	// CheckInvalidGeneratedSchema marks a committed generated schema that is
	// stale or does not match its generator's current output.
	CheckInvalidGeneratedSchema CheckCode = "doctor.invalid_generated_schema"
	// CheckConfigShadowing marks a configuration key owned by more than one
	// source, so precedence silently decides the effective value.
	CheckConfigShadowing CheckCode = "doctor.config_shadowing"
	// CheckWaiverExpired marks a waiver in doctor.waivers.json whose expiry has
	// passed. Detecting this requires a clock and is emitted by the CLI engine;
	// this package never evaluates it.
	CheckWaiverExpired CheckCode = "doctor.waiver_expired"
	// CheckWaiverUnknownCode marks a waiver that references a check code the
	// running doctor does not recognize, so it can never match a finding. The
	// CLI engine surfaces it as a finding rather than hard-failing.
	CheckWaiverUnknownCode CheckCode = "doctor.waiver_unknown_code"
	// CheckCommittedManifestStability marks a committed generated artifact whose
	// content is derived from WORKSPACE state rather than from the declared inputs
	// of the project that commits it — a workspace version string, an enumeration
	// of the reachable dependency closure, or a source binding. Such content makes
	// an unrelated edit elsewhere in the workspace re-stamp this project's tracked
	// file, which surfaces as a mutated worktree on a gate that never touched it.
	CheckCommittedManifestStability CheckCode = "doctor.committed_manifest_stability"
	// CheckUndeclaredSchemaCommit marks a project that commits generated schema
	// artifacts without declaring options.generate.schema, so whether those files
	// are tracked depends on an implicit default rather than a reviewed decision.
	CheckUndeclaredSchemaCommit CheckCode = "doctor.undeclared_schema_commit"
	// CheckCRLFCheckout marks a checkout whose line endings differ from the
	// committed bytes: a tracked text file has CRLF endings in the working tree
	// while the index has LF, or Git converts endings on checkout
	// (core.autocrlf=true) and no LF policy in .gitattributes covers the
	// workspace. Every content digest then differs from the same commit checked
	// out elsewhere.
	CheckCRLFCheckout CheckCode = "doctor.crlf_checkout"
	// CheckLongPathsDisabled marks a Windows host whose LongPathsEnabled
	// registry value is not 1, so programs without their own long-path
	// handling fail on paths longer than 260 characters.
	CheckLongPathsDisabled CheckCode = "doctor.long_paths_disabled"
	// CheckGitLongPathsDisabled marks a Windows host where Git does not set
	// core.longpaths, so Git cannot read or check out paths longer than 260
	// characters.
	CheckGitLongPathsDisabled CheckCode = "doctor.git_long_paths_disabled"
	// CheckVCRuntimeMissing marks a Windows host without the Microsoft Visual
	// C++ runtime (vcruntime140.dll in the system directory). Biome, the
	// formatter and linter the TypeScript extension runs, imports it, and
	// Windows does not start Biome without it.
	CheckVCRuntimeMissing CheckCode = "doctor.vc_runtime_missing"
	// CheckMissingReadme marks a project without a README.md at its root, the
	// page a reader or an agent opens first to learn what the project is for,
	// how to run its checks and where its documentation lives.
	CheckMissingReadme CheckCode = "doctor.missing_readme"
)

// ValidCheckCodes enumerates the canonical, frozen check-code taxonomy.
var ValidCheckCodes = map[CheckCode]bool{
	CheckIncompleteCapability:       true,
	CheckMissingRequiredConfig:      true,
	CheckVolatilePersistence:        true,
	CheckEphemeralSigningKey:        true,
	CheckInsecureTransport:          true,
	CheckLocalRateLimit:             true,
	CheckInvalidGeneratedSchema:     true,
	CheckConfigShadowing:            true,
	CheckWaiverExpired:              true,
	CheckWaiverUnknownCode:          true,
	CheckCommittedManifestStability: true,
	CheckUndeclaredSchemaCommit:     true,
	CheckCRLFCheckout:               true,
	CheckLongPathsDisabled:          true,
	CheckGitLongPathsDisabled:       true,
	CheckVCRuntimeMissing:           true,
	CheckMissingReadme:              true,
}

// CheckRemediations maps every check code to its baked, actionable remediation:
// the concrete next step a producer stamps into a Finding.Remediation. The
// strings are part of the frozen taxonomy — keep them stable and actionable.
var CheckRemediations = map[CheckCode]string{
	CheckIncompleteCapability:       "Declare the missing provider so the capability is complete, then re-run `putnami doctor --profile production`.",
	CheckMissingRequiredConfig:      "Set the required configuration key for this profile (or supply it through a binding) and re-run `putnami doctor`.",
	CheckVolatilePersistence:        "Bind the datasource to a durable, provisioned store instead of an in-memory or ephemeral one before promoting to production.",
	CheckEphemeralSigningKey:        "Configure a persistent, externally managed signing key instead of a process-generated ephemeral key so issued tokens survive a restart.",
	CheckInsecureTransport:          "Require TLS/HTTPS for this endpoint; plaintext transport is not allowed under the production profile.",
	CheckLocalRateLimit:             "Replace the in-process rate limiter with a shared/distributed limiter so limits hold across replicas.",
	CheckInvalidGeneratedSchema:     "Regenerate the committed schema with `putnami build` (or the owning generator) and commit the refreshed artifact.",
	CheckConfigShadowing:            "Remove the shadowing configuration source or rename the key so a single source owns it; see the config precedence order.",
	CheckWaiverExpired:              "Renew the entry in doctor.waivers.json by updating `expires`, or fix the underlying finding and delete the waiver.",
	CheckWaiverUnknownCode:          "Correct the `code` of the waiver in doctor.waivers.json to a current doctor check, or remove the stale waiver.",
	CheckCommittedManifestStability: "Regenerate the artifact with the current generator and commit it; a committed generated file must depend only on its own project's declared inputs, never on a workspace version, a dependency closure, or a source binding.",
	CheckUndeclaredSchemaCommit:     "Declare the commit regime explicitly in the project's putnami.json `options.generate.schema` — false keeps generated schemas in the gitignored .gen/ tree (the recommended default for applications), true opts into tracking them.",
	CheckCRLFCheckout:               "Add `* text=auto eol=lf` to the workspace-root .gitattributes and commit it, then, with no uncommitted changes, run `git rm --cached -r -q . && git reset --hard` so every tracked text file is checked out with LF endings.",
	CheckLongPathsDisabled:          "Enable Win32 long paths: in an elevated PowerShell, run `New-ItemProperty -Path 'HKLM:\\SYSTEM\\CurrentControlSet\\Control\\FileSystem' -Name LongPathsEnabled -Value 1 -PropertyType DWORD -Force`, then open a new terminal.",
	CheckGitLongPathsDisabled:       "Run `git config --global core.longpaths true` so Git for Windows reads and checks out paths longer than 260 characters.",
	CheckVCRuntimeMissing:           "Install the Microsoft Visual C++ Redistributable for x64: run `winget install --id Microsoft.VCRedist.2015+.x64`, or download and run https://aka.ms/vs/17/release/vc_redist.x64.exe.",
	CheckMissingReadme:              "Add a README.md at the project root with a one-line purpose, the project's putnami commands, and where its documentation lives; `putnami lint` then checks its links.",
}

// Remediation returns the baked remediation for a check code, or the empty
// string when the code is not part of the taxonomy.
func Remediation(code CheckCode) string {
	return CheckRemediations[code]
}

// Evidence points a reviewer at what produced a finding. It carries only a
// workspace-relative source Path and a configuration/field name Field.
//
// Evidence deliberately has NO slot for a resolved value: a doctor report is a
// committed, reviewable artifact and must never carry cleartext configuration
// values (which may be secrets). It names where to look, not what was found.
type Evidence struct {
	// Path is a workspace-relative path to the file that produced the finding.
	Path string `json:"path,omitempty"`
	// Field is the configuration path or field name the finding concerns, e.g.
	// "database.default.driver". It is a name, never a resolved value.
	Field string `json:"field,omitempty"`
}

// WaiverProvenance records that a finding was waived and by whom. It is set by
// the CLI engine when a committed waiver matched the finding; it mirrors the
// human-authored fields of the matching Waiver (owner, reason, expires) so a
// reader of the report can see why a finding is suppressed without opening the
// waiver file.
//
// Expires is opaque data here (see the package doc): validated for
// parsability, never compared to a clock.
type WaiverProvenance struct {
	// Owner is the person or team that accepted the finding.
	Owner string `json:"owner"`
	// Reason explains why the finding was accepted.
	Reason string `json:"reason"`
	// Expires is the RFC 3339 date the waiver lapses. Opaque to this package.
	Expires string `json:"expires"`
}

// Finding is one production-readiness problem doctor detected for a project.
//
// Field order is deliberate: it is the canonical serialization order the CLI
// engine (and any other producer) must reproduce byte-for-byte.
type Finding struct {
	// Code identifies the class of problem; it must be a member of ValidCheckCodes.
	Code CheckCode `json:"code"`
	// Severity classifies urgency.
	Severity Severity `json:"severity"`
	// Profile is the deployment profile this finding applies to.
	Profile Profile `json:"profile"`
	// Project is the affected project: a workspace-relative path or a project id.
	Project string `json:"project"`
	// Message is the human-readable description of the problem.
	Message string `json:"message"`
	// Evidence lists where to look; each entry is a path and/or field name only.
	Evidence []Evidence `json:"evidence,omitempty"`
	// Remediation is the actionable next step; producers stamp the baked
	// CheckRemediations entry for Code.
	Remediation string `json:"remediation"`
	// WaivedBy, when set, records that a committed waiver suppressed this finding.
	WaivedBy *WaiverProvenance `json:"waivedBy,omitempty"`
}

// Summary tallies the findings in a report. The counts must be internally
// consistent (see ValidateReport): the per-severity counts sum to Findings, and
// Waived counts findings that carry WaivedBy provenance. The summary is always
// present, even when zero, so the wire shape is stable.
type Summary struct {
	// Findings is the total number of findings in the report.
	Findings int `json:"findings"`
	// Info is the number of info-severity findings.
	Info int `json:"info"`
	// Warning is the number of warning-severity findings.
	Warning int `json:"warning"`
	// High is the number of high-severity findings.
	High int `json:"high"`
	// Critical is the number of critical-severity findings.
	Critical int `json:"critical"`
	// Waived is the number of findings suppressed by a matching waiver.
	Waived int `json:"waived"`
}

// Report is the deterministic result of one `putnami doctor` evaluation for a
// single profile.
//
// Field order is deliberate: it is the canonical serialization order.
type Report struct {
	// Schema is the optional URI of the JSON schema describing this report.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the doctor wire-contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Profile is the deployment profile the report was evaluated under.
	Profile Profile `json:"profile"`
	// Findings lists the detected problems in a stable, producer-defined order.
	Findings []Finding `json:"findings,omitempty"`
	// Summary tallies the findings; it must agree with Findings.
	Summary Summary `json:"summary"`
}

// Waiver is one accepted finding recorded in the committed WaiverFile. A waiver
// matches a finding by Code and, when Project is set, by project; an empty
// Project makes the waiver apply workspace-wide. When Field is set, the waiver
// additionally narrows to the single finding whose evidence field matches, so a
// project with several findings of one code can accept them one at a time
// (without Field a waiver suppresses every finding of its code in scope).
//
// Field is additive and emitted with omitempty: an unset value serializes to
// nothing, so the canonical form of an existing waiver is byte-for-byte
// unchanged. A parser that predates Field rejects a Field-bearing waiver as an
// unknown field, which fails closed (the gate blocks) rather than mis-applying
// a broad waiver.
//
// Expires is opaque data here (see the package doc): validated for
// parsability, never compared to a clock — expiry evaluation is the CLI
// engine's job.
type Waiver struct {
	// Code is the check code being waived; it must be a member of ValidCheckCodes.
	Code CheckCode `json:"code"`
	// Project scopes the waiver to one project (workspace-relative path or id).
	// Empty means the waiver applies to every project.
	Project string `json:"project,omitempty"`
	// Field, when set, narrows the waiver to the single finding whose first
	// evidence field equals it (a config path or field name). Empty means the
	// waiver matches every finding of its code within the project scope.
	Field string `json:"field,omitempty"`
	// Owner is the person or team accepting the finding.
	Owner string `json:"owner"`
	// Reason explains why the finding is accepted.
	Reason string `json:"reason"`
	// Expires is the RFC 3339 date the waiver lapses. Opaque to this package.
	Expires string `json:"expires"`
}

// WaiverFile is the committed, workspace-root doctor.waivers.json: the reviewed
// list of accepted findings.
//
// Field order is deliberate: it is the canonical serialization order.
type WaiverFile struct {
	// Schema is the optional URI of the JSON schema describing this file.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the doctor wire-contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Waivers lists the accepted findings.
	Waivers []Waiver `json:"waivers,omitempty"`
}
