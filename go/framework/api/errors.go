package api

import "go.putnami.dev/errors"

// Error codes for the api package. They mirror the sibling framework modules
// (app, inject) so callers and tooling can branch on errors.Code instead of
// matching error strings.
const (
	// CodeTransportUnsupported indicates the configured transport server does
	// not provide a capability a registered endpoint requires (today: stream
	// endpoints on a server that does not implement StreamServer).
	CodeTransportUnsupported errors.Code = "api.transport_unsupported"
	// CodeClientGenConfig indicates invalid typed-client generation options.
	CodeClientGenConfig errors.Code = "api.clientgen_config"
	// CodeClientGenCollision indicates two routes derive the same generated
	// client method name, which would emit non-compiling Go.
	CodeClientGenCollision errors.Code = "api.clientgen_collision"
	// CodeClientGenUnsupportedSemantic indicates the contract declares a shape
	// no first-party runtime can honor yet. It is a generation failure on
	// purpose: a client that compiles and then fails on its first message is the
	// permissive fallback the first-party contract forbids.
	CodeClientGenUnsupportedSemantic errors.Code = "api.clientgen_unsupported_semantic"
	// CodeClientGenFormat indicates the generated client source failed to gofmt,
	// which means the emitter produced syntactically invalid Go (a generator bug).
	CodeClientGenFormat errors.Code = "api.clientgen_format"
)
