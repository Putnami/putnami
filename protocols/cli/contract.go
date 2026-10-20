package cli

// CurrentContract is the base CLI ↔ extension contract version this CLI
// surface implements. An extension manifest declares the contract it was
// validated against in its `cliContract` field — a stamp EARNED at package time
// (the packager validates the staged manifest strictly and stamps it on
// success), never claimed by hand.
//
// Loaders negotiate instead of assuming the same release train. Since contract
// 3 the ladder has exactly two outcomes — the manifest loads under a contract
// this reader implements, or it does not load at all:
//
//   - manifest contract absent (0) or BELOW what its vocabulary requires →
//     fail loud: the manifest was stamped by a putnami that predates the
//     contract it needs, so what it says about itself cannot be trusted. The
//     remedy names the side that has to move (re-package the extension, or
//     `putnami extensions update`).
//   - manifest contract from its REQUIRED contract up to LatestContract →
//     enforce strictly: the author claimed compliance, so a reserved shadow
//     is a hard error.
//   - manifest contract ABOVE LatestContract → fail loud: "extension requires
//     a newer putnami" — a future contract is never half-interpreted.
//
// A manifest that declares NO contract surface at all (no commands, no command
// groups, no tools, no agent-content contribution — a hook-only framework
// package) is outside the ladder, for the same reason the package-time gate
// leaves it unstamped: there is nothing for the contract to govern, so there is
// nothing to be compatible about.
//
// From contract 5 on, an increment may be ADDITIVE: it adds vocabulary a
// manifest opts into, and nothing a manifest without that vocabulary means
// changes. A manifest is then stamped with the LOWEST contract whose vocabulary
// covers it. An older reader still sees a stamp above its own latest contract
// and refuses the manifest instead of silently dropping the part it cannot
// represent, while an extension that uses none of the new vocabulary keeps its
// CurrentContract stamp and keeps loading in every reader that implements
// CurrentContract. CurrentContract is the base every stamp starts from: the
// contract a manifest without additive vocabulary is stamped with, and the
// contract an extension runtime reports in its handshake.
//
// Changelog, newest first — every increment MUST list what changed. Up to
// contract 2 an increment also had to ship adaptation for contract N-1;
// contract 3 retires that rule with the adaptation itself (see below), so from
// here on a non-additive increment ships a MIGRATION instead of a silent
// downgrade. An additive increment needs none: a manifest that does not use
// its vocabulary keeps the stamp it already carries.
//
//	12: ADDITIVE — the publish channels a publish task serves
//	   (`declares.publishChannels`). From 12 on, a publishing run counts a
//	   project's declared publish channel as served only by a planned job
//	   whose task declares that channel. An older reader would judge the
//	   channels of a publishing run by another rule, so it must refuse the
//	   stamp. Only a manifest with a task that declares the field is
//	   stamped 12, and the floor binds it from every origin.
//	11: ADDITIVE — command kinds and traits. A command's traits may declare
//	   its kind (`kind`: verification, criteria, diagnostics or files), its
//	   workload (`workload`: serve or once), that it rewrites the source tree
//	   (`rewritesSources`) and that it binds a release set
//	   (`consumesReleaseSet`); a batchable task may declare the kind and
//	   phase of its shared invocation (`batchable.kind`, `batchable.phase`).
//	   An older reader would plan a declared kind or workload as a plain
//	   command, so it must refuse the stamp. Only a manifest whose command
//	   or verb traits, or whose task's batchable policy, declare one of them
//	   is stamped 11, and the floor binds it from every origin.
//	10: ADDITIVE — the source-bound cache policy (`cache.sourceBound`). From
//	   10 on, the CLI stores and looks up the task's entry at a key derived
//	   from its cache key and the source binding of the task's own project,
//	   so an entry serves only at the binding its outputs embed. The binding
//	   is not a key input. An older reader stores the entry at the cache key
//	   alone and would replay outputs that embed a stale binding, so it must
//	   refuse the stamp. Only a manifest with a task that declares
//	   the field is stamped 10, and the floor binds it from every origin.
//	9: ADDITIVE — the verb vocabulary (`verbs`): the verbs an extension
//	   declares, in order, with their description, aliases, default traits
//	   and related commands. From 9 on, help, completion and alias resolution
//	   read it, and a job whose manifest declares no traits takes the default
//	   traits of its verb from it. An older reader would plan those jobs with
//	   other traits, so it must refuse the stamp. Only a manifest that
//	   declares the section is stamped 9, and the floor binds it from every
//	   origin.
//	8: ADDITIVE — extensionDependencies that name a registry extension
//	   (@scope/name). From 8 on, the CLI installs, pins and loads every
//	   extension such a manifest depends on. An older reader loads the
//	   manifest without its dependencies, so it must refuse the stamp. The
//	   floor binds a manifest loaded from the artifact store or from a path
//	   the workspace declares, whose dependencies the CLI follows. A manifest
//	   loaded from node_modules keeps loading unstamped: the package manager
//	   owns its dependencies. For the stamper of a put archive, such a list is
//	   contract surface, so a hook-only dependent is stamped 8 too.
//	7: ADDITIVE — two task-input meanings that one CLI release added
//	   together, and a manifest that uses either is stamped 7:
//	   - the releaseBaseline runtime task input: the project's version-line
//	     baseline the CLI reads from git and folds into the task's cache
//	     key. An older reader resolves no value for a runtime input it does
//	     not know, so it would key the task without the baseline and serve a
//	     verdict computed against another tag or another breaking marker.
//	   - a `git:` task file pattern. From 7 on, its key holds each regular
//	     candidate's executable bit and the workspace membership the job
//	     context carries, and a selected unmerged candidate produces no key.
//	     An older reader keys a candidate's bytes alone, so a task whose
//	     verdict reads the bit (an evidence source binding) or the membership
//	     would replay a verdict across a chmod or a membership change. The
//	     pattern itself is older than 7, but no manifest declared one on a
//	     task before it; the floor makes a reader that keys the old meaning
//	     refuse the manifest.
//	6: ADDITIVE — go-embed:build and go-embed:test task file selectors.
//	   Older readers would silently treat them as unmatched globs and
//	   restore outputs against changed embedded bytes.
//	5: ADDITIVE — the agent-content contribution (`agentContent`): skills,
//	   worker profiles, references and helpers with their host adapters,
//	   bound by digest to the extension version that ships them. Only a
//	   manifest that declares one is stamped 5; every other manifest keeps
//	   contract 4 and loads exactly as before. A contract-4 reader has no
//	   representation for the field and would load the extension while
//	   silently dropping its instructions, so it must refuse the stamp —
//	   which it does, because 5 is above its latest contract.
//	4: dependent commands may declare sessionPrerequisites. The planner must
//	   execute those prerequisite commands in the same DAG, project their
//	   selection and invocation-local parameters, and connect their declared
//	   verification gates before the dependent command. A contract-3 reader
//	   does not know this field and would otherwise deploy without those gates.
//	3: the vNext core. Four contracts move together
//	   and a contract-3 extension must speak all four: the v3 task contract
//	   (tasks declare their outputs, effects and source mutation), job context
//	   v2 (`protocolVersion: 2` with a typed task identity), runtime event
//	   protocol v2 (the negotiated `ready` vocabulary), and lock format v2.
//	   Adaptation of contract ≤2 manifests is DELETED, not deprecated: a
//	   lower-contract manifest is rejected with the command that fixes it.
//	2: extension-contributed MCP tool descriptors and their subprocess call
//	   protocol. Older CLIs must reject tool-bearing manifests rather
//	   than silently ignoring their agent-facing surface.
//	1: the reserved global-flag registry — no extension may redefine
//	   a reserved global flag (flags.go); `--output`/`--json` semantics and
//	   the exit-code taxonomy, as documented in doc/01-contract.md.
//	0: the pre-registry world; manifests without a cliContract field.
const CurrentContract = 4

// AgentContentContract is the additive contract a manifest reaches by
// declaring an agent-content contribution. The package-time gate stamps it on
// exactly those manifests (extension.RequiredCLIContract), so a reader that
// predates it refuses the package instead of loading the extension without its
// instructions.
const AgentContentContract = 5

// GoEmbedInputsContract is required only by manifests declaring Go embed
// selectors. Older readers refuse its stamp before considering a cache hit.
const GoEmbedInputsContract = 6

// ReleaseBaselineInputContract is required only by manifests whose tasks
// declare the releaseBaseline runtime input. Older readers refuse its stamp
// instead of keying those tasks without the baseline.
const ReleaseBaselineInputContract = 7

// GitInputModeContract is required only by manifests whose tasks declare a
// `git:` file pattern, whose key holds each regular candidate's executable bit
// and refuses an unmerged candidate. It is rung 7, the rung of
// ReleaseBaselineInputContract: one CLI release added both. Older readers
// refuse its stamp instead of keying those tasks on bytes alone.
const GitInputModeContract = ReleaseBaselineInputContract

// ExtensionDependenciesContract is required by a manifest whose
// extensionDependencies names a registry extension, when the CLI loads it from
// the artifact store or from a path the workspace declares. Older readers
// refuse its stamp instead of loading the extension without its dependencies.
const ExtensionDependenciesContract = 8

// VerbVocabularyContract is required by a manifest that declares a verb
// vocabulary. Its default traits change the plan of every job of that verb
// whose manifest declares none, so an older reader refuses the stamp instead of
// planning those jobs with other traits.
const VerbVocabularyContract = 9

// SourceBoundCacheContract is required by a manifest with a task that declares
// `cache.sourceBound`. A reader at this contract stores and looks up the
// task's entry at a key that also binds the project's source binding, so an
// older reader refuses the stamp instead of replaying outputs that embed a
// stale binding.
const SourceBoundCacheContract = 10

// CommandKindContract is required by a manifest whose command or verb traits
// declare a kind, a workload, rewritesSources or consumesReleaseSet, or whose
// task's batchable policy declares a kind or a phase. Each one tells the host
// what a command's results are or how it runs, so an older reader refuses the
// stamp instead of planning such a command as a plain one.
const CommandKindContract = 11

// PublishChannelsContract is required by a manifest with a task whose v3
// declaration lists the publish channels it serves. A publishing run counts a
// project's declared channel as served only by a planned job whose task
// declares it, so an older reader refuses the stamp instead of judging those
// channels by another rule.
const PublishChannelsContract = 12

// LatestContract is the highest contract this CLI reads. A manifest stamped
// above it is refused as written for a newer putnami; a manifest stamped at or
// below it loads when the stamp covers the vocabulary the manifest uses.
const LatestContract = PublishChannelsContract
