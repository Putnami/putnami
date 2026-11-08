package stacks

import (
	"regexp"
	"strconv"
	"strings"
)

// A versionRange is a version constraint in Composer's syntax, such as
// ">=8.4", "^8.2 || ^9.0" or "8.1 - 8.3": a disjunction of conjunctions of
// bounds. See ADR 0002.
type versionRange [][]bound

// bound is one comparison against a numeric release.
type bound struct {
	op      string
	release []int
}

var (
	rangeOr      = regexp.MustCompile(`\s*\|\|?\s*`)
	rangeHyphen  = regexp.MustCompile(`^(\S+)\s+-\s+(\S+)$`)
	rangeOpSpace = regexp.MustCompile(`(>=|<=|!=|==|<>|>|<|=|\^|~)\s+`)
	rangeAnd     = regexp.MustCompile(`\s*,\s*|\s+`)
	rangeTerm    = regexp.MustCompile(`^(>=|<=|!=|==|<>|>|<|=|\^|~)?v?([0-9]+(?:\.[0-9]+)*)((?:\.[*xX])?)$`)
	rangeRelease = regexp.MustCompile(`^v?([0-9]+(?:\.[0-9]+)*)`)
)

// parseRange reads a version constraint. It reports false for a value that is
// no constraint the core reads, such as a channel name.
func parseRange(s string) (versionRange, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	var out versionRange
	for _, alt := range rangeOr.Split(s, -1) {
		alt = strings.TrimSpace(alt)
		if alt == "" {
			return nil, false
		}
		var conj []bound
		if m := rangeHyphen.FindStringSubmatch(alt); m != nil {
			lo, okLo := rangeNumbers(m[1])
			hi, okHi := rangeNumbers(m[2])
			if !okLo || !okHi {
				return nil, false
			}
			conj = append(conj, bound{">=", lo})
			if len(hi) >= 3 {
				conj = append(conj, bound{"<=", hi})
			} else {
				conj = append(conj, bound{"<", bump(hi, len(hi)-1)})
			}
			out = append(out, conj)
			continue
		}
		alt = rangeOpSpace.ReplaceAllString(alt, "$1")
		for _, term := range rangeAnd.Split(alt, -1) {
			bounds, ok := parseTerm(term)
			if !ok {
				return nil, false
			}
			conj = append(conj, bounds...)
		}
		out = append(out, conj)
	}
	return out, true
}

// parseTerm reads one condition into the bounds it stands for.
func parseTerm(term string) ([]bound, bool) {
	if i := strings.Index(term, "@"); i >= 0 {
		term = term[:i] // a stability flag, such as @dev, is not compared
	}
	if term == "*" || term == "x" || term == "X" {
		return nil, true
	}
	m := rangeTerm.FindStringSubmatch(term)
	if m == nil {
		return nil, false
	}
	op, release, wildcard := m[1], numbers(m[2]), m[3] != ""
	switch {
	case wildcard:
		if op != "" {
			return nil, false
		}
		return []bound{{">=", release}, {"<", bump(release, len(release)-1)}}, true
	case op == "^":
		// The first non-zero component is fixed: ^8.2 is [8.2, 9), ^0.3 is
		// [0.3, 0.4), ^0.0.3 is [0.0.3, 0.0.4).
		i := 0
		for i < len(release)-1 && release[i] == 0 {
			i++
		}
		return []bound{{">=", release}, {"<", bump(release, i)}}, true
	case op == "~":
		// The last given component may grow, the one before it is fixed: ~8.2
		// is [8.2, 9), ~8.2.1 is [8.2.1, 8.3); ~8 is [8, 9).
		i := len(release) - 2
		if i < 0 {
			i = 0
		}
		return []bound{{">=", release}, {"<", bump(release, i)}}, true
	case op == "" || op == "=" || op == "==":
		return []bound{{"=", release}}, true
	case op == "<>":
		return []bound{{"!=", release}}, true
	default:
		return []bound{{op, release}}, true
	}
}

// satisfiedBy reports whether a version's numeric release satisfies the
// range. A version with no numeric release satisfies none.
func (r versionRange) satisfiedBy(version string) bool {
	m := rangeRelease.FindStringSubmatch(strings.TrimSpace(version))
	if m == nil {
		return false
	}
	v := numbers(m[1])
	for _, conj := range r {
		ok := true
		for _, b := range conj {
			if !b.holds(v) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func (b bound) holds(v []int) bool {
	c := compareRelease(v, b.release)
	switch b.op {
	case ">=":
		return c >= 0
	case ">":
		return c > 0
	case "<=":
		return c <= 0
	case "<":
		return c < 0
	case "!=":
		return c != 0
	default:
		// As in Composer, an equal condition is one release, the missing
		// components read as zero: =8.3 holds for 8.3.0, not 8.3.6.
		return c == 0
	}
}

func rangeNumbers(s string) ([]int, bool) {
	m := rangeRelease.FindStringSubmatch(s)
	if m == nil || m[0] != s {
		return nil, false
	}
	return numbers(m[1]), true
}

func numbers(s string) []int {
	parts := strings.Split(s, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

// bump returns the release that follows r at component i, with the later
// components dropped: bump(8.2.1, 1) is 8.3.
func bump(r []int, i int) []int {
	out := append([]int(nil), r[:i+1]...)
	out[i]++
	return out
}

// compareRelease compares two numeric releases, the missing components read
// as zero.
func compareRelease(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
