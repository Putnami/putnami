// The tests of the CLI's install and smoke scripts are their own module and
// project so that their cache key holds the scripts and the install
// documentation, not the CLI's sources. Nothing publishes this module.
module go.putnami.dev/tooling/installscripts

go 1.25.7

require (
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/sdk/extension v0.0.0
	golang.org/x/sys v0.44.0
)

replace (
	go.putnami.dev/protocol/features => ../../protocols/features
	go.putnami.dev/sdk/extension => ../extension-sdk
)

replace go.putnami.dev/protocol/architecture => ../../protocols/architecture

replace go.putnami.dev/protocol/capabilities => ../../protocols/capabilities

replace go.putnami.dev/protocol/cli => ../../protocols/cli

replace go.putnami.dev/protocol/database => ../../protocols/database

replace go.putnami.dev/protocol/diagnostic => ../../protocols/diagnostic

replace go.putnami.dev/protocol/distribution => ../../protocols/distribution

replace go.putnami.dev/protocol/events => ../../protocols/events

replace go.putnami.dev/protocol/extension => ../../protocols/extension

replace go.putnami.dev/protocol/gomod => ../../protocols/gomod

replace go.putnami.dev/protocol/infra => ../../protocols/infra

replace go.putnami.dev/protocol/job => ../../protocols/job

replace go.putnami.dev/protocol/oci => ../../protocols/oci

replace go.putnami.dev/protocol/put => ../../protocols/put

replace go.putnami.dev/protocol/registry => ../../protocols/registry

replace go.putnami.dev/protocol/runtime => ../../protocols/runtime

replace go.putnami.dev/protocol/storage => ../../protocols/storage

replace go.putnami.dev/protocol/workspace => ../../protocols/workspace
