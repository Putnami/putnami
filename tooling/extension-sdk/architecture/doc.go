// Package architecture authors ARC/DARC domain manifests in Go and refuses to
// emit one that violates the architecture protocol.
//
// A putnami.architecture.json is normally hand-written, and the pilot that
// proved the format wrote roughly 900 lines of it by hand, with every
// binding copy-pasted out of `architecture validate` output. That does not
// scale, and the failure mode is quiet: a manifest a consumer's workspace would
// reject is only rejected when someone runs the gate, far from the domain that
// wrote it.
//
// This package moves that verdict to authoring time. Build runs the same
// verdict the gate runs — architecture.ValidateManifest, the protocol's own
// local-invariant judgment — so an authoring that violates
//
//   - IDENTITY — a domain, export, import, fact, or project ID that is not the
//     exact shape the protocol accepts;
//   - MODE SEMANTICS — a projection without bootstrap, updates, consistency,
//     deletion, and a local model; a query, snapshot, or command without one
//     transport; a projection whose projected fields are not exactly its
//     minimized imported facts;
//   - PLANNED IS NOT OBSERVED — a planned import that claims a current
//     project-dependency binding, or an active import carried by a transport
//     that is not itself active;
//
// fails in the authoring program with a diagnostic naming the field, instead of
// in a consumer's plan. The rules themselves are never restated here: they live
// in protocols/architecture, which is also what `architecture validate`
// enforces, so the SDK and the gate cannot drift apart.
//
// # What this package does not author
//
// The PERMISSION half of a manifest stays a human decision. Bind states that a
// named project dependency is an allowed implementation of a declared import,
// and it exists so a reviewer reads that permission in a diff — not so a
// generator can mint one from an observed edge. ADR 0001 forbids the second
// (`protocols/architecture/doc/adr/0001-…`): debt must never become permission
// by machine. Nothing here reads a workspace, a graph, or a detector.
//
// # Pin
//
// Pin holds a committed manifest to a builder authoring: the Go program is the
// author, the JSON is its projection, and a hand edit to the JSON that the
// program does not make is a test failure rather than a difference nobody
// notices. It generalizes @putnami/sdd's own manifest harness
// (tooling/sdd-extension/manifest_contract_test.go) to any domain.
package architecture
