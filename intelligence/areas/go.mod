module go.putnami.dev/intelligence/areas

go 1.25.7

require go.putnami.dev/protocol/features v0.0.0

replace (
	go.putnami.dev/protocol/capabilities => ../../protocols/capabilities
	go.putnami.dev/protocol/diagnostic => ../../protocols/diagnostic
	go.putnami.dev/protocol/features => ../../protocols/features
)
