# Domain Manifest Authoring

The `architecture` package builds a domain's `putnami.architecture.json` in Go and refuses to emit one that violates the ARC/DARC protocol. It is the authoring-time counterpart of `putnami architecture validate`: both run `architecture.ValidateManifest`, so a manifest that builds here is one the gate accepts.

> `go.putnami.dev/protocol/architecture` is **experimental**. Its wire format, finding IDs, and detector coverage may change without a migration path, and this package changes with it.

## Overview

A domain manifest is normally hand-written JSON. The pilot that proved the format wrote roughly 900 lines of it by hand, with every binding copy-pasted out of `architecture validate` output. Two things go wrong at that scale:

- an authoring mistake is only found when someone runs the gate, in a *workspace*, far from the domain that wrote it;
- the JSON is the only statement of the domain, so nothing holds it to a reviewed intent — a hand edit is just a diff.

Building the manifest through this package fixes the first. `Pin` fixes the second: the Go program is the author, the committed JSON is its projection, and a hand edit the program does not make fails the owning project's test run.

| Invariant | Meaning | Diagnostic code |
|-----------|---------|-----------------|
| IDENTITY | Domain, export, import, fact, and project IDs are the exact shapes the protocol accepts, and no identity is declared twice | `architecture.invalid_domain`, `architecture.invalid_id`, `architecture.invalid_project`, `architecture.duplicate_*` |
| MODE SEMANTICS | A projection declares bootstrap, updates, consistency, deletion, and a local model whose projected fields are exactly its minimized facts; a query, snapshot, or command declares one transport | `architecture.invalid_projection`, `architecture.invalid_transport`, `architecture.invalid_consistency`, `architecture.invalid_deletion` |
| PLANNED IS NOT OBSERVED | A planned import cannot claim a current project binding, and an active import cannot depend on a planned carrier | `architecture.invalid_binding`, `architecture.invalid_status` |

The rules live in `protocols/architecture` and are the same ones `architecture validate` applies, so the SDK and the gate cannot drift apart.

## The red line

The **permission** half of a manifest stays declarative and human-reviewed.

`Bind` states that a named project dependency is an allowed implementation of a declared import. It takes two exact project IDs from its caller and reads nothing — no workspace, no project graph, no detector. There is deliberately no constructor that turns an observed edge into an authorization: that is the "debt becomes permission" anti-pattern [ADR 0001](../../../protocols/architecture/doc/adr/0001-declarations-are-authority-observations-are-evidence.md) exists to forbid. `putnami architecture sync` mechanizes the *mechanical* half (the project list, stale-binding removal) and refuses to create an import for the same reason.

## Usage

```go
import (
    arch "go.putnami.dev/sdk/extension/architecture"
    archproto "go.putnami.dev/protocol/architecture"
)

func authoredDomain() *arch.Builder {
    return arch.NewDomain("observability", "observability").
        Projects("/sites/telemetry.putnami.dev").
        Owns(arch.Concept("observability.cli-usage-aggregates", archproto.OwnershipModel,
            "Daily anonymous CLI-usage aggregates; retention and bucketing are local authority.")).
        Export(archproto.Export{
            ID:          "observability.usage-ingest.v1",
            Version:     1,
            Status:      archproto.StatusActive,
            Description: "The anonymous CLI-usage ingest.",
            Facts: []archproto.Fact{
                arch.Fact("usage_ingest", "observability",
                    archproto.ClassificationInternal, archproto.PersonalDataNone),
            },
            Modes:         []archproto.AccessMode{archproto.ModeCommand},
            Compatibility: arch.Compatible(archproto.CompatibilityAdditive, 1),
        }).
        Import(arch.Reference("observability.protocol-contracts.v1", 1,
            arch.From("protocols", "protocols.wire-contracts.v1"),
            arch.As("observability.protocol-contracts"),
            arch.Status(archproto.StatusActive),
            arch.Facts("wire_contract_definitions"),
            arch.Justification("The workload consumes the shared strict contract packages, never a private parse."),
            arch.Bind("/sites/telemetry.putnami.dev", "/protocols/telemetry"),
        ))
}
```

`Build()` returns the validated manifest; `CanonicalBytes()` returns the exact bytes a committed file holds — identity-sorted, two-space indented, one trailing newline — and reads them back through the protocol's strict reader before returning them.

## Export facts

Use `Fact(name, authority, classification, personalData)` whenever an export
allows a data-carrying or effectful mode. Both metadata values remain explicit
for `query`, `snapshot`, `projection`, and `command` exports.

Use `ReferenceFact(name, authority)` for a pure code surface whose sole mode is
`reference`:

```go
archproto.Export{
    ID:          "protocols.wire-contracts.v1",
    Version:     1,
    Status:      archproto.StatusActive,
    Description: "Shared strict contract packages.",
    Facts: []archproto.Fact{
        arch.ReferenceFact("wire_contract_definitions", "protocols"),
    },
    Modes:         []archproto.AccessMode{archproto.ModeReference},
    Compatibility: arch.Compatible(archproto.CompatibilityAdditive, 1),
}
```

`ReferenceFact` supplies no defaults. `Build` rejects it if the export also
allows any non-reference mode, and `Fact` remains valid for a reference-only
surface whose author deliberately wants to retain explicit classifications.

## The five import constructors

There is one constructor per access mode, and each sets only the members its mode allows. That is why they are five functions rather than one `Mode` option: the combinations the protocol rejects are not writable through this package at all.

| Constructor | Carriers | What it means |
|---|---|---|
| `Reference` | none | The consumer keeps a stable handle and resolves it at the owner. Nothing is copied. |
| `Query` | one transport | A read the producer answers on demand. Needs `Guarantees`. |
| `Snapshot` | one transport | An immutable version-addressed copy, replaced rather than updated. Needs `Guarantees`. |
| `Command` | one transport | A request the producer decides whether to honor. |
| `Projection` | bootstrap **and** updates | A rebuildable local copy. Needs `Guarantees`, `Deletes` or `Tombstones`, and `Model`. |

Deletion has the same shape-split: `Tombstones(field)` names the marker a tombstone strategy requires, and `Deletes(strategy)` covers the strategies that must not name one. Transports likewise: `Carried(kind, contract, availability)` names a carrier, and `NoCarrier(availability)` cannot name one.

Every import defaults to `planned`. An authoring that forgets `Status` describes an intention, never as-built reality.

## Pinning a committed manifest to its authoring

```go
func TestCommittedManifestIsTheAuthoredOne(t *testing.T) {
    arch.Pin(t, "../../putnami.architecture.json", authoredDomain)
}
```

Both documents are compared in the **protocol's** canonical form: the committed file is read through the strict reader `architecture validate` uses and re-rendered by the protocol's own canonical writer, and the authoring is rendered by that same writer. The comparison is therefore over the document — indentation, member order inside an object, and the order sibling exports were written in belong to the encoder, not to the manifest. A committed file that is not itself in sorted order still passes; reordering a reviewed permission list is a diff nobody asked for.

The normalization has to be the protocol's. A hand-rolled one would be a second opinion about canonical form, and the drift between two opinions is exactly what the pin exists to catch.

`Pin` reads no workspace, so it cannot judge whether a bound project exists or belongs to the domain that claims it. Those are cross-document questions, and `architecture validate` answers them over the whole workspace in the gate.

## Scaffolding a new domain

`putnami architecture init <domain>` writes the first canonical manifest for a
domain without any Go code, and `putnami architecture sync` reconciles the
mechanical half of existing ones — see
[`@putnami/sdd`'s command reference](../../sdd-extension/doc/03-commands.md#init-and-sync--the-authoring-half).
Moving a manifest's authoring into a `Pin`-backed builder test is a later,
optional step, and it is what the domains in this repository do.
