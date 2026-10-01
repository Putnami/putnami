package extension

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// UnsafeInstallEnv lets users opt in to installing an archive whose
// authenticity cannot be verified (no lockfile entry and no integrity
// header from the resolver). It is intended for the transition period
// before resolvers advertise integrity hashes; routine use defeats the
// only defense against a tampered download.
const UnsafeInstallEnv = "PUTNAMI_UNSAFE_INSTALL"

// ReadAdvertisedIntegrity returns the SHA-256 hash the resolver advertised
// for the binary it just streamed, or "" when neither X-Integrity nor a
// SHA-256 Digest (RFC 9530) header is set.
//
// The X-Integrity header is preferred when both are present.
func ReadAdvertisedIntegrity(h http.Header) string {
	if v := strings.TrimSpace(h.Get("X-Integrity")); v != "" {
		return v
	}
	return parseDigestSHA256(h.Get("Digest"))
}

// parseDigestSHA256 extracts a SHA-256 digest from an RFC 9530 Digest
// header ("sha-256=<value>"). Returns "" when no SHA-256 digest is present.
func parseDigestSHA256(h string) string {
	const prefix = "sha-256="
	for part := range strings.SplitSeq(h, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(strings.ToLower(part), prefix) {
			return strings.TrimSpace(part[len(prefix):])
		}
	}
	return ""
}

// NormalizeIntegrity converts a resolver-supplied integrity string into a
// plain lowercase hex SHA-256 digest suitable for comparison against
// HashFile output. It accepts:
//   - "sha256:<hex>" / "sha-256:<hex>" (Docker / OCI convention)
//   - "sha256-<hex>" / "sha-256-<hex>" (SRI-ish)
//   - "<hex>" (raw)
//
// Other forms (base64 SRI, signatures) are rejected so the comparison has
// a single canonical path.
func NormalizeIntegrity(integrity string) (string, error) {
	v := strings.TrimSpace(integrity)
	for _, prefix := range []string{"sha256:", "sha-256:", "sha256-", "sha-256-"} {
		if rest, ok := strings.CutPrefix(strings.ToLower(v), prefix); ok {
			v = rest
			break
		}
	}
	v = strings.ToLower(strings.TrimSpace(v))
	if len(v) != 64 {
		return "", fmt.Errorf("expected 64-character hex SHA-256, got %d characters", len(v))
	}
	if _, err := hex.DecodeString(v); err != nil {
		return "", fmt.Errorf("not valid hex: %w", err)
	}
	return v, nil
}
