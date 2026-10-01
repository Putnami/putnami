package workspace

import "testing"

func TestParseSelector(t *testing.T) {
	cases := []struct {
		raw     string
		kind    SelectorKind
		value   string
		wantErr bool
	}{
		{raw: "tag:go", kind: SelectorTag, value: "go"},
		{raw: "group:ts-all", kind: SelectorGroup, value: "ts-all"},
		{raw: "scope:typescript/framework", kind: SelectorScope, value: "typescript/framework"},
		{raw: "@putnami/cli", kind: SelectorProject, value: "@putnami/cli"},
		{raw: "/tooling/cli", kind: SelectorProject, value: "/tooling/cli"},
		// Whitespace is authoring noise, not part of the value.
		{raw: "  tag:go  ", kind: SelectorTag, value: "go"},
		// An unknown prefix is not a fourth kind: it is a project id.
		{raw: "kind:whatever", kind: SelectorProject, value: "kind:whatever"},
		{raw: "", wantErr: true},
		{raw: "   ", wantErr: true},
		{raw: "tag:", wantErr: true},
		{raw: "group:  ", wantErr: true},
		{raw: "scope:", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			sel, err := ParseSelector(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSelector(%q) = %+v, want an error", tc.raw, sel)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSelector(%q): %v", tc.raw, err)
			}
			if sel.Kind != tc.kind || sel.Value != tc.value {
				t.Fatalf("ParseSelector(%q) = %+v, want {%s %s}", tc.raw, sel, tc.kind, tc.value)
			}
			// The rendered form parses back to the same selector, so a document
			// can round-trip a selection without changing what it selects.
			back, err := ParseSelector(sel.String())
			if err != nil {
				t.Fatalf("re-parse %q: %v", sel.String(), err)
			}
			if back != sel {
				t.Fatalf("round-trip of %q = %+v, want %+v", tc.raw, back, sel)
			}
		})
	}
}
