package runtime

// Runtime event protocol negotiation.
//
// The decision this file implements, including the alternatives that were
// rejected, is recorded in doc/adr/0001-one-stream-one-negotiated-version.md.
//
// A subprocess cannot know which event vocabulary its invoker understands, and
// the stream rules forbid guessing: one stream speaks ONE version
// (mixed-protocol-version in validate.go), so the choice is made once, before
// the first line, or not at all. This file is that seam — the single place the
// invoking CLI's advertisement is written and read.
//
// The contract is deliberately one integer in one reserved environment
// variable:
//
//	PUTNAMI_RUNTIME_EVENTS=<max protocol version the invoker accepts>
//
// Environment, not the job context file, because the advertisement must reach
// processes the context file never does: `--putnamiContext` is passed to the
// extension binary alone, while a serve job's environment is inherited by the
// workload the extension spawns. Reserved PUTNAMI_* protocol variables have
// precedent — PUTNAMI_INTERACTIVE (protocols/extension) and the
// PUTNAMI_AGENT_* identity set (agent_identity.go).
//
// Absent, unparsable, or lower than a version this package knows all mean the
// same thing: emit v1. That is the fail-closed direction — a 0.2.x CLI, a
// directly invoked extension binary, and a corrupted value all receive the
// byte-identical v1 stream they can parse, and a `ready` line can never leak
// into a reader that would reject the whole stream for it.

import (
	"strconv"
	"strings"
)

// AcceptedVersionEnv is the reserved environment variable an invoking CLI sets
// to advertise the highest runtime event protocol version it accepts. The value
// is a decimal integer, and clamping (below) is safe because an emitter may
// always answer at or below what the invoker advertised.
//
// What an invoker accepts BELOW its advertisement is the invoker's own policy,
// not something this package can promise on its behalf. Putnami's CLI accepts
// EXACTLY the version it advertises: the extension contract it requires obliges
// an extension to read this variable and answer at it, so a lower-versioned line
// means the stream disagrees with the manifest rather than that the emitter is
// old. Emitters must therefore answer at the advertised version — which
// NegotiatedVersion already returns — instead of treating it as a ceiling they
// may undercut.
const AcceptedVersionEnv = "PUTNAMI_RUNTIME_EVENTS"

// MaxKnownProtocolVersion is the highest protocol version this package can emit
// and validate. It is what a CLI built against this package advertises and the
// ceiling a reader clamps a higher advertisement down to.
const MaxKnownProtocolVersion = ProtocolVersion2

// NegotiatedVersion resolves an advertised value into the protocol version an
// emitter must stamp on every line of its stream.
//
// The resolution is total — every input maps to a version this package knows —
// so an emitter never has to decide what to do with a bad advertisement:
//
//   - absent, blank, or unparsable → ProtocolVersion (v1), the version every
//     consumer has always understood;
//   - below ProtocolVersion → ProtocolVersion, for the same reason;
//   - above MaxKnownProtocolVersion → MaxKnownProtocolVersion, because an
//     invoker that accepts a version newer than this build knows also accepts
//     every version below it.
func NegotiatedVersion(raw string) int {
	version, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return ProtocolVersion
	}
	return clampProtocolVersion(version)
}

// NegotiatedVersionFromEnv reads AcceptedVersionEnv through lookup (os.Getenv in
// production, a map in tests) and resolves it with NegotiatedVersion. A nil
// lookup yields v1, so a caller with no environment access is fail-closed too.
func NegotiatedVersionFromEnv(lookup func(string) string) int {
	if lookup == nil {
		return ProtocolVersion
	}
	return NegotiatedVersion(lookup(AcceptedVersionEnv))
}

// AdvertisedVersionEnv renders the "KEY=value" entry an invoker appends to a
// subprocess environment to advertise version. It exists so the writing side
// and the reading side (NegotiatedVersion) cannot disagree about the variable's
// name or its clamping: both go through this file.
func AdvertisedVersionEnv(version int) string {
	return AcceptedVersionEnv + "=" + strconv.Itoa(clampProtocolVersion(version))
}

// clampProtocolVersion bounds a version into [ProtocolVersion,
// MaxKnownProtocolVersion].
func clampProtocolVersion(version int) int {
	if version < ProtocolVersion {
		return ProtocolVersion
	}
	if version > MaxKnownProtocolVersion {
		return MaxKnownProtocolVersion
	}
	return version
}
