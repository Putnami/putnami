module go.putnami.dev/dotnet/extension

go 1.25.7

require (
	github.com/google/go-containerregistry v0.21.6
	go.putnami.dev/protocol/diagnostic v0.0.0
	go.putnami.dev/protocol/extension v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/protocol/job v0.0.0
	go.putnami.dev/protocol/registry v0.0.0
	go.putnami.dev/protocol/runtime v0.0.0
	go.putnami.dev/protocol/workspace v0.0.0
	go.putnami.dev/sdk/extension v0.0.0
	go.putnami.dev/tooling/observe v0.0.0
	go.putnami.dev/tooling/stacks v0.0.0
)

require (
	github.com/docker/cli v29.4.3+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.3 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	go.putnami.dev/evidence/readers v0.0.0 // indirect
	go.putnami.dev/protocol/capabilities v0.0.0 // indirect
	go.putnami.dev/protocol/cli v0.0.0 // indirect
	go.putnami.dev/protocol/codehost v0.0.0 // indirect
	go.putnami.dev/protocol/distribution v0.0.0 // indirect
	go.putnami.dev/protocol/evidence v0.0.0 // indirect
	go.putnami.dev/protocol/oci v0.0.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
)

replace (
	go.putnami.dev/evidence/readers => ../../tooling/evidence-readers
	go.putnami.dev/protocol/architecture => ../../protocols/architecture
	go.putnami.dev/protocol/capabilities => ../../protocols/capabilities
	go.putnami.dev/protocol/cli => ../../protocols/cli
	go.putnami.dev/protocol/codehost => ../../protocols/codehost
	go.putnami.dev/protocol/collaboration => ../../protocols/collaboration
	go.putnami.dev/protocol/database => ../../protocols/database
	go.putnami.dev/protocol/diagnostic => ../../protocols/diagnostic
	go.putnami.dev/protocol/distribution => ../../protocols/distribution
	go.putnami.dev/protocol/events => ../../protocols/events
	go.putnami.dev/protocol/evidence => ../../protocols/evidence
	go.putnami.dev/protocol/extension => ../../protocols/extension
	go.putnami.dev/protocol/features => ../../protocols/features
	go.putnami.dev/protocol/gomod => ../../protocols/gomod
	go.putnami.dev/protocol/infra => ../../protocols/infra
	go.putnami.dev/protocol/job => ../../protocols/job
	go.putnami.dev/protocol/oci => ../../protocols/oci
	go.putnami.dev/protocol/put => ../../protocols/put
	go.putnami.dev/protocol/registry => ../../protocols/registry
	go.putnami.dev/protocol/runtime => ../../protocols/runtime
	go.putnami.dev/protocol/storage => ../../protocols/storage
	go.putnami.dev/protocol/workspace => ../../protocols/workspace
	go.putnami.dev/sdk/extension => ../../tooling/extension-sdk
	go.putnami.dev/tooling/observe => ../../tooling/observe
	go.putnami.dev/tooling/stacks => ../../tooling/stacks
)
