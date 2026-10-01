module go.putnami.dev/keyringstore

go 1.25.7

require (
	github.com/jackc/pgx/v5 v5.9.1
	go.putnami.dev/database v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/protocol/keyring v0.0.0
	go.putnami.dev/protocol/transaction v0.0.0
	go.putnami.dev/security v0.0.1
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	go.putnami.dev/app v0.0.1 // indirect
	go.putnami.dev/cache v0.0.1 // indirect
	go.putnami.dev/config v0.0.1 // indirect
	go.putnami.dev/ctxutil v0.0.0 // indirect
	go.putnami.dev/errors v0.0.1 // indirect
	go.putnami.dev/http v0.0.1 // indirect
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
	go.putnami.dev/protocol/migration v0.0.0 // indirect
	go.putnami.dev/protocol/runtime v0.0.0 // indirect
	go.putnami.dev/protocol/storage v0.0.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	go.putnami.dev/app => ../app
	go.putnami.dev/cache => ../cache
	go.putnami.dev/config => ../config
	go.putnami.dev/ctxutil => ../ctxutil
	go.putnami.dev/database => ../database
	go.putnami.dev/errors => ../errors
	go.putnami.dev/http => ../http
	go.putnami.dev/inject => ../inject
	go.putnami.dev/logger => ../logger
	go.putnami.dev/migration => ../migration
	go.putnami.dev/schema => ../schema
	go.putnami.dev/security => ../security
)

replace (
	go.putnami.dev/protocol/capabilities => ../../../protocols/capabilities
	go.putnami.dev/protocol/config => ../../../protocols/config
	go.putnami.dev/protocol/contracts => ../../../protocols/contracts
	go.putnami.dev/protocol/database => ../../../protocols/database
	go.putnami.dev/protocol/diagnostic => ../../../protocols/diagnostic
	go.putnami.dev/protocol/events => ../../../protocols/events
	go.putnami.dev/protocol/identity => ../../../protocols/identity
	go.putnami.dev/protocol/infra => ../../../protocols/infra
	go.putnami.dev/protocol/keyring => ../../../protocols/keyring
	go.putnami.dev/protocol/migration => ../../../protocols/migration
	go.putnami.dev/protocol/storage => ../../../protocols/storage
	go.putnami.dev/protocol/transaction => ../../../protocols/transaction
)

replace go.putnami.dev/protocol/http-routes => ../../../protocols/http-routes

replace go.putnami.dev/protocol/runtime => ../../../protocols/runtime

replace go.putnami.dev/protocol/features => ../../../protocols/features

// The v0.1 series predates the open-source cut: its sources will not exist in
// the public repository, whose history starts at the v0.2.0 initial commit.
// The range starts at the -0 prerelease floor so the v0.1.x-<sha> candidate
// versions are covered too (a prerelease sorts below its release).
retract [v0.1.0-0, v0.1.99999] // Pre-open-source builds; public history starts at v0.2.0.

replace go.putnami.dev/protocol/architecture => ../../../protocols/architecture
