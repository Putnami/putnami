package language

import "strings"

// ExcludedLanguagePath matches fixtures and test files where quoted signals
// are expected rather than public prose. The exact source paths of the two
// evaluators, the core's and this package's, are also excluded: their signal
// vocabulary is the detector's own data.
func ExcludedLanguagePath(path string) bool {
	path = strings.ReplaceAll(path, `\`, "/")
	if path == "tooling/cli/internal/commands/shared/language.go" ||
		path == "tooling/contributor/internal/language/evaluate.go" {
		return true
	}
	base := path[strings.LastIndex(path, "/")+1:]
	if base == "english-only.sh" || strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "_test.py") ||
		strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py") ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return true
	}
	for _, part := range strings.Split(path, "/") {
		if part == "testdata" || part == "fixtures" {
			return true
		}
	}
	return false
}
