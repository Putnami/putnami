package extension

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Version represents a parsed semantic version (major.minor.patch with optional prerelease).
type Version struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string // e.g., "alpha.1", "beta.2"
}

// ParseVersion parses a semantic version string like "1.2.3" or "1.0.0-beta.1".
func ParseVersion(s string) (Version, error) {
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return Version{}, fmt.Errorf("empty version string")
	}

	var v Version

	// Split off prerelease at first hyphen
	if idx := strings.IndexByte(s, '-'); idx >= 0 {
		v.Prerelease = s[idx+1:]
		s = s[:idx]
	}

	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return Version{}, fmt.Errorf("invalid version format: %q", s)
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return Version{}, fmt.Errorf("invalid major version: %q", parts[0])
	}
	v.Major = major

	if len(parts) >= 2 {
		minor, err := strconv.Atoi(parts[1])
		if err != nil {
			return Version{}, fmt.Errorf("invalid minor version: %q", parts[1])
		}
		v.Minor = minor
	}

	if len(parts) >= 3 {
		patch, err := strconv.Atoi(parts[2])
		if err != nil {
			return Version{}, fmt.Errorf("invalid patch version: %q", parts[2])
		}
		v.Patch = patch
	}

	return v, nil
}

// String returns the version as "major.minor.patch" (with optional prerelease).
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Prerelease != "" {
		s += "-" + v.Prerelease
	}
	return s
}

// Compare returns -1 if v < other, 0 if equal, 1 if v > other.
func (v Version) Compare(other Version) int {
	if v.Major != other.Major {
		return cmpInt(v.Major, other.Major)
	}
	if v.Minor != other.Minor {
		return cmpInt(v.Minor, other.Minor)
	}
	if v.Patch != other.Patch {
		return cmpInt(v.Patch, other.Patch)
	}
	// Prerelease versions have lower precedence than the release version.
	// A version without prerelease > a version with prerelease.
	if v.Prerelease == "" && other.Prerelease == "" {
		return 0
	}
	if v.Prerelease == "" {
		return 1 // release > prerelease
	}
	if other.Prerelease == "" {
		return -1
	}
	return comparePrerelease(v.Prerelease, other.Prerelease)
}

// LessThan returns true if v < other.
func (v Version) LessThan(other Version) bool {
	return v.Compare(other) < 0
}

// GreaterThan returns true if v > other.
func (v Version) GreaterThan(other Version) bool {
	return v.Compare(other) > 0
}

// Equal returns true if v == other.
func (v Version) Equal(other Version) bool {
	return v.Compare(other) == 0
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// comparePrerelease compares two prerelease strings following semver rules.
func comparePrerelease(a, b string) int {
	partsA := strings.Split(a, ".")
	partsB := strings.Split(b, ".")

	maxLen := len(partsA)
	if len(partsB) > maxLen {
		maxLen = len(partsB)
	}

	for i := 0; i < maxLen; i++ {
		if i >= len(partsA) {
			return -1 // a is shorter → a < b
		}
		if i >= len(partsB) {
			return 1 // b is shorter → a > b
		}

		pa, pb := partsA[i], partsB[i]
		na, errA := strconv.Atoi(pa)
		nb, errB := strconv.Atoi(pb)

		if errA == nil && errB == nil {
			// Both numeric: compare as integers
			c := cmpInt(na, nb)
			if c != 0 {
				return c
			}
		} else if errA == nil {
			return -1 // numeric < string
		} else if errB == nil {
			return 1 // string > numeric
		} else {
			// Both strings: lexicographic
			if pa < pb {
				return -1
			}
			if pa > pb {
				return 1
			}
		}
	}
	return 0
}

// SortVersions sorts version strings in ascending order.
func SortVersions(versions []string) {
	sort.Slice(versions, func(i, j int) bool {
		vi, errI := ParseVersion(versions[i])
		vj, errJ := ParseVersion(versions[j])
		if errI != nil || errJ != nil {
			return versions[i] < versions[j]
		}
		return vi.LessThan(vj)
	})
}
