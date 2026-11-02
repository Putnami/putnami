package language

import "strings"

// ExcludedLanguagePath matches fixtures and test files where quoted signals
// are expected rather than public prose. The exact source path of this
// package's evaluator is also excluded: its signal vocabulary is the
// detector's own data. So is a translation catalog of a locale other than
// English (`locales/fr.json`): its text is that locale's by design, while the
// English catalog and every default message stay in scope.
func ExcludedLanguagePath(path string) bool {
	path = strings.ReplaceAll(path, `\`, "/")
	if path == "tooling/contributor/internal/language/evaluate.go" {
		return true
	}
	base := path[strings.LastIndex(path, "/")+1:]
	if base == "english-only.sh" || strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "_test.py") ||
		strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py") ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return true
	}
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if part == "testdata" || part == "fixtures" {
			return true
		}
	}
	return isForeignCatalog(parts)
}

// isForeignCatalog matches `<project>/locales/<tag>.json` where the language
// subtag of <tag> is not `en`.
func isForeignCatalog(parts []string) bool {
	if len(parts) < 2 || parts[len(parts)-2] != "locales" {
		return false
	}
	tag, ok := strings.CutSuffix(parts[len(parts)-1], ".json")
	if !ok || tag == "" {
		return false
	}
	language, _, _ := strings.Cut(strings.ToLower(tag), "-")
	return language != "en" && len(language) >= 2 && len(language) <= 3
}
