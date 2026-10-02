package sdd

import (
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	arch "go.putnami.dev/sdk/extension/architecture"
)

// Three architecture manifests, authored in Go: this extension's own `sdd`
// domain, plus the `protocols` and `observability` domains.
//
// The builder IS the author and the committed putnami.architecture.json is its
// projection: a hand edit the program does not make fails here rather than
// surviving as a difference nobody notices. Both documents are compared in the
// protocol's canonical form, so member order and formatting stay the encoder's
// business — see tooling/extension-sdk/doc/08-architecture-manifest.md.
//
// THE RED LINE. Every arch.Bind call below is a PERMISSION: it says this
// consumer project may depend on that producer project, and names the declared
// contract that authorizes it. Bind takes two exact project IDs from this file
// and reads nothing — no workspace, no graph, no detector — because turning an
// observed dependency into an authorization is the "debt becomes permission"
// anti-pattern ADR 0001 exists to forbid. `putnami architecture sync` refuses to
// create an import for the same reason; adding one here is a reviewed diff, and
// that review IS the authorization.
//
// # Why two of these are hosted rather than owned
//
// A pin belongs in a project the domain owns. Two domains cannot take that:
//
//   - every member of `protocols` is a PUBLISHED Go module, and the authoring
//     runs through `go.putnami.dev/sdk/extension/architecture`, which is not
//     published. A protocol module that imported it would require a module its
//     own consumers cannot resolve, and it would invert the layering the
//     repository is built on, where protocols are shared BY tooling and never
//     depend on it.
//   - `observability`'s only member is `/sites/telemetry.putnami.dev`, a
//     deployed workload. Requiring the SDK there is not free: `putnami projects
//     sync` maps every module in the requirement graph, which pulled six more
//     protocol projects into the workload's declared dependencies and turned a
//     test-only import into SEVEN new cross-domain permissions. A pin must not
//     cost the domain it protects a set of authorizations nothing else needs.
//
// This extension is the closest thing to an owner either can have: it is where
// `putnami architecture validate` runs, and it already depends on the protocol
// and the SDK. Both pins are hosted, not adopted — each domain still owns its
// own declaration.
//
// Four more domains are hosted here for the same reason and for the same price:
// `go-framework`, `typescript-framework`, `public-docs`, and `agent-workflows`,
// in architecture_repository_pin_test.go. That file explains why a pin inside
// `/go/framework/*` or `/typescript/framework/*` would invert the repository's
// layering.

func authoredSDDDomain() *arch.Builder {
	return arch.NewDomain("sdd", "sdd").
		Projects(
			"/tooling/sdd-extension",
		).
		Owns(
			arch.Concept("sdd.method-documentation", archproto.OwnershipFact,
				"The authored spec-driven development documentation tree under /tooling/sdd-extension/doc: the method's own wording, its ADRs, and its contract references. This domain decides what the method claims; a site that publishes it copies the tree and never edits it."),
			arch.Concept("sdd.spec-driven-development", archproto.OwnershipModel,
				"Feature, spec, architecture, and contract validation. The extension learns workspace facts from the job wire only; it never imports CLI internals and never execs putnami."),
		).
		Export(archproto.Export{
			ID:          "sdd.method-documentation.v1",
			Version:     1,
			Status:      archproto.StatusActive,
			Description: "The authored spec-driven development documentation tree (/tooling/sdd-extension/doc): the markdown a publisher copies verbatim, addressed by a deterministic digest over its sorted (relative path, content digest) pairs. The carrier contract is putnami.documentation-tree.v1.",
			Facts: []archproto.Fact{
				arch.Fact("spec_driven_development_documentation", "sdd",
					archproto.ClassificationPublic, archproto.PersonalDataNone),
			},
			Modes:         []archproto.AccessMode{archproto.ModeSnapshot},
			Compatibility: arch.Compatible(archproto.CompatibilityAdditive, 1),
		}).
		Import(
			arch.Reference("sdd.extension-runtime.v1", 1, arch.From("extension-sdk", "extension-sdk.extension-runtime.v1"),
				arch.As("sdd.extension-runtime"),
				arch.Status(archproto.StatusActive),
				arch.Facts("extension_runtime_api"),
				arch.Justification("This is an extension binary: it emits its phases, diagnostics, artifacts, and interactive result envelopes through the SDK runtime instead of writing the JSONL wire itself. The ADR 0013 boundary allows exactly this reference and forbids reaching into CLI internals."),
				arch.Bind("/tooling/sdd-extension", "/tooling/extension-sdk"),
			),
			arch.Reference("sdd.protocol-contracts.v1", 1, arch.From("protocols", "protocols.wire-contracts.v1"),
				arch.As("sdd.protocol-contracts"),
				arch.Status(archproto.StatusActive),
				arch.Facts("wire_contract_definitions"),
				arch.Justification("The SDD extension validates features, specs, and architecture through the shared strict contract packages; the ADR 0013 boundary allows protocol and SDK references and forbids CLI internals."),
				arch.Bind("/tooling/sdd-extension", "/protocols/architecture"),
				arch.Bind("/tooling/sdd-extension", "/protocols/capabilities"),
				arch.Bind("/tooling/sdd-extension", "/protocols/cli"),
				arch.Bind("/tooling/sdd-extension", "/protocols/config"),
				arch.Bind("/tooling/sdd-extension", "/protocols/contracts"),
				arch.Bind("/tooling/sdd-extension", "/protocols/database"),
				arch.Bind("/tooling/sdd-extension", "/protocols/diagnostic"),
				arch.Bind("/tooling/sdd-extension", "/protocols/distribution"),
				arch.Bind("/tooling/sdd-extension", "/protocols/events"),
				arch.Bind("/tooling/sdd-extension", "/protocols/extension"),
				arch.Bind("/tooling/sdd-extension", "/protocols/features"),
				arch.Bind("/tooling/sdd-extension", "/protocols/gomod"),
				arch.Bind("/tooling/sdd-extension", "/protocols/infra"),
				arch.Bind("/tooling/sdd-extension", "/protocols/job"),
				arch.Bind("/tooling/sdd-extension", "/protocols/oci"),
				arch.Bind("/tooling/sdd-extension", "/protocols/put"),
				arch.Bind("/tooling/sdd-extension", "/protocols/registry"),
				arch.Bind("/tooling/sdd-extension", "/protocols/runtime"),
				arch.Bind("/tooling/sdd-extension", "/protocols/storage"),
				arch.Bind("/tooling/sdd-extension", "/protocols/support"),
				arch.Bind("/tooling/sdd-extension", "/protocols/workspace"),
			),
		)
}

func authoredProtocolsDomain() *arch.Builder {
	return arch.NewDomain("protocols", "protocols").
		Projects(
			"/protocols/agentcontext",
			"/protocols/analytics",
			"/protocols/architecture",
			"/protocols/cache",
			"/protocols/capabilities",
			"/protocols/ci",
			"/protocols/cli",
			"/protocols/clientcontract",
			"/protocols/collaboration",
			"/protocols/config",
			"/protocols/contracts",
			"/protocols/database",
			"/protocols/diagnostic",
			"/protocols/distribution",
			"/protocols/doccov",
			"/protocols/doctor",
			"/protocols/events",
			"/protocols/extension",
			"/protocols/features",
			"/protocols/gomod",
			"/protocols/http-routes",
			"/protocols/identity",
			"/protocols/infra",
			"/protocols/job",
			"/protocols/keyring",
			"/protocols/migration",
			"/protocols/oci",
			"/protocols/platform",
			"/protocols/put",
			"/protocols/qualify",
			"/protocols/registry",
			"/protocols/runner",
			"/protocols/runtime",
			"/protocols/sitecontent",
			"/protocols/storage",
			"/protocols/support",
			"/protocols/telemetry",
			"/protocols/template",
			"/protocols/transaction",
			"/protocols/workspace",
		).
		Owns(
			arch.Concept("protocols.wire-contracts", archproto.OwnershipSchema,
				"The strict wire formats, parsers, and validators every Putnami surface shares: one interpretation per document, reused by the CLI, extensions, and agents."),
		).
		Export(archproto.Export{
			ID:          "protocols.wire-contracts.v1",
			Version:     1,
			Status:      archproto.StatusActive,
			Description: "Strict contract types and validators for Putnami wire documents. Consumers hold a stable reference to the published contract packages; the protocols domain retains sole authority over document meaning.",
			Facts: []archproto.Fact{
				arch.ReferenceFact("wire_contract_definitions", "protocols"),
			},
			Modes:         []archproto.AccessMode{archproto.ModeReference},
			Compatibility: arch.Compatible(archproto.CompatibilityAdditive, 1),
		}).
		Import()
}

func authoredObservabilityDomain() *arch.Builder {
	return arch.NewDomain("observability", "observability").
		Projects(
			"/sites/telemetry.putnami.dev",
		).
		Owns(
			arch.Concept("observability.cli-usage-aggregates", archproto.OwnershipModel,
				"Daily anonymous CLI-usage aggregates. No command argument, device id, or IP is ever reflected; retention and bucketing policy are local authority."),
		).
		Export(archproto.Export{
			ID:          "observability.usage-ingest.v1",
			Version:     1,
			Status:      archproto.StatusActive,
			Description: "The anonymous CLI-usage ingest: OTLP/JSON log records accepted only after sanitization, with origin=cli-anon stamped unspoofably at the resource level and every caller-supplied origin dropped.",
			Facts: []archproto.Fact{
				arch.Fact("usage_ingest", "observability",
					archproto.ClassificationInternal, archproto.PersonalDataNone),
			},
			Modes:         []archproto.AccessMode{archproto.ModeCommand},
			Compatibility: arch.Compatible(archproto.CompatibilityAdditive, 1),
		}).
		Import(
			arch.Reference("observability.application-runtime.v1", 1, arch.From("go-framework", "go-framework.application-runtime.v1"),
				arch.As("observability.application-runtime"),
				arch.Status(archproto.StatusActive),
				arch.Facts("go_application_runtime_api"),
				arch.Justification("The telemetry receiver is an ordinary Putnami Go application: it boots on the app lifecycle, serves its ingest over the framework HTTP runtime, stores aggregates through the framework database and migration runtime, and reports itself through the framework telemetry runtime. It consumes the public go.putnami.dev API exactly as a tenant workload does and reaches into no framework internal."),
				arch.Bind("/sites/telemetry.putnami.dev", "/go/framework/app"),
				arch.Bind("/sites/telemetry.putnami.dev", "/go/framework/cache"),
				arch.Bind("/sites/telemetry.putnami.dev", "/go/framework/client"),
				arch.Bind("/sites/telemetry.putnami.dev", "/go/framework/config"),
				arch.Bind("/sites/telemetry.putnami.dev", "/go/framework/ctxutil"),
				arch.Bind("/sites/telemetry.putnami.dev", "/go/framework/database"),
				arch.Bind("/sites/telemetry.putnami.dev", "/go/framework/errors"),
				arch.Bind("/sites/telemetry.putnami.dev", "/go/framework/http"),
				arch.Bind("/sites/telemetry.putnami.dev", "/go/framework/inject"),
				arch.Bind("/sites/telemetry.putnami.dev", "/go/framework/logger"),
				arch.Bind("/sites/telemetry.putnami.dev", "/go/framework/migration"),
				arch.Bind("/sites/telemetry.putnami.dev", "/go/framework/schema"),
				arch.Bind("/sites/telemetry.putnami.dev", "/go/framework/security"),
				arch.Bind("/sites/telemetry.putnami.dev", "/go/framework/telemetry"),
			),
			arch.Reference("observability.protocol-contracts.v1", 1, arch.From("protocols", "protocols.wire-contracts.v1"),
				arch.As("observability.protocol-contracts"),
				arch.Status(archproto.StatusActive),
				arch.Facts("wire_contract_definitions"),
				arch.Justification("The telemetry workload is an ordinary Putnami Go application; it consumes the shared strict contract packages like any tenant, never a private parse."),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/architecture"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/capabilities"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/clientcontract"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/config"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/contracts"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/database"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/diagnostic"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/events"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/features"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/http-routes"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/identity"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/infra"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/keyring"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/migration"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/runtime"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/storage"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/telemetry"),
				arch.Bind("/sites/telemetry.putnami.dev", "/protocols/transaction"),
			),
		)
}

// TestCommittedSDDDomainManifestIsTheAuthoredOne holds this extension's own
// committed manifest to its authoring.
func TestCommittedSDDDomainManifestIsTheAuthoredOne(t *testing.T) {
	arch.Pin(t, "putnami.architecture.json", authoredSDDDomain)
}

// TestCommittedProtocolsDomainManifestIsTheAuthoredOne holds the protocols
// domain's committed manifest to its authoring.
func TestCommittedProtocolsDomainManifestIsTheAuthoredOne(t *testing.T) {
	arch.Pin(t, "../../protocols/putnami.architecture.json", authoredProtocolsDomain)
}

// TestCommittedObservabilityDomainManifestIsTheAuthoredOne holds the
// observability domain's committed manifest to its authoring. The domain's own
// project pins the same manifest's EXPORT against the sanitizer constants it
// describes (sites/telemetry.putnami.dev/darc_conformance_test.go), so the
// declaration is still held to its code where it runs.
func TestCommittedObservabilityDomainManifestIsTheAuthoredOne(t *testing.T) {
	arch.Pin(t, "../../sites/telemetry.putnami.dev/putnami.architecture.json", authoredObservabilityDomain)
}
