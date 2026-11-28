package emit

import (
	"path/filepath"
	"slices"
	"testing"
)

// The build stamp changes on every run, and the scheduler rewrites it after
// each restore, so the manifest leaves it out; every other .gen file stays.
func TestGenFilesLeaveTheBuildStampOut(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".gen", "version.json"), `{"buildTime":"now"}`)
	writeFile(t, filepath.Join(root, ".gen", "generate-result.json"), `{}`)
	writeFile(t, filepath.Join(root, ".gen", "schema", "version.json"), `{}`)
	got := genFiles(root)
	want := []string{".gen/generate-result.json", ".gen/schema/version.json"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("genFiles = %v, want %v", got, want)
	}
}
