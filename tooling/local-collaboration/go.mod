module go.putnami.dev/tooling/local-collaboration

go 1.25.7

require (
	go.putnami.dev/protocol/cli v0.0.0
	go.putnami.dev/protocol/collaboration v0.0.0
	go.putnami.dev/protocol/diagnostic v0.0.0
	go.putnami.dev/protocol/extension v0.0.0
	go.putnami.dev/protocol/runtime v0.0.0
	go.putnami.dev/sdk/extension v0.0.0
)

require golang.org/x/sys v0.44.0 // indirect

replace (
	go.putnami.dev/protocol/architecture => ../../protocols/architecture
	go.putnami.dev/protocol/capabilities => ../../protocols/capabilities
	go.putnami.dev/protocol/cli => ../../protocols/cli
	go.putnami.dev/protocol/collaboration => ../../protocols/collaboration
	go.putnami.dev/protocol/database => ../../protocols/database
	go.putnami.dev/protocol/diagnostic => ../../protocols/diagnostic
	go.putnami.dev/protocol/distribution => ../../protocols/distribution
	go.putnami.dev/protocol/events => ../../protocols/events
	go.putnami.dev/protocol/extension => ../../protocols/extension
	go.putnami.dev/protocol/features => ../../protocols/features
	go.putnami.dev/protocol/gomod => ../../protocols/gomod
	go.putnami.dev/protocol/infra => ../../protocols/infra
	go.putnami.dev/protocol/job => ../../protocols/job
	go.putnami.dev/protocol/oci => ../../protocols/oci
	go.putnami.dev/protocol/registry => ../../protocols/registry
	go.putnami.dev/protocol/runtime => ../../protocols/runtime
	go.putnami.dev/protocol/storage => ../../protocols/storage
	go.putnami.dev/protocol/workspace => ../../protocols/workspace
	go.putnami.dev/sdk/extension => ../extension-sdk
)
