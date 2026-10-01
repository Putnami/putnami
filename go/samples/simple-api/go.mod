module go.putnami.dev/examples/simple-api

go 1.25.7

require (
	go.putnami.dev/app v0.0.1
	go.putnami.dev/http v0.0.1
	go.putnami.dev/logger v0.0.1
	go.putnami.dev/platform v0.0.1
	go.putnami.dev/protocol/capabilities v0.0.0
)

require (
	go.putnami.dev/config v0.0.1 // indirect
	go.putnami.dev/errors v0.0.1 // indirect
	go.putnami.dev/inject v0.0.1 // indirect
	go.putnami.dev/migration v0.0.0 // indirect
	go.putnami.dev/protocol/config v0.0.0 // indirect
	go.putnami.dev/protocol/database v0.0.0 // indirect
	go.putnami.dev/protocol/diagnostic v0.0.0 // indirect
	go.putnami.dev/protocol/events v0.0.0 // indirect
	go.putnami.dev/protocol/features v0.0.0 // indirect
	go.putnami.dev/protocol/http-routes v0.0.0 // indirect
	go.putnami.dev/protocol/identity v0.0.0 // indirect
	go.putnami.dev/protocol/infra v0.0.0 // indirect
	go.putnami.dev/protocol/migration v0.0.0 // indirect
	go.putnami.dev/protocol/platform v0.0.0 // indirect
	go.putnami.dev/protocol/runtime v0.0.0 // indirect
	go.putnami.dev/protocol/storage v0.0.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	go.putnami.dev/app => ../../framework/app
	go.putnami.dev/errors => ../../framework/errors
	go.putnami.dev/http => ../../framework/http
	go.putnami.dev/inject => ../../framework/inject
	go.putnami.dev/logger => ../../framework/logger
	go.putnami.dev/platform => ../../framework/platform
	go.putnami.dev/schema => ../../framework/schema
)

replace go.putnami.dev/config => ../../framework/config

replace go.putnami.dev/migration => ../../framework/migration

replace go.putnami.dev/protocol/config => ../../../protocols/config

replace go.putnami.dev/protocol/infra => ../../../protocols/infra

replace go.putnami.dev/protocol/migration => ../../../protocols/migration

replace go.putnami.dev/protocol/database => ../../../protocols/database

replace go.putnami.dev/protocol/diagnostic => ../../../protocols/diagnostic

replace go.putnami.dev/protocol/events => ../../../protocols/events

replace go.putnami.dev/protocol/storage => ../../../protocols/storage

replace go.putnami.dev/protocol/capabilities => ../../../protocols/capabilities

replace go.putnami.dev/protocol/contracts => ../../../protocols/contracts

replace go.putnami.dev/protocol/identity => ../../../protocols/identity

replace go.putnami.dev/protocol/http-routes => ../../../protocols/http-routes

replace go.putnami.dev/protocol/runtime => ../../../protocols/runtime

replace go.putnami.dev/protocol/features => ../../../protocols/features

replace go.putnami.dev/protocol/platform => ../../../protocols/platform

replace go.putnami.dev/protocol/architecture => ../../../protocols/architecture
