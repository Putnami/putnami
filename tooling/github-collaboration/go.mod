module go.putnami.dev/tooling/github-collaboration

go 1.25.7

require (
	go.putnami.dev/protocol/cli v0.0.0
	go.putnami.dev/protocol/collaboration v0.0.0
	go.putnami.dev/protocol/extension v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/protocol/runtime v0.0.0
)

require go.putnami.dev/protocol/diagnostic v0.0.0

replace (
	go.putnami.dev/protocol/capabilities => ../../protocols/capabilities
	go.putnami.dev/protocol/cli => ../../protocols/cli
	go.putnami.dev/protocol/collaboration => ../../protocols/collaboration
	go.putnami.dev/protocol/diagnostic => ../../protocols/diagnostic
	go.putnami.dev/protocol/extension => ../../protocols/extension
	go.putnami.dev/protocol/features => ../../protocols/features
	go.putnami.dev/protocol/runtime => ../../protocols/runtime
)
