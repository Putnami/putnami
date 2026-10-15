package extension

import (
	"reflect"
	"testing"
)

// PublicDeclarers names, per requested command, every loaded extension that
// declares it public, sorted and once. An internal declaration, an unknown
// command and a nil extension name no one.
func TestPublicDeclarersNamesEveryPublicDeclarerOnce(t *testing.T) {
	t.Parallel()
	extensions := []*ExtensionDescription{
		{
			Name:              "@putnami/go",
			Commands:          map[string]string{"build": "", "workspace-sync": ""},
			CommandVisibility: map[string]string{"build": "public", "workspace-sync": "internal"},
		},
		nil,
		{
			Name:              "@acme/rust",
			Commands:          map[string]string{"build": "", "deploy": ""},
			CommandVisibility: map[string]string{"deploy": "public"},
		},
	}

	got := PublicDeclarers([]string{"build", "deploy", "workspace-sync", "unknown", "build"}, extensions)
	want := map[string][]string{
		"build":  {"@acme/rust", "@putnami/go"},
		"deploy": {"@acme/rust"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PublicDeclarers = %v, want %v", got, want)
	}
}
