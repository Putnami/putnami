package extension

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
)

// TestDiscoverExtensionsDetailed_RefusesAVolumeReferenceNamingIt pins how
// discovery treats a Windows reference that names a volume: an absolute path
// that holds a manifest loads, and any other one is refused with a skip that
// names the reference, instead of being joined under the workspace or
// node_modules.
func TestDiscoverExtensionsDetailed_RefusesAVolumeReferenceNamingIt(t *testing.T) {
	ws := t.TempDir()
	present := filepath.Join(t.TempDir(), "present")
	writeExtensionManifest(t, present, fmt.Sprintf(`{"name": "@acme/present", "cliContract": %d}`, protocolcli.CurrentContract))
	missing := filepath.Join(t.TempDir(), "missing")
	driveRelative := filepath.VolumeName(ws) + "ext"

	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{
		present: "", missing: "", driveRelative: "",
	}}}
	result, err := DiscoverExtensionsDetailed(ws, cfg, nil)
	if err != nil {
		t.Fatalf("DiscoverExtensionsDetailed: %v", err)
	}
	if len(result.Extensions) != 1 || result.Extensions[0].Name != "@acme/present" || !result.Extensions[0].LocalSource {
		t.Fatalf("extensions = %+v, want only the absolute path that holds a manifest, as a local source", result.Extensions)
	}
	refused := map[string]string{}
	for _, skip := range result.Skipped {
		refused[skip.Ref] = skip.Reason.Error()
	}
	for _, ref := range []string{missing, driveRelative} {
		reason, ok := refused[ref]
		if !ok || !strings.Contains(reason, fmt.Sprintf("%q names volume %s", ref, filepath.VolumeName(ws))) {
			t.Errorf("skip for %q = %q (present %v), want a refusal that names the reference and its volume", ref, reason, ok)
		}
	}
	if len(result.Skipped) != 2 {
		t.Errorf("skipped = %+v, want exactly the two refused references", result.Skipped)
	}
}
