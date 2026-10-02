module go.putnami.dev/go/extension

go 1.25.7

require (
	go.putnami.dev/protocol/architecture v0.0.0
	go.putnami.dev/protocol/config v0.0.0
	go.putnami.dev/protocol/distribution v0.0.0
	go.putnami.dev/protocol/gomod v0.0.0
	go.putnami.dev/protocol/infra v0.0.0
	go.putnami.dev/sdk/extension v0.0.0
)

require (
	github.com/rogpeppe/go-internal v1.14.1
	go.putnami.dev/protocol/cache v0.0.0-00010101000000-000000000000
	go.putnami.dev/protocol/cli v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/protocol/job v0.0.0
	go.putnami.dev/protocol/registry v0.0.0
	go.putnami.dev/protocol/runtime v0.0.0
	go.putnami.dev/protocol/support v0.0.0
	go.putnami.dev/protocol/workspace v0.0.0
	golang.org/x/mod v0.36.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/docker/cli v29.4.3+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.3 // indirect
	github.com/google/go-containerregistry v0.21.6 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	go.putnami.dev/protocol/capabilities v0.0.0 // indirect
	go.putnami.dev/protocol/events v0.0.0 // indirect
	go.putnami.dev/protocol/oci v0.0.0 // indirect
	go.putnami.dev/protocol/storage v0.0.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
)

require (
	github.com/kr/pretty v0.3.1 // indirect
	go.putnami.dev/protocol/database v0.0.0
	go.putnami.dev/protocol/diagnostic v0.0.0
	go.putnami.dev/protocol/extension v0.0.0
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)

replace (
	go.putnami.dev/protocol/cache => ../../protocols/cache
	go.putnami.dev/protocol/cli => ../../protocols/cli
	go.putnami.dev/protocol/config => ../../protocols/config
	go.putnami.dev/protocol/diagnostic => ../../protocols/diagnostic
	go.putnami.dev/protocol/distribution => ../../protocols/distribution
	go.putnami.dev/protocol/gomod => ../../protocols/gomod
	go.putnami.dev/protocol/infra => ../../protocols/infra
	go.putnami.dev/sdk/extension => ../../tooling/extension-sdk
)

replace go.putnami.dev/protocol/database => ../../protocols/database

replace go.putnami.dev/protocol/features => ../../protocols/features

replace go.putnami.dev/protocol/capabilities => ../../protocols/capabilities

replace go.putnami.dev/protocol/events => ../../protocols/events

replace go.putnami.dev/protocol/job => ../../protocols/job

replace go.putnami.dev/protocol/oci => ../../protocols/oci

replace go.putnami.dev/protocol/registry => ../../protocols/registry

replace go.putnami.dev/protocol/runtime => ../../protocols/runtime

replace go.putnami.dev/protocol/storage => ../../protocols/storage

replace go.putnami.dev/protocol/extension => ../../protocols/extension

replace go.putnami.dev/protocol/workspace => ../../protocols/workspace

replace go.putnami.dev/protocol/support => ../../protocols/support

replace go.putnami.dev/protocol/architecture => ../../protocols/architecture
