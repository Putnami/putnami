module go.putnami.dev/protocol/runner

go 1.25.7

require (
	go.putnami.dev/protocol/cli v0.0.0
	// features is TEST-ONLY: spectest binds the conformance tests to the
	// requirements of runner/runner-contract.
	go.putnami.dev/protocol/features v0.0.0
)

replace go.putnami.dev/protocol/capabilities => ../capabilities

replace go.putnami.dev/protocol/cli => ../cli

replace go.putnami.dev/protocol/diagnostic => ../diagnostic

replace go.putnami.dev/protocol/features => ../features
