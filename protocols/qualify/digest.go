package qualify

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// DigestPrefix is the algorithm prefix every contract digest carries.
const DigestPrefix = "sha256:"

// canonicalRequest fixes the member order of a request's canonical JSON:
// members sorted by name, which is what a sorted-keys serializer produces in
// every language.
type canonicalRequest struct {
	ID         string `json:"id"`
	MaxStatus  int    `json:"maxStatus"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Provenance string `json:"provenance"`
}

// ContractDigest returns "sha256:" plus the lowercase hex SHA-256 of the
// canonical JSON of requests: an array (never null) of objects whose members
// are sorted by name, with no insignificant whitespace and no HTML escaping.
// The TypeScript twin computes the same bytes, and the shared fixture corpus
// pins the value both must agree on.
func ContractDigest(requests []Request) string {
	canonical := make([]canonicalRequest, 0, len(requests))
	for _, request := range requests {
		canonical = append(canonical, canonicalRequest{
			ID:         request.ID,
			MaxStatus:  request.MaxStatus,
			Method:     request.Method,
			Path:       request.Path,
			Provenance: request.Provenance,
		})
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	// Encoding plain strings and ints cannot fail.
	_ = encoder.Encode(canonical)
	sum := sha256.Sum256(bytes.TrimSuffix(buffer.Bytes(), []byte("\n")))
	return DigestPrefix + hex.EncodeToString(sum[:])
}
