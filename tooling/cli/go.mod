module go.putnami.dev/tooling/cli

go 1.25.7

require (
	github.com/google/go-containerregistry v0.21.6
	go.putnami.dev/cli/model v0.0.0
	go.putnami.dev/protocol/agentcontext v0.0.0
	go.putnami.dev/protocol/architecture v0.0.0
	go.putnami.dev/protocol/cache v0.0.0
	go.putnami.dev/protocol/capabilities v0.0.0
	go.putnami.dev/protocol/ci v0.0.0
	go.putnami.dev/protocol/cli v0.0.0
	go.putnami.dev/protocol/clientcontract v0.0.0
	go.putnami.dev/protocol/collaboration v0.0.0
	go.putnami.dev/protocol/config v0.0.0
	go.putnami.dev/protocol/contracts v0.0.0
	go.putnami.dev/protocol/database v0.0.0
	go.putnami.dev/protocol/diagnostic v0.0.0
	go.putnami.dev/protocol/distribution v0.0.0
	go.putnami.dev/protocol/doctor v0.0.0
	go.putnami.dev/protocol/extension v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/protocol/http-routes v0.0.0-00010101000000-000000000000
	go.putnami.dev/protocol/infra v0.0.0
	go.putnami.dev/protocol/job v0.0.0
	go.putnami.dev/protocol/platform v0.0.0-00010101000000-000000000000
	go.putnami.dev/protocol/qualify v0.0.0-00010101000000-000000000000
	go.putnami.dev/protocol/registry v0.0.0
	go.putnami.dev/protocol/runner v0.0.0
	go.putnami.dev/protocol/runtime v0.0.0
	go.putnami.dev/protocol/support v0.0.0
	go.putnami.dev/protocol/telemetry v0.0.0
	go.putnami.dev/protocol/template v0.0.0
	go.putnami.dev/protocol/workspace v0.0.0
	go.putnami.dev/sdk/extension v0.0.0
	golang.org/x/sys v0.44.0
)

require (
	github.com/docker/cli v29.4.3+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.3 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	go.putnami.dev/protocol/events v0.0.0 // indirect
	go.putnami.dev/protocol/gomod v0.0.0 // indirect
	go.putnami.dev/protocol/oci v0.0.0 // indirect
	go.putnami.dev/protocol/storage v0.0.0 // indirect
	golang.org/x/mod v0.36.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
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
	go.putnami.dev/protocol/diagnostic => ../../protocols/diagnostic
	go.putnami.dev/protocol/distribution => ../../protocols/distribution
	go.putnami.dev/protocol/doctor => ../../protocols/doctor
	go.putnami.dev/protocol/extension => ../../protocols/extension
	go.putnami.dev/protocol/features => ../../protocols/features
	go.putnami.dev/protocol/gomod => ../../protocols/gomod
	go.putnami.dev/protocol/http-routes => ../../protocols/http-routes
	go.putnami.dev/protocol/infra => ../../protocols/infra
	go.putnami.dev/protocol/job => ../../protocols/job
	go.putnami.dev/protocol/platform => ../../protocols/platform
	go.putnami.dev/protocol/qualify => ../../protocols/qualify
	go.putnami.dev/protocol/runner => ../../protocols/runner
	go.putnami.dev/protocol/runtime => ../../protocols/runtime
	go.putnami.dev/protocol/support => ../../protocols/support
	go.putnami.dev/protocol/telemetry => ../../protocols/telemetry
	go.putnami.dev/protocol/template => ../../protocols/template
	go.putnami.dev/protocol/workspace => ../../protocols/workspace
	go.putnami.dev/sdk/extension => ../extension-sdk
)

replace go.putnami.dev/protocol/database => ../../protocols/database

replace go.putnami.dev/protocol/events => ../../protocols/events

replace go.putnami.dev/protocol/storage => ../../protocols/storage

replace go.putnami.dev/protocol/oci => ../../protocols/oci

replace go.putnami.dev/protocol/registry => ../../protocols/registry
