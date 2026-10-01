package sdd

import (
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	arch "go.putnami.dev/sdk/extension/architecture"
)

// Four more of this repository's architecture manifests, authored in Go:
// `go-framework`, `typescript-framework`, `public-docs`, and `agent-workflows`.
//
// The builder IS the author and each committed putnami.architecture.json is its
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
// # Why all four are hosted here
//
// A pin must not create a new observed dependency edge, and the authoring runs
// through go.putnami.dev/sdk/extension/architecture. A pin inside
// `/go/framework/*` or `/typescript/framework/*` would make the FRAMEWORK depend
// on the tooling SDK — the exact inversion the repository layering forbids, and
// one that would cost each framework module a set of cross-domain permissions
// nothing else needs. `/sites/putnami.dev` is a deployed TypeScript application
// and has no Go module at all. `/tooling/contributor` is a content-only
// extension with no Go module either.
//
// This extension is where `putnami architecture validate` runs and it already
// depends on both the protocol and the SDK, so hosting costs no new edge. The
// pins are hosted, not adopted: each domain still owns its own declaration, and
// this file may not change one without the owning team's review.
//
// # putnami.documentation-tree.v1
//
// Five of the imports below name that carrier contract. It is a CARRIER, not a
// published wire schema: the markdown tree one `generate.assets` entry copies,
// addressed by a deterministic digest over its sorted (relative path, content
// digest) pairs. Two trees with the same digest are the same document, which is
// what lets `tree_digest` serve as both the source version and the idempotency
// key.

func authoredGoFrameworkDomain() *arch.Builder {
	return arch.NewDomain("go-framework", "framework").
		Projects(
			"/go/framework/api",
			"/go/framework/app",
			"/go/framework/cache",
			"/go/framework/client",
			"/go/framework/config",
			"/go/framework/ctxutil",
			"/go/framework/database",
			"/go/framework/errors",
			"/go/framework/events",
			"/go/framework/grpc",
			"/go/framework/http",
			"/go/framework/inject",
			"/go/framework/keyringstore",
			"/go/framework/logger",
			"/go/framework/migration",
			"/go/framework/migration/migratecli",
			"/go/framework/openapi",
			"/go/framework/parallel",
			"/go/framework/platform",
			"/go/framework/proto",
			"/go/framework/schema",
			"/go/framework/security",
			"/go/framework/storage",
			"/go/framework/telemetry",
		).
		Owns(
			arch.Concept("go-framework.api-documentation", archproto.OwnershipFact,
				"The authored Go framework documentation tree under /go/doc/framework. This domain decides what the published Go reference says; a site that publishes it copies the tree and never edits it."),
			arch.Concept("go-framework.application-runtime", archproto.OwnershipAPI,
				"The Go application runtime surface published as go.putnami.dev/*: dependency injection, configuration, the app lifecycle, HTTP, database and transactions, storage, events, security, and telemetry. This domain decides what a Putnami Go application can express; a tenant application composes that surface and owns none of its shape."),
		).
		Export(archproto.Export{
			ID:          "go-framework.api-documentation.v1",
			Version:     1,
			Status:      archproto.StatusActive,
			Description: "The authored Go framework documentation tree (/go/doc/framework): the markdown a publisher copies verbatim, addressed by a deterministic digest over its sorted (relative path, content digest) pairs. The carrier contract is putnami.documentation-tree.v1.",
			Facts: []archproto.Fact{
				arch.Fact("go_framework_documentation", "go-framework",
					archproto.ClassificationPublic, archproto.PersonalDataNone),
			},
			Modes:         []archproto.AccessMode{archproto.ModeSnapshot},
			Compatibility: arch.Compatible(archproto.CompatibilityAdditive, 1),
		}).
		Export(archproto.Export{
			ID:          "go-framework.application-runtime.v1",
			Version:     1,
			Status:      archproto.StatusActive,
			Description: "The published go.putnami.dev/* runtime modules a Putnami Go application links: injection, configuration, the app lifecycle, HTTP, database, storage, events, security, and telemetry. A consumer holds a stable reference to the modules; this domain retains sole authority over their API.",
			Facts: []archproto.Fact{
				arch.Fact("go_application_runtime_api", "go-framework",
					archproto.ClassificationPublic, archproto.PersonalDataNone),
			},
			Modes:         []archproto.AccessMode{archproto.ModeReference},
			Compatibility: arch.Compatible(archproto.CompatibilityAdditive, 1),
		}).
		Import(
			arch.Reference("go-framework.protocol-contracts.v1", 1, arch.From("protocols", "protocols.wire-contracts.v1"),
				arch.As("go-framework.protocol-contracts"),
				arch.Status(archproto.StatusActive),
				arch.Facts("wire_contract_definitions"),
				arch.Justification("The framework modules read and write Putnami wire documents — runtime and infra descriptors, capabilities, http-routes, features, migrations, storage and database configuration — through the shared strict contract packages. A running application, the CLI that planned it, and the extension that built it therefore agree on one interpretation of every document, instead of the framework carrying a private parse of documents it does not own."),
				arch.Bind("/go/framework/api", "/protocols/architecture"),
				arch.Bind("/go/framework/api", "/protocols/capabilities"),
				arch.Bind("/go/framework/api", "/protocols/clientcontract"),
				arch.Bind("/go/framework/api", "/protocols/config"),
				arch.Bind("/go/framework/api", "/protocols/contracts"),
				arch.Bind("/go/framework/api", "/protocols/database"),
				arch.Bind("/go/framework/api", "/protocols/diagnostic"),
				arch.Bind("/go/framework/api", "/protocols/events"),
				arch.Bind("/go/framework/api", "/protocols/features"),
				arch.Bind("/go/framework/api", "/protocols/http-routes"),
				arch.Bind("/go/framework/api", "/protocols/identity"),
				arch.Bind("/go/framework/api", "/protocols/infra"),
				arch.Bind("/go/framework/api", "/protocols/migration"),
				arch.Bind("/go/framework/api", "/protocols/platform"),
				arch.Bind("/go/framework/api", "/protocols/runtime"),
				arch.Bind("/go/framework/api", "/protocols/storage"),
				arch.Bind("/go/framework/app", "/protocols/architecture"),
				arch.Bind("/go/framework/app", "/protocols/capabilities"),
				arch.Bind("/go/framework/app", "/protocols/config"),
				arch.Bind("/go/framework/app", "/protocols/database"),
				arch.Bind("/go/framework/app", "/protocols/diagnostic"),
				arch.Bind("/go/framework/app", "/protocols/events"),
				arch.Bind("/go/framework/app", "/protocols/features"),
				arch.Bind("/go/framework/app", "/protocols/infra"),
				arch.Bind("/go/framework/app", "/protocols/migration"),
				arch.Bind("/go/framework/app", "/protocols/storage"),
				arch.Bind("/go/framework/cache", "/protocols/capabilities"),
				arch.Bind("/go/framework/cache", "/protocols/diagnostic"),
				arch.Bind("/go/framework/cache", "/protocols/features"),
				arch.Bind("/go/framework/client", "/protocols/architecture"),
				arch.Bind("/go/framework/client", "/protocols/capabilities"),
				arch.Bind("/go/framework/client", "/protocols/clientcontract"),
				arch.Bind("/go/framework/client", "/protocols/config"),
				arch.Bind("/go/framework/client", "/protocols/contracts"),
				arch.Bind("/go/framework/client", "/protocols/database"),
				arch.Bind("/go/framework/client", "/protocols/diagnostic"),
				arch.Bind("/go/framework/client", "/protocols/events"),
				arch.Bind("/go/framework/client", "/protocols/features"),
				arch.Bind("/go/framework/client", "/protocols/http-routes"),
				arch.Bind("/go/framework/client", "/protocols/identity"),
				arch.Bind("/go/framework/client", "/protocols/infra"),
				arch.Bind("/go/framework/client", "/protocols/migration"),
				arch.Bind("/go/framework/client", "/protocols/runtime"),
				arch.Bind("/go/framework/client", "/protocols/storage"),
				arch.Bind("/go/framework/config", "/protocols/capabilities"),
				arch.Bind("/go/framework/config", "/protocols/diagnostic"),
				arch.Bind("/go/framework/config", "/protocols/features"),
				arch.Bind("/go/framework/ctxutil", "/protocols/capabilities"),
				arch.Bind("/go/framework/ctxutil", "/protocols/diagnostic"),
				arch.Bind("/go/framework/ctxutil", "/protocols/features"),
				arch.Bind("/go/framework/database", "/protocols/architecture"),
				arch.Bind("/go/framework/database", "/protocols/capabilities"),
				arch.Bind("/go/framework/database", "/protocols/config"),
				arch.Bind("/go/framework/database", "/protocols/database"),
				arch.Bind("/go/framework/database", "/protocols/diagnostic"),
				arch.Bind("/go/framework/database", "/protocols/events"),
				arch.Bind("/go/framework/database", "/protocols/features"),
				arch.Bind("/go/framework/database", "/protocols/infra"),
				arch.Bind("/go/framework/database", "/protocols/migration"),
				arch.Bind("/go/framework/database", "/protocols/storage"),
				arch.Bind("/go/framework/database", "/protocols/transaction"),
				arch.Bind("/go/framework/errors", "/protocols/capabilities"),
				arch.Bind("/go/framework/errors", "/protocols/diagnostic"),
				arch.Bind("/go/framework/errors", "/protocols/features"),
				arch.Bind("/go/framework/events", "/protocols/architecture"),
				arch.Bind("/go/framework/events", "/protocols/capabilities"),
				arch.Bind("/go/framework/events", "/protocols/config"),
				arch.Bind("/go/framework/events", "/protocols/contracts"),
				arch.Bind("/go/framework/events", "/protocols/database"),
				arch.Bind("/go/framework/events", "/protocols/diagnostic"),
				arch.Bind("/go/framework/events", "/protocols/events"),
				arch.Bind("/go/framework/events", "/protocols/features"),
				arch.Bind("/go/framework/events", "/protocols/http-routes"),
				arch.Bind("/go/framework/events", "/protocols/identity"),
				arch.Bind("/go/framework/events", "/protocols/infra"),
				arch.Bind("/go/framework/events", "/protocols/keyring"),
				arch.Bind("/go/framework/events", "/protocols/migration"),
				arch.Bind("/go/framework/events", "/protocols/runtime"),
				arch.Bind("/go/framework/events", "/protocols/storage"),
				arch.Bind("/go/framework/grpc", "/protocols/architecture"),
				arch.Bind("/go/framework/grpc", "/protocols/capabilities"),
				arch.Bind("/go/framework/grpc", "/protocols/clientcontract"),
				arch.Bind("/go/framework/grpc", "/protocols/config"),
				arch.Bind("/go/framework/grpc", "/protocols/contracts"),
				arch.Bind("/go/framework/grpc", "/protocols/database"),
				arch.Bind("/go/framework/grpc", "/protocols/diagnostic"),
				arch.Bind("/go/framework/grpc", "/protocols/events"),
				arch.Bind("/go/framework/grpc", "/protocols/features"),
				arch.Bind("/go/framework/grpc", "/protocols/http-routes"),
				arch.Bind("/go/framework/grpc", "/protocols/identity"),
				arch.Bind("/go/framework/grpc", "/protocols/infra"),
				arch.Bind("/go/framework/grpc", "/protocols/keyring"),
				arch.Bind("/go/framework/grpc", "/protocols/migration"),
				arch.Bind("/go/framework/grpc", "/protocols/platform"),
				arch.Bind("/go/framework/grpc", "/protocols/runtime"),
				arch.Bind("/go/framework/grpc", "/protocols/storage"),
				arch.Bind("/go/framework/http", "/protocols/architecture"),
				arch.Bind("/go/framework/http", "/protocols/capabilities"),
				arch.Bind("/go/framework/http", "/protocols/config"),
				arch.Bind("/go/framework/http", "/protocols/contracts"),
				arch.Bind("/go/framework/http", "/protocols/database"),
				arch.Bind("/go/framework/http", "/protocols/diagnostic"),
				arch.Bind("/go/framework/http", "/protocols/events"),
				arch.Bind("/go/framework/http", "/protocols/features"),
				arch.Bind("/go/framework/http", "/protocols/http-routes"),
				arch.Bind("/go/framework/http", "/protocols/identity"),
				arch.Bind("/go/framework/http", "/protocols/infra"),
				arch.Bind("/go/framework/http", "/protocols/migration"),
				arch.Bind("/go/framework/http", "/protocols/runtime"),
				arch.Bind("/go/framework/http", "/protocols/storage"),
				arch.Bind("/go/framework/inject", "/protocols/capabilities"),
				arch.Bind("/go/framework/inject", "/protocols/diagnostic"),
				arch.Bind("/go/framework/inject", "/protocols/features"),
				arch.Bind("/go/framework/keyringstore", "/protocols/architecture"),
				arch.Bind("/go/framework/keyringstore", "/protocols/capabilities"),
				arch.Bind("/go/framework/keyringstore", "/protocols/config"),
				arch.Bind("/go/framework/keyringstore", "/protocols/contracts"),
				arch.Bind("/go/framework/keyringstore", "/protocols/database"),
				arch.Bind("/go/framework/keyringstore", "/protocols/diagnostic"),
				arch.Bind("/go/framework/keyringstore", "/protocols/events"),
				arch.Bind("/go/framework/keyringstore", "/protocols/features"),
				arch.Bind("/go/framework/keyringstore", "/protocols/http-routes"),
				arch.Bind("/go/framework/keyringstore", "/protocols/identity"),
				arch.Bind("/go/framework/keyringstore", "/protocols/infra"),
				arch.Bind("/go/framework/keyringstore", "/protocols/keyring"),
				arch.Bind("/go/framework/keyringstore", "/protocols/migration"),
				arch.Bind("/go/framework/keyringstore", "/protocols/runtime"),
				arch.Bind("/go/framework/keyringstore", "/protocols/storage"),
				arch.Bind("/go/framework/keyringstore", "/protocols/transaction"),
				arch.Bind("/go/framework/logger", "/protocols/capabilities"),
				arch.Bind("/go/framework/logger", "/protocols/diagnostic"),
				arch.Bind("/go/framework/logger", "/protocols/features"),
				arch.Bind("/go/framework/migration", "/protocols/capabilities"),
				arch.Bind("/go/framework/migration", "/protocols/database"),
				arch.Bind("/go/framework/migration", "/protocols/diagnostic"),
				arch.Bind("/go/framework/migration", "/protocols/events"),
				arch.Bind("/go/framework/migration", "/protocols/features"),
				arch.Bind("/go/framework/migration", "/protocols/infra"),
				arch.Bind("/go/framework/migration", "/protocols/migration"),
				arch.Bind("/go/framework/migration", "/protocols/storage"),
				arch.Bind("/go/framework/migration/migratecli", "/protocols/architecture"),
				arch.Bind("/go/framework/migration/migratecli", "/protocols/capabilities"),
				arch.Bind("/go/framework/migration/migratecli", "/protocols/config"),
				arch.Bind("/go/framework/migration/migratecli", "/protocols/database"),
				arch.Bind("/go/framework/migration/migratecli", "/protocols/diagnostic"),
				arch.Bind("/go/framework/migration/migratecli", "/protocols/events"),
				arch.Bind("/go/framework/migration/migratecli", "/protocols/features"),
				arch.Bind("/go/framework/migration/migratecli", "/protocols/infra"),
				arch.Bind("/go/framework/migration/migratecli", "/protocols/migration"),
				arch.Bind("/go/framework/migration/migratecli", "/protocols/storage"),
				arch.Bind("/go/framework/openapi", "/protocols/architecture"),
				arch.Bind("/go/framework/openapi", "/protocols/capabilities"),
				arch.Bind("/go/framework/openapi", "/protocols/clientcontract"),
				arch.Bind("/go/framework/openapi", "/protocols/config"),
				arch.Bind("/go/framework/openapi", "/protocols/contracts"),
				arch.Bind("/go/framework/openapi", "/protocols/database"),
				arch.Bind("/go/framework/openapi", "/protocols/diagnostic"),
				arch.Bind("/go/framework/openapi", "/protocols/events"),
				arch.Bind("/go/framework/openapi", "/protocols/features"),
				arch.Bind("/go/framework/openapi", "/protocols/http-routes"),
				arch.Bind("/go/framework/openapi", "/protocols/identity"),
				arch.Bind("/go/framework/openapi", "/protocols/infra"),
				arch.Bind("/go/framework/openapi", "/protocols/keyring"),
				arch.Bind("/go/framework/openapi", "/protocols/migration"),
				arch.Bind("/go/framework/openapi", "/protocols/platform"),
				arch.Bind("/go/framework/openapi", "/protocols/runtime"),
				arch.Bind("/go/framework/openapi", "/protocols/storage"),
				arch.Bind("/go/framework/parallel", "/protocols/capabilities"),
				arch.Bind("/go/framework/parallel", "/protocols/diagnostic"),
				arch.Bind("/go/framework/parallel", "/protocols/features"),
				arch.Bind("/go/framework/platform", "/protocols/architecture"),
				arch.Bind("/go/framework/platform", "/protocols/capabilities"),
				arch.Bind("/go/framework/platform", "/protocols/config"),
				arch.Bind("/go/framework/platform", "/protocols/contracts"),
				arch.Bind("/go/framework/platform", "/protocols/database"),
				arch.Bind("/go/framework/platform", "/protocols/diagnostic"),
				arch.Bind("/go/framework/platform", "/protocols/events"),
				arch.Bind("/go/framework/platform", "/protocols/features"),
				arch.Bind("/go/framework/platform", "/protocols/http-routes"),
				arch.Bind("/go/framework/platform", "/protocols/identity"),
				arch.Bind("/go/framework/platform", "/protocols/infra"),
				arch.Bind("/go/framework/platform", "/protocols/migration"),
				arch.Bind("/go/framework/platform", "/protocols/platform"),
				arch.Bind("/go/framework/platform", "/protocols/runtime"),
				arch.Bind("/go/framework/platform", "/protocols/storage"),
				arch.Bind("/go/framework/proto", "/protocols/architecture"),
				arch.Bind("/go/framework/proto", "/protocols/capabilities"),
				arch.Bind("/go/framework/proto", "/protocols/clientcontract"),
				arch.Bind("/go/framework/proto", "/protocols/config"),
				arch.Bind("/go/framework/proto", "/protocols/contracts"),
				arch.Bind("/go/framework/proto", "/protocols/database"),
				arch.Bind("/go/framework/proto", "/protocols/diagnostic"),
				arch.Bind("/go/framework/proto", "/protocols/events"),
				arch.Bind("/go/framework/proto", "/protocols/features"),
				arch.Bind("/go/framework/proto", "/protocols/http-routes"),
				arch.Bind("/go/framework/proto", "/protocols/identity"),
				arch.Bind("/go/framework/proto", "/protocols/infra"),
				arch.Bind("/go/framework/proto", "/protocols/migration"),
				arch.Bind("/go/framework/proto", "/protocols/platform"),
				arch.Bind("/go/framework/proto", "/protocols/runtime"),
				arch.Bind("/go/framework/proto", "/protocols/storage"),
				arch.Bind("/go/framework/schema", "/protocols/capabilities"),
				arch.Bind("/go/framework/schema", "/protocols/diagnostic"),
				arch.Bind("/go/framework/schema", "/protocols/features"),
				arch.Bind("/go/framework/security", "/protocols/architecture"),
				arch.Bind("/go/framework/security", "/protocols/capabilities"),
				arch.Bind("/go/framework/security", "/protocols/config"),
				arch.Bind("/go/framework/security", "/protocols/contracts"),
				arch.Bind("/go/framework/security", "/protocols/database"),
				arch.Bind("/go/framework/security", "/protocols/diagnostic"),
				arch.Bind("/go/framework/security", "/protocols/events"),
				arch.Bind("/go/framework/security", "/protocols/features"),
				arch.Bind("/go/framework/security", "/protocols/http-routes"),
				arch.Bind("/go/framework/security", "/protocols/identity"),
				arch.Bind("/go/framework/security", "/protocols/infra"),
				arch.Bind("/go/framework/security", "/protocols/keyring"),
				arch.Bind("/go/framework/security", "/protocols/migration"),
				arch.Bind("/go/framework/security", "/protocols/runtime"),
				arch.Bind("/go/framework/security", "/protocols/storage"),
				arch.Bind("/go/framework/storage", "/protocols/architecture"),
				arch.Bind("/go/framework/storage", "/protocols/capabilities"),
				arch.Bind("/go/framework/storage", "/protocols/config"),
				arch.Bind("/go/framework/storage", "/protocols/database"),
				arch.Bind("/go/framework/storage", "/protocols/diagnostic"),
				arch.Bind("/go/framework/storage", "/protocols/events"),
				arch.Bind("/go/framework/storage", "/protocols/features"),
				arch.Bind("/go/framework/storage", "/protocols/infra"),
				arch.Bind("/go/framework/storage", "/protocols/migration"),
				arch.Bind("/go/framework/storage", "/protocols/storage"),
				arch.Bind("/go/framework/telemetry", "/protocols/architecture"),
				arch.Bind("/go/framework/telemetry", "/protocols/capabilities"),
				arch.Bind("/go/framework/telemetry", "/protocols/clientcontract"),
				arch.Bind("/go/framework/telemetry", "/protocols/config"),
				arch.Bind("/go/framework/telemetry", "/protocols/contracts"),
				arch.Bind("/go/framework/telemetry", "/protocols/database"),
				arch.Bind("/go/framework/telemetry", "/protocols/diagnostic"),
				arch.Bind("/go/framework/telemetry", "/protocols/events"),
				arch.Bind("/go/framework/telemetry", "/protocols/features"),
				arch.Bind("/go/framework/telemetry", "/protocols/http-routes"),
				arch.Bind("/go/framework/telemetry", "/protocols/identity"),
				arch.Bind("/go/framework/telemetry", "/protocols/infra"),
				arch.Bind("/go/framework/telemetry", "/protocols/migration"),
				arch.Bind("/go/framework/telemetry", "/protocols/runtime"),
				arch.Bind("/go/framework/telemetry", "/protocols/storage"),
				arch.Bind("/go/framework/telemetry", "/protocols/telemetry"),
			),
		)
}

func authoredTypeScriptFrameworkDomain() *arch.Builder {
	return arch.NewDomain("typescript-framework", "framework").
		Projects(
			"/typescript/framework/analytics",
			"/typescript/framework/application",
			"/typescript/framework/cli-protocol",
			"/typescript/framework/client",
			"/typescript/framework/database",
			"/typescript/framework/document",
			"/typescript/framework/events",
			"/typescript/framework/migration",
			"/typescript/framework/runtime",
			"/typescript/framework/spectest",
			"/typescript/framework/storage",
			"/typescript/framework/ui",
			"/typescript/framework/utils",
			"/typescript/framework/web",
		).
		Owns(
			arch.Concept("typescript-framework.api-documentation", archproto.OwnershipFact,
				"The authored TypeScript framework documentation tree under /typescript/doc/framework. This domain decides what the published TypeScript reference says; a site that publishes it copies the tree and never edits it."),
			arch.Concept("typescript-framework.application-runtime", archproto.OwnershipAPI,
				"The TypeScript application runtime surface published as @putnami/*: the application container and lifecycle, HTTP and web rendering, the UI component set, database and storage access, events, and the client. This domain decides what a Putnami TypeScript application can express; a tenant application composes that surface and owns none of its shape."),
		).
		Export(archproto.Export{
			ID:          "typescript-framework.api-documentation.v1",
			Version:     1,
			Status:      archproto.StatusActive,
			Description: "The authored TypeScript framework documentation tree (/typescript/doc/framework): the markdown a publisher copies verbatim, addressed by a deterministic digest over its sorted (relative path, content digest) pairs. The carrier contract is putnami.documentation-tree.v1.",
			Facts: []archproto.Fact{
				arch.Fact("typescript_framework_documentation", "typescript-framework",
					archproto.ClassificationPublic, archproto.PersonalDataNone),
			},
			Modes:         []archproto.AccessMode{archproto.ModeSnapshot},
			Compatibility: arch.Compatible(archproto.CompatibilityAdditive, 1),
		}).
		Export(archproto.Export{
			ID:          "typescript-framework.application-runtime.v1",
			Version:     1,
			Status:      archproto.StatusActive,
			Description: "The published @putnami/* runtime packages a Putnami TypeScript application links: the application container and lifecycle, web rendering, the UI component set, database, storage, events, analytics collection, and the client. A consumer holds a stable reference to the packages; this domain retains sole authority over their API.",
			Facts: []archproto.Fact{
				arch.Fact("typescript_application_runtime_api", "typescript-framework",
					archproto.ClassificationPublic, archproto.PersonalDataNone),
			},
			Modes:         []archproto.AccessMode{archproto.ModeReference},
			Compatibility: arch.Compatible(archproto.CompatibilityAdditive, 1),
		}).
		Import()
}

func authoredPublicDocsDomain() *arch.Builder {
	return arch.NewDomain("public-docs", "public-docs").
		Projects(
			"/sites/putnami.dev",
		).
		Owns(
			arch.Concept("public-docs.published-documentation-site", archproto.OwnershipModel,
				"putnami.dev's published information architecture: which documentation trees appear on the site, at which section, in what order, and under which navigation. This domain owns placement, navigation, and the site's own authored pages; it never owns the content of a tree another domain authored."),
		).
		Import(
			arch.Reference("public-docs.application-runtime.v1", 1, arch.From("typescript-framework", "typescript-framework.application-runtime.v1"),
				arch.As("public-docs.application-runtime"),
				arch.Status(archproto.StatusActive),
				arch.Facts("typescript_application_runtime_api"),
				arch.Justification("putnami.dev is an ordinary Putnami TypeScript application: it boots on the application container, renders its pages through the web runtime, draws them with the shared UI components, and measures its own audience with the analytics plugin into a database it owns. The site consumes the public framework API the same way a tenant application does, and reaches into no framework internal."),
				arch.Bind("/sites/putnami.dev", "/typescript/framework/analytics"),
				arch.Bind("/sites/putnami.dev", "/typescript/framework/application"),
				arch.Bind("/sites/putnami.dev", "/typescript/framework/database"),
				arch.Bind("/sites/putnami.dev", "/typescript/framework/runtime"),
				arch.Bind("/sites/putnami.dev", "/typescript/framework/ui"),
				arch.Bind("/sites/putnami.dev", "/typescript/framework/utils"),
				arch.Bind("/sites/putnami.dev", "/typescript/framework/web"),
			),
			arch.Snapshot("public-docs.go-framework-documentation.v1", 1, arch.From("go-framework", "go-framework.api-documentation.v1"), arch.Carried(archproto.TransportFile, "putnami.documentation-tree.v1", archproto.StatusActive),
				arch.As("public-docs.go-framework-documentation"),
				arch.Status(archproto.StatusActive),
				arch.Facts("go_framework_documentation"),
				arch.Guarantees(archproto.Consistency{
					MaxStaleness:   "720h",
					OnMissing:      archproto.FailureFailClosed,
					OnStale:        archproto.FailureUseStale,
					Ordering:       archproto.OrderingSourceVersion,
					SourceVersion:  "tree_digest",
					IdempotencyKey: "tree_digest",
					LateEvents:     archproto.LateEventIgnoreOlder,
				}),
				arch.Justification("The site publishes the Go framework reference at public/docs/09-frameworks/02-go by copying the /go/doc/framework tree verbatim at build time. The go-framework domain decides what that reference says; the site decides only where it appears and in what order. A missing tree fails the build rather than publishing a gap, and a tree older than the freshness bound is still published because stale documentation beats no documentation."),
			),
			arch.Snapshot("public-docs.method-documentation.v1", 1, arch.From("sdd", "sdd.method-documentation.v1"), arch.Carried(archproto.TransportFile, "putnami.documentation-tree.v1", archproto.StatusActive),
				arch.As("public-docs.method-documentation"),
				arch.Status(archproto.StatusActive),
				arch.Facts("spec_driven_development_documentation"),
				arch.Guarantees(archproto.Consistency{
					MaxStaleness:   "720h",
					OnMissing:      archproto.FailureFailClosed,
					OnStale:        archproto.FailureUseStale,
					Ordering:       archproto.OrderingSourceVersion,
					SourceVersion:  "tree_digest",
					IdempotencyKey: "tree_digest",
					LateEvents:     archproto.LateEventIgnoreOlder,
				}),
				arch.Justification("The site publishes the spec-driven development method at public/docs/07-spec-driven-development by copying the /tooling/sdd-extension/doc tree verbatim at build time. The sdd domain owns the method's wording; the site owns only its placement in the published information architecture."),
			),
			arch.Snapshot("public-docs.python-surface-documentation.v1", 1, arch.From("extension-providers", "extension-providers.python-surface-documentation.v1"), arch.Carried(archproto.TransportFile, "putnami.documentation-tree.v1", archproto.StatusActive),
				arch.As("public-docs.python-surface-documentation"),
				arch.Status(archproto.StatusActive),
				arch.Facts("python_surface_documentation"),
				arch.Guarantees(archproto.Consistency{
					MaxStaleness:   "720h",
					OnMissing:      archproto.FailureFailClosed,
					OnStale:        archproto.FailureUseStale,
					Ordering:       archproto.OrderingSourceVersion,
					SourceVersion:  "tree_digest",
					IdempotencyKey: "tree_digest",
					LateEvents:     archproto.LateEventIgnoreOlder,
				}),
				arch.Justification("The site publishes the Python surface reference at public/docs/09-frameworks/03-python by copying the /python/doc/framework tree verbatim at build time. The Python surface is the extension and its templates only, and the extension-providers domain decides what that reference claims; the site never promotes it to framework parity by where it places it."),
			),
			arch.Snapshot("public-docs.typescript-framework-documentation.v1", 1, arch.From("typescript-framework", "typescript-framework.api-documentation.v1"), arch.Carried(archproto.TransportFile, "putnami.documentation-tree.v1", archproto.StatusActive),
				arch.As("public-docs.typescript-framework-documentation"),
				arch.Status(archproto.StatusActive),
				arch.Facts("typescript_framework_documentation"),
				arch.Guarantees(archproto.Consistency{
					MaxStaleness:   "720h",
					OnMissing:      archproto.FailureFailClosed,
					OnStale:        archproto.FailureUseStale,
					Ordering:       archproto.OrderingSourceVersion,
					SourceVersion:  "tree_digest",
					IdempotencyKey: "tree_digest",
					LateEvents:     archproto.LateEventIgnoreOlder,
				}),
				arch.Justification("The site publishes the TypeScript framework reference at public/docs/09-frameworks/01-typescript by copying the /typescript/doc/framework tree verbatim at build time. The typescript-framework domain decides what that reference says; the site decides only where it appears. This is a separate contract from the site's runtime dependency on the same domain: one is a copied document, the other is a linked API."),
			),
			arch.Snapshot("public-docs.workspace-documentation.v1", 1, arch.From("cli", "cli.workspace-documentation.v1"), arch.Carried(archproto.TransportFile, "putnami.documentation-tree.v1", archproto.StatusActive),
				arch.As("public-docs.workspace-documentation"),
				arch.Status(archproto.StatusActive),
				arch.Facts("workspace_documentation"),
				arch.Guarantees(archproto.Consistency{
					MaxStaleness:   "720h",
					OnMissing:      archproto.FailureFailClosed,
					OnStale:        archproto.FailureUseStale,
					Ordering:       archproto.OrderingSourceVersion,
					SourceVersion:  "tree_digest",
					IdempotencyKey: "tree_digest",
					LateEvents:     archproto.LateEventIgnoreOlder,
				}),
				arch.Justification("The site publishes the workspace and CLI reference at public/docs/08-tooling-&-workspace by copying the /tooling/doc tree verbatim at build time. The cli domain decides what that reference says; the site owns only its section number and its position in the navigation."),
			),
		)
}

// The agent-workflows domain owns content and a rule, not a mechanism: the
// agent content of the @putnami/contributor extension. The builder that turns
// that source into an archive belongs to the packager, so this domain carries
// no code and no protocol dependency of its own; what it exports is the source
// a packager reads.
func authoredAgentWorkflowsDomain() *arch.Builder {
	return arch.NewDomain("agent-workflows", "tooling").
		Projects(
			"/tooling/contributor",
		).
		Owns(
			arch.Concept("agent-workflows.public-agent-workflows", archproto.OwnershipModel,
				"The agent workflows Putnami publishes and consumes: the agent content of the @putnami/contributor extension, with its source skills and worker bodies, the host metadata written beside them, the discovery layout, and the declared content policy that decides what may ship under the extension's identity. The repository-root .agents, .claude, and .codex trees are generated from this source; only the audit skill and host policy files stay outside this authority."),
		).
		Export(archproto.Export{
			ID:          "agent-workflows.artifact-source.v1",
			Version:     1,
			Status:      archproto.StatusActive,
			Description: "One extension's packageable agent-content source: the closed src/ layout, the extension identity, and the content policy declared in the project. A packager reads it to produce the distributable archive; nothing here decides how the archive is built or published.",
			Facts: []archproto.Fact{
				arch.Fact("agent_workflow_content", "agent-workflows",
					archproto.ClassificationPublic, archproto.PersonalDataNone),
				arch.Fact("agent_artifact_content_policy", "agent-workflows",
					archproto.ClassificationPublic, archproto.PersonalDataNone),
			},
			Modes:         []archproto.AccessMode{archproto.ModeReference},
			Compatibility: arch.Compatible(archproto.CompatibilityAdditive, 1),
		})
}

// TestCommittedGoFrameworkDomainManifestIsTheAuthoredOne holds the go-framework
// domain's committed manifest to its authoring.
func TestCommittedGoFrameworkDomainManifestIsTheAuthoredOne(t *testing.T) {
	arch.Pin(t, "../../go/framework/putnami.architecture.json", authoredGoFrameworkDomain)
}

// TestCommittedTypeScriptFrameworkDomainManifestIsTheAuthoredOne holds the
// typescript-framework domain's committed manifest to its authoring.
func TestCommittedTypeScriptFrameworkDomainManifestIsTheAuthoredOne(t *testing.T) {
	arch.Pin(t, "../../typescript/framework/putnami.architecture.json", authoredTypeScriptFrameworkDomain)
}

// TestCommittedPublicDocsDomainManifestIsTheAuthoredOne holds the public-docs
// domain's committed manifest to its authoring.
func TestCommittedPublicDocsDomainManifestIsTheAuthoredOne(t *testing.T) {
	arch.Pin(t, "../../sites/putnami.dev/putnami.architecture.json", authoredPublicDocsDomain)
}

// TestCommittedAgentWorkflowsDomainManifestIsTheAuthoredOne holds the
// agent-workflows domain's committed manifest to its authoring.
func TestCommittedAgentWorkflowsDomainManifestIsTheAuthoredOne(t *testing.T) {
	arch.Pin(t, "../contributor/putnami.architecture.json", authoredAgentWorkflowsDomain)
}
