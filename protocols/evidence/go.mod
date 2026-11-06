module go.putnami.dev/protocol/evidence

go 1.25.7

require (
	go.putnami.dev/protocol/codehost v0.0.0
	go.putnami.dev/protocol/diagnostic v0.0.0
	// features is TEST-ONLY: spectest binds the conformance tests to the
	// requirements of evidence/change-evidence-document.
	go.putnami.dev/protocol/features v0.0.0
)

replace go.putnami.dev/protocol/capabilities => ../capabilities

replace go.putnami.dev/protocol/codehost => ../codehost

replace go.putnami.dev/protocol/diagnostic => ../diagnostic

replace go.putnami.dev/protocol/features => ../features
