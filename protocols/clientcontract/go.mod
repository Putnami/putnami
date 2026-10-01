module go.putnami.dev/protocol/clientcontract

go 1.25.7

require go.putnami.dev/protocol/diagnostic v0.0.0

require go.putnami.dev/protocol/features v0.0.0

require google.golang.org/protobuf v1.36.11

replace go.putnami.dev/protocol/diagnostic => ../diagnostic

replace go.putnami.dev/protocol/features => ../features

replace go.putnami.dev/protocol/capabilities => ../capabilities
