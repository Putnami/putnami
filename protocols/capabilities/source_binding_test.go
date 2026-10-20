package capabilities

import "testing"

// SourceBindingExcludes answers exactly what ComputeSourceBinding leaves out,
// and its answer for a directory holds for every path below it. A caller that
// skips work for a write confined to an excluded path relies on both.
func TestSourceBindingExcludesMatchesTheBindingAndHoldsBelowADirectory(t *testing.T) {
	base := []SourceBindingFile{{Path: "main.go", Mode: SourceModeRegular, Digest: SourceDigest([]byte("package main\n"))}}
	baseBinding, err := ComputeSourceBinding(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path     string
		excluded bool
	}{
		{".gen", true},
		{".gen/schema/openapi.json", true},
		{"nested/.gen/version.json", true},
		{".git/HEAD", true},
		{"schema/" + ManifestFilename, true},
		{"schema/feature-evidence", true},
		{"schema/feature-evidence/go-framework.json", true},
		{"nested/schema/feature-evidence/a/b.json", true},
		{"schema", false},
		{"schema/openapi.json", false},
		{"feature-evidence/go-framework.json", false},
		{"generated/.genx", false},
		{"main_test.go", false},
	} {
		if got := SourceBindingExcludes(test.path); got != test.excluded {
			t.Errorf("SourceBindingExcludes(%q) = %t, want %t", test.path, got, test.excluded)
		}
		withPath := append([]SourceBindingFile{{Path: test.path, Mode: SourceModeRegular, Digest: SourceDigest([]byte("x"))}}, base...)
		binding, err := ComputeSourceBinding(withPath)
		if err != nil {
			t.Fatalf("bind %q: %v", test.path, err)
		}
		if moved := binding != baseBinding; moved == test.excluded {
			t.Errorf("a file at %q moved the binding: %t, want %t", test.path, moved, !test.excluded)
		}
		if test.excluded && !SourceBindingExcludes(test.path+"/below/file.json") {
			t.Errorf("SourceBindingExcludes(%q) holds but not for a path below it", test.path)
		}
	}
}
