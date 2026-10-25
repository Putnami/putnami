package cliargs

import (
	"errors"
	"slices"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

var testFlags = []Flag{{Long: "--port", ValueName: "<n>"}, {Long: "--no-watch"}}

func TestParse_ReadsPositionalsSwitchesAndValuesInEitherSpelling(t *testing.T) {
	parsed, err := Parse("compose", []string{"/app", "--port", "0", "--no-watch", "-5s", "--", "--port"}, testFlags)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := []string{"/app", "-5s", "--port"}; !slices.Equal(parsed.Positionals, want) {
		t.Errorf("positionals = %q, want %q", parsed.Positionals, want)
	}
	if !parsed.Has("--no-watch") || !parsed.Has("--port") || parsed.Value("--port") != "0" {
		t.Errorf("flags = %+v", parsed)
	}

	inline, err := Parse("compose", []string{"--port=8080", "/app"}, testFlags)
	if err != nil {
		t.Fatalf("Parse inline: %v", err)
	}
	if inline.Value("--port") != "8080" || inline.Has("--no-watch") || !slices.Equal(inline.Positionals, []string{"/app"}) {
		t.Errorf("inline = %+v", inline)
	}
}

func TestParse_RefusesWhatTheCommandDoesNotAccept(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"unknown flag": {
			args: []string{"/app", "--prot", "1"},
			want: "unknown flag --prot for `putnami compose`\n  Accepted: --port, --no-watch\n  Run `putnami help compose` for the accepted flags.",
		},
		"value without a flag value": {
			args: []string{"/app", "--port"},
			want: "flag --port requires a value: --port <n>",
		},
		"value before the separator only": {
			args: []string{"/app", "--port", "--", "1"},
			want: "flag --port requires a value: --port <n>",
		},
		"switch with a value": {
			args: []string{"--no-watch=true"},
			want: "flag --no-watch is a switch: write --no-watch, not --no-watch=true",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse("compose", tc.args, testFlags)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if !errors.Is(err, protocolcli.ErrUsage) {
				t.Errorf("err is not a usage error: %v", err)
			}
		})
	}
	if _, err := Parse("qualify", []string{"--x"}, nil); err == nil || err.Error() != "unknown flag --x for `putnami qualify`\n  Run `putnami help qualify` for the accepted flags." {
		t.Errorf("no accepted flags: %v", err)
	}
}
