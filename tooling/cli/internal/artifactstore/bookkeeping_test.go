package artifactstore

import (
	"os"
	"testing"
	"time"
)

func TestIsBookkeepingNamesTheSidecarAndItsTemporaryFiles(t *testing.T) {
	for name, want := range map[string]bool{
		"lastused":              true,
		"lastused-1234567890":   true,
		".lastused.4242":        true,
		"lastused.txt":          false,
		"lastusedx":             false,
		"putnami.template.json": false,
		"main.go":               false,
		".lastused":             false,
	} {
		if got := IsBookkeeping(name); got != want {
			t.Errorf("IsBookkeeping(%q) = %v, want %v", name, got, want)
		}
	}
}

// The sidecar the store's writer leaves in an entry is one IsBookkeeping
// reports.
func TestIsBookkeepingCoversWhatStampUsedWrites(t *testing.T) {
	dir := t.TempDir()
	stampUsed(dir, time.Unix(1, 0))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !IsBookkeeping(entries[0].Name()) {
		t.Fatalf("stampUsed left %v", entries)
	}
}
