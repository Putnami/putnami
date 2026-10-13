package cli

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// `serve --watch=false` turned watch ON and left a stray "false"
// token. It now turns serve's own watch flag off, as --no-watch does.
func TestParseArgs_WatchFalseIsNoWatch(t *testing.T) {
	t.Parallel()
	for _, spelling := range []string{"--watch=false", "--no-watch"} {
		parsed := ParseArgs([]string{"serve", "--projects", "app", spelling}, nil, nil)
		if parsed.Err != nil {
			t.Fatalf("%s: %v", spelling, parsed.Err)
		}
		if parsed.Global.Watch {
			t.Errorf("%s turned the global watch on", spelling)
		}
		if params := buildCommandParams(parsed.RawJobArgs); len(params) != 1 || params["watch"] != false {
			t.Errorf("%s: params = %v, want watch=false", spelling, params)
		}
	}
}

func TestParseArgs_SwitchTakesAnInlineBoolean(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args    []string
		verbose bool
		watch   bool
	}{
		{args: []string{"build", "--verbose=true"}, verbose: true},
		{args: []string{"build", "--verbose=false"}},
		{args: []string{"serve", "--watch=true"}, watch: true},
	} {
		parsed := ParseArgs(tc.args, nil, nil)
		if parsed.Err != nil {
			t.Fatalf("%v: %v", tc.args, parsed.Err)
		}
		if parsed.Global.Verbose != tc.verbose || parsed.Global.Watch != tc.watch {
			t.Errorf("%v: verbose=%v watch=%v, want %v %v", tc.args, parsed.Global.Verbose, parsed.Global.Watch, tc.verbose, tc.watch)
		}
		if len(parsed.RawJobArgs) != 0 {
			t.Errorf("%v left tokens behind: %v", tc.args, parsed.RawJobArgs)
		}
	}
}

// compose watches its target by default too, and reads its own --no-watch.
func TestParseArgs_ComposeWatchFalseIsNoWatch(t *testing.T) {
	t.Parallel()
	parsed := ParseArgs([]string{"compose", "app", "--watch=false"}, nil, nil)
	if parsed.Err != nil || parsed.Global.Watch || !slices.Contains(parsed.RawJobArgs, "--no-watch") {
		t.Errorf("watch=%v raw=%v err=%v, want --no-watch", parsed.Global.Watch, parsed.RawJobArgs, parsed.Err)
	}
}

// Only serve and compose declare a `watch` flag: elsewhere `--watch=false` leaves watch
// off and hands the job no undeclared --no-watch.
func TestParseArgs_WatchFalseOutsideServeOnlyLeavesWatchOff(t *testing.T) {
	t.Parallel()
	parsed := ParseArgs([]string{"test", "--projects", "app", "--watch=false"}, nil, nil)
	if parsed.Err != nil || parsed.Global.Watch || len(parsed.RawJobArgs) != 0 {
		t.Errorf("watch=%v raw=%v err=%v, want watch off and no tokens", parsed.Global.Watch, parsed.RawJobArgs, parsed.Err)
	}
}

// `--color=false` is `--no-color`, and the reverse: the pair are one switch.
func TestParseArgs_FalseOnAPairedSwitchIsItsOtherHalf(t *testing.T) {
	t.Parallel()
	for spelling, want := range map[string]bool{"--color=false": false, "--no-color=false": true} {
		parsed := ParseArgs([]string{"build", spelling}, nil, nil)
		if parsed.Err != nil || parsed.Global.Color == nil || *parsed.Global.Color != want {
			t.Errorf("%s: color=%v err=%v, want %v", spelling, parsed.Global.Color, parsed.Err, want)
		}
	}
}

func TestParseArgs_SwitchRefusesAnyOtherInlineValue(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"serve", "--watch=no"},
		{"build", "--verbose=1"},
		{"build", "--no-cache="},
	} {
		if parsed := ParseArgs(args, nil, nil); parsed.Err == nil {
			t.Errorf("ParseArgs(%v) should be a usage error", args)
		}
	}
}

// A token past the passthrough separator is the user's payload: it sets no
// global flag, an inline value there is never refused as a switch value, and
// the job receives it as spelled.
func TestParseArgs_PassthroughTokensReachTheJob(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"run", "app", "--", "--verbose"}, []string{"--", "--verbose"}},
		{[]string{"run", "app", "--", "--watch=no"}, []string{"--", "--watch=no"}},
		{[]string{"run", "app", "--", "--help", "-v"}, []string{"--", "--help", "-v"}},
		{[]string{"run", "app", "--", "--projects", "x", "--output=json"}, []string{"--", "--projects", "x", "--output=json"}},
		{[]string{"run", "app", "--verbose", "--", "--quiet"}, []string{"--", "--quiet"}},
		{[]string{"run", "app", "--", "--", "--verbose"}, []string{"--", "--", "--verbose"}},
	}
	for _, tc := range cases {
		parsed := ParseArgs(tc.args, nil, nil)
		if parsed.Err != nil {
			t.Fatalf("ParseArgs(%q) = %v", tc.args, parsed.Err)
		}
		if !reflect.DeepEqual(parsed.RawJobArgs, tc.want) {
			t.Errorf("ParseArgs(%q).RawJobArgs = %q, want %q", tc.args, parsed.RawJobArgs, tc.want)
		}
		g := parsed.Global
		if g.Watch || g.Help || g.Quiet || g.Output != "" || g.Projects != "app" {
			t.Errorf("ParseArgs(%q) bound a payload token: %+v", tc.args, g)
		}
	}
	if parsed := ParseArgs([]string{"run", "app", "--", "--verbose"}, nil, nil); parsed.Global.Verbose {
		t.Error("--verbose past the separator turned verbose on")
	}
	if parsed := ParseArgs([]string{"run", "app", "--verbose", "--", "x"}, nil, nil); !parsed.Global.Verbose {
		t.Error("--verbose before the separator left verbose off")
	}
}

// A value-taking global flag never takes the separator as its value.
func TestParseArgs_ValueFlagBeforePassthroughHasNoValue(t *testing.T) {
	t.Parallel()
	parsed := ParseArgs([]string{"build", "--projects", "--", "app"}, nil, nil)
	if parsed.Err == nil || !strings.Contains(parsed.Err.Error(), "flag --projects requires a value") {
		t.Errorf("ParseArgs = %v, want a usage error naming --projects", parsed.Err)
	}
}
