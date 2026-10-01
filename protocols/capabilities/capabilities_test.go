package capabilities

import "testing"

// TestCanonicalFilenames pins the emit path constants. The .gen/schema/ prefix
// is load-bearing: the codegen committer only promotes files under .gen/schema/
// into the tracked tree, so an emitter that writes anywhere under the .gen/ root
// instead produces an ephemeral manifest that is never committed or shipped in a
// packaged workload. Mirrors protocols/infra's TestCanonicalFilenames.
func TestCanonicalFilenames(t *testing.T) {
	if ManifestFilename != "capabilities.json" {
		t.Errorf("ManifestFilename = %q, want capabilities.json", ManifestFilename)
	}
	if EmitDir != ".gen/schema" {
		t.Errorf("EmitDir = %q, want .gen/schema (committer promotes only .gen/schema/*)", EmitDir)
	}
	if CommittedPath != "schema/capabilities.json" {
		t.Errorf("CommittedPath = %q, want schema/capabilities.json", CommittedPath)
	}
}
