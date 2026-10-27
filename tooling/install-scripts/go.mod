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
