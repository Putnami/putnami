module telemetry.putnami.dev

go 1.26.1

require (
	go.putnami.dev/app v0.0.1
	go.putnami.dev/config v0.1.0-405dcd542
	go.putnami.dev/database v0.0.0
	go.putnami.dev/http v0.0.1
	go.putnami.dev/inject v0.1.0-405dcd542
	go.putnami.dev/logger v0.0.1
	go.putnami.dev/migration v0.0.0
	go.putnami.dev/protocol/architecture v0.0.0
	go.putnami.dev/protocol/capabilities v0.0.0
	go.putnami.dev/protocol/diagnostic v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/protocol/http-routes v0.0.0
	go.putnami.dev/protocol/migration v0.0.0
	go.putnami.dev/protocol/telemetry v0.0.0
	go.putnami.dev/security v0.0.1
	go.putnami.dev/telemetry v0.0.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.9.1 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.43.0 // indirect
	go.opentelemetry.io/otel/metric v1.43.0 // indirect
	go.opentelemetry.io/otel/sdk v1.43.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.43.0 // indirect
	go.opentelemetry.io/otel/trace v1.43.0 // indirect
	go.putnami.dev/cache v0.0.1 // indirect
	go.putnami.dev/client v0.0.1 // indirect
	go.putnami.dev/ctxutil v0.0.0 // indirect
	go.putnami.dev/errors v0.1.0-405dcd542 // indirect
	go.putnami.dev/protocol/clientcontract v0.0.0 // indirect
	go.putnami.dev/protocol/config v0.0.0 // indirect
	go.putnami.dev/protocol/database v0.0.0 // indirect
	go.putnami.dev/protocol/events v0.0.0 // indirect
	go.putnami.dev/protocol/identity v0.0.0 // indirect
	go.putnami.dev/protocol/infra v0.0.0 // indirect
	go.putnami.dev/protocol/keyring v0.0.0 // indirect
	go.putnami.dev/protocol/runtime v0.0.0 // indirect
	go.putnami.dev/protocol/storage v0.0.0 // indirect
	go.putnami.dev/protocol/transaction v0.0.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	go.putnami.dev/app => ../../go/framework/app
	go.putnami.dev/cache => ../../go/framework/cache
	go.putnami.dev/ctxutil => ../../go/framework/ctxutil
	go.putnami.dev/database => ../../go/framework/database
	go.putnami.dev/errors => ../../go/framework/errors
	go.putnami.dev/http => ../../go/framework/http
	go.putnami.dev/inject => ../../go/framework/inject
	go.putnami.dev/logger => ../../go/framework/logger
	go.putnami.dev/schema => ../../go/framework/schema
	go.putnami.dev/security => ../../go/framework/security
	go.putnami.dev/telemetry => ../../go/framework/telemetry
)

replace go.putnami.dev/config => ../../go/framework/config

replace go.putnami.dev/migration => ../../go/framework/migration

replace go.putnami.dev/protocol/transaction => ../../protocols/transaction

replace go.putnami.dev/protocol/config => ../../protocols/config

replace go.putnami.dev/protocol/infra => ../../protocols/infra

replace go.putnami.dev/protocol/migration => ../../protocols/migration

replace go.putnami.dev/protocol/database => ../../protocols/database

replace go.putnami.dev/protocol/diagnostic => ../../protocols/diagnostic

replace go.putnami.dev/protocol/events => ../../protocols/events

replace go.putnami.dev/protocol/storage => ../../protocols/storage

replace go.putnami.dev/protocol/capabilities => ../../protocols/capabilities

replace go.putnami.dev/protocol/contracts => ../../protocols/contracts

replace go.putnami.dev/protocol/identity => ../../protocols/identity

replace go.putnami.dev/protocol/http-routes => ../../protocols/http-routes

replace go.putnami.dev/protocol/telemetry => ../../protocols/telemetry

replace go.putnami.dev/protocol/keyring => ../../protocols/keyring

replace go.putnami.dev/protocol/runtime => ../../protocols/runtime

replace go.putnami.dev/protocol/features => ../../protocols/features

replace go.putnami.dev/protocol/architecture => ../../protocols/architecture

replace go.putnami.dev/client => ../../go/framework/client

replace go.putnami.dev/protocol/clientcontract => ../../protocols/clientcontract
