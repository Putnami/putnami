package main

import (
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	arch "go.putnami.dev/sdk/extension/architecture"
)

// The extension-providers domain's architecture manifest, authored in Go.
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
func authoredExtensionProvidersDomain() *arch.Builder {
	return arch.NewDomain("extension-providers", "extension-providers").
		Projects(
			"/go/extension",
			"/python/extension",
			"/tooling/clientgen-extension",
			"/tooling/github-collaboration",
			"/tooling/local-collaboration",
			"/tooling/memory-store",
			"/tooling/scaffold",
			"/typescript/extension",
		).
		Owns(
			arch.Concept("extension-providers.python-surface-documentation", archproto.OwnershipFact,
				"The authored Python surface documentation tree under /python/doc/framework. The Python surface is the extension and its templates only, and this domain decides what that reference claims about it; no Putnami Python framework package family ships today."),
			arch.Concept("extension-providers.verification-observations", archproto.OwnershipFact,
				"Per-session feature-verification observation reports emitted by the language test jobs; the executable-spec gate consumes them, it never fabricates one."),
			arch.Concept("extension-providers.probe-answers", archproto.OwnershipFact,
				"Workspace probe answers: each provider's authoritative statement of the projects it owns and their dependency edges."),
		).
		Export(archproto.Export{
			ID:          "extension-providers.verification-observations.v1",
			Version:     1,
			Status:      archproto.StatusActive,
			Description: "The putnami-feature-verification artifacts a test run produces: which spec requirements each project's tests proved, with provenance bound to the emitting project.",
			Facts: []archproto.Fact{
				arch.Fact("feature_verification_observations", "extension-providers",
					archproto.ClassificationInternal, archproto.PersonalDataNone),
			},
			Modes:         []archproto.AccessMode{archproto.ModeSnapshot},
			Compatibility: arch.Compatible(archproto.CompatibilityAdditive, 1),
		}).
		Export(archproto.Export{
			ID:          "extension-providers.python-surface-documentation.v1",
			Version:     1,
			Status:      archproto.StatusActive,
			Description: "The authored Python surface documentation tree (/python/doc/framework): the markdown a publisher copies verbatim, addressed by a deterministic digest over its sorted (relative path, content digest) pairs. The carrier contract is putnami.documentation-tree.v1. The Python surface is experimental and opt-in, and publishing this tree next to the Go and TypeScript references does not promote it.",
			Facts: []archproto.Fact{
				arch.Fact("python_surface_documentation", "extension-providers",
					archproto.ClassificationPublic, archproto.PersonalDataNone),
			},
			Modes:         []archproto.AccessMode{archproto.ModeSnapshot},
			Compatibility: arch.Compatible(archproto.CompatibilityAdditive, 1),
		}).
		Export(archproto.Export{
			ID:          "extension-providers.probe-answers.v1",
			Version:     1,
			Status:      archproto.StatusActive,
			Description: "Merged probe answers: project identity and dependency edges. The recorded workspace index is a local projection of this authority; the providers remain the only source of truth.",
			Facts: []archproto.Fact{
				arch.Fact("project_identity", "extension-providers",
					archproto.ClassificationInternal, archproto.PersonalDataNone),
				arch.Fact("dependency_edges", "extension-providers",
					archproto.ClassificationInternal, archproto.PersonalDataNone),
			},
			Modes:         []archproto.AccessMode{archproto.ModeProjection},
			Compatibility: arch.Compatible(archproto.CompatibilityAdditive, 1),
		}).
		Import(
			arch.Reference("extension-providers.extension-runtime.v1", 1, arch.From("extension-sdk", "extension-sdk.extension-runtime.v1"),
				arch.As("extension-providers.extension-runtime"),
				arch.Status(archproto.StatusActive),
				arch.Facts("extension_runtime_api"),
				arch.Justification("Every provider in this domain is an extension binary: it parses its job context, emits phases, progress, diagnostics and artifacts, and serves its workspace probe through the SDK runtime rather than writing the JSONL wire itself. One shared runtime is what keeps three language providers and the template packager answering the orchestrator identically. The collaboration and memory stores take their cross-process file lock from the same SDK, so every Putnami binary locks one way on every platform."),
				arch.Bind("/go/extension", "/tooling/extension-sdk"),
				arch.Bind("/python/extension", "/tooling/extension-sdk"),
				arch.Bind("/tooling/clientgen-extension", "/tooling/extension-sdk"),
				arch.Bind("/tooling/local-collaboration", "/tooling/extension-sdk"),
				arch.Bind("/tooling/memory-store", "/tooling/extension-sdk"),
				arch.Bind("/tooling/scaffold", "/tooling/extension-sdk"),
				arch.Bind("/typescript/extension", "/tooling/extension-sdk"),
			),
			// The workspace client extension carries the framework's own Go
			// emitter as a tools-only module dependency so bin/prepare can build
			// it and package the binary beside the runtime. Nothing compiled into
			// that runtime imports go.putnami.dev/api — the emitter is a
			// neighboring executable, never a linked library — and
			// TestNoCompiledPackageLinksTheFrameworkEmitter holds that line.
			arch.Reference("extension-providers.go-client-emitter.v1", 1, arch.From("go-framework", "go-framework.application-runtime.v1"),
				arch.As("extension-providers.go-client-emitter"),
				arch.Status(archproto.StatusActive),
				arch.Facts("go_application_runtime_api"),
				arch.Justification("The workspace client extension packages the Go framework's own generator command and invokes that pinned emitter; it does not maintain a second Go emitter or resolve source from the consumer checkout."),
				arch.Bind("/tooling/clientgen-extension", "/go/framework/api"),
				arch.Bind("/tooling/clientgen-extension", "/go/framework/app"),
				arch.Bind("/tooling/clientgen-extension", "/go/framework/config"),
				arch.Bind("/tooling/clientgen-extension", "/go/framework/errors"),
				arch.Bind("/tooling/clientgen-extension", "/go/framework/http"),
				arch.Bind("/tooling/clientgen-extension", "/go/framework/inject"),
				arch.Bind("/tooling/clientgen-extension", "/go/framework/logger"),
				arch.Bind("/tooling/clientgen-extension", "/go/framework/migration"),
				arch.Bind("/tooling/clientgen-extension", "/go/framework/schema"),
			),
			arch.Reference("extension-providers.typescript-client-emitter.v1", 1, arch.From("typescript-framework", "typescript-framework.application-runtime.v1"),
				arch.As("extension-providers.typescript-client-emitter"),
				arch.Status(archproto.StatusActive),
				arch.Facts("typescript_application_runtime_api"),
				arch.Justification("The workspace client extension invokes the @putnami/client package binary installed by Putnami; it never reaches into a consumer checkout for the framework source or maintains a second TypeScript emitter."),
				arch.Bind("/tooling/clientgen-extension", "/typescript/framework/client"),
			),
			arch.Reference("extension-providers.protocol-contracts.v1", 1, arch.From("protocols", "protocols.wire-contracts.v1"),
				arch.As("extension-providers.protocol-contracts"),
				arch.Status(archproto.StatusActive),
				arch.Facts("wire_contract_definitions"),
				arch.Justification("The language and provider extensions speak to the CLI exclusively through the shared strict wire contracts: job context and provider requests in, task events, artifacts and provider results out."),
				arch.Bind("/go/extension", "/protocols/architecture"),
				arch.Bind("/go/extension", "/protocols/cache"),
				arch.Bind("/go/extension", "/protocols/capabilities"),
				arch.Bind("/go/extension", "/protocols/cli"),
				arch.Bind("/go/extension", "/protocols/config"),
				arch.Bind("/go/extension", "/protocols/database"),
				arch.Bind("/go/extension", "/protocols/diagnostic"),
				arch.Bind("/go/extension", "/protocols/distribution"),
				arch.Bind("/go/extension", "/protocols/events"),
				arch.Bind("/go/extension", "/protocols/extension"),
				arch.Bind("/go/extension", "/protocols/features"),
				arch.Bind("/go/extension", "/protocols/gomod"),
				arch.Bind("/go/extension", "/protocols/infra"),
				arch.Bind("/go/extension", "/protocols/job"),
				arch.Bind("/go/extension", "/protocols/oci"),
				arch.Bind("/go/extension", "/protocols/registry"),
				arch.Bind("/go/extension", "/protocols/runtime"),
				arch.Bind("/go/extension", "/protocols/storage"),
				arch.Bind("/go/extension", "/protocols/workspace"),
				arch.Bind("/python/extension", "/protocols/architecture"),
				arch.Bind("/python/extension", "/protocols/capabilities"),
				arch.Bind("/python/extension", "/protocols/cli"),
				arch.Bind("/python/extension", "/protocols/database"),
				arch.Bind("/python/extension", "/protocols/diagnostic"),
				arch.Bind("/python/extension", "/protocols/distribution"),
				arch.Bind("/python/extension", "/protocols/events"),
				arch.Bind("/python/extension", "/protocols/extension"),
				arch.Bind("/python/extension", "/protocols/features"),
				arch.Bind("/python/extension", "/protocols/gomod"),
				arch.Bind("/python/extension", "/protocols/infra"),
				arch.Bind("/python/extension", "/protocols/job"),
				arch.Bind("/python/extension", "/protocols/oci"),
				arch.Bind("/python/extension", "/protocols/registry"),
				arch.Bind("/python/extension", "/protocols/runtime"),
				arch.Bind("/python/extension", "/protocols/storage"),
				arch.Bind("/python/extension", "/protocols/workspace"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/architecture"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/capabilities"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/cli"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/clientcontract"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/config"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/contracts"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/database"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/diagnostic"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/distribution"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/events"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/extension"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/features"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/gomod"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/http-routes"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/identity"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/infra"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/job"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/migration"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/oci"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/platform"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/registry"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/runtime"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/storage"),
				arch.Bind("/tooling/clientgen-extension", "/protocols/workspace"),
				arch.Bind("/tooling/github-collaboration", "/protocols/capabilities"),
				arch.Bind("/tooling/github-collaboration", "/protocols/cli"),
				arch.Bind("/tooling/github-collaboration", "/protocols/collaboration"),
				arch.Bind("/tooling/github-collaboration", "/protocols/diagnostic"),
				arch.Bind("/tooling/github-collaboration", "/protocols/extension"),
				arch.Bind("/tooling/github-collaboration", "/protocols/features"),
				arch.Bind("/tooling/github-collaboration", "/protocols/runtime"),
				arch.Bind("/tooling/local-collaboration", "/protocols/architecture"),
				arch.Bind("/tooling/local-collaboration", "/protocols/capabilities"),
				arch.Bind("/tooling/local-collaboration", "/protocols/cli"),
				arch.Bind("/tooling/local-collaboration", "/protocols/collaboration"),
				arch.Bind("/tooling/local-collaboration", "/protocols/database"),
				arch.Bind("/tooling/local-collaboration", "/protocols/diagnostic"),
				arch.Bind("/tooling/local-collaboration", "/protocols/distribution"),
				arch.Bind("/tooling/local-collaboration", "/protocols/events"),
				arch.Bind("/tooling/local-collaboration", "/protocols/extension"),
				arch.Bind("/tooling/local-collaboration", "/protocols/features"),
				arch.Bind("/tooling/local-collaboration", "/protocols/gomod"),
				arch.Bind("/tooling/local-collaboration", "/protocols/infra"),
				arch.Bind("/tooling/local-collaboration", "/protocols/job"),
				arch.Bind("/tooling/local-collaboration", "/protocols/oci"),
				arch.Bind("/tooling/local-collaboration", "/protocols/registry"),
				arch.Bind("/tooling/local-collaboration", "/protocols/runtime"),
				arch.Bind("/tooling/local-collaboration", "/protocols/storage"),
				arch.Bind("/tooling/local-collaboration", "/protocols/workspace"),
				arch.Bind("/tooling/memory-store", "/protocols/architecture"),
				arch.Bind("/tooling/memory-store", "/protocols/capabilities"),
				arch.Bind("/tooling/memory-store", "/protocols/cli"),
				arch.Bind("/tooling/memory-store", "/protocols/collaboration"),
				arch.Bind("/tooling/memory-store", "/protocols/database"),
				arch.Bind("/tooling/memory-store", "/protocols/diagnostic"),
				arch.Bind("/tooling/memory-store", "/protocols/distribution"),
				arch.Bind("/tooling/memory-store", "/protocols/events"),
				arch.Bind("/tooling/memory-store", "/protocols/extension"),
				arch.Bind("/tooling/memory-store", "/protocols/features"),
				arch.Bind("/tooling/memory-store", "/protocols/gomod"),
				arch.Bind("/tooling/memory-store", "/protocols/infra"),
				arch.Bind("/tooling/memory-store", "/protocols/job"),
				arch.Bind("/tooling/memory-store", "/protocols/oci"),
				arch.Bind("/tooling/memory-store", "/protocols/registry"),
				arch.Bind("/tooling/memory-store", "/protocols/runtime"),
				arch.Bind("/tooling/memory-store", "/protocols/storage"),
				arch.Bind("/tooling/memory-store", "/protocols/workspace"),
				arch.Bind("/tooling/scaffold", "/protocols/architecture"),
				arch.Bind("/tooling/scaffold", "/protocols/capabilities"),
				arch.Bind("/tooling/scaffold", "/protocols/cli"),
				arch.Bind("/tooling/scaffold", "/protocols/database"),
				arch.Bind("/tooling/scaffold", "/protocols/diagnostic"),
				arch.Bind("/tooling/scaffold", "/protocols/distribution"),
				arch.Bind("/tooling/scaffold", "/protocols/events"),
				arch.Bind("/tooling/scaffold", "/protocols/extension"),
				arch.Bind("/tooling/scaffold", "/protocols/features"),
				arch.Bind("/tooling/scaffold", "/protocols/gomod"),
				arch.Bind("/tooling/scaffold", "/protocols/infra"),
				arch.Bind("/tooling/scaffold", "/protocols/job"),
				arch.Bind("/tooling/scaffold", "/protocols/oci"),
				arch.Bind("/tooling/scaffold", "/protocols/registry"),
				arch.Bind("/tooling/scaffold", "/protocols/runtime"),
				arch.Bind("/tooling/scaffold", "/protocols/storage"),
				arch.Bind("/tooling/scaffold", "/protocols/template"),
				arch.Bind("/tooling/scaffold", "/protocols/workspace"),
				arch.Bind("/typescript/extension", "/protocols/architecture"),
				arch.Bind("/typescript/extension", "/protocols/capabilities"),
				arch.Bind("/typescript/extension", "/protocols/cli"),
				arch.Bind("/typescript/extension", "/protocols/database"),
				arch.Bind("/typescript/extension", "/protocols/diagnostic"),
				arch.Bind("/typescript/extension", "/protocols/distribution"),
				arch.Bind("/typescript/extension", "/protocols/events"),
				arch.Bind("/typescript/extension", "/protocols/extension"),
				arch.Bind("/typescript/extension", "/protocols/features"),
				arch.Bind("/typescript/extension", "/protocols/gomod"),
				arch.Bind("/typescript/extension", "/protocols/http-routes"),
				arch.Bind("/typescript/extension", "/protocols/infra"),
				arch.Bind("/typescript/extension", "/protocols/job"),
				arch.Bind("/typescript/extension", "/protocols/oci"),
				arch.Bind("/typescript/extension", "/protocols/registry"),
				arch.Bind("/typescript/extension", "/protocols/runtime"),
				arch.Bind("/typescript/extension", "/protocols/storage"),
				arch.Bind("/typescript/extension", "/protocols/workspace"),
			),
		)
}

// TestCommittedExtensionProvidersDomainManifestIsTheAuthoredOne holds the
// committed manifest to the authoring above. The domain spans three language
// extensions and only one of them hosts the declaration, so the pin runs in the
// project that owns the file.
func TestCommittedExtensionProvidersDomainManifestIsTheAuthoredOne(t *testing.T) {
	arch.Pin(t, "../../putnami.architecture.json", authoredExtensionProvidersDomain)
}
