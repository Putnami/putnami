module go.putnami.dev/protocol/codehost

go 1.25.7

require (
	// features is TEST-ONLY: spectest binds the conformance tests to the
	// requirements of codehost/code-host-vocabulary.
	go.putnami.dev/protocol/features v0.0.0
)

replace go.putnami.dev/protocol/capabilities => ../capabilities

replace go.putnami.dev/protocol/diagnostic => ../diagnostic

replace go.putnami.dev/protocol/features => ../features
