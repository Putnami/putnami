package sdk

import (
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	arch "go.putnami.dev/sdk/extension/architecture"
)

// The extension-sdk domain's architecture manifest, authored in Go.
//
// The builder below IS the author and the committed putnami.architecture.json
// is its projection: a hand edit the program does not make fails here rather
// than surviving as a difference nobody notices. Both documents are compared in
// the protocol's canonical form, so member order and formatting stay the
// encoder's business — see doc/08-architecture-manifest.md.
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
// # Why the pin lives in this module
//
// A pin must not create a new observed dependency edge. The authoring runs
// through go.putnami.dev/sdk/extension/architecture, so any project that hosts a
// pin acquires an edge to THIS module. Hosting the extension-sdk domain's own
// pin anywhere else would therefore have made the SDK depend on its consumer to
// be pinned. Here it costs nothing: the package under test is a sibling of the
// package that runs it.
func authoredExtensionSDKDomain() *arch.Builder {
	return arch.NewDomain("extension-sdk", "tooling").
		Projects(
			"/tooling/extension-sdk",
		).
		Owns(
			arch.Concept("extension-sdk.extension-runtime", archproto.OwnershipAPI,
				"The Go runtime an extension binary links: the JSONL event emitter, the job-context parser, the lifecycle runner, the result envelope, and the shared adapters an extension may reuse. This domain decides what an extension is able to say to the orchestrator and in what shape; what the orchestrator then does with it is the cli domain's authority."),
		).
		Export(archproto.Export{
			ID:          "extension-sdk.extension-runtime.v1",
			Version:     1,
			Status:      archproto.StatusActive,
			Description: "The linkable Go extension runtime: JSONL protocol emission, job-context parsing, subcommand dispatch, flag parsing, result envelopes, release-set reading, and the ARC manifest authoring builder. A consumer holds a stable reference to the SDK packages; this domain retains sole authority over the runtime's shape.",
			Facts: []archproto.Fact{
				arch.Fact("extension_runtime_api", "extension-sdk",
					archproto.ClassificationInternal, archproto.PersonalDataNone),
			},
			Modes:         []archproto.AccessMode{archproto.ModeReference},
			Compatibility: arch.Compatible(archproto.CompatibilityAdditive, 1),
		}).
		Import(
			arch.Reference("extension-sdk.protocol-contracts.v1", 1, arch.From("protocols", "protocols.wire-contracts.v1"),
				arch.As("extension-sdk.protocol-contracts"),
				arch.Status(archproto.StatusActive),
				arch.Facts("wire_contract_definitions"),
				arch.Justification("The SDK parses the job context and renders every event, artifact, and result envelope with the shared strict contract packages, so an extension binary and the CLI read one document exactly the same way. Re-implementing the wire here would create a second interpretation of every Putnami document."),
				arch.Bind("/tooling/extension-sdk", "/protocols/architecture"),
				arch.Bind("/tooling/extension-sdk", "/protocols/capabilities"),
				arch.Bind("/tooling/extension-sdk", "/protocols/cli"),
				arch.Bind("/tooling/extension-sdk", "/protocols/database"),
				arch.Bind("/tooling/extension-sdk", "/protocols/diagnostic"),
				arch.Bind("/tooling/extension-sdk", "/protocols/distribution"),
				arch.Bind("/tooling/extension-sdk", "/protocols/events"),
				arch.Bind("/tooling/extension-sdk", "/protocols/extension"),
				arch.Bind("/tooling/extension-sdk", "/protocols/features"),
				arch.Bind("/tooling/extension-sdk", "/protocols/gomod"),
				arch.Bind("/tooling/extension-sdk", "/protocols/infra"),
				arch.Bind("/tooling/extension-sdk", "/protocols/job"),
				arch.Bind("/tooling/extension-sdk", "/protocols/oci"),
				arch.Bind("/tooling/extension-sdk", "/protocols/put"),
				arch.Bind("/tooling/extension-sdk", "/protocols/registry"),
				arch.Bind("/tooling/extension-sdk", "/protocols/runtime"),
				arch.Bind("/tooling/extension-sdk", "/protocols/storage"),
				arch.Bind("/tooling/extension-sdk", "/protocols/workspace"),
			),
		)
}

// TestCommittedExtensionSDKDomainManifestIsTheAuthoredOne holds the committed
// manifest to the authoring above.
func TestCommittedExtensionSDKDomainManifestIsTheAuthoredOne(t *testing.T) {
	arch.Pin(t, "putnami.architecture.json", authoredExtensionSDKDomain)
}
