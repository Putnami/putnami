package screens

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// manifestDoc writes a putnami.features.json of acme/home whose requirements
// carry the given stages.
func manifestDoc(stages map[string]string) string {
	var requirements []string
	for _, id := range []string{"greets", "lists-recent-changes"} {
		if stage, ok := stages[id]; ok {
			requirements = append(requirements, `{"id": "`+id+`", "stage": "`+stage+`", "evidenceKinds": ["attestation"]}`)
		}
	}
	return `{"protocolVersion": 2, "namespace": "acme", "features": [{"id": "acme/home", "type": "feature", "name": "Home",
  "outcome": "A person lands", "owner": "web", "target": "coded", "requirements": [` + strings.Join(requirements, ", ") + `]}]}`
}

// asManifest turns a feature document into the putnami.features.json that
// declares the same feature.
func asManifest(document string) string {
	manifest := strings.Replace(strings.TrimSpace(document), `"feature": {`, `"features": [{`, 1)
	return strings.TrimSuffix(manifest, "}") + "]}"
}

// TestTheTouchedRequirementsAreThoseAChangeAddsOrChanges reads both forms of
// feature declaration at the base and the head; a requirement that moves from
// one form to the other unchanged is not touched.
func TestTheTouchedRequirementsAreThoseAChangeAddsOrChanges(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"base:apps/web/putnami.features.json":   manifestDoc(map[string]string{"greets": "coded", "lists-recent-changes": "coded"}),
		"head:apps/web/putnami.features.json":   manifestDoc(map[string]string{"greets": "coded", "lists-recent-changes": "operated"}),
		"head:apps/tasks/features/tasks.json":   featureDoc(t, map[string]string{"lists-open-tasks": "coded"}),
		"base:apps/old/features/old.json":       featureDoc(t, map[string]string{"shows-one-task": "coded"}),
		"base:apps/web/src/features/x.json":     `{"a": 1}`,
		"head:apps/web/src/features/x.json":     `{"a": 2}`,
		"head:apps/web/features/deep/y.json":    featureDoc(t, map[string]string{"filters-by-owner": "coded"}),
		"head:apps/web/specs/home.json":         `{"feature": "acme/home"}`,
		"base:apps/moved/putnami.features.json": asManifest(featureDoc(t, map[string]string{"shows-one-task": "coded"})),
		"head:apps/moved/features/tasks.json":   featureDoc(t, map[string]string{"shows-one-task": "coded"}),
	}
	read := func(commit, file string) ([]byte, bool, error) {
		content, ok := files[commit+":"+file]
		return []byte(content), ok, nil
	}
	changed := []string{"apps/web/putnami.features.json", "apps/tasks/features/tasks.json", "apps/old/features/old.json",
		"apps/web/src/features/x.json", "apps/web/features/deep/y.json", "apps/web/specs/home.json",
		"apps/moved/putnami.features.json", "apps/moved/features/tasks.json"}
	touched, err := TouchedRequirements(changed, "base", "head", read)
	if err != nil {
		t.Fatal(err)
	}
	want := map[Requirement]bool{
		{Feature: "acme/home", Requirement: "lists-recent-changes"}: true,
		{Feature: "acme/tasks", Requirement: "lists-open-tasks"}:    true,
	}
	if !reflect.DeepEqual(touched, want) {
		t.Fatalf("touched = %v, want %v", touched, want)
	}
	failing := func(string, string) ([]byte, bool, error) { return nil, false, errors.New("broken repository") }
	if _, err := TouchedRequirements([]string{"apps/web/putnami.features.json"}, "base", "head", failing); err == nil ||
		!strings.Contains(err.Error(), "broken repository") {
		t.Fatalf("a failed read: %v", err)
	}
	for file, want := range map[string]bool{"putnami.features.json": true, "features/a.json": true, "a/features/b.json": true,
		"a/features/b/c.json": false, "a/features.json": false, "a/features/b.md": false} {
		if FeatureFile(file) != want {
			t.Errorf("FeatureFile(%q) = %v", file, !want)
		}
	}
	spectest.Proves(t, screensFeature, touchedPages, "a-page-proving-a-touched-requirement-is-touched")
}
