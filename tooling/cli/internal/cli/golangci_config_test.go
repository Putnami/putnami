package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestLocalGolangciExclusionsMatchDirectoryComponentsOnly keeps the CLI's
// stricter local config from hiding ordinary packages whose names merely
// contain an excluded output-directory name, packages named build or bin
// (those names hold Go source, never output to skip), and paths that climb out
// of the project, which a symlinked working directory produces.
func TestLocalGolangciExclusionsMatchDirectoryComponentsOnly(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", ".golangci.yml"))
	if err != nil {
		t.Fatalf("read local golangci config: %v", err)
	}
	paths := yamlListUnder(string(data), "    paths:")
	want := []string{
		`^([^/]*[^./][^/]*/)*vendor(/|$)`,
		`^([^/]*[^./][^/]*/)*node_modules(/|$)`,
		`^([^/]*[^./][^/]*/)*dist(/|$)`,
		`^([^/]*[^./][^/]*/)*\.putnami(/|$)`,
	}
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("exclusions.paths = %q, want anchored directory patterns %q", paths, want)
	}

	for _, path := range []string{
		"vendor/dependency.go",
		"nested/node_modules/dependency.go",
		"dist/output.go",
		"nested/.putnami/out/cache.json",
	} {
		if !matchesAny(paths, path) {
			t.Errorf("intended directory path %q is not excluded", path)
		}
	}
	for _, path := range []string{
		"internal/binding/binding.go",
		"pkg/distance/distance.go",
		"pkg/builders/builders.go",
		"pkg/node_moduleship/module.go",
		"cmd/putnami/main.go",
		"pkg/vendorized/vendorized.go",
		"internal/jobs/build/build.go",
		"bin/tool.go",
		"../../home/vendor/workspace/main.go",
	} {
		if matchesAny(paths, path) {
			t.Errorf("ordinary source path %q is unexpectedly excluded", path)
		}
	}
}

func yamlListUnder(contents, key string) []string {
	start := strings.Index(contents, key)
	if start < 0 {
		return nil
	}
	rest, ok := strings.CutPrefix(contents[start:], key)
	if !ok {
		return nil
	}
	var values []string
	for _, line := range strings.Split(rest, "\n")[1:] {
		if strings.HasPrefix(line, "      - ") {
			values = append(values, strings.TrimPrefix(line, "      - "))
			continue
		}
		if strings.TrimSpace(line) != "" {
			break
		}
	}
	return values
}

func matchesAny(patterns []string, path string) bool {
	for _, pattern := range patterns {
		if regexp.MustCompile(pattern).MatchString(path) {
			return true
		}
	}
	return false
}
