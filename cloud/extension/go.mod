module go.putnami.dev/cloud/extension

go 1.26.1

require (
	github.com/google/go-containerregistry v0.21.6
	go.putnami.dev/api v0.0.0
	go.putnami.dev/app v0.0.1
	go.putnami.dev/client v0.0.0
	go.putnami.dev/cloud/clients/auth-server v0.0.0
	go.putnami.dev/cloud/clients/cache-server v0.0.0
	go.putnami.dev/cloud/clients/config-api v0.0.0
	go.putnami.dev/cloud/clients/control-api v0.0.0
	go.putnami.dev/cloud/clients/data-api v0.0.0
	go.putnami.dev/cloud/clients/db-gateway v0.0.0
	go.putnami.dev/cloud/clients/delivery-api v0.0.0
	go.putnami.dev/cloud/clients/distribution-api v0.0.0
	go.putnami.dev/cloud/clients/identity-api v0.0.0
	go.putnami.dev/cloud/clients/observability-api v0.0.0
	go.putnami.dev/cloud/clients/oci-server v0.0.0
	go.putnami.dev/cloud/clients/put-server v0.0.0
	go.putnami.dev/cloud/clients/runtime-api v0.0.0
	go.putnami.dev/cloud/clients/source-api v0.0.0
	go.putnami.dev/errors v0.0.1
	go.putnami.dev/http v0.0.1
	go.putnami.dev/inject v0.0.1
	go.putnami.dev/protocol/cache v0.0.0
	go.putnami.dev/protocol/ci v0.0.0
	go.putnami.dev/protocol/cli v0.0.0
	go.putnami.dev/protocol/clientcontract v0.0.0
	go.putnami.dev/protocol/config v0.0.0
	go.putnami.dev/protocol/diagnostic v0.0.0
	go.putnami.dev/protocol/distribution v0.0.0
	go.putnami.dev/protocol/extension v0.0.0
	go.putnami.dev/protocol/features v0.0.0
	go.putnami.dev/protocol/infra v0.0.0
	go.putnami.dev/protocol/job v0.0.0
	go.putnami.dev/protocol/migration v0.0.0
	go.putnami.dev/protocol/put v0.0.0
	go.putnami.dev/protocol/registry v0.0.0
	go.putnami.dev/protocol/runtime v0.0.0
	go.putnami.dev/protocol/sitecontent v0.0.0
	go.putnami.dev/protocol/workspace v0.0.0
	go.putnami.dev/sdk/extension v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/docker/cli v29.4.3+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.3 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	go.putnami.dev/config v0.0.1 // indirect
	go.putnami.dev/logger v0.0.1 // indirect
	go.putnami.dev/migration v0.0.0 // indirect
	go.putnami.dev/protocol/capabilities v0.0.0 // indirect
	go.putnami.dev/protocol/database v0.0.0 // indirect
	go.putnami.dev/protocol/events v0.0.0 // indirect
	go.putnami.dev/protocol/http-routes v0.0.0 // indirect
	go.putnami.dev/protocol/identity v0.0.0 // indirect
	go.putnami.dev/protocol/platform v0.0.0 // indirect
	go.putnami.dev/protocol/storage v0.0.0 // indirect
	go.putnami.dev/schema v0.0.1 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace go.putnami.dev/api => ../../go/framework/api

replace go.putnami.dev/app => ../../go/framework/app

replace go.putnami.dev/client => ../../go/framework/client

replace go.putnami.dev/cloud/clients/auth-server => ../clients/auth-server

replace go.putnami.dev/cloud/clients/cache-server => ../clients/cache-server

replace go.putnami.dev/cloud/clients/config-api => ../clients/config-api

replace go.putnami.dev/cloud/clients/control-api => ../clients/control-api

replace go.putnami.dev/cloud/clients/data-api => ../clients/data-api

replace go.putnami.dev/cloud/clients/db-gateway => ../clients/db-gateway

replace go.putnami.dev/cloud/clients/delivery-api => ../clients/delivery-api

replace go.putnami.dev/cloud/clients/distribution-api => ../clients/distribution-api

replace go.putnami.dev/cloud/clients/identity-api => ../clients/identity-api

replace go.putnami.dev/cloud/clients/observability-api => ../clients/observability-api

replace go.putnami.dev/cloud/clients/oci-server => ../clients/oci-server

replace go.putnami.dev/cloud/clients/put-server => ../clients/put-server

replace go.putnami.dev/cloud/clients/runtime-api => ../clients/runtime-api

replace go.putnami.dev/cloud/clients/source-api => ../clients/source-api

replace go.putnami.dev/config => ../../go/framework/config

replace go.putnami.dev/errors => ../../go/framework/errors

replace go.putnami.dev/http => ../../go/framework/http

replace go.putnami.dev/inject => ../../go/framework/inject

replace go.putnami.dev/logger => ../../go/framework/logger

replace go.putnami.dev/migration => ../../go/framework/migration

replace go.putnami.dev/protocol/architecture => ../../protocols/architecture

replace go.putnami.dev/protocol/cache => ../../protocols/cache

replace go.putnami.dev/protocol/capabilities => ../../protocols/capabilities

replace go.putnami.dev/protocol/ci => ../../protocols/ci

replace go.putnami.dev/protocol/cli => ../../protocols/cli

replace go.putnami.dev/protocol/clientcontract => ../../protocols/clientcontract

replace go.putnami.dev/protocol/config => ../../protocols/config

replace go.putnami.dev/protocol/contracts => ../../protocols/contracts

replace go.putnami.dev/protocol/database => ../../protocols/database

replace go.putnami.dev/protocol/diagnostic => ../../protocols/diagnostic

replace go.putnami.dev/protocol/distribution => ../../protocols/distribution

replace go.putnami.dev/protocol/events => ../../protocols/events

replace go.putnami.dev/protocol/extension => ../../protocols/extension

replace go.putnami.dev/protocol/features => ../../protocols/features

replace go.putnami.dev/protocol/gomod => ../../protocols/gomod

replace go.putnami.dev/protocol/http-routes => ../../protocols/http-routes

replace go.putnami.dev/protocol/identity => ../../protocols/identity

replace go.putnami.dev/protocol/infra => ../../protocols/infra

replace go.putnami.dev/protocol/job => ../../protocols/job

replace go.putnami.dev/protocol/migration => ../../protocols/migration

replace go.putnami.dev/protocol/oci => ../../protocols/oci

replace go.putnami.dev/protocol/platform => ../../protocols/platform

replace go.putnami.dev/protocol/put => ../../protocols/put

replace go.putnami.dev/protocol/registry => ../../protocols/registry

replace go.putnami.dev/protocol/runtime => ../../protocols/runtime

replace go.putnami.dev/protocol/sitecontent => ../../protocols/sitecontent

replace go.putnami.dev/protocol/storage => ../../protocols/storage

replace go.putnami.dev/protocol/workspace => ../../protocols/workspace

replace go.putnami.dev/schema => ../../go/framework/schema

replace go.putnami.dev/sdk/extension => ../../tooling/extension-sdk
