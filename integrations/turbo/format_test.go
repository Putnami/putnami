package turbo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// A putnami.json the repository's formatter rewrote keeps its layout: the
// sync compares content, not bytes, so a Biome-formatted file whose content
// is what the sync would write is not rewritten.
func TestTheSyncKeepsAFormattedPutnamiJSONWhoseContentIsEqual(t *testing.T) {
	d := readDemo(t)
	root := repository(t, d)
	writeFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "demo", "options": {"@putnami/turbo": {"mode": "delegate"}}}`)
	if _, err := Sync(root, Options{}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "apps/api", wsproto.ConfigFilename)
	written, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	// Biome's layout: an array that fits on its line is written on one line,
	// with tab indentation.
	var doc map[string]any
	if err := json.Unmarshal(written, &doc); err != nil {
		t.Fatal(err)
	}
	formatted := "{\n" +
		"\t\"$schema\": \"" + ProjectSchema + "\",\n" +
		"\t\"dependencies\": [\"/packages/pricing\"],\n" +
		"\t\"disable\": { \"jobs\": [\"lint\"] },\n" +
		"\t\"extensions\": [\"" + ExtensionName + "\"],\n" +
		"\t\"tags\": [\"turbo\"]\n" +
		"}\n"
	var reformatted map[string]any
	if err := json.Unmarshal([]byte(formatted), &reformatted); err != nil || !reflect.DeepEqual(doc, reformatted) {
		t.Fatalf("the formatted fixture does not hold what the sync wrote:\n%s\nwant the content of\n%s", formatted, written)
	}
	writeFile(t, root, "apps/api/putnami.json", formatted)

	result, err := Sync(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(result.Written, "apps/api/putnami.json") {
		t.Errorf("written = %v, want apps/api/putnami.json current", result.Written)
	}
	if got, _ := os.ReadFile(file); string(got) != formatted {
		t.Errorf("apps/api/putnami.json =\n%s\nwant it kept as formatted", got)
	}
}
