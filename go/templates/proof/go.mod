// The proof that the Go templates build and pass their own tests against this
// workspace's framework modules. Its test renders each template into a
// temporary directory; the module itself depends on nothing. Nothing publishes
// it.
module go.putnami.dev/go/templates/proof

go 1.25.7
