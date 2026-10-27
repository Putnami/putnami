module go.putnami.dev/protocol/put

go 1.25.7

require (
	go.putnami.dev/protocol/diagnostic v0.0.0
	go.putnami.dev/protocol/distribution v0.0.0
	// features is TEST-ONLY: spectest binds the conformance tests to the
	// requirements of put/immutable-write-contract.
	go.putnami.dev/protocol/features v0.0.0
)

replace (
	go.putnami.dev/protocol/capabilities => ../capabilities
	go.putnami.dev/protocol/diagnostic => ../diagnostic
	go.putnami.dev/protocol/distribution => ../distribution
	go.putnami.dev/protocol/features => ../features
)
