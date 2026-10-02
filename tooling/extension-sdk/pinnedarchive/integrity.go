package pinnedarchive

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// ReadAdvertisedIntegrity returns the SHA-256 digest a registry advertises for
// the archive it serves, as the registry spells it, or "" when it advertises
// none. It reads the X-Integrity header and, when that is absent, the
// sha-256 entry of the legacy Digest header (`Digest: sha-256=<value>`, RFC
// 3230). Content-Digest and Repr-Digest (RFC 9530) are not read.
func ReadAdvertisedIntegrity(h http.Header) string {
	if v := strings.TrimSpace(h.Get("X-Integrity")); v != "" {
		return v
	}
	return parseDigestSHA256(h.Get("Digest"))
}

// parseDigestSHA256 returns the value of the sha-256 entry of a Digest header,
// or "" when it has none.
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

// NormalizeIntegrity converts an advertised integrity string into a plain
// lowercase hex SHA-256 digest. It accepts:
//   - "sha256:<hex>" / "sha-256:<hex>" (Docker / OCI convention)
//   - "sha256-<hex>" / "sha-256-<hex>"
//   - "<hex>" (raw)
//
// Any other form, such as a base64 value, is an error.
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
