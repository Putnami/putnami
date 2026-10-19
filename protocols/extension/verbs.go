package extension

import (
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// CommandTraits captures the orchestration-relevant semantics of a
// command in machine-readable form. The orchestrator consults traits
// instead of matching command names, so a new extension inherits
// correct scheduling and gating behavior by declaring them (or by using
// a well-known verb, which carries defaults).
//
// A manifest-declared traits object replaces the verb's defaults, except
// that a declaration naming no side effects keeps the verb's: `publish`
// always mutates a registry and `deploy` a cloud, so no declaration can make
// either look harmless to the gates that read SideEffects.
//
// Traits describe the COMMAND, never the workload the command runs
// against. Contract 3 deleted the one trait that broke that
// rule — `preflight: config-schema`, which asked core to walk a
// project's Go imports and infra markers to decide whether an artifact
// it does not own had to exist. A task that needs another task's output
// says so with a typed input port (`from: "task"`); a port that is not
// `optional` has no producer at plan time is a plan error naming both
// identities. See doc/02-commands-and-tasks.md.
type CommandTraits struct {
	// Heavy marks the command's jobs as CPU/memory heavy so worker
	// tuning leaves them headroom instead of oversubscribing cores.
	// Per-step granularity is available via PipelineStep.Heavy.
	Heavy bool `json:"heavy,omitempty"`
	// RequiresRunnable skips the command for projects without a
	// runnable entrypoint (libraries).
	RequiresRunnable bool `json:"requiresRunnable,omitempty"`
	// InjectPublishChannels auto-activates the project's declared
	// publish channels as boolean params (e.g. publish: ["go","docker"]
	// activates --go and --docker).
	InjectPublishChannels bool `json:"injectPublishChannels,omitempty"`
	// SideEffects classifies what the command mutates outside the
	// workspace: "" (none), "registry", or "cloud".
	SideEffects string `json:"sideEffects,omitempty"`
	// Kind names what the host reads from the command's results, one of
	// ValidCommandKinds: CommandKindVerification, CommandKindCriteria,
	// CommandKindDiagnostics or CommandKindFiles. Empty means the host reads
	// the command's status only. A command that declares it requires CLI
	// contract protocolcli.CommandKindContract.
	Kind string `json:"kind,omitempty"`
	// Workload names the lifecycle of a command that runs the project's
	// workload, one of ValidWorkloads: WorkloadServe streams it until
	// interrupted, WorkloadOnce streams it once and forwards its exit code.
	// It describes how the command runs, never what the workload contains.
	// Empty means the command runs no workload. A command that declares it
	// requires CLI contract protocolcli.CommandKindContract.
	Workload string `json:"workload,omitempty"`
	// RewritesSources declares that the command rewrites the source tree.
	// It is distinct from a task's declares.mutatesSources, which keeps its
	// cache meaning. A command that declares it requires CLI contract
	// protocolcli.CommandKindContract.
	RewritesSources bool `json:"rewritesSources,omitempty"`
	// ConsumesReleaseSet declares that the command binds a release set and
	// an environment. A command that declares it requires CLI contract
	// protocolcli.CommandKindContract.
	ConsumesReleaseSet bool `json:"consumesReleaseSet,omitempty"`
}

// Command kinds, the closed vocabulary of CommandTraits.Kind. Each names a
// record type the host reads from the command's results, never a verb.
const (
	// CommandKindVerification: the command runs the project's verification;
	// its results carry the test counts and coverage of the verification
	// report.
	CommandKindVerification = "verification"
	// CommandKindCriteria: the command checks the project or the workspace
	// against its criteria.
	CommandKindCriteria = "criteria"
	// CommandKindDiagnostics: the command's result is a tally of errors and
	// warnings.
	CommandKindDiagnostics = "diagnostics"
	// CommandKindFiles: the command's result is a count of produced files.
	CommandKindFiles = "files"
)

// ValidCommandKinds is the CommandTraits.Kind vocabulary in canonical order.
var ValidCommandKinds = []string{CommandKindCriteria, CommandKindDiagnostics, CommandKindFiles, CommandKindVerification}

// Workload lifecycles, the closed vocabulary of CommandTraits.Workload.
const (
	// WorkloadServe streams the project's workload until interrupted.
	WorkloadServe = "serve"
	// WorkloadOnce streams the project's workload once and forwards its exit
	// code.
	WorkloadOnce = "once"
)

// ValidWorkloads is the CommandTraits.Workload vocabulary in canonical order.
var ValidWorkloads = []string{WorkloadOnce, WorkloadServe}

// DeclaresCommandKind reports whether traits declare one of the fields
// protocolcli.CommandKindContract adds: a kind, a workload, rewritesSources
// or consumesReleaseSet.
func (t *CommandTraits) DeclaresCommandKind() bool {
	return t != nil && (t.Kind != "" || t.Workload != "" || t.RewritesSources || t.ConsumesReleaseSet)
}

// ValidateTraits checks the closed vocabularies of a command's or a verb's
// traits: a kind from ValidCommandKinds and a workload from ValidWorkloads.
// field is the path of the traits object, for the diagnostics. It is inert
// for nil traits, and it leaves sideEffects to its callers: a verb keeps the
// side effects WellKnownVerbs gives it (ValidateVerbs).
func ValidateTraits(traits *CommandTraits, field string) []diag.Diagnostic {
	if traits == nil {
		return nil
	}
	var diags []diag.Diagnostic
	if traits.Kind != "" && !slices.Contains(ValidCommandKinds, traits.Kind) {
		diags = append(diags, diag.Errorf("invalid-enum", field+".kind",
			"invalid command kind %q; must be one of: %s", traits.Kind, strings.Join(ValidCommandKinds, ", ")))
	}
	if traits.Workload != "" && !slices.Contains(ValidWorkloads, traits.Workload) {
		diags = append(diags, diag.Errorf("invalid-enum", field+".workload",
			"invalid workload %q; must be one of: %s", traits.Workload, strings.Join(ValidWorkloads, ", ")))
	}
	return diags
}

// SideEffects classifications.
const (
	SideEffectsNone     = ""
	SideEffectsRegistry = "registry"
	SideEffectsCloud    = "cloud"

	// CloudTokenEnv is the process-level transport for an already resource-scoped
	// cloud capability. With a non-empty CloudCapabilityAfterEnv, the CLI captures
	// it before repository-controlled work; token-only local use stays ambient for
	// backward-compatible publish and upgrade commands.
	CloudTokenEnv = "PUTNAMI_CLOUD_TOKEN"
	// CloudCapabilityAfterEnv is runner-owned control data naming the commands
	// whose complete functional leaf set must successfully dominate a cloud or
	// registry job before the scheduler may re-deliver CloudTokenEnv. It is never
	// delivered to extension subprocesses or represented in manifests.
	CloudCapabilityAfterEnv = "PUTNAMI_CLOUD_CAPABILITY_AFTER"
)

// VerbSpec documents a well-known verb: its purpose, whether it targets
// long-running workloads only, and the traits it carries by default.
type VerbSpec struct {
	Description string
	// LongRunning verbs (serve) only apply to workload types with a
	// run-forever lifecycle; jobs and commands skip them.
	LongRunning bool
	Traits      CommandTraits
}

// WellKnownVerbs is the canonical verb vocabulary. Extensions may
// declare any command name, but commands using these names inherit the
// documented semantics and default traits. See doc/06-verbs.md.
var WellKnownVerbs = map[string]VerbSpec{
	"build": {
		Description: "Compile or transpile project sources into runnable or distributable outputs.",
	},
	// test carries no command-level heavy default because heaviness is
	// step-granular: the runner step is heavy, prerequisite steps (e.g.
	// describe/generate) are not. Manifests declare `heavy` on the
	// runner step instead.
	"test": {
		Description: "Run the project's tests, emitting test counts and coverage metrics.",
	},
	"lint": {
		Description: "Check code for style and correctness findings, emitting per-issue diagnostics.",
	},
	"format": {
		Description: "Rewrite sources into canonical formatting.",
	},
	"serve": {
		Description: "Run the project's dev server until interrupted.",
		LongRunning: true,
		Traits:      CommandTraits{RequiresRunnable: true},
	},
	"run": {
		Description: "Execute the project once and exit.",
		Traits:      CommandTraits{RequiresRunnable: true},
	},
	"package": {
		Description: "Assemble distributable artifacts for the project's publish channels.",
		Traits:      CommandTraits{InjectPublishChannels: true},
	},
	"publish": {
		Description: "Push packaged artifacts to registries.",
		Traits: CommandTraits{
			InjectPublishChannels: true,
			SideEffects:           SideEffectsRegistry,
		},
	},
	"deploy": {
		Description: "Converge a cloud environment on the workload's aggregated infra manifest.",
		Traits: CommandTraits{
			SideEffects: SideEffectsCloud,
		},
	},
	"workspace-fetch": {
		Description: "Download the language ecosystem's dependencies for the workspace, running none of their code.",
	},
	"workspace-install": {
		Description: "Install the language ecosystem's dependencies for the workspace.",
	},
	"deps-upgrade": {
		Description: "Upgrade framework and tooling dependencies.",
	},
	"config-extract": {
		Description: "Extract the project's config schema manifest.",
	},
	"config-merge": {
		Description: "Merge config defaults from workspace dependencies.",
	},
}

// BaseCommandName strips a pipeline step suffix from a job name
// (e.g. "build~transpile" → "build").
func BaseCommandName(name string) string {
	if i := strings.IndexByte(name, '~'); i >= 0 {
		return name[:i]
	}
	return name
}

// EffectiveTraits resolves the traits for a command: a manifest-declared
// traits object wins, except that its empty SideEffects keeps the well-known
// verb's; otherwise the well-known verb's default traits apply; unknown verbs
// default to the zero traits. SideEffects holds one value, so a declared side
// effect still replaces the verb's.
func EffectiveTraits(commandName string, declared *CommandTraits) CommandTraits {
	defaults := WellKnownVerbs[BaseCommandName(commandName)].Traits
	if declared == nil {
		return defaults
	}
	traits := *declared
	if traits.SideEffects == SideEffectsNone {
		traits.SideEffects = defaults.SideEffects
	}
	return traits
}
