module go.putnami.dev/tooling/sdd/extension

go 1.25.7

require (
	go.putnami.dev/protocol/architecture v0.0.0
	go.putnami.dev/protocol/capabilities v0.0.0
	go.putnami.dev/protocol/cli v0.0.0
	go.putnami.dev/protocol/contracts v0.0.0
	go.putnami.dev/protocol/diagnostic v0.0.0
	go.putnami.dev/protocol/extension v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/protocol/runtime v0.0.0
	go.putnami.dev/protocol/support v0.0.0
	go.putnami.dev/protocol/workspace v0.0.0
	go.putnami.dev/sdk/extension v0.0.0
)

require (
	go.putnami.dev/protocol/config v0.0.0 // indirect
	go.putnami.dev/protocol/job v0.0.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
)

replace (
	go.putnami.dev/protocol/architecture => ../../protocols/architecture
	go.putnami.dev/protocol/capabilities => ../../protocols/capabilities
	go.putnami.dev/protocol/cli => ../../protocols/cli
	go.putnami.dev/protocol/config => ../../protocols/config
	go.putnami.dev/protocol/contracts => ../../protocols/contracts
	go.putnami.dev/protocol/diagnostic => ../../protocols/diagnostic
	go.putnami.dev/protocol/extension => ../../protocols/extension
	go.putnami.dev/protocol/features => ../../protocols/features
	go.putnami.dev/protocol/job => ../../protocols/job
	go.putnami.dev/protocol/runtime => ../../protocols/runtime
	go.putnami.dev/protocol/support => ../../protocols/support
	go.putnami.dev/protocol/workspace => ../../protocols/workspace
	go.putnami.dev/sdk/extension => ../../tooling/extension-sdk
)

replace go.putnami.dev/protocol/database => ../../protocols/database

replace go.putnami.dev/protocol/events => ../../protocols/events

replace go.putnami.dev/protocol/infra => ../../protocols/infra

replace go.putnami.dev/protocol/oci => ../../protocols/oci

replace go.putnami.dev/protocol/registry => ../../protocols/registry

replace go.putnami.dev/protocol/storage => ../../protocols/storage

replace go.putnami.dev/protocol/distribution => ../../protocols/distribution
