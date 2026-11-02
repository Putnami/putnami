module go.putnami.dev/protocol/i18n

go 1.25.7

// features is TEST-ONLY: spectest binds the schema tests to the requirements
// of i18n/catalog-format.
require go.putnami.dev/protocol/features v0.0.0

replace go.putnami.dev/protocol/capabilities => ../capabilities

replace go.putnami.dev/protocol/diagnostic => ../diagnostic

replace go.putnami.dev/protocol/features => ../features
