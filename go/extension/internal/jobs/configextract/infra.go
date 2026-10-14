package configextract

import (
	"fmt"
	"sort"
	"strings"

	protocfg "go.putnami.dev/protocol/config"
	diag "go.putnami.dev/protocol/diagnostic"
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
	manifest := infra.PerProjectManifest{ProtocolVersion: infra.ProtocolVersion, Secrets: collectSecretNames(configs)}
	// The generator drops a fragment that fails validation, and every secret
	// of the project with it, so a config key the secret-name grammar rejects
	// fails here and is named.
	if errs := diag.Errors(infra.ValidatePerProjectManifest(&manifest)); len(errs) > 0 {
		messages := make([]string, 0, len(errs))
		for _, d := range errs {
			messages = append(messages, d.String())
		}
		return fmt.Errorf("a sensitive config field has a config key that is not a valid secret name; rename the field or its config path: %s", strings.Join(messages, "; "))
	}
	return infra.WriteSidecar(projectPath, sidecarSlug, manifest)
}

// removeInfraRequirements deletes the project's secrets infra scratch fragment if
// present. A missing file is not an error.
func removeInfraRequirements(projectPath string) error {
	return infra.RemoveSidecar(projectPath, sidecarSlug)
}

// collectSecretNames walks every config block, gathers the config key of each
// sensitive field (nested objects, array elements, and map values included),
// and returns them sorted and deduplicated. A secret is named by the key the
// application reads it at, even when an env binding also sets the field
// (protocols/infra ADR 0004).
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
		emit(path)
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

// joinPath joins a dot-path prefix and a segment, tolerating an empty prefix.
func joinPath(prefix, segment string) string {
	if prefix == "" {
		return segment
	}
	return prefix + "." + segment
}
