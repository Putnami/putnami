module go.putnami.dev/evidence/readers

go 1.25.7

require (
	go.putnami.dev/protocol/diagnostic v0.0.0
	// features is TEST-ONLY: spectest binds the tests to the requirements of
	// evidence-readers/report-readers.
	go.putnami.dev/protocol/features v0.0.0
)

replace (
	go.putnami.dev/protocol/capabilities => ../../protocols/capabilities
	go.putnami.dev/protocol/diagnostic => ../../protocols/diagnostic
	go.putnami.dev/protocol/features => ../../protocols/features
)
