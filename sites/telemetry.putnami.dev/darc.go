package main

import (
	archproto "go.putnami.dev/protocol/architecture"

	"go.putnami.dev/app/darc"
)

// The observability domain's cross-domain access, as runtime components.
//
// This workload declares two imports in `putnami.architecture.json`, both
// REFERENCES: one to the shared strict wire contracts, one to the Go
// application runtime API. Nothing is copied, so the only promise a reference
// makes is minimization — the import names the exact facts it consumes, and
// reaching past them is a boundary crossing nobody reviewed.
//
// Registering a component does two things. It makes that promise enforceable
// where it is used, and it makes the implementation VISIBLE: describe records
// one `domainAccess` row per component in this project's capability manifest,
// and `putnami architecture validate` joins those rows to the declarations.
// Without a row, "the manifest says this domain imports the wire contracts" and
// "the code does" are two unconnected statements.
//
// Both contracts below must stay the committed declaration. They are not copies
// that may drift: if one says something the manifest does not, the emitted row
// matches no declaration and `architecture validate` fails with
// `architecture.evidence_without_declaration`. That failure is the point.
//
// The two are also atomic. `declared_without_evidence` fires inside a domain
// that already emits evidence, and then for EVERY active import it declares —
// so `observability` cannot implement one of its two imports and stay silent
// about the other.
var protocolContractsImport = archproto.Import{
	ID:      "observability.protocol-contracts.v1",
	Version: 1,
	From: archproto.ExportReference{
		Domain: "protocols",
		Export: "protocols.wire-contracts.v1",
	},
	As:     "observability.protocol-contracts",
	Mode:   archproto.ModeReference,
	Status: archproto.StatusActive,
	Facts:  []string{"wire_contract_definitions"},
	Justification: "The telemetry workload is an ordinary Putnami Go application; it consumes the shared strict contract packages " +
		"like any tenant, never a private parse.",
}

// The second reference: the Go application runtime API this workload is built
// on. `main.go` is an ordinary Putnami Go application — it calls `app.New`,
// mounts an `http` server, opens a `database` pool, exports through `telemetry`,
// authenticates the aggregate route through `security`, and ships its schema
// through `migration`. Every one of those is a framework surface owned by the
// `go-framework` domain, resolved at its owner and never forked here.
//
// Reference is the honest mode for it. A runtime API is linked, not copied:
// there is no local model to keep fresh, no ordering to preserve, and no
// staleness bound to enforce — the compiler resolves the identity and the
// framework stays the authority for what it means.
var applicationRuntimeImport = archproto.Import{
	ID:      "observability.application-runtime.v1",
	Version: 1,
	From: archproto.ExportReference{
		Domain: "go-framework",
		Export: "go-framework.application-runtime.v1",
	},
	As:     "observability.application-runtime",
	Mode:   archproto.ModeReference,
	Status: archproto.StatusActive,
	Facts:  []string{"go_application_runtime_api"},
	Justification: "The telemetry receiver is an ordinary Putnami Go application: it boots on the app lifecycle, serves its " +
		"ingest over the framework HTTP runtime, stores aggregates through the framework database and migration runtime, and " +
		"reports itself through the framework telemetry runtime. It consumes the public go.putnami.dev API exactly as a tenant " +
		"workload does and reaches into no framework internal.",
}

// newDomainAccessPlugin builds the components this workload enforces and the
// describe-only carrier that reports them. Each constructor validates its
// contract through `go.putnami.dev/protocol/architecture` — the same verdict the
// gate applies — so a contract the gate would reject never reaches the runtime.
func newDomainAccessPlugin() (*darc.Plugin, error) {
	components := make([]darc.Component, 0, len(domainAccessImports))
	for _, contract := range domainAccessImports {
		component, err := darc.NewReference(contract)
		if err != nil {
			return nil, err
		}
		components = append(components, component)
	}
	return darc.NewPlugin("observability-contracts", components...), nil
}

// domainAccessImports is every contract this workload enforces, in declaration
// order. The plugin sorts the emitted rows itself, so the order here is for
// readers only.
var domainAccessImports = []archproto.Import{
	protocolContractsImport,
	applicationRuntimeImport,
}
