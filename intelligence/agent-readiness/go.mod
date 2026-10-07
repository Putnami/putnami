module go.putnami.dev/intelligence/agent-readiness

go 1.26.1

require (
	github.com/santhosh-tekuri/jsonschema/v5 v5.3.1
	go.putnami.dev/app v0.0.1
	go.putnami.dev/client v0.0.0
	go.putnami.dev/errors v0.0.1
	go.putnami.dev/inject v0.0.1
	go.putnami.dev/protocol/cli v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/sdk/extension v0.0.0
)

require (
	go.putnami.dev/config v0.0.1 // indirect
	go.putnami.dev/http v0.0.1 // indirect
	go.putnami.dev/logger v0.0.1 // indirect
	go.putnami.dev/migration v0.0.0 // indirect
	go.putnami.dev/protocol/capabilities v0.0.0 // indirect
	go.putnami.dev/protocol/clientcontract v0.0.0 // indirect
	go.putnami.dev/protocol/config v0.0.0 // indirect
	go.putnami.dev/protocol/database v0.0.0 // indirect
	go.putnami.dev/protocol/diagnostic v0.0.0 // indirect
	go.putnami.dev/protocol/events v0.0.0 // indirect
	go.putnami.dev/protocol/http-routes v0.0.0 // indirect
	go.putnami.dev/protocol/identity v0.0.0 // indirect
	go.putnami.dev/protocol/infra v0.0.0 // indirect
	go.putnami.dev/protocol/job v0.0.0 // indirect
	go.putnami.dev/protocol/migration v0.0.0 // indirect
	go.putnami.dev/protocol/runtime v0.0.0 // indirect
	go.putnami.dev/protocol/storage v0.0.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	go.putnami.dev/protocol/architecture => ../../protocols/architecture
	go.putnami.dev/protocol/capabilities => ../../protocols/capabilities
	go.putnami.dev/protocol/cli => ../../protocols/cli
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
	go.putnami.dev/protocol/put => ../../protocols/put
	go.putnami.dev/protocol/registry => ../../protocols/registry
	go.putnami.dev/protocol/runtime => ../../protocols/runtime
	go.putnami.dev/protocol/storage => ../../protocols/storage
)

replace go.putnami.dev/protocol/workspace => ../../protocols/workspace

replace go.putnami.dev/sdk/extension => ../../tooling/extension-sdk

replace go.putnami.dev/client => ../../go/framework/client

replace go.putnami.dev/app => ../../go/framework/app

replace go.putnami.dev/config => ../../go/framework/config

replace go.putnami.dev/inject => ../../go/framework/inject

replace go.putnami.dev/logger => ../../go/framework/logger

replace go.putnami.dev/errors => ../../go/framework/errors

replace go.putnami.dev/http => ../../go/framework/http

replace go.putnami.dev/migration => ../../go/framework/migration

replace go.putnami.dev/schema => ../../go/framework/schema

replace go.putnami.dev/protocol/clientcontract => ../../protocols/clientcontract

replace go.putnami.dev/protocol/config => ../../protocols/config

replace go.putnami.dev/protocol/contracts => ../../protocols/contracts

replace go.putnami.dev/protocol/http-routes => ../../protocols/http-routes

replace go.putnami.dev/protocol/identity => ../../protocols/identity

replace go.putnami.dev/protocol/migration => ../../protocols/migration
