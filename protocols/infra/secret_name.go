package infra

import (
	"strings"
	"unicode"
)

// SecretName is the requirement name of a sensitive config field: the name
// "secrets" lists for it. Pass the field's env binding when it has one,
// otherwise its config dot-path.
//
// The resource-name grammar is lowercase-only, so a camelCase boundary
// becomes '_' and the result is lowercased ("crm.apiKey" → "crm.api_key",
// "DB_PASSWORD" → "db_password"). Any other character the grammar rejects
// becomes '_', leading separators are dropped, and the result is cut to 64
// characters. The TypeScript runtime's canonicalSecretName applies the same
// camelCase rule to a config path, so a camelCase field path gets one
// requirement name in either language. Unlike Go producers, the TypeScript
// one never derives a name from an env binding, and it refuses a path the
// grammar rejects instead of rewriting it.
//
// A requirement name is not a config key: the application reads the secret
// at its config dot-path, and that is the key the secret is stored under.
func SecretName(s string) string {
	var split strings.Builder
	var prev rune
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(prev) || unicode.IsDigit(prev)) {
			split.WriteByte('_')
		}
		split.WriteRune(r)
		prev = r
	}

	var b strings.Builder
	for _, r := range strings.ToLower(split.String()) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.', r == '/', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}

	out := strings.TrimLeft(b.String(), "_./-")
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}
