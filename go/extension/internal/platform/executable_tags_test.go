package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A declared executable may carry build tags; a tag that is empty or holds a
// comma, a space or a tab is refused, since it would join the -tags list as
// something else.
func TestReadGoExecutablesTags(t *testing.T) {
	write := func(t *testing.T, tags string) string {
		t.Helper()
		root := t.TempDir()
		config := `{"options":{"@putnami/go":{"executables":[{"name":"x","package":"./cmd/x","tags":` + tags + `}]}}}`
		if err := os.WriteFile(filepath.Join(root, "putnami.json"), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
		return root
	}
	got, err := ReadGoExecutables(write(t, `["putnami_runtime_codegen"]`))
	if err != nil || len(got) != 1 || strings.Join(got[0].Tags, ",") != "putnami_runtime_codegen" {
		t.Fatalf("executables = %+v, %v", got, err)
	}
	for _, bad := range []string{`[""]`, `["a,b"]`, `["a b"]`, `["a\tb"]`} {
		if _, err := ReadGoExecutables(write(t, bad)); err == nil {
			t.Errorf("tags %s were accepted", bad)
		}
	}
}
