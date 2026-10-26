package areas

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestSkipped(t *testing.T) {
	spectest.Proves(t, "intelligence/repository-areas", "ignored-files", "ignores-build-output")
	spectest.Proves(t, "intelligence/repository-areas", "ignored-files", "ignores-lockfiles")
	spectest.Proves(t, "intelligence/repository-areas", "ignored-files", "ignores-minified-bundles")
	spectest.Proves(t, "intelligence/repository-areas", "ignored-files", "ignores-vendored-dependencies")
	spectest.Proves(t, "intelligence/repository-areas", "ignored-files", "keeps-files-whose-names-only-resemble-excluded-ones")
	for file, want := range map[string]bool{
		"api/main.go":                             false,
		"web/node_modules/x/a.js":                 true,
		"vendor/lib/a.go":                         true,
		"web/dist/app.js":                         true,
		"svc/.gen/types.ts":                       true,
		"pnpm-lock.yaml":                          true,
		"web/app.min.js":                          true,
		"web/app.min.css":                         true,
		"docs/distribution.md":                    false,
		"frontend/.yarn/releases/yarn-1.19.0.cjs": true,
		"frontend/vendors/gantt/codebase/a.js":    true,
		"third_party/proto/any.proto":             true,
		"web/bower_components/x/a.js":             true,
		"vendor":                                  false,
	} {
		if got := skipped(file); got != want {
			t.Errorf("skipped(%q) = %v, want %v", file, got, want)
		}
	}
}
