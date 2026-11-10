module go.putnami.dev/cloud/runtime

go 1.25.7

require (
	go.putnami.dev/app v0.0.1
	go.putnami.dev/config v0.0.1
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/telemetry v0.0.1
	google.golang.org/api v0.276.0
)

require go.opentelemetry.io/otel/sdk/metric v1.43.0 // indirect

require (
	cloud.google.com/go/auth v0.20.0 // indirect
	cloud.google.com/go/auth/oauth2adapt v0.2.8 // indirect
	cloud.google.com/go/compute/metadata v0.9.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/s2a-go v0.1.9 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/googleapis/enterprise-certificate-proxy v0.3.14 // indirect
	github.com/googleapis/gax-go/v2 v2.21.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.67.0 // indirect
	go.opentelemetry.io/otel v1.43.0 // indirect
	go.opentelemetry.io/otel/metric v1.43.0 // indirect
	go.opentelemetry.io/otel/sdk v1.43.0 // indirect
	go.opentelemetry.io/otel/trace v1.43.0 // indirect
	go.putnami.dev/client v0.0.1 // indirect
	go.putnami.dev/errors v0.0.1 // indirect
	go.putnami.dev/http v0.0.1 // indirect
	go.putnami.dev/inject v0.0.1 // indirect
	go.putnami.dev/logger v0.0.1 // indirect
	go.putnami.dev/migration v0.0.0 // indirect
	go.putnami.dev/protocol/capabilities v0.0.0 // indirect
	go.putnami.dev/protocol/clientcontract v0.0.0 // indirect
	go.putnami.dev/protocol/config v0.0.0 // indirect
	go.putnami.dev/protocol/database v0.0.0 // indirect
	go.putnami.dev/protocol/diagnostic v0.0.0 // indirect
	go.putnami.dev/protocol/events v0.0.0 // indirect
	go.putnami.dev/protocol/http-routes v0.0.0 // indirect
	go.putnami.dev/protocol/identity v0.0.0 // indirect
	go.putnami.dev/protocol/infra v0.0.0 // indirect
	go.putnami.dev/protocol/migration v0.0.0 // indirect
	go.putnami.dev/protocol/runtime v0.0.0 // indirect
	go.putnami.dev/protocol/storage v0.0.0 // indirect
	go.putnami.dev/protocol/telemetry v0.0.0 // indirect
	golang.org/x/crypto v0.49.0 // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.35.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/grpc v1.80.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	go.putnami.dev/app => ../../../go/framework/app
	go.putnami.dev/client => ../../../go/framework/client
	go.putnami.dev/config => ../../../go/framework/config
	go.putnami.dev/errors => ../../../go/framework/errors
	go.putnami.dev/http => ../../../go/framework/http
	go.putnami.dev/inject => ../../../go/framework/inject
	go.putnami.dev/logger => ../../../go/framework/logger
	go.putnami.dev/migration => ../../../go/framework/migration
	go.putnami.dev/protocol/capabilities => ../../../protocols/capabilities
	go.putnami.dev/protocol/clientcontract => ../../../protocols/clientcontract
	go.putnami.dev/protocol/config => ../../../protocols/config
	go.putnami.dev/protocol/database => ../../../protocols/database
	go.putnami.dev/protocol/diagnostic => ../../../protocols/diagnostic
	go.putnami.dev/protocol/events => ../../../protocols/events
	go.putnami.dev/protocol/features => ../../../protocols/features
	go.putnami.dev/protocol/http-routes => ../../../protocols/http-routes
	go.putnami.dev/protocol/identity => ../../../protocols/identity
	go.putnami.dev/protocol/infra => ../../../protocols/infra
	go.putnami.dev/protocol/migration => ../../../protocols/migration
	go.putnami.dev/protocol/runtime => ../../../protocols/runtime
	go.putnami.dev/protocol/storage => ../../../protocols/storage
	go.putnami.dev/protocol/telemetry => ../../../protocols/telemetry
	go.putnami.dev/telemetry => ../../../go/framework/telemetry
)

replace go.putnami.dev/protocol/architecture => ../../../protocols/architecture

replace go.putnami.dev/protocol/contracts => ../../../protocols/contracts
