// The document gate of the CLI is a sibling module, not a sibling directory of
// the CLI's own packages, so `go test ./...` inside tooling/cli never compiles
// it. Its module path is a CHILD of the CLI's on purpose: Go scopes `internal`
// visibility by import path, so go.putnami.dev/tooling/cli/documents may import
// go.putnami.dev/tooling/cli/internal/... while go.putnami.dev/tooling/cli-model
// or any other sibling path may not. Nothing publishes this module.
module go.putnami.dev/tooling/cli/documents

go 1.25.7

require (
	go.putnami.dev/protocol/ci v0.0.0
	go.putnami.dev/protocol/cli v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/protocol/workspace v0.0.0
	go.putnami.dev/tooling/cli v0.0.0
)

require (
	go.putnami.dev/cli/model v0.0.0 // indirect
	go.putnami.dev/protocol/agentcontext v0.0.0 // indirect
	go.putnami.dev/protocol/cache v0.0.0 // indirect
	go.putnami.dev/protocol/capabilities v0.0.0 // indirect
	go.putnami.dev/protocol/clientcontract v0.0.0 // indirect
	go.putnami.dev/protocol/collaboration v0.0.0 // indirect
	go.putnami.dev/protocol/config v0.0.0 // indirect
	go.putnami.dev/protocol/contracts v0.0.0 // indirect
	go.putnami.dev/protocol/database v0.0.0 // indirect
	go.putnami.dev/protocol/diagnostic v0.0.0 // indirect
	go.putnami.dev/protocol/distribution v0.0.0 // indirect
	go.putnami.dev/protocol/doctor v0.0.0 // indirect
	go.putnami.dev/protocol/events v0.0.0 // indirect
	go.putnami.dev/protocol/extension v0.0.0 // indirect
	go.putnami.dev/protocol/http-routes v0.0.0-00010101000000-000000000000 // indirect
	go.putnami.dev/protocol/infra v0.0.0 // indirect
	go.putnami.dev/protocol/job v0.0.0 // indirect
	go.putnami.dev/protocol/platform v0.0.0-00010101000000-000000000000 // indirect
	go.putnami.dev/protocol/qualify v0.0.0-00010101000000-000000000000 // indirect
	go.putnami.dev/protocol/registry v0.0.0 // indirect
	go.putnami.dev/protocol/runner v0.0.0 // indirect
	go.putnami.dev/protocol/runtime v0.0.0 // indirect
	go.putnami.dev/protocol/storage v0.0.0 // indirect
	go.putnami.dev/protocol/telemetry v0.0.0 // indirect
	go.putnami.dev/protocol/template v0.0.0 // indirect
	go.putnami.dev/sdk/extension v0.0.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
)

replace (
	go.putnami.dev/cli/model => ../cli-model
	go.putnami.dev/protocol/agentcontext => ../../protocols/agentcontext
	go.putnami.dev/protocol/architecture => ../../protocols/architecture
	go.putnami.dev/protocol/cache => ../../protocols/cache
	go.putnami.dev/protocol/capabilities => ../../protocols/capabilities
	go.putnami.dev/protocol/ci => ../../protocols/ci
	go.putnami.dev/protocol/cli => ../../protocols/cli
	go.putnami.dev/protocol/clientcontract => ../../protocols/clientcontract
	go.putnami.dev/protocol/collaboration => ../../protocols/collaboration
	go.putnami.dev/protocol/config => ../../protocols/config
	go.putnami.dev/protocol/contracts => ../../protocols/contracts
	go.putnami.dev/protocol/database => ../../protocols/database
	go.putnami.dev/protocol/diagnostic => ../../protocols/diagnostic
	go.putnami.dev/protocol/distribution => ../../protocols/distribution
	go.putnami.dev/protocol/doctor => ../../protocols/doctor
	go.putnami.dev/protocol/events => ../../protocols/events
	go.putnami.dev/protocol/extension => ../../protocols/extension
	go.putnami.dev/protocol/features => ../../protocols/features
	go.putnami.dev/protocol/http-routes => ../../protocols/http-routes
	go.putnami.dev/protocol/infra => ../../protocols/infra
	go.putnami.dev/protocol/job => ../../protocols/job
	go.putnami.dev/protocol/oci => ../../protocols/oci
	go.putnami.dev/protocol/platform => ../../protocols/platform
	go.putnami.dev/protocol/qualify => ../../protocols/qualify
	go.putnami.dev/protocol/registry => ../../protocols/registry
	go.putnami.dev/protocol/runtime => ../../protocols/runtime
	go.putnami.dev/protocol/storage => ../../protocols/storage
	go.putnami.dev/protocol/support => ../../protocols/support
	go.putnami.dev/protocol/telemetry => ../../protocols/telemetry
	go.putnami.dev/protocol/template => ../../protocols/template
	go.putnami.dev/protocol/workspace => ../../protocols/workspace
	go.putnami.dev/sdk/extension => ../extension-sdk
	go.putnami.dev/tooling/cli => ../cli
)

replace go.putnami.dev/protocol/runner => ../../protocols/runner
