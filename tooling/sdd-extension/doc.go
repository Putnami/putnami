// Package sdd is the @putnami/sdd extension: the specification-driven
// development vertical — features, specs, architecture and contracts — as a
// standalone first-party extension instead of a compiled-in part of the CLI.
//
// This package itself holds no code. It exists so putnami.extension.json has a
// conformance harness that lives beside it, in manifest_contract_test.go: the
// committed manifest must be exactly what the extension SDK's builder produces
// for it, so the document has ONE author and a hand edit that drifts from the
// authoring program is a test failure rather than a discovery-time surprise in
// a consumer's workspace.
//
// The extension's executable is cmd/putnami-sdd. The engines it dispatches to
// arrive in later commits; what is here now is the scaffold: the
// project, the prepared runtime, and the manifest contract.
package sdd
