// The proof that go.putnami.dev/protocol/diagnostic holds the rules of its
// spec. Its tests use only the protocol's exported API. The protocol cannot
// bind them itself: go.putnami.dev/protocol/features requires it, so a test of
// the protocol that imported spectest would make the two projects depend on
// each other. Nothing publishes this module.
module go.putnami.dev/protocol/diagnostic/proof

go 1.25.7

require (
	go.putnami.dev/protocol/diagnostic v0.0.0
	go.putnami.dev/protocol/features v0.0.0
)

replace (
	go.putnami.dev/protocol/capabilities => ../../capabilities
	go.putnami.dev/protocol/diagnostic => ../../diagnostic
	go.putnami.dev/protocol/features => ../../features
)
