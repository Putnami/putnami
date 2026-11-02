package language

import "strings"

// ExcludedLanguagePath matches fixtures and test files where quoted signals
// are expected rather than public prose. The exact source path of this
// package's evaluator is also excluded: its signal vocabulary is the
// detector's own data. So is a translation catalog of a locale other than
// English (`locales/fr.json`, or `locales/fr/home.json` with one file per
// namespace): its text is that locale's by design, while the English catalog
// and every default message stay in scope.
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

// isForeignCatalog matches `<project>/locales/<tag>.json` and
// `<project>/locales/<tag>/<namespace path>.json` where the language subtag of
// <tag> is not `en`.
func isForeignCatalog(parts []string) bool {
	last := parts[len(parts)-1]
	if !strings.HasSuffix(last, ".json") {
		return false
	}
	for i := len(parts) - 2; i >= 0; i-- {
		if parts[i] != "locales" {
			continue
		}
		tag := parts[i+1]
		if i+1 == len(parts)-1 {
			tag = strings.TrimSuffix(tag, ".json")
		}
		return isForeignTag(tag)
	}
	return false
}

func isForeignTag(tag string) bool {
	language, _, _ := strings.Cut(strings.ToLower(tag), "-")
	return language != "en" && len(language) >= 2 && len(language) <= 3
}
