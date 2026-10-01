module go.putnami.dev/protocol/collaboration

go 1.25.7

require (
	go.putnami.dev/protocol/diagnostic v0.0.0
	go.putnami.dev/protocol/extension v0.0.0
)

require go.putnami.dev/protocol/cli v0.0.0 // indirect

replace go.putnami.dev/protocol/cli => ../cli

replace go.putnami.dev/protocol/diagnostic => ../diagnostic

replace go.putnami.dev/protocol/extension => ../extension
