module go.putnami.dev/tooling/stacks

go 1.25.7

require (
	go.putnami.dev/evidence/readers v0.0.0
	go.putnami.dev/protocol/diagnostic v0.0.0
)

replace (
	go.putnami.dev/evidence/readers => ../evidence-readers
	go.putnami.dev/protocol/capabilities => ../../protocols/capabilities
	go.putnami.dev/protocol/diagnostic => ../../protocols/diagnostic
	go.putnami.dev/protocol/features => ../../protocols/features
)
