module go.putnami.dev/migration

go 1.25.7

require (
	go.putnami.dev/errors v0.0.1
	go.putnami.dev/logger v0.0.1
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/protocol/infra v0.0.0
	go.putnami.dev/protocol/migration v0.0.0
)

require (
	go.putnami.dev/protocol/database v0.0.0 // indirect
	go.putnami.dev/protocol/diagnostic v0.0.0 // indirect
	go.putnami.dev/protocol/events v0.0.0 // indirect
	go.putnami.dev/protocol/storage v0.0.0 // indirect
)

replace (
	go.putnami.dev/errors => ../errors
	go.putnami.dev/logger => ../logger
	go.putnami.dev/protocol/diagnostic => ../../../protocols/diagnostic
	go.putnami.dev/protocol/infra => ../../../protocols/infra
	go.putnami.dev/protocol/migration => ../../../protocols/migration
)

replace go.putnami.dev/protocol/database => ../../../protocols/database

replace go.putnami.dev/protocol/events => ../../../protocols/events

replace go.putnami.dev/protocol/storage => ../../../protocols/storage

replace go.putnami.dev/protocol/capabilities => ../../../protocols/capabilities

replace go.putnami.dev/protocol/features => ../../../protocols/features

// The v0.1 series predates the open-source cut: its sources will not exist in
// the public repository, whose history starts at the v0.2.0 initial commit.
// The range starts at the -0 prerelease floor so the v0.1.x-<sha> candidate
// versions are covered too (a prerelease sorts below its release).
retract [v0.1.0-0, v0.1.99999] // Pre-open-source builds; public history starts at v0.2.0.
