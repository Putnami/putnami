package configextract

import (
	"sort"
	"strings"
	"unicode"

	protocfg "go.putnami.dev/protocol/config"
	"go.putnami.dev/protocol/infra"
)

// sidecarSlug names this producer's per-project infra scratch fragment
// (<project>/.gen/infra/secrets.json).
const sidecarSlug = "secrets"

// writeInfraRequirements derives the project's secret requirements from the
// sensitive fields in its resolved config schema and writes the secrets
// producer's scratch fragment at <project>/.gen/infra/secrets.json. The Go
// generator syncs that fragment into committed infra/requirements.json.
//
// The schema's sensitive markers are the single source of truth for which
// config values are secrets (protocols/config ADR 0001: `sensitive` decides
// the store), so every secret a developer declares becomes an infra requirement without
// being restated. When the schema declares no sensitive fields, any scratch
// fragment left by a prior build is removed so sync never reads stale data.
func writeInfraRequirements(projectPath string, configs []protocfg.Block) error {
	manifest := infra.PerProjectManifest{Secrets: collectSecretNames(configs)}
	return infra.WriteSidecar(projectPath, sidecarSlug, manifest)
}

// removeInfraRequirements deletes the project's secrets infra scratch fragment if
// present. A missing file is not an error.
func removeInfraRequirements(projectPath string) error {
	return infra.RemoveSidecar(projectPath, sidecarSlug)
}

// collectSecretNames walks every config block, gathers the canonical secret
// name of each sensitive field (nested objects, array elements, and map values
// included), and returns them sorted and deduplicated.
func collectSecretNames(blocks []protocfg.Block) []string {
	seen := make(map[string]bool)
	var names []string
	emit := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	for _, b := range blocks {
		for _, f := range b.Fields {
			walkSensitive(b.Path, f, emit)
		}
	}
	sort.Strings(names)
	return names
}

// walkSensitive descends through a field, emitting a secret name for the field
// itself (when marked sensitive) and for any sensitive field nested inside an
// object, array element, or map value. prefix is the dot-path accumulated so
// far; array elements and map values share their container field's path because
// they carry no field name of their own.
func walkSensitive(prefix string, f protocfg.FieldSchema, emit func(string)) {
	path := prefix
	if f.Name != "" {
		path = joinPath(prefix, f.Name)
	}
	if f.Sensitive {
		emit(secretName(f, path))
	}
	switch f.Type {
	case protocfg.FieldTypeObject:
		for _, sub := range f.Fields {
			walkSensitive(path, sub, emit)
		}
	case protocfg.FieldTypeArray:
		if f.Items != nil {
			walkSensitive(path, *f.Items, emit)
		}
	case protocfg.FieldTypeMap:
		if f.Values != nil {
			walkSensitive(path, *f.Values, emit)
		}
	}
}

// secretName picks the canonical identifier for a sensitive field. A field with
// an explicit env binding uses that env-var name (the concrete way the value
// enters a deployed process); otherwise the field's config dot-path is used.
// Either way the result is canonicalized to a valid infra resource name.
func secretName(f protocfg.FieldSchema, path string) string {
	if f.Env != "" {
		return canonicalizeName(f.Env)
	}
	return canonicalizeName(path)
}

// joinPath joins a dot-path prefix and a segment, tolerating an empty prefix.
func joinPath(prefix, segment string) string {
	if prefix == "" {
		return segment
	}
	return prefix + "." + segment
}

// canonicalizeName maps an arbitrary config path or env-var name to the infra
// resource-name vocabulary: lowercase letters, digits, and '-', '_', '.', '/',
// starting with a letter or digit and at most 64 characters
// (^[a-z0-9][a-z0-9_./-]{0,63}$). camelCase and digit→letter boundaries are
// split with '_' so word breaks survive the lowercasing
// (e.g. "clientSecret" → "client_secret", "DB_PASSWORD" → "db_password").
//
// Snake_case matches the TypeScript runtime's canonicalSecretName
// (typescript/framework/runtime/src/config/infra-requirements.ts), so a
// workload that aggregates secrets from both Go and TypeScript projects
// sees the same canonical name for the same source field. Any other
// character that the grammar doesn't accept is rewritten to '_' too.
func canonicalizeName(s string) string {
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
