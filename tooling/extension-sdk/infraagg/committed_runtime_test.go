package infraagg

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/infra"
)

// committedRuntimeRelPath is the workload-relative location sourceRuntime reads
// authored runtime intent from, built from the same constants so a rename moves
// the guard with it.
const committedRuntimeRelPath = infra.PerProjectManifestDir + "/" + RuntimeManifestFilename

// Every committed <workload>/infra/runtime.json in the repository must parse
// under the infra protocol this SDK ships against.
//
// Nothing else checks them. A runtime.json is human-authored, carries no
// protocolVersion, and is read by readRuntimeFile with DisallowUnknownFields —
// so one key the Go types no longer know kills the whole file. sourceRuntime
// then hands Aggregate a nil runtime and the workload's ingress domains,
// scaling ceilings, and platform-auth posture vanish from the emitted manifest,
// or the manifest is removed outright when the runtime block was the workload's
// only declaration. Job reports all of that as a warning and still returns OK
// (see its findings-are-warnings contract), so the gate stays green and the
// loss surfaces at deploy time instead.
//
// Infra protocol v2 removed runtime.scaling.min while both site workloads still
// declared it. That is the drift this guard exists to catch, and it is why the
// guard parses with the production reader rather than a copy of it.
func TestCommittedRuntimeManifestsParseUnderTheCurrentProtocol(t *testing.T) {
	root := workspaceRoot(t)

	var checked []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			// .gen holds the framework-written defaults sidecar, which is an
			// artifact of the code under test rather than authored intent.
			case ".git", ".gen", ".putnami", "node_modules", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if !strings.HasSuffix(filepath.ToSlash(rel), "/"+committedRuntimeRelPath) {
			return nil
		}
		checked = append(checked, filepath.ToSlash(rel))
		if _, diags := readRuntimeFile(path); diag.HasErrors(diags) {
			t.Errorf("%s does not parse under the current protocol: %v", rel, diags)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// A corpus that silently emptied — a moved directory, a broadened skip
	// list — would turn this guard into a rubber stamp.
	if len(checked) == 0 {
		t.Fatalf("no committed %s found under %s; the guard would pass vacuously",
			committedRuntimeRelPath, root)
	}
	t.Logf("checked %d committed runtime manifests: %s", len(checked), strings.Join(checked, ", "))
}

// workspaceRoot walks up from the test's working directory to the directory
// holding putnami.workspace.json. The SDK is published and consumed out of
// tree, where no such root exists and there is no corpus to guard; the test
// skips there rather than failing on its absence.
func workspaceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "putnami.workspace.json")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("no putnami.workspace.json above the test directory: not an in-repo run")
		}
		dir = parent
	}
}
