module go.putnami.dev/cloud/clients/runtime-api

go 1.26.1

replace go.putnami.dev/app => ../../../go/framework/app

replace go.putnami.dev/client => ../../../go/framework/client

replace go.putnami.dev/config => ../../../go/framework/config

replace go.putnami.dev/errors => ../../../go/framework/errors

replace go.putnami.dev/http => ../../../go/framework/http

replace go.putnami.dev/inject => ../../../go/framework/inject

replace go.putnami.dev/logger => ../../../go/framework/logger

replace go.putnami.dev/migration => ../../../go/framework/migration

replace go.putnami.dev/protocol/architecture => ../../../protocols/architecture

replace go.putnami.dev/protocol/capabilities => ../../../protocols/capabilities

replace go.putnami.dev/protocol/clientcontract => ../../../protocols/clientcontract

replace go.putnami.dev/protocol/config => ../../../protocols/config

replace go.putnami.dev/protocol/contracts => ../../../protocols/contracts

replace go.putnami.dev/protocol/database => ../../../protocols/database

replace go.putnami.dev/protocol/diagnostic => ../../../protocols/diagnostic

replace go.putnami.dev/protocol/events => ../../../protocols/events

replace go.putnami.dev/protocol/features => ../../../protocols/features

replace go.putnami.dev/protocol/http-routes => ../../../protocols/http-routes

replace go.putnami.dev/protocol/identity => ../../../protocols/identity

replace go.putnami.dev/protocol/infra => ../../../protocols/infra

replace go.putnami.dev/protocol/migration => ../../../protocols/migration

replace go.putnami.dev/protocol/runtime => ../../../protocols/runtime

replace go.putnami.dev/protocol/storage => ../../../protocols/storage

require (
	go.putnami.dev/app v0.0.1
	go.putnami.dev/client v0.0.0-00010101000000-000000000000
	go.putnami.dev/inject v0.0.1
)

require (
	go.putnami.dev/config v0.0.1 // indirect
	go.putnami.dev/errors v0.0.1 // indirect
	go.putnami.dev/http v0.0.1 // indirect
	go.putnami.dev/logger v0.0.1 // indirect
	go.putnami.dev/migration v0.0.0 // indirect
	go.putnami.dev/protocol/capabilities v0.0.0 // indirect
	go.putnami.dev/protocol/clientcontract v0.0.0 // indirect
	go.putnami.dev/protocol/config v0.0.0 // indirect
	go.putnami.dev/protocol/database v0.0.0 // indirect
	go.putnami.dev/protocol/diagnostic v0.0.0 // indirect
	go.putnami.dev/protocol/events v0.0.0 // indirect
	go.putnami.dev/protocol/features v0.0.0 // indirect
	go.putnami.dev/protocol/http-routes v0.0.0 // indirect
	go.putnami.dev/protocol/identity v0.0.0 // indirect
	go.putnami.dev/protocol/infra v0.0.0 // indirect
	go.putnami.dev/protocol/migration v0.0.0 // indirect
	go.putnami.dev/protocol/runtime v0.0.0 // indirect
	go.putnami.dev/protocol/storage v0.0.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
