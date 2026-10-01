// Package identity anchors the frameworks' consumer-side identity vocabulary:
// the well-known token claims the Go and TypeScript frameworks extract from a
// verified credential and the principal kinds they recognize.
//
// Unlike its sibling protocol packages, this module's source of truth is not
// hand-written Go: it is the authored contract manifest at
// schema/contracts.json (the canonical IR of go.putnami.dev/protocol/contracts),
// from which `putnami contracts generate` produces the committed artifacts —
// the Go type twin (schema/contracts.gen.go, package
// go.putnami.dev/protocol/identity/schema), the TypeScript twin
// (schema/contracts.gen.ts), the JSON Schema over the DTO vocabulary
// (schema/contracts.schema.json), and the reference tables
// (schema/contracts.md). `putnami contracts check` and
// schema/contracts_test.go guard the committed artifacts against drift.
//
// The vocabulary is deliberately thin: the frameworks are token consumers, so
// the contract declares the claims they read (sub, iss, client_id, roles,
// scope, aud, exp), the principal kinds they produce (user, apikey), and the
// authorization decision labels they report (AuthDecision) — and no scopes or
// grants, because issuer-side vocabulary lives with the issuer.
package identity
