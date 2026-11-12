// Package clientgen implements the @putnami/clientgen extension.
//
// The client-generate task invokes the packaged Go emitter or the
// Putnami-installed TypeScript package shim against a provider's committed,
// versioned client contract. A client project names its provider in the
// generatedFrom block of its putnami.json, and a provider declares its
// in-module Go client in its committed clientgen.json. Generated clients are
// built, not committed.
//
// The workspace check discovers every indexed provider, names the binding each
// client exports, and fails validation on a tracked generated file, an invalid
// contract, an unclassified external input or a handwritten first-party
// transport. Deployment URLs, client identity and credential material remain
// framework configuration.
package clientgen
