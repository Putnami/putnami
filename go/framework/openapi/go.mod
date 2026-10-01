module go.putnami.dev/openapi

go 1.25.7

require (
	go.putnami.dev/api v0.0.0-00010101000000-000000000000
	go.putnami.dev/app v0.0.1
	go.putnami.dev/client v0.0.0-00010101000000-000000000000
	go.putnami.dev/errors v0.0.1
	go.putnami.dev/http v0.0.1
	go.putnami.dev/protocol/clientcontract v0.0.0
	go.putnami.dev/protocol/contracts v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/schema v0.0.1
	go.putnami.dev/security v0.0.1
)

require (
	go.putnami.dev/cache v0.0.1 // indirect
	go.putnami.dev/config v0.0.1 // indirect
	go.putnami.dev/ctxutil v0.0.0 // indirect
	go.putnami.dev/inject v0.0.1 // indirect
	go.putnami.dev/logger v0.0.1 // indirect
	go.putnami.dev/migration v0.0.0 // indirect
	go.putnami.dev/protocol/capabilities v0.0.0 // indirect
	go.putnami.dev/protocol/config v0.0.0 // indirect
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
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	go.putnami.dev/api => ../api
	go.putnami.dev/app => ../app
	go.putnami.dev/client => ../client
	go.putnami.dev/errors => ../errors
	go.putnami.dev/http => ../http
	go.putnami.dev/inject => ../inject
	go.putnami.dev/logger => ../logger
	go.putnami.dev/schema => ../schema
	go.putnami.dev/security => ../security
)

replace go.putnami.dev/config => ../config

replace go.putnami.dev/migration => ../migration

replace go.putnami.dev/protocol/config => ../../../protocols/config

replace go.putnami.dev/protocol/clientcontract => ../../../protocols/clientcontract

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

// The v0.1 series predates the open-source cut: its sources will not exist in
// the public repository, whose history starts at the v0.2.0 initial commit.
// The range starts at the -0 prerelease floor so the v0.1.x-<sha> candidate
// versions are covered too (a prerelease sorts below its release).
retract [v0.1.0-0, v0.1.99999] // Pre-open-source builds; public history starts at v0.2.0.

replace go.putnami.dev/protocol/architecture => ../../../protocols/architecture

replace go.putnami.dev/cache => ../cache

replace go.putnami.dev/ctxutil => ../ctxutil

replace go.putnami.dev/protocol/keyring => ../../../protocols/keyring
