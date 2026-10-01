// Package registry defines the credential seam between the framework's
// publishers and @putnami/cloud: registry-token/v1.
//
// The framework owns no Putnami host list, recipe model, or stored credential
// file. A publisher that needs a bearer for a registry host shells the cloud:
//
//	putnami cloud registry-token --host <host>
//	  → stdout: a bare bearer token, nothing else        (exit 0)
//	  → exit non-zero / empty stdout: no credential; the publisher falls back to
//	    explicit or native credentials, and the cloud's stderr (if any) is
//	    surfaced as a hint
//
// An installer needs the same credential in the NATIVE form its package manager
// reads, so the seam has a second shape:
//
//	putnami cloud registry-token --host <host> --materialize
//	  → the cloud writes the host's native credential file and prints nothing
//	  → exit 2 (usage): this cloud does not implement the shape; the installer
//	    keeps going with whatever credential the machine already has
//	  → exit 3 (auth): the user is not signed in
//
// Passing the host as data keeps core cloud-agnostic: a host the cloud does not
// manage, a cloud that is absent (a core-only install), or one the user is not
// signed in to all yield "no token", and the standard publish floor still works.
//
// # The retired publish-provider marker
//
// PublishProviderCommandName was a marker command an extension declared so the
// framework could feature-detect a publish-capable cloud at extension-load time.
// That gate is deleted on both sides: nothing declares the command and nothing
// looks for it. Advanced publishing is selected by the tasks a manifest
// declares, and credentials by this seam, which is host-keyed and needs no
// capability probe (doc/adr/0001-host-keyed-credential-seam.md).
//
// The name survives as a RESERVED spelling, not a live signal. Both repositories
// assert its absence against this constant, so a reintroduced marker fails a
// conformance test instead of quietly becoming a second capability mechanism.
package registry

import (
	"strings"
	"unicode"
)

// ProtocolVersion is the current registry credential-seam protocol version.
const ProtocolVersion = 1

// The registry-token credential seam invocation:
// `putnami cloud registry-token --host <host>`.
const (
	// CLIExecutableEnv carries the absolute executable of the CLI that spawned
	// an extension. SDK seams use it to call back into that same CLI even when a
	// local bootstrap binary is not installed on PATH.
	CLIExecutableEnv = "PUTNAMI_CLI_EXECUTABLE"
	// SeamParentCommand is the cloud CLI command group that owns the seam.
	SeamParentCommand = "cloud"
	// SeamSubcommand is the subcommand the framework shells to resolve a bearer.
	SeamSubcommand = "registry-token"
	// SeamHostFlag is the flag carrying the registry host the bearer is for.
	SeamHostFlag = "host"
	// SeamMaterializeFlag asks the cloud to WRITE the native credential for the
	// host itself instead of printing a bearer: the `//<host>/:_authToken=` line
	// in the user's .npmrc, the `machine <host>` entry in the user's .netrc, the
	// docker config entry. Nothing is written to stdout.
	//
	// It exists because an install runs a package manager the framework does not
	// speak the credential protocol of. `bun install` reads .npmrc, `go` reads
	// .netrc; neither takes a bearer on a pipe. The cloud already knows which
	// native form a host's registry kind uses, so it writes the file and the
	// framework stays free of both the credential format and the host list.
	SeamMaterializeFlag = "materialize"
)

// PublishProviderCommandName is the retired capability marker (see the package
// doc). It is kept so both repositories name the same reserved spelling when
// they assert that nothing declares it; no code path detects it any more.
const PublishProviderCommandName = "publish-provider"

// ValidBearer reports whether s is a well-formed bearer for the seam: a non-empty
// token with no internal whitespace.
//
// The seam contract is a bare bearer on stdout and nothing else. A value carrying
// whitespace is a human status line written to the wrong stream; sent verbatim it
// would become a malformed `Authorization: Bearer` header the registry rejects as
// an opaque 401. Treat such a value as "no token".
func ValidBearer(s string) bool {
	if s == "" {
		return false
	}
	return strings.IndexFunc(s, unicode.IsSpace) < 0
}
