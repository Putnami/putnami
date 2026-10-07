package scope

import "testing"

func TestAuthored(t *testing.T) {
	s := &Scope{generated: map[string]bool{"api/gen.go": true}}
	for file, want := range map[string]bool{
		"api/main.go":                             true,
		"api/gen.go":                              false,
		"web/node_modules/x/a.js":                 false,
		"vendor/lib/a.go":                         false,
		"web/dist/app.js":                         false,
		"svc/.gen/types.ts":                       false,
		"pnpm-lock.yaml":                          false,
		"web/app.min.js":                          false,
		"docs/distribution.md":                    true,
		"frontend/.yarn/releases/yarn-1.19.0.cjs": false,
		"frontend/vendors/gantt/codebase/a.js":    false,
		"third_party/proto/any.proto":             false,
		"web/bower_components/x/a.js":             false,
	} {
		if got := s.Authored(file); got != want {
			t.Errorf("Authored(%q) = %v, want %v", file, got, want)
		}
	}
	if InSkippedDir("pnpm-lock.yaml") || !InSkippedDir("a/vendor/b.go") {
		t.Fatal("InSkippedDir must only look at directories")
	}
	specs := ExcludePathspecs()
	if len(specs) != len(skippedDirs) || specs[0] != ":(exclude,glob)**/.gen/**" {
		t.Fatalf("ExcludePathspecs() = %v", specs)
	}
	var none *Scope
	if !none.Authored("a.go") {
		t.Fatal("a nil scope refuses an ordinary file")
	}
}
