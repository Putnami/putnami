package cli

import (
	"testing"
)

func TestParseFlags_Empty(t *testing.T) {
	flags := ParseFlags(nil)
	if len(flags) != 0 {
		t.Errorf("expected empty flags, got %v", flags)
	}
}

func TestParseFlags_BoolFlags(t *testing.T) {
	flags := ParseFlags([]string{"--verbose", "--debug"})
	if flags["verbose"] != "true" {
		t.Errorf("verbose = %q, want %q", flags["verbose"], "true")
	}
	if flags["debug"] != "true" {
		t.Errorf("debug = %q, want %q", flags["debug"], "true")
	}
}

func TestParseFlags_ValueFlags(t *testing.T) {
	flags := ParseFlags([]string{"--target", "linux/amd64", "--output", "json"})
	if flags["target"] != "linux/amd64" {
		t.Errorf("target = %q, want %q", flags["target"], "linux/amd64")
	}
	if flags["output"] != "json" {
		t.Errorf("output = %q, want %q", flags["output"], "json")
	}
}

func TestParseFlags_EqualsSign(t *testing.T) {
	flags := ParseFlags([]string{"--target=linux/amd64", "--count=5"})
	if flags["target"] != "linux/amd64" {
		t.Errorf("target = %q, want %q", flags["target"], "linux/amd64")
	}
	if flags["count"] != "5" {
		t.Errorf("count = %q, want %q", flags["count"], "5")
	}
}

func TestParseFlags_NoPrefix(t *testing.T) {
	flags := ParseFlags([]string{"--no-cache", "--no-color"})
	if flags["cache"] != "false" {
		t.Errorf("cache = %q, want %q", flags["cache"], "false")
	}
	if flags["color"] != "false" {
		t.Errorf("color = %q, want %q", flags["color"], "false")
	}
}

func TestParseFlags_NonFlagArgsSkipped(t *testing.T) {
	flags := ParseFlags([]string{"positional", "--verbose", "argval"})
	if _, ok := flags["positional"]; ok {
		t.Error("positional arg should not be in flags")
	}
	if flags["verbose"] != "argval" {
		t.Errorf("verbose = %q, want %q", flags["verbose"], "argval")
	}
}

func TestParseFlags_SingleDash(t *testing.T) {
	flags := ParseFlags([]string{"-v"})
	if flags["v"] != "true" {
		t.Errorf("v = %q, want %q", flags["v"], "true")
	}
}

func TestParseFlags_NextArgIsFlag(t *testing.T) {
	flags := ParseFlags([]string{"--verbose", "--debug"})
	if flags["verbose"] != "true" {
		t.Errorf("verbose = %q, want %q", flags["verbose"], "true")
	}
	if flags["debug"] != "true" {
		t.Errorf("debug = %q, want %q", flags["debug"], "true")
	}
}

func TestParseFlags_MultiWordHyphenValue(t *testing.T) {
	flags := ParseFlags([]string{"--args", "--check --dry-run", "--concurrent", "--update-snapshots"})
	if flags["args"] != "--check --dry-run" {
		t.Errorf("args = %q, want %q", flags["args"], "--check --dry-run")
	}
	if flags["concurrent"] != "true" {
		t.Errorf("concurrent = %q, want %q", flags["concurrent"], "true")
	}
	if flags["update-snapshots"] != "true" {
		t.Errorf("update-snapshots = %q, want %q", flags["update-snapshots"], "true")
	}
}

func TestParseFlags_LastFlagBool(t *testing.T) {
	flags := ParseFlags([]string{"--verbose"})
	if flags["verbose"] != "true" {
		t.Errorf("verbose = %q, want %q", flags["verbose"], "true")
	}
}

func TestParseFlags_MixedFlagTypes(t *testing.T) {
	flags := ParseFlags([]string{
		"--verbose",
		"--target", "linux/amd64",
		"--no-color",
		"--count=5",
		"--debug",
	})

	tests := []struct {
		key  string
		want string
	}{
		{"verbose", "true"},
		{"target", "linux/amd64"},
		{"color", "false"},
		{"count", "5"},
		{"debug", "true"},
	}

	for _, tt := range tests {
		if got := flags[tt.key]; got != tt.want {
			t.Errorf("flags[%q] = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestFlagString_Present(t *testing.T) {
	flags := map[string]string{"target": "linux/amd64"}
	if got := FlagString(flags, "target", "default"); got != "linux/amd64" {
		t.Errorf("FlagString = %q, want %q", got, "linux/amd64")
	}
}

func TestFlagString_Missing(t *testing.T) {
	flags := map[string]string{}
	if got := FlagString(flags, "target", "default-val"); got != "default-val" {
		t.Errorf("FlagString = %q, want %q", got, "default-val")
	}
}

func TestFlagString_EmptyValue(t *testing.T) {
	flags := map[string]string{"target": ""}
	if got := FlagString(flags, "target", "default"); got != "" {
		t.Errorf("FlagString = %q, want empty string", got)
	}
}

func TestFlagBool_True(t *testing.T) {
	truthy := []string{"true", "1", "yes", "on"}
	for _, v := range truthy {
		flags := map[string]string{"verbose": v}
		if got := FlagBool(flags, "verbose", false); !got {
			t.Errorf("FlagBool(%q) = false, want true", v)
		}
	}
}

func TestFlagBool_False(t *testing.T) {
	falsy := []string{"false", "0", "no", "off"}
	for _, v := range falsy {
		flags := map[string]string{"verbose": v}
		if got := FlagBool(flags, "verbose", true); got {
			t.Errorf("FlagBool(%q) = true, want false", v)
		}
	}
}

func TestFlagBool_Missing(t *testing.T) {
	flags := map[string]string{}
	if got := FlagBool(flags, "verbose", true); !got {
		t.Error("FlagBool missing key should return default true")
	}
	if got := FlagBool(flags, "verbose", false); got {
		t.Error("FlagBool missing key should return default false")
	}
}

func TestFlagBool_InvalidValue(t *testing.T) {
	flags := map[string]string{"verbose": "maybe"}
	if got := FlagBool(flags, "verbose", true); !got {
		t.Error("FlagBool invalid value should return default true")
	}
	if got := FlagBool(flags, "verbose", false); got {
		t.Error("FlagBool invalid value should return default false")
	}
}

func TestFlagInt_Present(t *testing.T) {
	flags := map[string]string{"count": "42"}
	if got := FlagInt(flags, "count", 0); got != 42 {
		t.Errorf("FlagInt = %d, want 42", got)
	}
}

func TestFlagInt_Missing(t *testing.T) {
	flags := map[string]string{}
	if got := FlagInt(flags, "count", 10); got != 10 {
		t.Errorf("FlagInt missing = %d, want 10", got)
	}
}

func TestFlagInt_Invalid(t *testing.T) {
	flags := map[string]string{"count": "abc"}
	if got := FlagInt(flags, "count", 5); got != 5 {
		t.Errorf("FlagInt invalid = %d, want 5", got)
	}
}

func TestFlagInt_Negative(t *testing.T) {
	flags := map[string]string{"count": "-3"}
	if got := FlagInt(flags, "count", 0); got != -3 {
		t.Errorf("FlagInt negative = %d, want -3", got)
	}
}
