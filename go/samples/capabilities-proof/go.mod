module go.putnami.dev/examples/capabilities-proof

go 1.25.7

require (
	go.putnami.dev/app v0.0.1
	go.putnami.dev/config v0.0.1
	go.putnami.dev/migration v0.0.0
	go.putnami.dev/protocol/capabilities v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/protocol/infra v0.0.0
)

require (
	go.putnami.dev/errors v0.0.1 // indirect
	go.putnami.dev/inject v0.0.1 // indirect
	go.putnami.dev/logger v0.0.1 // indirect
	go.putnami.dev/protocol/config v0.0.0 // indirect
	go.putnami.dev/protocol/database v0.0.0 // indirect
	go.putnami.dev/protocol/diagnostic v0.0.0 // indirect
	go.putnami.dev/protocol/events v0.0.0 // indirect
	go.putnami.dev/protocol/migration v0.0.0 // indirect
	go.putnami.dev/protocol/runtime v0.0.0 // indirect
	go.putnami.dev/protocol/storage v0.0.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	go.putnami.dev/app => ../../framework/app
	go.putnami.dev/config => ../../framework/config
	go.putnami.dev/errors => ../../framework/errors
	go.putnami.dev/inject => ../../framework/inject
	go.putnami.dev/logger => ../../framework/logger
	go.putnami.dev/migration => ../../framework/migration
	go.putnami.dev/protocol/capabilities => ../../../protocols/capabilities
	go.putnami.dev/protocol/config => ../../../protocols/config
	go.putnami.dev/protocol/database => ../../../protocols/database
	go.putnami.dev/protocol/diagnostic => ../../../protocols/diagnostic
	go.putnami.dev/protocol/events => ../../../protocols/events
	go.putnami.dev/protocol/features => ../../../protocols/features
	go.putnami.dev/protocol/infra => ../../../protocols/infra
	go.putnami.dev/protocol/migration => ../../../protocols/migration
	go.putnami.dev/protocol/storage => ../../../protocols/storage
)

replace go.putnami.dev/protocol/architecture => ../../../protocols/architecture

replace go.putnami.dev/protocol/runtime => ../../../protocols/runtime
