package ci

import (
	"fmt"
	"strings"
)

// ValidateBranchGlob validates the version 2 slash-aware glob dialect.
func ValidateBranchGlob(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("must not be empty")
	}
	if strings.HasPrefix(pattern, "refs/") {
		return fmt.Errorf("must omit provider prefixes such as refs/heads/")
	}
	if strings.HasPrefix(pattern, "/") || strings.HasSuffix(pattern, "/") || strings.Contains(pattern, "//") {
		return fmt.Errorf("must contain non-empty slash-delimited segments")
	}
	for _, char := range pattern {
		if char < 0x20 || char == 0x7f {
			return fmt.Errorf("must not contain control characters")
		}
	}
	for _, segment := range strings.Split(pattern, "/") {
		if strings.ContainsAny(segment, `?[]{}!\\`) {
			return fmt.Errorf("contains an unsupported glob operator")
		}
		if strings.Contains(segment, "**") && segment != "**" {
			return fmt.Errorf("** is valid only as a complete segment")
		}
	}
	return nil
}

// MatchBranch applies the version 2 full-string, slash-aware glob grammar.
func MatchBranch(pattern, branch string) (bool, error) {
	if err := ValidateBranchGlob(pattern); err != nil {
		return false, err
	}
	if branch == "" || strings.HasPrefix(branch, "refs/") || strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") || strings.Contains(branch, "//") {
		return false, fmt.Errorf("branch must be a normalized provider-independent branch name")
	}
	patternSegments := strings.Split(pattern, "/")
	branchSegments := strings.Split(branch, "/")
	memo := map[[2]int]bool{}
	seen := map[[2]int]bool{}
	var match func(int, int) bool
	match = func(patternIndex, branchIndex int) bool {
		key := [2]int{patternIndex, branchIndex}
		if seen[key] {
			return memo[key]
		}
		seen[key] = true
		var result bool
		switch {
		case patternIndex == len(patternSegments):
			result = branchIndex == len(branchSegments)
		case patternSegments[patternIndex] == "**":
			result = match(patternIndex+1, branchIndex) ||
				(branchIndex < len(branchSegments) && match(patternIndex, branchIndex+1))
		case branchIndex < len(branchSegments):
			result = matchSegment(patternSegments[patternIndex], branchSegments[branchIndex]) &&
				match(patternIndex+1, branchIndex+1)
		}
		memo[key] = result
		return result
	}
	return match(0, 0), nil
}

func matchSegment(pattern, value string) bool {
	valueRunes := []rune(value)
	previous := make([]bool, len(valueRunes)+1)
	previous[0] = true
	for _, char := range pattern {
		current := make([]bool, len(valueRunes)+1)
		if char == '*' {
			current[0] = previous[0]
			for index := 1; index <= len(valueRunes); index++ {
				current[index] = previous[index] || current[index-1]
			}
		} else {
			for index := 1; index <= len(valueRunes); index++ {
				current[index] = previous[index-1] && char == valueRunes[index-1]
			}
		}
		previous = current
	}
	return previous[len(valueRunes)]
}
