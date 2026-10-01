module go.putnami.dev/database

go 1.25.7

require (
	github.com/jackc/pgx/v5 v5.9.1
	go.putnami.dev/app v0.0.1
	go.putnami.dev/config v0.0.1
	go.putnami.dev/errors v0.0.1
	go.putnami.dev/inject v0.0.1
	go.putnami.dev/logger v0.0.1
	go.putnami.dev/migration v0.0.0
	go.putnami.dev/protocol/capabilities v0.0.0
	go.putnami.dev/protocol/database v0.0.0
	go.putnami.dev/protocol/diagnostic v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/protocol/infra v0.0.0
	go.putnami.dev/protocol/migration v0.0.0
	go.putnami.dev/protocol/transaction v0.0.0
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	go.putnami.dev/protocol/config v0.0.0 // indirect
	go.putnami.dev/protocol/events v0.0.0 // indirect
	go.putnami.dev/protocol/storage v0.0.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	go.putnami.dev/app => ../app
	go.putnami.dev/config => ../config
	go.putnami.dev/errors => ../errors
	go.putnami.dev/inject => ../inject
	go.putnami.dev/logger => ../logger
	go.putnami.dev/migration => ../migration
	go.putnami.dev/protocol/diagnostic => ../../../protocols/diagnostic
	go.putnami.dev/protocol/infra => ../../../protocols/infra
	go.putnami.dev/protocol/migration => ../../../protocols/migration
	go.putnami.dev/protocol/transaction => ../../../protocols/transaction
)

replace go.putnami.dev/protocol/database => ../../../protocols/database

replace go.putnami.dev/protocol/config => ../../../protocols/config

replace go.putnami.dev/protocol/events => ../../../protocols/events

replace go.putnami.dev/protocol/storage => ../../../protocols/storage

replace go.putnami.dev/protocol/capabilities => ../../../protocols/capabilities

replace go.putnami.dev/protocol/features => ../../../protocols/features

// The v0.1 series predates the open-source cut: its sources will not exist in
// the public repository, whose history starts at the v0.2.0 initial commit.
// The range starts at the -0 prerelease floor so the v0.1.x-<sha> candidate
// versions are covered too (a prerelease sorts below its release).
retract [v0.1.0-0, v0.1.99999] // Pre-open-source builds; public history starts at v0.2.0.

replace go.putnami.dev/protocol/architecture => ../../../protocols/architecture
