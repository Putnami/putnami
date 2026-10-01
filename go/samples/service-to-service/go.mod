module go.putnami.dev/examples/service-to-service

go 1.25.7

require (
	go.opentelemetry.io/otel v1.43.0
	go.opentelemetry.io/otel/sdk v1.43.0
	go.opentelemetry.io/otel/sdk/metric v1.43.0
	go.opentelemetry.io/otel/trace v1.43.0
	go.putnami.dev/api v0.0.1
	go.putnami.dev/app v0.0.1
	go.putnami.dev/client v0.0.1
	go.putnami.dev/errors v0.0.1
	go.putnami.dev/grpc v0.0.0
	go.putnami.dev/http v0.0.1
	go.putnami.dev/inject v0.0.1
	go.putnami.dev/openapi v0.0.1
	go.putnami.dev/platform v0.0.1
	go.putnami.dev/proto v0.0.0
	go.putnami.dev/protocol/capabilities v0.0.0
	go.putnami.dev/protocol/clientcontract v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/security v0.0.1
	go.putnami.dev/telemetry v0.0.1
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/metric v1.43.0 // indirect
	go.putnami.dev/cache v0.0.1 // indirect
	go.putnami.dev/config v0.0.1 // indirect
	go.putnami.dev/ctxutil v0.0.0 // indirect
	go.putnami.dev/logger v0.0.1 // indirect
	go.putnami.dev/migration v0.0.0 // indirect
	go.putnami.dev/protocol/config v0.0.0 // indirect
	go.putnami.dev/protocol/contracts v0.0.0 // indirect
	go.putnami.dev/protocol/database v0.0.0 // indirect
	go.putnami.dev/protocol/diagnostic v0.0.0 // indirect
	go.putnami.dev/protocol/events v0.0.0 // indirect
	go.putnami.dev/protocol/http-routes v0.0.0 // indirect
	go.putnami.dev/protocol/identity v0.0.0 // indirect
	go.putnami.dev/protocol/infra v0.0.0 // indirect
	go.putnami.dev/protocol/keyring v0.0.0 // indirect
	go.putnami.dev/protocol/migration v0.0.0 // indirect
	go.putnami.dev/protocol/platform v0.0.0 // indirect
	go.putnami.dev/protocol/runtime v0.0.0 // indirect
	go.putnami.dev/protocol/storage v0.0.0 // indirect
	go.putnami.dev/protocol/telemetry v0.0.0 // indirect
	go.putnami.dev/schema v0.0.1 // indirect
	golang.org/x/net v0.54.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
	google.golang.org/grpc v1.80.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	go.putnami.dev/api => ../../framework/api
	go.putnami.dev/app => ../../framework/app
	go.putnami.dev/client => ../../framework/client
	go.putnami.dev/config => ../../framework/config
	go.putnami.dev/errors => ../../framework/errors
	go.putnami.dev/events => ../../framework/events
	go.putnami.dev/grpc => ../../framework/grpc
	go.putnami.dev/http => ../../framework/http
	go.putnami.dev/inject => ../../framework/inject
	go.putnami.dev/logger => ../../framework/logger
	go.putnami.dev/migration => ../../framework/migration
	go.putnami.dev/openapi => ../../framework/openapi
	go.putnami.dev/parallel => ../../framework/parallel
	go.putnami.dev/platform => ../../framework/platform
	go.putnami.dev/proto => ../../framework/proto
	go.putnami.dev/protocol/diagnostic => ../../../protocols/diagnostic
	go.putnami.dev/protocol/migration => ../../../protocols/migration
	go.putnami.dev/protocol/platform => ../../../protocols/platform
	go.putnami.dev/protocol/telemetry => ../../../protocols/telemetry
	go.putnami.dev/schema => ../../framework/schema
	go.putnami.dev/security => ../../framework/security
	go.putnami.dev/telemetry => ../../framework/telemetry
)

replace go.putnami.dev/protocol/config => ../../../protocols/config

replace go.putnami.dev/protocol/infra => ../../../protocols/infra

replace go.putnami.dev/protocol/database => ../../../protocols/database

replace go.putnami.dev/protocol/events => ../../../protocols/events

replace go.putnami.dev/protocol/storage => ../../../protocols/storage

replace go.putnami.dev/protocol/capabilities => ../../../protocols/capabilities

replace go.putnami.dev/protocol/contracts => ../../../protocols/contracts

replace go.putnami.dev/protocol/identity => ../../../protocols/identity

replace go.putnami.dev/protocol/http-routes => ../../../protocols/http-routes

replace go.putnami.dev/protocol/runtime => ../../../protocols/runtime

replace go.putnami.dev/protocol/features => ../../../protocols/features

replace go.putnami.dev/protocol/architecture => ../../../protocols/architecture

replace go.putnami.dev/protocol/clientcontract => ../../../protocols/clientcontract

replace go.putnami.dev/cache => ../../framework/cache

replace go.putnami.dev/ctxutil => ../../framework/ctxutil

replace go.putnami.dev/protocol/keyring => ../../../protocols/keyring
