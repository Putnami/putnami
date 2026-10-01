package extension

import "strings"

// CommandTraits captures the orchestration-relevant semantics of a
// command in machine-readable form. The orchestrator consults traits
// instead of matching command names, so a new extension inherits
// correct scheduling and gating behavior by declaring them (or by using
// a well-known verb, which carries defaults).
//
// A manifest-declared traits object replaces the verb's defaults
// wholesale — fields are not merged.
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
// traits object wins wholesale; otherwise the well-known verb's default
// traits apply; unknown verbs default to the zero traits.
func EffectiveTraits(commandName string, declared *CommandTraits) CommandTraits {
	if declared != nil {
		return *declared
	}
	if spec, ok := WellKnownVerbs[BaseCommandName(commandName)]; ok {
		return spec.Traits
	}
	return CommandTraits{}
}
