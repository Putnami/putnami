package specgate

import (
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	arch "go.putnami.dev/sdk/extension/architecture"
)

// The cli domain's architecture manifest, authored in Go.
//
// The builder below IS the author and the committed
// putnami.architecture.json is its projection: a hand edit the program does not
// make fails here rather than surviving as a difference nobody notices. Both
// documents are compared in the protocol's canonical form, so member order and
// formatting stay the encoder's business — see
// tooling/extension-sdk/doc/08-architecture-manifest.md.
//
// THE RED LINE. Every arch.Bind call below is a PERMISSION: it says this
// consumer project may depend on that producer project, and names the declared
// contract that authorizes it. Bind takes two exact project IDs from this file
// and reads nothing — no workspace, no graph, no detector — because turning an
// observed dependency into an authorization is the "debt becomes permission"
// anti-pattern ADR 0001 exists to forbid. `putnami architecture sync` refuses to
// create an import for the same reason; adding one here is a reviewed diff, and
// that review IS the authorization.
func authoredCLIDomain() *arch.Builder {
	return arch.NewDomain("cli", "cli").
		Projects(
			"/tooling/cli",
			"/tooling/cli-documents",
			"/tooling/cli-model",
		).
		Owns(
			arch.Concept("cli.workspace-documentation", archproto.OwnershipFact,
				"The authored workspace, CLI, jobs, extensions, and templates documentation tree under /tooling/doc. This domain decides what that reference says; a site that publishes it copies the tree and never edits it."),
			arch.Concept("cli.workspace-engine", archproto.OwnershipModel,
				"Workspace resolution, the job planner and scheduler, caching, and the command dispatcher. Internal packages are not a consumable surface; other domains integrate through wire contracts only."),
		).
		Export(archproto.Export{
			ID:          "cli.workspace-documentation.v1",
			Version:     1,
			Status:      archproto.StatusActive,
			Description: "The authored workspace and CLI documentation tree (/tooling/doc): the markdown a publisher copies verbatim, addressed by a deterministic digest over its sorted (relative path, content digest) pairs. The carrier contract is putnami.documentation-tree.v1.",
			Facts: []archproto.Fact{
				arch.Fact("workspace_documentation", "cli",
					archproto.ClassificationPublic, archproto.PersonalDataNone),
			},
			Modes:         []archproto.AccessMode{archproto.ModeSnapshot},
			Compatibility: arch.Compatible(archproto.CompatibilityAdditive, 1),
		}).
		Import(
			arch.Reference("cli.extension-runtime.v1", 1, arch.From("extension-sdk", "extension-sdk.extension-runtime.v1"),
				arch.As("cli.extension-runtime"),
				arch.Status(archproto.StatusActive),
				arch.Facts("extension_runtime_api"),
				arch.Justification("The CLI and the extensions share one implementation of every contract both sides read or write, so the orchestrator and the extension that produced a document never disagree on it: release sets and package metadata (releaseset, pkgmeta), channel credentials (registrycred, privatebroker), recorded results, agent artifacts, context, profiles, architecture declarations, JSONL events, OCI and Docker publication, the compose test database (dbtestenv), and the source-v1 record of one worktree path (sourcebinding). A few packages are shared platform primitives instead: filelock, the one cross-process lock every Putnami process takes on the same files; scratch, the reclaimable scratch directory both sides create; proctree, which starts and stops a process tree the same way on every OS; dirlink, the directory link (a symbolic link on Unix, a junction on Windows); treearchive, which stages a tree and packs it into a reproducible .tar.gz without starting a process; envkeys, the environment-name rule (case-insensitive on Windows); and ownerperm, which restricts a private file to its owner (an access list on Windows). None of them carries an extension's behavior."),
				arch.Bind("/tooling/cli", "/tooling/extension-sdk"),
				arch.Bind("/tooling/cli-documents", "/tooling/extension-sdk"),
			),
			arch.Reference("cli.protocol-contracts.v1", 1, arch.From("protocols", "protocols.wire-contracts.v1"),
				arch.As("cli.protocol-contracts"),
				arch.Status(archproto.StatusActive),
				arch.Facts("wire_contract_definitions"),
				arch.Justification("The CLI reads and writes every Putnami wire document through the shared strict contract packages instead of a private parse, so one interpretation serves the CLI, extensions, and agents."),
				arch.Bind("/tooling/cli", "/protocols/agentcontext"),
				arch.Bind("/tooling/cli", "/protocols/architecture"),
				arch.Bind("/tooling/cli", "/protocols/cache"),
				arch.Bind("/tooling/cli", "/protocols/capabilities"),
				arch.Bind("/tooling/cli", "/protocols/ci"),
				arch.Bind("/tooling/cli", "/protocols/cli"),
				arch.Bind("/tooling/cli", "/protocols/clientcontract"),
				arch.Bind("/tooling/cli", "/protocols/collaboration"),
				arch.Bind("/tooling/cli", "/protocols/config"),
				arch.Bind("/tooling/cli", "/protocols/contracts"),
				arch.Bind("/tooling/cli", "/protocols/database"),
				arch.Bind("/tooling/cli", "/protocols/diagnostic"),
				arch.Bind("/tooling/cli", "/protocols/distribution"),
				arch.Bind("/tooling/cli", "/protocols/doctor"),
				arch.Bind("/tooling/cli", "/protocols/events"),
				arch.Bind("/tooling/cli", "/protocols/extension"),
				arch.Bind("/tooling/cli", "/protocols/features"),
				arch.Bind("/tooling/cli", "/protocols/gomod"),
				arch.Bind("/tooling/cli", "/protocols/http-routes"),
				arch.Bind("/tooling/cli", "/protocols/infra"),
				arch.Bind("/tooling/cli", "/protocols/job"),
				arch.Bind("/tooling/cli", "/protocols/oci"),
				arch.Bind("/tooling/cli", "/protocols/platform"),
				arch.Bind("/tooling/cli", "/protocols/qualify"),
				arch.Bind("/tooling/cli", "/protocols/registry"),
				arch.Bind("/tooling/cli", "/protocols/runner"),
				arch.Bind("/tooling/cli", "/protocols/runtime"),
				arch.Bind("/tooling/cli", "/protocols/storage"),
				arch.Bind("/tooling/cli", "/protocols/support"),
				arch.Bind("/tooling/cli", "/protocols/telemetry"),
				arch.Bind("/tooling/cli", "/protocols/template"),
				arch.Bind("/tooling/cli", "/protocols/workspace"),
				arch.Bind("/tooling/cli-documents", "/protocols/agentcontext"),
				arch.Bind("/tooling/cli-documents", "/protocols/architecture"),
				arch.Bind("/tooling/cli-documents", "/protocols/cache"),
				arch.Bind("/tooling/cli-documents", "/protocols/capabilities"),
				arch.Bind("/tooling/cli-documents", "/protocols/ci"),
				arch.Bind("/tooling/cli-documents", "/protocols/cli"),
				arch.Bind("/tooling/cli-documents", "/protocols/config"),
				arch.Bind("/tooling/cli-documents", "/protocols/contracts"),
				arch.Bind("/tooling/cli-documents", "/protocols/database"),
				arch.Bind("/tooling/cli-documents", "/protocols/diagnostic"),
				arch.Bind("/tooling/cli-documents", "/protocols/distribution"),
				arch.Bind("/tooling/cli-documents", "/protocols/doctor"),
				arch.Bind("/tooling/cli-documents", "/protocols/events"),
				arch.Bind("/tooling/cli-documents", "/protocols/extension"),
				arch.Bind("/tooling/cli-documents", "/protocols/features"),
				arch.Bind("/tooling/cli-documents", "/protocols/gomod"),
				arch.Bind("/tooling/cli-documents", "/protocols/clientcontract"),
				arch.Bind("/tooling/cli-documents", "/protocols/collaboration"),
				arch.Bind("/tooling/cli-documents", "/protocols/http-routes"),
				arch.Bind("/tooling/cli-documents", "/protocols/infra"),
				arch.Bind("/tooling/cli-documents", "/protocols/job"),
				arch.Bind("/tooling/cli-documents", "/protocols/oci"),
				arch.Bind("/tooling/cli-documents", "/protocols/platform"),
				arch.Bind("/tooling/cli-documents", "/protocols/qualify"),
				arch.Bind("/tooling/cli-documents", "/protocols/registry"),
				arch.Bind("/tooling/cli-documents", "/protocols/runner"),
				arch.Bind("/tooling/cli-documents", "/protocols/runtime"),
				arch.Bind("/tooling/cli-documents", "/protocols/storage"),
				arch.Bind("/tooling/cli-documents", "/protocols/support"),
				arch.Bind("/tooling/cli-documents", "/protocols/telemetry"),
				arch.Bind("/tooling/cli-documents", "/protocols/template"),
				arch.Bind("/tooling/cli-documents", "/protocols/workspace"),
				arch.Bind("/tooling/cli-model", "/protocols/agentcontext"),
				arch.Bind("/tooling/cli-model", "/protocols/cache"),
				arch.Bind("/tooling/cli-model", "/protocols/capabilities"),
				arch.Bind("/tooling/cli-model", "/protocols/ci"),
				arch.Bind("/tooling/cli-model", "/protocols/cli"),
				arch.Bind("/tooling/cli-model", "/protocols/config"),
				arch.Bind("/tooling/cli-model", "/protocols/contracts"),
				arch.Bind("/tooling/cli-model", "/protocols/database"),
				arch.Bind("/tooling/cli-model", "/protocols/diagnostic"),
				arch.Bind("/tooling/cli-model", "/protocols/doctor"),
				arch.Bind("/tooling/cli-model", "/protocols/events"),
				arch.Bind("/tooling/cli-model", "/protocols/extension"),
				arch.Bind("/tooling/cli-model", "/protocols/features"),
				arch.Bind("/tooling/cli-model", "/protocols/infra"),
				arch.Bind("/tooling/cli-model", "/protocols/job"),
				arch.Bind("/tooling/cli-model", "/protocols/runtime"),
				arch.Bind("/tooling/cli-model", "/protocols/storage"),
				arch.Bind("/tooling/cli-model", "/protocols/support"),
				arch.Bind("/tooling/cli-model", "/protocols/telemetry"),
				arch.Bind("/tooling/cli-model", "/protocols/template"),
				arch.Bind("/tooling/cli-model", "/protocols/workspace"),
			),
			arch.Snapshot("cli.verification-observations.v1", 1, arch.From("extension-providers", "extension-providers.verification-observations.v1"), arch.Carried(archproto.TransportFile, "putnami.feature-verification.v1", archproto.StatusActive),
				arch.As("cli.spec-gate-observations"),
				arch.Status(archproto.StatusActive),
				arch.Facts("feature_verification_observations"),
				arch.Guarantees(archproto.Consistency{
					MaxStaleness:   "1h",
					OnMissing:      archproto.FailureFailClosed,
					OnStale:        archproto.FailureFailClosed,
					Ordering:       archproto.OrderingNone,
					SourceVersion:  "session_id",
					IdempotencyKey: "task_key",
					LateEvents:     archproto.LateEventReject,
				}),
				arch.Justification("The executable-spec gate joins the criteria projection with the observation artifacts the language test jobs emitted in the SAME session, and — only for a group that join left unresolved — with the report a dependency's own test cache entry holds at the key the current inputs derive, read in place and never materialized. An observation whose provenance is not contained in the declaring project is dropped, restored or not. A requirement whose checks a consulted source did not report is sanctioned; one for which no source existed in this run's scope at all is reported unobserved and warned. Neither path ever infers support."),
			),
			arch.Projection("cli.workspace-probe-view.v1", 1, arch.From("extension-providers", "extension-providers.probe-answers.v1"), arch.Carried(archproto.TransportInProcess, "putnami.workspace-probe.v1", archproto.StatusActive), arch.Carried(archproto.TransportInProcess, "putnami.workspace-probe.v1", archproto.StatusActive),
				arch.As("cli.workspace-index"),
				arch.Status(archproto.StatusActive),
				arch.Facts(
					"project_identity",
					"dependency_edges",
				),
				arch.Guarantees(archproto.Consistency{
					MaxStaleness:   "24h",
					OnMissing:      archproto.FailureFailClosed,
					OnStale:        archproto.FailureUseStale,
					Ordering:       archproto.OrderingSourceVersion,
					SourceVersion:  "probe_digest",
					IdempotencyKey: "identity_digest",
					LateEvents:     archproto.LateEventIgnoreOlder,
				}),
				arch.Deletes(archproto.DeletionNotApplicable),
				arch.Model(archproto.LocalModel{
					Name:            "cli.workspace-index",
					Kind:            archproto.LocalModelProjection,
					SourceIdentity:  "project_identity",
					ProjectedFields: []string{"project_identity", "dependency_edges"},
					LocalFields:     []string{},
					ProvenanceField: "probe_digest",
					ObservedAtField: "observed_at",
					FreshnessField:  "freshness_state",
					Writer:          "cli.workspace-loader",
					Rebuildable:     true,
					Rebuild:         archproto.RebuildBootstrap,
				}),
				arch.Justification("The recorded workspace index (.putnami/workspace-index.json) lets read-only commands and MCP tools answer without paying a probe. It carries the declared provenance, observation time and freshness fields, and the graph surfaces act on them: an absent copy fails closed with the command that rebuilds it, and a copy past the freshness bound is answered from and marked stale."),
			),
			arch.Command("cli.usage-telemetry.v1", 1, arch.From("observability", "observability.usage-ingest.v1"), arch.Carried(archproto.TransportEvent, "putnami.cli-usage.otlp-logs.v1", archproto.StatusActive),
				arch.As("cli.usage-telemetry"),
				arch.Status(archproto.StatusActive),
				arch.Facts("usage_ingest"),
				arch.Justification("After the one-time notice, the CLI fire-and-forgets anonymous usage records to the observability ingest; the device id is anonymous and rotates monthly, no command arguments are sent, and a refused or unreachable ingest never fails a run."),
			),
		)
}

// TestCommittedCLIDomainManifestIsTheAuthoredOne holds the committed manifest to
// the authoring above.
//
// It lives beside darc_conformance_test.go on purpose: that test joins the
// declared halves to the constants this package's gate runs on, and this one
// joins the committed bytes to their author. Together they make the manifest
// unable to rot in either direction — away from the code, or away from its
// reviewed authoring.
func TestCommittedCLIDomainManifestIsTheAuthoredOne(t *testing.T) {
	arch.Pin(t, "../../putnami.architecture.json", authoredCLIDomain)
}
