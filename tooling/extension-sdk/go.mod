module go.putnami.dev/sdk/extension

go 1.25.7

require (
	github.com/google/go-containerregistry v0.21.6
	go.putnami.dev/protocol/architecture v0.0.0
	go.putnami.dev/protocol/capabilities v0.0.0
	go.putnami.dev/protocol/cli v0.0.0
	go.putnami.dev/protocol/database v0.0.0
	go.putnami.dev/protocol/diagnostic v0.0.0
	go.putnami.dev/protocol/distribution v0.0.0
	go.putnami.dev/protocol/extension v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/protocol/gomod v0.0.0
	go.putnami.dev/protocol/infra v0.0.0
	go.putnami.dev/protocol/job v0.0.0
	go.putnami.dev/protocol/oci v0.0.0
	go.putnami.dev/protocol/registry v0.0.0
	go.putnami.dev/protocol/runtime v0.0.0
	go.putnami.dev/protocol/workspace v0.0.0
	golang.org/x/mod v0.36.0
	golang.org/x/sync v0.20.0
	golang.org/x/sys v0.44.0
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/docker/cli v29.4.3+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.3 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	go.putnami.dev/protocol/events v0.0.0 // indirect
	go.putnami.dev/protocol/storage v0.0.0 // indirect
	gotest.tools/v3 v3.5.2 // indirect
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
	go.putnami.dev/protocol/registry => ../../protocols/registry
	go.putnami.dev/protocol/runtime => ../../protocols/runtime
	go.putnami.dev/protocol/storage => ../../protocols/storage
)

replace go.putnami.dev/protocol/workspace => ../../protocols/workspace
