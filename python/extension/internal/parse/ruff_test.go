package parse

import (
	"go.putnami.dev/protocol/features/spectest"

	"testing"
)

func TestRuffFormatIssues(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   int
	}{
		{
			name:   "empty output",
			output: "",
			want:   0,
		},
		{
			name:   "one file to reformat",
			output: "Would reformat: src/main.py\n1 file would be reformatted",
			want:   1,
		},
		{
			name:   "multiple files to reformat",
			output: "Would reformat: src/a.py\nWould reformat: src/b.py\nWould reformat: src/c.py\n3 files would be reformatted",
			want:   3,
		},
		{
			name:   "already formatted",
			output: "1 file left unchanged",
			want:   1,
		},
		{
			name:   "no files matched",
			output: "No Python files found under the given path(s)",
			want:   0,
		},
		{
			name:   "reformatted with extra context",
			output: "Reformatted src/main.py\n2 files reformatted, 3 files left unchanged",
			want:   2,
		},
		{
			name:   "line with file count and leading whitespace",
			output: "  5 files would be reformatted",
			want:   5,
		},
		{
			name:   "output without file count pattern",
			output: "Some output without matching pattern",
			want:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RuffFormatIssues(tt.output)
			if got != tt.want {
				t.Errorf("RuffFormatIssues(%q) = %d, want %d", tt.output, got, tt.want)
			}
		})
	}
}

func TestRuffCheckIssues(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   int
	}{
		{
			name:   "empty output",
			output: "",
			want:   0,
		},
		{
			name:   "no issues",
			output: "All checks passed!",
			want:   0,
		},
		{
			name:   "single issue line",
			output: "src/main.py:10:5: E501 Line too long",
			want:   1,
		},
		{
			name:   "multiple issue lines",
			output: "src/main.py:10:5: E501 Line too long\nsrc/utils.py:3:1: F401 Unused import\nsrc/utils.py:7:8: E302 Expected 2 blank lines",
			want:   3,
		},
		{
			name:   "found N error fallback",
			output: "Found 42 errors.",
			want:   42,
		},
		{
			name:   "issue lines take priority over Found N error",
			output: "src/main.py:1:1: E501 issue\nFound 100 errors.",
			want:   1,
		},
		{
			name:   "issue line format with deeply nested path",
			output: "src/deeply/nested/module/file.py:100:20: W291 trailing whitespace",
			want:   1,
		},
		{
			name:   "found 0 errors",
			output: "Found 0 errors.",
			want:   0,
		},
		{
			name:   "non-issue lines mixed with issue lines",
			output: "Checking src/main.py\nsrc/main.py:5:1: E302 Expected 2 blank lines\nDone.",
			want:   1,
		},
		{
			name:   "line without column does not match issue pattern",
			output: "src/main.py:10: some message without column",
			want:   0,
		},
		{
			name:   "windows-style path separators are not issues",
			output: "some text without colon-digit-colon pattern",
			want:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RuffCheckIssues(tt.output)
			if got != tt.want {
				t.Errorf("RuffCheckIssues(%q) = %d, want %d", tt.output, got, tt.want)
			}
		})
	}
}

func TestRuffCheckJSONFindings(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "native-tools", "ruff-findings-are-parsed-into-a-structured-report")
	stdout := `[
		{"code":"F401","filename":"/ws/pkg_a/src/main.py","message":"unused import","severity":"error","location":{"row":1,"column":8},"end_location":{"row":1,"column":10}},
		{"code":null,"filename":"pkg_b/src/main.py","message":"SyntaxError","location":{"row":3,"column":1}}
	]`
	findings, ok := RuffCheckJSONFindings(stdout)
	if !ok {
		t.Fatal("expected JSON parse to succeed")
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %d, want 2", len(findings))
	}
	if findings[0].File != "/ws/pkg_a/src/main.py" || findings[0].Code != "F401" ||
		findings[0].Line != 1 || findings[0].Column != 8 || findings[0].Severity != "error" {
		t.Fatalf("finding[0] = %+v", findings[0])
	}
	// Null code and missing severity fall back gracefully.
	if findings[1].Code != "" || findings[1].Severity != "error" || findings[1].Line != 3 {
		t.Fatalf("finding[1] = %+v", findings[1])
	}
}

func TestRuffCheckJSONFindings_NotJSON(t *testing.T) {
	for _, in := range []string{"", "All checks passed!", "panic: boom", "{not-an-array}"} {
		if findings, ok := RuffCheckJSONFindings(in); ok || findings != nil {
			t.Fatalf("RuffCheckJSONFindings(%q) = (%v, %v), want (nil, false)", in, findings, ok)
		}
	}
}

func TestRuffCheckJSONFindings_Empty(t *testing.T) {
	findings, ok := RuffCheckJSONFindings("[]")
	if !ok {
		t.Fatal("empty array must parse as valid JSON")
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %d, want 0", len(findings))
	}
}

func TestRuffFormatFiles(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   []string
	}{
		{"empty", "", nil},
		{"single", "Would reformat: pkg_a/src/main.py\n1 file would be reformatted", []string{"pkg_a/src/main.py"}},
		{
			"multiple with noise",
			"Installed 1 package in 2ms\nWould reformat: pkg_a/src/main.py\nWould reformat: pkg_b/src/main.py\n2 files would be reformatted",
			[]string{"pkg_a/src/main.py", "pkg_b/src/main.py"},
		},
		{"already formatted", "2 files left unchanged", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RuffFormatFiles(tt.output)
			if len(got) != len(tt.want) {
				t.Fatalf("RuffFormatFiles(%q) = %v, want %v", tt.output, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("RuffFormatFiles(%q)[%d] = %q, want %q", tt.output, i, got[i], tt.want[i])
				}
			}
		})
	}
}
