// The proof that the Go templates build and pass their own tests against this
// workspace's framework modules. Its test renders each template into a
// temporary directory. The module requires go.putnami.dev/protocol/features
// for spectest and go.putnami.dev/protocol/template for the render rules, and
// nothing else. Nothing publishes it.
module go.putnami.dev/go/templates/proof

go 1.25.7

require (
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/protocol/template v0.0.0
)

require go.putnami.dev/protocol/diagnostic v0.0.0 // indirect

replace (
	go.putnami.dev/protocol/capabilities => ../../../protocols/capabilities
	go.putnami.dev/protocol/diagnostic => ../../../protocols/diagnostic
	go.putnami.dev/protocol/features => ../../../protocols/features
	go.putnami.dev/protocol/template => ../../../protocols/template
)
