package cli

import (
	"slices"
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

// A token past the passthrough separator is the user's payload: an inline
// value there is never refused as a switch value.
func TestParseArgs_SwitchValueAfterPassthroughIsNotRefused(t *testing.T) {
	t.Parallel()
	if parsed := ParseArgs([]string{"run", "app", "--", "--watch=no"}, nil, nil); parsed.Err != nil {
		t.Fatal(parsed.Err)
	}
}
