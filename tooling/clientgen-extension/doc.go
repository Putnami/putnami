// Package clientgen implements the @putnami/clientgen extension.
//
// Project tasks invoke the packaged Go emitter or the Putnami-installed
// TypeScript package shim against a provider's built, versioned client contract.
// Workspace tasks discover every indexed provider, synchronize all configured
// targets, report binding lineage and fail validation on contract or generated
// byte drift, missing coverage, forged artifacts, unclassified external inputs
// and handwritten first-party transports.
//
// Generated targets own their complete config-selected output directory and
// publish a strict client.putnami.json inventory. Consumers register the actual
// binding symbols in that inventory; deployment URLs, client identity and
// credential material remain framework configuration.
package clientgen
