package sdd

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	proto "go.putnami.dev/protocol/extension"
	sdkmanifest "go.putnami.dev/sdk/extension/manifest"
)

// The four SDD command groups, authored.
//
// This file holds the interactive HALF of the manifest and the assertions that
// pin it. It is separate from manifest_contract_test.go only because of size:
// eighteen subcommands, each with its flat command and its task, is three times
// the manifest the validation jobs needed, and the one-author rule reads better
// when the table it authors is next to it.
//
// # Why every subcommand needs its own flat command
//
// A command group is a NAMING surface: `subcommands.<name>.command` points at a
// flat command in the same manifest, and that command's pipeline is what runs
// (extension.ResolveSubcommand → ext.Jobs[subdef.Command]). The subcommand name
// itself never reaches the process, so two subcommands routed to one flat
// command would be indistinguishable to the binary. Sixteen subcommands
// therefore mean sixteen commands and sixteen tasks, each passing its own
// `<group> <subcommand>` pair as the task's args.
//
// The flat commands are `visibility: "internal"`. They exist to carry a
// pipeline, not to be invoked as `putnami sdd-features-list`; the surface a user
// reads is the group.
//
// # Why none of them is cacheable
//
// Every one is `interactive: true` (D6) — the deliberate scheduler bypass. The
// subprocess inherits the terminal and the CLI never parses its stdout, so
// there is no result document to store and nothing a restored entry could
// replay. `cache: {enabled: false}` states that rather than leaving a policy
// the runner would have to ignore.

// subcommandSpec is one row of the interactive surface: what the user types,
// what the help says, and what the binary is handed.
type subcommandSpec struct {
	// name is the subcommand as typed: `putnami <group> <name>`.
	name string
	// summary is the flat command's one-line description, ported from the CLI
	// command catalog's Summary (commandmeta/catalog_data.go:1044-1270).
	summary string
	// description is the subcommand's help text, ported from the same catalog
	// row's Description.
	description string
	// positionals are the arguments in ARGUMENT ORDER. They are help and
	// completion metadata: arity is enforced by the binary, which owns the error
	// text a user reads.
	positionals []proto.PositionalDefinition
	// flags are the subcommand's OWN flags. Global flags (--projects, --baseline,
	// --output, …) are deliberately absent: the CLI parses them before dispatch
	// and they are documented once, globally. `dry-run` is the exception that
	// proves it — the CLI forwards the global only to a subcommand whose
	// effective flag surface names it.
	flags map[string]proto.FlagDefinition
	// examples are the catalog's runnable examples, in catalog order.
	examples []string
	// mutatesSources marks the subcommands that write into the source tree.
	mutatesSources bool
}

// groupSpec is one command group and its subcommands.
type groupSpec struct {
	name        string
	description string
	subcommands []subcommandSpec
}

// sddCommandGroups is the whole interactive surface, in the order the inventory
// table lists it.
func sddCommandGroups() []groupSpec {
	return []groupSpec{
		{
			name:        "features",
			description: "List, validate, inspect, and compare product feature evidence",
			subcommands: []subcommandSpec{
				{
					name:    "list",
					summary: "List the compact catalog of authored product features",
					description: "List every explicitly authored product feature — native design declarations plus durable " +
						"putnami.features.json entries at the workspace and exact project roots — with its outcome, owner, " +
						"implementing projects, and exact declaration sources. Project selection is applied first and bounds " +
						"the work as well as the answer: only the selected projects' design graphs are read, while durable " +
						"declarations stay workspace-wide so identity and duplicate detection never depend on the selection. " +
						"An optional query then filters ids, names, outcomes, and owners",
					positionals: []proto.PositionalDefinition{{Name: "query"}},
					examples: []string{
						"putnami features list",
						"putnami features list --projects @acme/billing",
						"putnami features list --impacted --output=json",
						`putnami features list "invoice export"`,
					},
				},
				{
					name:    "validate",
					summary: "Validate feature intent, evidence, and contribution references",
					description: "Discover only workspace and exact project-root feature artifacts, validate their strict " +
						"protocol shapes and cross-document references, and report every sorted structural error and " +
						"assessment warning. Missing, stale, contradicted, and unclassified evidence remains advisory. " +
						"Project selection scopes the run to the selected projects' features while still following the " +
						"declarations, evidence, and technical contributions those features depend on across project " +
						"boundaries; a failure wholly outside the selection never fails a scoped run",
					examples: []string{
						"putnami features validate",
						"putnami features validate --impacted",
						"putnami features validate --projects @acme/billing --output=jsonl",
					},
				},
				{
					name:    "snapshot",
					summary: "Summarize the deterministic current-workspace feature view",
					description: "Derive the current feature assessment from authored intent and source-bound evidence. " +
						"Human output is a concise summary; structured output carries the explicitly provisional " +
						"deterministic snapshot, the resolved selection metadata, and sorted diagnostics. Project selection " +
						"scopes the snapshot the same way it scopes validation",
					examples: []string{
						"putnami features snapshot",
						"putnami features snapshot --projects @acme/billing",
						"putnami features snapshot --output=json",
					},
				},
				{
					name:    "inspect",
					summary: "Inspect one feature and its exact evidence provenance",
					description: "Show one feature's authored intent, target and current maturity, every requirement and " +
						"evidence state, exact semantic contribution identities, source-bound declarations and artifacts, " +
						"transport containers, related unclassified facts, revision, and sorted diagnostics. The target is " +
						"already exact, so project selection flags are rejected rather than silently ignored: narrowing an " +
						"exact lookup could only turn a found answer into a missing one",
					positionals: []proto.PositionalDefinition{{Name: "feature-id", Required: true}},
					examples: []string{
						"putnami features inspect billing/checkout",
						"putnami features inspect billing/checkout --output=jsonl",
					},
				},
				{
					name:    "diff",
					summary: "Compare feature evidence between immutable revisions",
					description: "Resolve both revisions to exact commits, independently rebuild each revision's workspace " +
						"membership and source-bound feature assessment directly from Git objects, and report deterministic " +
						"added, removed, promoted, regressed, stale, contradicted, and newly unclassified deltas without " +
						"checkout or worktree mutation. The two revisions are the target, so project selection flags are " +
						"rejected rather than silently ignored",
					positionals: []proto.PositionalDefinition{
						{Name: "base-revision", Required: true},
						{Name: "head-revision", Required: true},
					},
					examples: []string{
						"putnami features diff origin/main HEAD",
						"putnami features diff HEAD~1 HEAD --output=jsonl",
					},
				},
			},
		},
		{
			name:        "specs",
			description: "Discover, inspect, verify, and start durable feature specs",
			subcommands: []subcommandSpec{
				{
					name:    "list",
					summary: "List durable specs with their features and outcomes",
					description: "List every canonical specs/*.json document with the exact feature it details, its file path " +
						"and owning project, its first intended outcomes, and its non-goal, requirement, and decision counts. " +
						"Narrow it by owning project with the normal selection flags; there is deliberately no filename " +
						"filter, because a filename never mints identity: the feature field does",
					examples: []string{
						"putnami specs list",
						"putnami specs list --projects @acme/billing",
						"putnami specs list --impacted --output=json",
					},
				},
				{
					name:    "validate",
					summary: "Validate every spec and report completeness gaps",
					description: "Validate every discovered spec through the durable spec contract — strict shape, canonical " +
						"location, resolvable feature reference, contained decision links, and one spec per feature — then " +
						"report the authoring gaps around it: authored features with no spec, publishable projects with no " +
						"reviewed support entry, and publishable projects with no owning feature link. Contract violations " +
						"fail; completeness gaps stay warnings. Project selection scopes both the validated documents and " +
						"the completeness counts to the selected projects, while duplicate detection stays workspace-wide",
					examples: []string{
						"putnami specs validate",
						"putnami specs validate --impacted",
						"putnami specs validate --projects @acme/billing --output=jsonl",
					},
				},
				{
					name:    "verify",
					summary: "Replay the recorded executable-spec verification verdict",
					description: "Reproduce what the spec gate decided for the selected projects from the record persisted " +
						"beside an engine session (decision 9): per spec-owning project, the effective " +
						"enforce/report/off mode with its provenance, every textual requirement's " +
						"verified/unmapped/unexecutable/missing/stale/contradicted state with per-check detail, the " +
						"observation reports read with their digests, and the recorded blocking decision. --session names " +
						"an exact recorded session; the default is the latest. A spec no session covered is evaluated " +
						"against zero observations through the same shared rule, so nothing unproven reads as green. " +
						"Structural spec errors fail exactly as specs validate fails; an effective off policy reports " +
						"automaticEvaluation: false and succeeds",
					flags: map[string]proto.FlagDefinition{
						"session": {
							Type:        "string",
							Description: "Recorded session ID to replay (default: the latest recorded session).",
						},
					},
					examples: []string{
						"putnami specs verify",
						"putnami specs verify --projects go.putnami.dev/logger",
						"putnami specs verify --session latest --output=json",
					},
				},
				{
					name:    "baseline",
					summary: "Derive the enforced-spec floor and ratchet the committed baseline",
					description: "Derive the enforced-spec floor — every project whose effective specs verification mode is " +
						"enforce, with the feature#requirement identities its criteria make executable — and compare it " +
						"byte-for-byte with the committed specs.baseline.json files, one in the directory of each enforced " +
						"project. --update rewrites them to the canonical derived floor and deletes a file the floor no " +
						"longer names; this command is the one place the ratchet's baseline is " +
						"raised or deliberately lowered, while the validate-workspace task only ever compares. The floor is " +
						"whole-workspace by construction, so project selection flags are rejected rather than silently ignored",
					flags: map[string]proto.FlagDefinition{
						"update": {
							Type:        "boolean",
							Description: "Rewrite the committed specs.baseline.json files to record the derived floor.",
						},
					},
					examples: []string{
						"putnami specs baseline",
						"putnami specs baseline --update",
						"putnami specs baseline --output=json",
					},
					mutatesSources: true,
				},
				{
					name:    "inspect",
					summary: "Inspect one feature's spec, declaration, and decisions",
					description: "Show one already-authored feature's durable declaration, its spec's outcomes, non-goals and " +
						"requirements, every linked decision record with whether that record exists in this worktree, the " +
						"exact source path and owning project, and the diagnostics scoped to that document. No design graph, " +
						"evidence document, or capability manifest is read. The feature is already an exact target, so " +
						"project selection flags are rejected rather than silently ignored",
					positionals: []proto.PositionalDefinition{{Name: "feature-id", Required: true}},
					examples: []string{
						"putnami specs inspect billing/invoice-export",
						"putnami specs inspect billing/invoice-export --output=jsonl",
					},
				},
				{
					name:    "init",
					summary: "Create one minimal spec for an authored feature",
					description: "Create one canonical spec skeleton next to the declaration of an already-authored feature, " +
						"carrying only what that declaration states. It refuses an unknown feature, refuses a feature that " +
						"already has a spec, and never overwrites an existing file; --dry-run prints the target path and the " +
						"exact canonical bytes without writing. Requirements, non-goals, and decision links stay for you to " +
						"author. The feature is an exact target, so project selection flags are rejected rather than " +
						"silently ignored",
					positionals: []proto.PositionalDefinition{{Name: "feature-id", Required: true}},
					// The global --dry-run is forwarded ONLY to a subcommand whose
					// effective flag surface declares it
					// (extension_command.go:180-186), so this declaration is what
					// makes `putnami specs init <id> --dry-run` reach the binary at
					// all. It is not a second flag: the CLI still parses the global.
					flags: map[string]proto.FlagDefinition{
						"dry-run": {
							Type:        "boolean",
							Description: "Print the target path and the exact canonical bytes without writing the file.",
						},
					},
					examples: []string{
						"putnami specs init billing/invoice-export --dry-run",
						"putnami specs init billing/invoice-export",
					},
					mutatesSources: true,
				},
			},
		},
		{
			name:        "architecture",
			description: "Validate and inspect executable domain architecture",
			subcommands: []subcommandSpec{
				{
					name:    "validate",
					summary: "Validate ARC/DARC declarations and structural drift",
					description: "Strictly validate every putnami.architecture.json declaration and its cross-domain " +
						"references, compare exact mapped project dependencies with declared bindings, then fail only for " +
						"new drift, stale debt metadata, expired waivers, or growth of the frozen baseline. Database, HTTP, " +
						"and event observations remain explicitly outside v1 detector coverage",
					examples: []string{
						"putnami architecture validate",
						"putnami architecture validate --baseline origin/main --output=jsonl",
					},
				},
				{
					name:    "snapshot",
					summary: "Emit the deterministic declared and observed architecture",
					description: "Aggregate validated domain declarations into one canonical graph, attach exact cross-domain " +
						"project edges, explicit detector coverage, stable findings, and ratchet dispositions, and emit the " +
						"same snapshot used by validation",
					examples: []string{
						"putnami architecture snapshot",
						"putnami architecture snapshot --output=json",
					},
				},
				{
					name:    "inspect",
					summary: "Inspect one declared domain and its relationships",
					description: "Select one exact manifest domain ID and show its owner, source, projects, owned concepts, " +
						"exports, imports, inbound and outbound DARC edges, observed project dependencies, scoped findings, " +
						"detector coverage, and baseline provenance",
					positionals: []proto.PositionalDefinition{{Name: "domain-id", Required: true}},
					examples: []string{
						"putnami architecture inspect observability",
						"putnami architecture inspect runtime --output=jsonl",
					},
				},
				{
					name:    "init",
					summary: "Scaffold one domain manifest",
					description: "Create one canonical putnami.architecture.json for a domain that does not exist yet, " +
						"carrying only what you stated: the domain id, its owner, and the projects your selection resolved to. " +
						"It refuses a domain another manifest already declares and never overwrites an existing file. " +
						"Exports, imports, and bindings stay for you to author, because each is an agreement between two " +
						"domains that no generator can derive. --dry-run prints the target path and the exact canonical bytes " +
						"without writing",
					positionals: []proto.PositionalDefinition{{Name: "domain-id", Required: true}},
					// The project selection is this subcommand's CONTENT, not its
					// scope, so the selection flags are honored. `owner` and `at`
					// are its own, and the global `--dry-run` reaches it only
					// because the effective flag surface names it
					// (extension_command.go:180-186).
					flags: map[string]proto.FlagDefinition{
						"owner": {
							Type:        "string",
							Description: "Team or role accountable for the domain (default: the domain id).",
						},
						"at": {
							Type:        "string",
							Description: "Workspace-relative directory for the manifest (default: the first mapped project, else the workspace root).",
						},
						"dry-run": {
							Type:        "boolean",
							Description: "Print the target path and the exact canonical bytes without writing the file.",
						},
					},
					examples: []string{
						"putnami architecture init billing --projects /billing,/billing-api --dry-run",
						"putnami architecture init billing --projects /billing --owner billing-team",
					},
					mutatesSources: true,
				},
				{
					name:    "sync",
					summary: "Reconcile the mechanical half of every domain manifest",
					description: "Recompute what follows from the resolved graph and nothing else: drop a project entry the " +
						"workspace no longer contains, drop a binding whose project edge is no longer observed, and propose a " +
						"binding for an observed edge the consumer domain ALREADY declares an import for. It never writes an " +
						"import: an observed dependency with no declared contract is refused and named, and stays a failing " +
						"architecture finding until a human declares it. Without --apply nothing is written; with it, the git " +
						"diff is where each proposed permission is authorized. The reconciliation covers every manifest, so " +
						"project selection flags are rejected rather than silently ignored",
					flags: map[string]proto.FlagDefinition{
						"apply": {
							Type:        "boolean",
							Description: "Write the reconciled manifests instead of only printing the suggestion.",
						},
					},
					examples: []string{
						"putnami architecture sync",
						"putnami architecture sync --apply",
						"putnami architecture sync --output=json",
					},
					mutatesSources: true,
				},
			},
		},
		{
			name:        "contracts",
			description: "Generate and check contract artifacts",
			subcommands: []subcommandSpec{
				{
					name:    "generate",
					summary: "Generate committed contract artifacts from a manifest",
					description: "Validate the authored contract manifest and (atomically) regenerate the committed canonical " +
						"IR, Go type twin, and JSON Schema under schema/",
					flags: map[string]proto.FlagDefinition{
						"project": {Type: "string", Description: "Project ID, path, or name"},
					},
					examples: []string{
						"putnami contracts generate --project /tooling/cli",
						"putnami contracts generate --project @putnami/cli --output=jsonl",
					},
					mutatesSources: true,
				},
				{
					name:    "check",
					summary: "Check contract artifacts for drift and breaking changes",
					description: "Regenerate in memory and fail (exit 2) on artifact drift or on a renamed/removed enum " +
						"value, scope, claim, or grant vs the git-committed prior contract; emits a machine-readable " +
						"compatibility report",
					flags: map[string]proto.FlagDefinition{
						"project": {Type: "string", Description: "Project ID, path, or name"},
					},
					examples: []string{
						"putnami contracts check --project /tooling/cli",
						"putnami contracts check --project @putnami/cli --output=jsonl",
					},
				},
			},
		},
	}
}

// interactiveTimeoutMs bounds a wedged interactive subprocess and nothing else.
//
// Before the extraction these commands ran in-process with no deadline at all,
// so the value is not a budget anybody measured: it is generous on purpose,
// because `features diff` rebuilds two whole revisions out of git objects and a
// deadline that fired on a large repository would be a regression dressed as a
// timeout.
const interactiveTimeoutMs = 600000

// flatCommandName is the internal command one subcommand routes to.
func flatCommandName(group, sub string) string { return "sdd-" + group + "-" + sub }

// taskName is the task that flat command runs.
func taskName(group, sub string) string { return flatCommandName(group, sub) + "-exec" }

// withSDDCommandGroups adds the four groups plus the flat command and task each
// subcommand needs.
func withSDDCommandGroups(builder *sdkmanifest.Builder) *sdkmanifest.Builder {
	for _, group := range sddCommandGroups() {
		options := make([]sdkmanifest.GroupOption, 0, len(group.subcommands))
		for _, sub := range group.subcommands {
			options = append(options, subcommandOption(group.name, sub))
			builder = builder.
				Command(flatCommandName(group.name, sub.name), proto.CommandDefinition{
					Description: sub.summary,
					// The group is the surface; the flat command only carries the
					// pipeline. workspace-once because an SDD answer is about the
					// workspace or about one exact target, never about "each
					// project in turn".
					Visibility: "internal",
					Activation: "workspace-once",
					Run:        []proto.PipelineStep{{ID: "run", Task: taskName(group.name, sub.name)}},
				}).
				Task(taskName(group.name, sub.name), subcommandTask(group.name, sub))
		}
		builder = builder.CommandGroup(group.name, group.description, options...)
	}
	return builder
}

func subcommandOption(group string, sub subcommandSpec) sdkmanifest.GroupOption {
	options := []sdkmanifest.SubcommandOption{
		sdkmanifest.Runs(flatCommandName(group, sub.name)),
		// D6: the scheduler bypass. The subprocess writes the command's own
		// document to the terminal's stdout, which no renderer-driven run can
		// offer.
		sdkmanifest.Interactive(),
	}
	for _, positional := range sub.positionals {
		if positional.Required {
			options = append(options, sdkmanifest.RequiredPositional(positional.Name))
			continue
		}
		options = append(options, sdkmanifest.Positional(positional.Name))
	}
	for _, name := range sortedKeys(sub.flags) {
		options = append(options, sdkmanifest.SubcommandFlag(name, sub.flags[name]))
	}
	for _, example := range sub.examples {
		options = append(options, sdkmanifest.Example(example, ""))
	}
	return sdkmanifest.Subcommand(sub.name, sub.description, options...)
}

func subcommandTask(group string, sub subcommandSpec) proto.TaskDefinition {
	task := proto.TaskDefinition{
		Description: sub.summary + ". Interactive: the CLI runs it with the terminal's own stdout and parses nothing, " +
			"so it writes the command's result envelope (or its human rendering) itself and declares no output ports.",
		Kind:    "command",
		Command: "{extensionRuntime}",
		// The group and the subcommand, in that order, because the CLI appends
		// the user's own tokens after them.
		Args:      []string{group, sub.name},
		Cwd:       "{workspaceRoot}",
		TimeoutMs: interactiveTimeoutMs,
		// Uncacheable, and not as a policy choice: an interactive task produces
		// no result document for the store to keep, so there is nothing a hit
		// could restore.
		Cache:    sdkmanifest.NoCache(),
		Declares: sdkmanifest.Declares(),
	}
	if sub.mutatesSources {
		task.Declares = sdkmanifest.Declares(sdkmanifest.MutatesSources())
		task.Writes = []proto.ResourceRef{sdkmanifest.SourcesWrite()}
	}
	return task
}

// TestCommandGroupsCarryTheWholeInventory pins the surface against the
// inventory table: four groups, eighteen subcommands (the original fourteen, plus
// `specs verify` and `specs baseline`, plus `architecture init`
// and `architecture sync`), and no extras.
func TestCommandGroupsCarryTheWholeInventory(t *testing.T) {
	m := committedManifest(t)

	want := map[string][]string{
		"features":     {"diff", "inspect", "list", "snapshot", "validate"},
		"specs":        {"baseline", "init", "inspect", "list", "validate", "verify"},
		"architecture": {"init", "inspect", "snapshot", "sync", "validate"},
		"contracts":    {"check", "generate"},
	}
	if got := sortedKeys(m.CommandGroups); !reflect.DeepEqual(got, []string{"architecture", "contracts", "features", "specs"}) {
		t.Fatalf("command groups = %v, want the four SDD groups", got)
	}
	for group, subs := range want {
		got := sortedKeys(m.CommandGroups[group].Subcommands)
		if !reflect.DeepEqual(got, subs) {
			t.Errorf("group %q subcommands = %v, want %v", group, got, subs)
		}
	}
}

// reservedFlags are the tokens the CLI parses before dispatch —
// `--projects`, `--impacted`, `--all`, `--baseline`, `--tag`, `--exclude-tag`,
// `--exclude` and `--output` — so a subcommand that declared one would document
// a token that never arrives in its argv. The resolved answers reach the binary
// another way: `selection` on the wire plus three params the CLI forwards, which
// is the point of D3.
var reservedFlags = []string{"projects", "impacted", "all", "baseline", "tag", "exclude-tag", "exclude", "output", "json"}

// TestEverySubcommandRoutesAndRunsTheOneWayD6Settles walks every subcommand row
// ONCE and states the six things that make it dispatchable and readable. They
// were five tests over five copies of this nested walk; the properties are
// unchanged.
//
//  1. `interactive` (D6). A subcommand that lost it would be planned instead of
//     executed directly, and the live renderer would then own stdout — so the
//     ResultV2 envelope the binary writes would be swallowed as job chatter and
//     `putnami features list --output=json` would return nothing a consumer can
//     parse. The failure is silent, which is why it is asserted.
//  2. The routing: it names a flat command this manifest defines, that command
//     is internal (the group is the surface), and it carries exactly one step.
//  3. The argv: the task hands the binary `<group> <subcommand>` through the
//     prepared runtime, rooted at the workspace, and is uncacheable — an
//     interactive run writes no result document for a hit to restore.
//  4. Honest source mutation. Thirteen of the eighteen read and report. Five write
//     into the tree a human then commits — `specs init` creates a spec document,
//     `specs baseline --update` rewrites the committed enforce floor,
//     `contracts generate` rewrites the artifacts under schema/,
//     `architecture init` creates a domain manifest, and `architecture sync
//     --apply` rewrites the mechanical half of existing ones — and only those
//     five may declare `mutatesSources` with the project-scoped `sources` write
//     resource, which is what the planner serializes source mutation on. The set
//     is spelled out here so a subcommand that gained the declaration has to be
//     added to it deliberately.
//  5. No reserved flag, per reservedFlags above.
//  6. The help survived the extraction. The catalog rows the CLI lost carried a
//     description AND runnable examples for every one of the sixteen. A
//     subcommand that arrived here with neither would still work and would still
//     validate; the loss would only show up in `putnami features list --help`,
//     months later.
func TestEverySubcommandRoutesAndRunsTheOneWayD6Settles(t *testing.T) {
	m := committedManifest(t)
	writers := map[string]bool{
		"specs init": true, "specs baseline": true, "contracts generate": true,
		"architecture init": true, "architecture sync": true,
	}

	for _, group := range sortedKeys(m.CommandGroups) {
		definition := m.CommandGroups[group]
		for _, name := range sortedKeys(definition.Subcommands) {
			sub := definition.Subcommands[name]
			path := group + " " + name
			if !sub.Interactive {
				t.Errorf("%s is not interactive; its result envelope would be parsed as job output", path)
			}

			for _, flag := range sortedKeys(sub.Flags) {
				for _, reserved := range reservedFlags {
					if flag == reserved {
						t.Errorf("%s declares --%s, which the CLI consumes before dispatch", path, reserved)
					}
				}
			}

			if len(strings.TrimSpace(sub.Description)) < 80 {
				t.Errorf("%s has a %d-character description; the catalog row it replaces had the full one", path, len(sub.Description))
			}
			if len(sub.Examples) == 0 {
				t.Errorf("%s carries no example; every catalog row it replaces had at least two", path)
			}
			for _, example := range sub.Examples {
				if !strings.HasPrefix(example.Command, "putnami "+path) {
					t.Errorf("%s example %q does not invoke it", path, example.Command)
				}
			}

			flat, found := m.Commands[sub.Command]
			if !found {
				t.Errorf("%s routes to %q, which this manifest does not define", path, sub.Command)
				continue
			}
			if flat.Visibility != "internal" {
				t.Errorf("flat command %q is public; the group is the surface, the command only carries the pipeline", sub.Command)
			}
			if len(flat.Run) != 1 {
				t.Errorf("flat command %q has %d steps; an interactive subcommand runs exactly one", sub.Command, len(flat.Run))
				continue
			}

			taskID := flat.Run[0].Task
			task, found := m.Tasks[taskID]
			if !found {
				t.Errorf("%s runs task %q, which this manifest does not define", path, taskID)
				continue
			}
			if task.Command != "{extensionRuntime}" {
				t.Errorf("task %q runs %q, want the prepared runtime", taskID, task.Command)
			}
			if want := []string{group, name}; !reflect.DeepEqual(task.Args, want) {
				t.Errorf("task %q args = %v, want %v", taskID, task.Args, want)
			}
			if task.Cwd != "{workspaceRoot}" {
				t.Errorf("task %q runs in %q; every SDD answer is rooted at the workspace", taskID, task.Cwd)
			}
			if task.Cache.IsEnabled() {
				t.Errorf("task %q is cacheable; an interactive run writes no result document to store", taskID)
			}

			if task.Declares == nil {
				t.Errorf("task %q carries no v3 declaration", taskID)
				continue
			}
			wantMutates := writers[path]
			if task.Declares.MutatesSources != wantMutates {
				t.Errorf("task %q mutatesSources = %v, want %v", taskID, task.Declares.MutatesSources, wantMutates)
			}
			writesSources := false
			for _, resource := range task.Writes {
				if resource.ID == proto.ResourceIDSources {
					writesSources = true
				}
			}
			if writesSources != wantMutates {
				t.Errorf("task %q writes the sources resource = %v, want %v", taskID, writesSources, wantMutates)
			}
		}
	}
}

// TestTheGroupsShareNoFlagAndSpecsInitKeepsDryRun states the two flag facts
// that are not per-subcommand.
//
// The four SDD groups declare no shared flag at all. And `dry-run` is the
// deliberate exception to reservedFlags, asserted positively: the CLI forwards
// the global ONLY to a subcommand whose effective flag surface names it, so
// `specs init` must declare it to receive it.
func TestTheGroupsShareNoFlagAndSpecsInitKeepsDryRun(t *testing.T) {
	m := committedManifest(t)
	for _, group := range sortedKeys(m.CommandGroups) {
		for name := range m.CommandGroups[group].Flags {
			t.Errorf("group %q declares a shared flag %q; the four SDD groups share none", group, name)
		}
	}
	if _, declared := m.CommandGroups["specs"].Subcommands["init"].Flags["dry-run"]; !declared {
		t.Error("specs init does not declare dry-run; the CLI forwards the global only to a subcommand that names it")
	}
}

// TestAuthoringIsDeterministic keeps the authoring program from being the thing
// that drifts: building it twice must produce the same document, or the
// one-author comparison would fail intermittently on map iteration order. It
// compares the WHOLE canonical document rather than the groups alone, because
// the groups are the largest map here but not the only one.
func TestAuthoringIsDeterministic(t *testing.T) {
	first, err := json.Marshal(authoredManifest(t))
	if err != nil {
		t.Fatalf("encode the first authoring: %v", err)
	}
	second, err := json.Marshal(authoredManifest(t))
	if err != nil {
		t.Fatalf("encode the second authoring: %v", err)
	}
	if canonical(t, canonicalDocument(t, first)) != canonical(t, canonicalDocument(t, second)) {
		t.Fatal("two authorings produced different manifests")
	}
}
