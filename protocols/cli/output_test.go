package cli

import "testing"

func TestOutputModeIsStructured(t *testing.T) {
	structured := []OutputMode{OutputJSON, OutputJSONL}
	for _, m := range structured {
		if !m.IsStructured() {
			t.Errorf("%q.IsStructured() = false, want true", m)
		}
	}
	plain := []OutputMode{OutputAuto, OutputText, OutputCloudLogging}
	for _, m := range plain {
		if m.IsStructured() {
			t.Errorf("%q.IsStructured() = true, want false", m)
		}
	}
}

func TestResolveOutputMode(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		jsonF   bool
		want    OutputMode
		wantErr bool
	}{
		{"empty auto", "", false, OutputAuto, false},
		{"text", "text", false, OutputText, false},
		{"json", "json", false, OutputJSON, false},
		{"jsonl", "jsonl", false, OutputJSONL, false},
		{"cloud-logging", "cloud-logging", false, OutputCloudLogging, false},
		{"trims spaces", "  jsonl  ", false, OutputJSONL, false},
		{"--json alias", "", true, OutputJSON, false},
		{"--json with --output=json", "json", true, OutputJSON, false},
		{"--json conflicts with jsonl", "jsonl", true, "", true},
		{"--json conflicts with text", "text", true, "", true},
		{"unknown mode", "yaml", false, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveOutputMode(c.raw, c.jsonF)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ResolveOutputMode(%q, %v) = %q, nil; want error", c.raw, c.jsonF, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveOutputMode(%q, %v) unexpected error: %v", c.raw, c.jsonF, err)
			}
			if got != c.want {
				t.Errorf("ResolveOutputMode(%q, %v) = %q, want %q", c.raw, c.jsonF, got, c.want)
			}
		})
	}
}
