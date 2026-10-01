module go.putnami.dev/cli/model

go 1.25.7

replace (
	go.putnami.dev/protocol/agentcontext => ../../protocols/agentcontext
	go.putnami.dev/protocol/cache => ../../protocols/cache
	go.putnami.dev/protocol/capabilities => ../../protocols/capabilities
	go.putnami.dev/protocol/ci => ../../protocols/ci
	go.putnami.dev/protocol/cli => ../../protocols/cli
	go.putnami.dev/protocol/config => ../../protocols/config
	go.putnami.dev/protocol/contracts => ../../protocols/contracts
	go.putnami.dev/protocol/database => ../../protocols/database
	go.putnami.dev/protocol/diagnostic => ../../protocols/diagnostic
	go.putnami.dev/protocol/doctor => ../../protocols/doctor
	go.putnami.dev/protocol/events => ../../protocols/events
	go.putnami.dev/protocol/extension => ../../protocols/extension
	go.putnami.dev/protocol/features => ../../protocols/features
	go.putnami.dev/protocol/infra => ../../protocols/infra
	go.putnami.dev/protocol/job => ../../protocols/job
	go.putnami.dev/protocol/runtime => ../../protocols/runtime
	go.putnami.dev/protocol/storage => ../../protocols/storage
	go.putnami.dev/protocol/support => ../../protocols/support
	go.putnami.dev/protocol/telemetry => ../../protocols/telemetry
	go.putnami.dev/protocol/template => ../../protocols/template
	go.putnami.dev/protocol/workspace => ../../protocols/workspace
)

require (
	go.putnami.dev/protocol/diagnostic v0.0.0
	go.putnami.dev/protocol/extension v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/protocol/job v0.0.0
	go.putnami.dev/protocol/runtime v0.0.0
	go.putnami.dev/protocol/workspace v0.0.0
)

require go.putnami.dev/protocol/cli v0.0.0

require go.putnami.dev/protocol/capabilities v0.0.0 // indirect
