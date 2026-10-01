package distribution

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	diag "go.putnami.dev/protocol/diagnostic"
)

// NormalizeReleaseSet returns a deep, normalized copy without mutating input.
// Members and dependencies are sorted by (ecosystem, coordinate), and nil
// collections become empty arrays so equivalent callers hash the same bytes.
// Duplicate coordinates are deliberately retained for validation to reject;
// normalization never hides an ambiguous declaration.
func NormalizeReleaseSet(input *ReleaseSet) *ReleaseSet {
	if input == nil {
		return nil
	}
	normalized := &ReleaseSet{
		ProtocolVersion: input.ProtocolVersion,
		Namespace:       input.Namespace,
		Members:         make([]ReleaseSetMember, len(input.Members)),
	}
	for i, member := range input.Members {
		normalized.Members[i] = ReleaseSetMember{
			Ecosystem:            member.Ecosystem,
			Coordinate:           member.Coordinate,
			Version:              member.Version,
			ArtifactDigest:       member.ArtifactDigest,
			Dependencies:         append([]ReleaseSetDependency{}, member.Dependencies...),
			SourceRevision:       member.SourceRevision,
			SelectionFingerprint: member.SelectionFingerprint,
			Platforms:            clonePlatforms(member.Platforms),
			Project:              member.Project,
			Kind:                 member.Kind,
			SourceTree:           member.SourceTree,
		}
		sort.Slice(normalized.Members[i].Dependencies, func(a, b int) bool {
			return coordinateLess(
				normalized.Members[i].Dependencies[a].Ecosystem,
				normalized.Members[i].Dependencies[a].Coordinate,
				normalized.Members[i].Dependencies[b].Ecosystem,
				normalized.Members[i].Dependencies[b].Coordinate,
			)
		})
	}
	sort.Slice(normalized.Members, func(i, j int) bool {
		return coordinateLess(
			normalized.Members[i].Ecosystem,
			normalized.Members[i].Coordinate,
			normalized.Members[j].Ecosystem,
			normalized.Members[j].Coordinate,
		)
	})
	return normalized
}

// clonePlatforms copies a member's per-platform digests. A nil or empty map
// stays nil so a member without platforms omits the field from canonical bytes;
// json.Marshal orders map keys, so equivalent inputs hash the same.
func clonePlatforms(platforms map[string]string) map[string]string {
	if len(platforms) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(platforms))
	for platform, digest := range platforms {
		cloned[platform] = digest
	}
	return cloned
}

func coordinateLess(aEcosystem Ecosystem, aCoordinate string, bEcosystem Ecosystem, bCoordinate string) bool {
	if aEcosystem != bEcosystem {
		return aEcosystem < bEcosystem
	}
	return aCoordinate < bCoordinate
}

// CanonicalReleaseSetBytes validates and serializes the normalized hash
// projection as fixed-order JSON with no insignificant whitespace or newline.
func CanonicalReleaseSetBytes(input *ReleaseSet) ([]byte, []diag.Diagnostic) {
	normalized := NormalizeReleaseSet(input)
	diagnostics := ValidateReleaseSet(normalized)
	if diag.HasErrors(diagnostics) {
		return nil, diagnostics
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "encode canonical release set: %v", err)}
	}
	return encoded, nil
}

// DeriveReleaseSetRef computes both public reference forms from canonical
// release-set bytes. Derived values are never included in the hashed document.
func DeriveReleaseSetRef(input *ReleaseSet) (ReleaseSetRef, []diag.Diagnostic) {
	canonical, diagnostics := CanonicalReleaseSetBytes(input)
	if diag.HasErrors(diagnostics) {
		return ReleaseSetRef{}, diagnostics
	}
	hash := sha256.Sum256(canonical)
	hexDigest := hex.EncodeToString(hash[:])
	return ReleaseSetRef{
		ID:     "rs_" + hexDigest,
		Digest: "sha256:" + hexDigest,
	}, nil
}

// ParseCanonicalReleaseSet accepts only the exact canonical byte projection.
// Whitespace, alternate ordering, a trailing newline, or any other equivalent
// but non-canonical spelling is rejected.
func ParseCanonicalReleaseSet(data []byte) (*ReleaseSet, ReleaseSetRef, []diag.Diagnostic) {
	releaseSet, diagnostics := ParseAndValidateReleaseSet(data)
	if releaseSet == nil || diag.HasErrors(diagnostics) {
		return nil, ReleaseSetRef{}, diagnostics
	}
	canonical, canonicalDiagnostics := CanonicalReleaseSetBytes(releaseSet)
	diagnostics = append(diagnostics, canonicalDiagnostics...)
	if diag.HasErrors(diagnostics) {
		return nil, ReleaseSetRef{}, boundDiagnostics(diagnostics)
	}
	if !bytes.Equal(data, canonical) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeNonCanonical, "", "release-set bytes are not the exact normalized, fixed-order, whitespace-free JSON projection"))
		return nil, ReleaseSetRef{}, boundDiagnostics(diagnostics)
	}
	ref, refDiagnostics := DeriveReleaseSetRef(releaseSet)
	diagnostics = append(diagnostics, refDiagnostics...)
	if diag.HasErrors(diagnostics) {
		return nil, ReleaseSetRef{}, boundDiagnostics(diagnostics)
	}
	return releaseSet, ref, diagnostics
}
