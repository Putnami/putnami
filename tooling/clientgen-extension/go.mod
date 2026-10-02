module go.putnami.dev/tooling/clientgen/extension

go 1.25.7

require (
	go.putnami.dev/api v0.0.0
	go.putnami.dev/protocol/clientcontract v0.0.0
	go.putnami.dev/protocol/diagnostic v0.0.0
	go.putnami.dev/protocol/extension v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/protocol/registry v0.0.0
	go.putnami.dev/sdk/extension v0.0.0
)

require (
	go.putnami.dev/app v0.0.1 // indirect
	go.putnami.dev/config v0.0.1 // indirect
	go.putnami.dev/errors v0.0.1 // indirect
	go.putnami.dev/http v0.0.1 // indirect
	go.putnami.dev/inject v0.0.1 // indirect
	go.putnami.dev/logger v0.0.1 // indirect
	go.putnami.dev/migration v0.0.0 // indirect
	go.putnami.dev/protocol/capabilities v0.0.0 // indirect
	go.putnami.dev/protocol/cli v0.0.0 // indirect
	go.putnami.dev/protocol/config v0.0.0 // indirect
	go.putnami.dev/protocol/database v0.0.0 // indirect
	go.putnami.dev/protocol/distribution v0.0.0 // indirect
	go.putnami.dev/protocol/events v0.0.0 // indirect
	go.putnami.dev/protocol/http-routes v0.0.0 // indirect
	go.putnami.dev/protocol/identity v0.0.0 // indirect
	go.putnami.dev/protocol/infra v0.0.0 // indirect
	go.putnami.dev/protocol/job v0.0.0 // indirect
	go.putnami.dev/protocol/migration v0.0.0 // indirect
	go.putnami.dev/protocol/platform v0.0.0 // indirect
	go.putnami.dev/protocol/runtime v0.0.0 // indirect
	go.putnami.dev/protocol/storage v0.0.0 // indirect
	go.putnami.dev/schema v0.0.1 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	go.putnami.dev/api => ../../go/framework/api
	go.putnami.dev/app => ../../go/framework/app
	go.putnami.dev/config => ../../go/framework/config
	go.putnami.dev/errors => ../../go/framework/errors
	go.putnami.dev/http => ../../go/framework/http
	go.putnami.dev/inject => ../../go/framework/inject
	go.putnami.dev/logger => ../../go/framework/logger
	go.putnami.dev/migration => ../../go/framework/migration
	go.putnami.dev/protocol/cli => ../../protocols/cli
	go.putnami.dev/protocol/clientcontract => ../../protocols/clientcontract
	go.putnami.dev/protocol/config => ../../protocols/config
	go.putnami.dev/protocol/contracts => ../../protocols/contracts
	go.putnami.dev/protocol/database => ../../protocols/database
	go.putnami.dev/protocol/diagnostic => ../../protocols/diagnostic
	go.putnami.dev/protocol/distribution => ../../protocols/distribution
	go.putnami.dev/protocol/events => ../../protocols/events
	go.putnami.dev/protocol/extension => ../../protocols/extension
	go.putnami.dev/protocol/features => ../../protocols/features
	go.putnami.dev/protocol/gomod => ../../protocols/gomod
	go.putnami.dev/protocol/http-routes => ../../protocols/http-routes
	go.putnami.dev/protocol/identity => ../../protocols/identity
	go.putnami.dev/protocol/infra => ../../protocols/infra
	go.putnami.dev/protocol/job => ../../protocols/job
	go.putnami.dev/protocol/migration => ../../protocols/migration
	go.putnami.dev/protocol/oci => ../../protocols/oci
	go.putnami.dev/protocol/platform => ../../protocols/platform
	go.putnami.dev/protocol/put => ../../protocols/put
	go.putnami.dev/protocol/registry => ../../protocols/registry
	go.putnami.dev/protocol/runtime => ../../protocols/runtime
	go.putnami.dev/protocol/storage => ../../protocols/storage
	go.putnami.dev/schema => ../../go/framework/schema
	go.putnami.dev/sdk/extension => ../../tooling/extension-sdk
)

replace go.putnami.dev/protocol/capabilities => ../../protocols/capabilities

replace go.putnami.dev/protocol/architecture => ../../protocols/architecture

replace go.putnami.dev/protocol/workspace => ../../protocols/workspace
