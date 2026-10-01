package pkg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestTypeScriptRuntimeManifestLocalAndPackagedParity(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "runtime-manifest-parity", "the-local-and-packaged-runtime-manifests-describe-the-same-runtime")
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Runtime struct {
			Executable string `json:"executable"`
			Prepare    any    `json:"prepare"`
		} `json:"runtime"`
		Hooks map[string]struct {
			Command string `json:"command"`
		} `json:"hooks"`
		Tasks map[string]struct {
			Command string `json:"command"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Runtime.Executable != "compiled/putnami-ts" || manifest.Runtime.Prepare == nil {
		t.Fatalf("runtime = %+v, want local prepare targeting packaged executable path", manifest.Runtime)
	}
	for name, task := range manifest.Tasks {
		if task.Command != "{extensionRuntime}" {
			t.Errorf("task %q command = %q, want {extensionRuntime}", name, task.Command)
		}
	}
	for name, hook := range manifest.Hooks {
		if hook.Command != "{extensionRuntime}" {
			t.Errorf("hook %q command = %q, want {extensionRuntime}", name, hook.Command)
		}
	}
}
