package clicore

import "testing"

func TestProjectIDFromPathDropsGroupingFolders(t *testing.T) {
	for path, want := range map[string]string{
		"":                       "/",
		".":                      "/",
		"sites/putnami.dev":      "/sites/putnami.dev",
		"sites/(web)/example":    "/sites/example",
		"(apps)/sites/(web)/doc": "/sites/doc",
		"libs/()/one":            "/libs/()/one",
		"libs/(x/one":            "/libs/(x/one",
	} {
		if got := ProjectIDFromPath(path); got != want {
			t.Errorf("ProjectIDFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}
