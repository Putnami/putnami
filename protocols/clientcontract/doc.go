// Package clientcontract defines versioned x-putnami-client metadata for
// first-party generated clients.
//
// The standards document remains authoritative for operation identity and wire
// schemas. This package models service identity, value-free credential profiles,
// transport and stream modes, typed framework errors, idempotency, and resilience
// semantics those standards cannot express without loss. Provider projection,
// client generation, bindings, and transport execution are separate consumers.
package clientcontract
