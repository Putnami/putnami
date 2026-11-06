package nodeworkspace

import (
	"strconv"
	"strings"
)

// version is a parsed semantic version.
type version struct {
	major, minor, patch int
	prerelease          string
}

func (v version) compare(o version) int {
	for _, d := range [3]int{v.major - o.major, v.minor - o.minor, v.patch - o.patch} {
		if d != 0 {
			return d
		}
	}
	switch {
	case v.prerelease == o.prerelease:
		return 0
	case v.prerelease == "":
		return 1
	case o.prerelease == "":
		return -1
	}
	return strings.Compare(v.prerelease, o.prerelease)
}

// parseVersion reads a full version such as 1.2.3 or v1.2.3-beta.1+build.
func parseVersion(s string) (version, bool) {
	parts, prerelease, ok := partial(s)
	if !ok || len(parts) != 3 {
		return version{}, false
	}
	return version{parts[0], parts[1], parts[2], prerelease}, true
}

// partial reads a version whose trailing parts may be missing or wildcards:
// 1, 1.2, 1.x, 1.2.*. It returns the numeric parts before the first wildcard.
func partial(s string) ([]int, string, bool) {
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "="), "v")
	s, _, _ = strings.Cut(s, "+")
	s, prerelease, _ := strings.Cut(s, "-")
	var parts []int
	for i, field := range strings.Split(s, ".") {
		if i > 2 {
			return nil, "", false
		}
		if field == "x" || field == "X" || field == "*" {
			if prerelease != "" {
				return nil, "", false
			}
			return parts, "", true
		}
		n, err := strconv.Atoi(field)
		if err != nil || n < 0 {
			return nil, "", false
		}
		if len(parts) != i {
			return nil, "", false
		}
		parts = append(parts, n)
	}
	if prerelease != "" && len(parts) != 3 {
		return nil, "", false
	}
	return parts, prerelease, true
}

// comparator is one bound of a range: op is one of >=, >, <, <=, =.
type comparator struct {
	op string
	v  version
}

func (c comparator) test(v version) bool {
	d := v.compare(c.v)
	switch c.op {
	case ">=":
		return d >= 0
	case ">":
		return d > 0
	case "<":
		return d < 0
	case "<=":
		return d <= 0
	}
	return d == 0
}

// Satisfies reports whether a version satisfies an npm version range. It
// covers what workspace manifests use: exact versions, x-ranges, caret, tilde,
// comparisons, hyphen ranges and ||. A range it cannot read is not satisfied,
// so it makes no edge. Pre-release versions compare by plain ordering.
func Satisfies(versionText, rangeText string) bool {
	rangeText = strings.TrimSpace(rangeText)
	if rangeText == "" || rangeText == "*" || rangeText == "x" || rangeText == "X" {
		return true
	}
	v, ok := parseVersion(versionText)
	if !ok {
		return false
	}
	for _, alternative := range strings.Split(rangeText, "||") {
		set, ok := comparatorSet(alternative)
		if !ok {
			continue
		}
		matched := true
		for _, c := range set {
			if !c.test(v) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// comparatorSet desugars one alternative of a range into comparators that all
// must hold.
func comparatorSet(text string) ([]comparator, bool) {
	fields := strings.Fields(text)
	if len(fields) == 3 && fields[1] == "-" {
		low, ok1 := expand(">=", fields[0])
		high, ok2 := expand("<=", fields[2])
		return append(low, high...), ok1 && ok2
	}
	var set []comparator
	for i := 0; i < len(fields); i++ {
		token := fields[i]
		op := operator(token)
		if op == token && i+1 < len(fields) {
			// An operator written apart from its version: ">= 1.2.3".
			i++
			token += fields[i]
		}
		expanded, ok := expand(op, strings.TrimPrefix(token, op))
		if !ok {
			return nil, false
		}
		set = append(set, expanded...)
	}
	return set, len(fields) > 0
}

func operator(token string) string {
	for _, op := range []string{">=", "<=", ">", "<", "=", "^", "~"} {
		if strings.HasPrefix(token, op) {
			return op
		}
	}
	return ""
}

// expand turns one operator and a possibly partial version into comparators.
func expand(op, text string) ([]comparator, bool) {
	parts, prerelease, ok := partial(text)
	if !ok {
		return nil, false
	}
	low := version{prerelease: prerelease}
	for i, n := range parts {
		switch i {
		case 0:
			low.major = n
		case 1:
			low.minor = n
		case 2:
			low.patch = n
		}
	}
	// next is the first version past the given parts: 1.2 -> 1.3.0.
	next := func(n int) version {
		switch n {
		case 1:
			return version{major: low.major + 1}
		case 2:
			return version{major: low.major, minor: low.minor + 1}
		}
		return version{major: low.major, minor: low.minor, patch: low.patch + 1}
	}
	n := len(parts)
	anyVersion := []comparator{{">=", version{}}}
	switch op {
	case "", "=":
		if n == 0 {
			return anyVersion, true
		}
		if n == 3 {
			return []comparator{{"=", low}}, true
		}
		return []comparator{{">=", low}, {"<", next(n)}}, true
	case "^":
		if n == 0 {
			return anyVersion, true
		}
		switch {
		case low.major != 0 || n == 1:
			return []comparator{{">=", low}, {"<", next(1)}}, true
		case low.minor != 0 || n == 2:
			return []comparator{{">=", low}, {"<", next(2)}}, true
		}
		return []comparator{{">=", low}, {"<", next(3)}}, true
	case "~":
		if n == 0 {
			return anyVersion, true
		}
		if n == 1 {
			return []comparator{{">=", low}, {"<", next(1)}}, true
		}
		return []comparator{{">=", low}, {"<", next(2)}}, true
	case ">=":
		return []comparator{{">=", low}}, true
	case "<":
		if n == 0 {
			return []comparator{{"<", version{}}}, true
		}
		return []comparator{{"<", low}}, true
	case ">":
		if n == 0 {
			return []comparator{{"<", version{}}}, true
		}
		if n == 3 {
			return []comparator{{">", low}}, true
		}
		return []comparator{{">=", next(n)}}, true
	case "<=":
		if n == 0 || n == 3 {
			if n == 0 {
				return anyVersion, true
			}
			return []comparator{{"<=", low}}, true
		}
		return []comparator{{"<", next(n)}}, true
	}
	return nil, false
}
