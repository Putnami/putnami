package extension

import (
	"fmt"
	"strings"
)

// VersionConstraint represents a version constraint like "^1.0.0", "~2.0.0",
// ">=1.0.0 <2.0.0", or "latest".
type VersionConstraint struct {
	Raw      string
	Ranges   []constraintRange
	IsLatest bool
}

type constraintRange struct {
	op      string // "=", ">=", ">", "<=", "<"
	version Version
}

// ParseConstraint parses a version constraint string.
// Supported formats:
//   - "latest" — matches any version, prefer newest
//   - "1.2.3" — exact match
//   - "^1.0.0" — compatible with (>=1.0.0 <2.0.0)
//   - "~1.2.0" — approximately (>=1.2.0 <1.3.0)
//   - ">=1.0.0" — greater than or equal
//   - ">=1.0.0 <2.0.0" — range
//   - "*" — any version
func ParseConstraint(s string) (VersionConstraint, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "*" || s == "latest" {
		return VersionConstraint{Raw: s, IsLatest: true}, nil
	}

	c := VersionConstraint{Raw: s}

	// Handle caret (^)
	if strings.HasPrefix(s, "^") {
		v, err := ParseVersion(s[1:])
		if err != nil {
			return c, fmt.Errorf("parse caret constraint %q: %w", s, err)
		}
		// ^1.2.3 → >=1.2.3 <2.0.0  (major > 0)
		// ^0.2.3 → >=0.2.3 <0.3.0  (major == 0, minor > 0)
		// ^0.0.3 → >=0.0.3 <0.0.4  (major == 0, minor == 0)
		c.Ranges = append(c.Ranges, constraintRange{op: ">=", version: v})
		upper := caretUpper(v)
		c.Ranges = append(c.Ranges, constraintRange{op: "<", version: upper})
		return c, nil
	}

	// Handle tilde (~)
	if strings.HasPrefix(s, "~") {
		v, err := ParseVersion(s[1:])
		if err != nil {
			return c, fmt.Errorf("parse tilde constraint %q: %w", s, err)
		}
		// ~1.2.3 → >=1.2.3 <1.3.0
		c.Ranges = append(c.Ranges, constraintRange{op: ">=", version: v})
		upper := Version{Major: v.Major, Minor: v.Minor + 1, Patch: 0}
		c.Ranges = append(c.Ranges, constraintRange{op: "<", version: upper})
		return c, nil
	}

	// Handle space-separated ranges: ">=1.0.0 <2.0.0"
	parts := strings.Fields(s)
	for _, part := range parts {
		r, err := parseOneRange(part)
		if err != nil {
			return c, err
		}
		c.Ranges = append(c.Ranges, r)
	}

	return c, nil
}

// Match returns true if the given version satisfies this constraint.
func (c VersionConstraint) Match(v Version) bool {
	if c.IsLatest {
		return true
	}
	for _, r := range c.Ranges {
		if !r.match(v) {
			return false
		}
	}
	return true
}

// caretUpper computes the upper bound for a caret constraint.
func caretUpper(v Version) Version {
	if v.Major > 0 {
		return Version{Major: v.Major + 1}
	}
	if v.Minor > 0 {
		return Version{Major: 0, Minor: v.Minor + 1}
	}
	return Version{Major: 0, Minor: 0, Patch: v.Patch + 1}
}

func parseOneRange(s string) (constraintRange, error) {
	s = strings.TrimSpace(s)
	for _, op := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(s, op) {
			v, err := ParseVersion(strings.TrimSpace(s[len(op):]))
			if err != nil {
				return constraintRange{}, fmt.Errorf("parse range %q: %w", s, err)
			}
			return constraintRange{op: op, version: v}, nil
		}
	}
	// No operator → exact match
	v, err := ParseVersion(s)
	if err != nil {
		return constraintRange{}, fmt.Errorf("parse version %q: %w", s, err)
	}
	return constraintRange{op: "=", version: v}, nil
}

func (r constraintRange) match(v Version) bool {
	cmp := v.Compare(r.version)
	switch r.op {
	case "=":
		return cmp == 0
	case ">=":
		return cmp >= 0
	case ">":
		return cmp > 0
	case "<=":
		return cmp <= 0
	case "<":
		return cmp < 0
	default:
		return false
	}
}
