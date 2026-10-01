package releaseset

import (
	"crypto/sha256"
	"encoding/hex"
)

// selectionDomain separates this hash from every other sha256 the workspace
// computes: the same execution key must never be mistakable for a cache
// address, and a future revision of the derivation changes this string.
const selectionDomain = "release-set-selection/v2\n"

// SelectionFingerprint derives the value a member records so a later
// publication can decide whether to republish it.
//
// The input is the member's package task EXECUTION KEY computed without the
// embedded version: every declared input of the packaging recipe — the sources,
// the packager's own identity and version, the toolchain, the task contract,
// the resolved parameters, and every upstream task key — but not the version
// string being stamped. Two identities come out of one computation: the cache
// key, which includes the version and decides whether the package task may be
// served from cache, and this one, which excludes it and decides whether the
// member is republished. A member whose fingerprint equals the head's record is
// inherited, whatever the git history in between; a packager upgrade or an
// upstream change moves it and republishes, which a tree hash never saw.
func SelectionFingerprint(executionKey string) string {
	sum := sha256.Sum256([]byte(selectionDomain + executionKey))
	return "sha256:" + hex.EncodeToString(sum[:])
}
