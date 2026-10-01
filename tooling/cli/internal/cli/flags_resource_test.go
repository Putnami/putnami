package cli

import (
	"strings"
	"testing"
)

// --resource <name>=<units> declares how much of a named scarce resource the
// run has, so the scheduler can admit against it beside the worker count.
// The flag is repeatable, so these tests pin the accumulation as much
// as the parsing.

func TestParseArgs_ResourceBudgets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want map[string]int
	}{
		{
			name: "space-separated pair",
			args: []string{"test", "--resource", "db-connections=400"},
			want: map[string]int{"db-connections": 400},
		},
		{
			// splitInlineValues cuts at the FIRST "=", so the pair survives intact.
			name: "inline pair",
			args: []string{"test", "--resource=db-connections=400"},
			want: map[string]int{"db-connections": 400},
		},
		{
			name: "repeated for several resources",
			args: []string{"test", "--resource", "db-connections=400", "--resource", "ports=64"},
			want: map[string]int{"db-connections": 400, "ports": 64},
		},
		{
			name: "repeated name is last-wins",
			args: []string{"test", "--resource", "db-connections=400", "--resource", "db-connections=520"},
			want: map[string]int{"db-connections": 520},
		},
		{
			// A zero budget is well-formed: it says the resource exists with no
			// capacity, and plan-time validation is what names the task it refuses.
			name: "zero budget parses",
			args: []string{"test", "--resource", "db-connections=0"},
			want: map[string]int{"db-connections": 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed := ParseArgs(tt.args, nil, nil)
			if parsed.Err != nil {
				t.Fatalf("ParseArgs(%v) = %v, want no error", tt.args, parsed.Err)
			}
			got := parsed.Global.ResourceBudgets
			if len(got) != len(tt.want) {
				t.Fatalf("resource budgets = %v, want %v", got, tt.want)
			}
			for name, units := range tt.want {
				if got[name] != units {
					t.Errorf("budget %q = %d, want %d", name, got[name], units)
				}
			}
			if len(parsed.RawJobArgs) != 0 {
				t.Errorf("RawJobArgs = %v, want the flag fully consumed", parsed.RawJobArgs)
			}
		})
	}
}

// TestParseArgs_RejectsMalformedResourceBudgets keeps a mistyped budget a hard
// usage error. Silently dropping it is a failure mode removed from
// --retry and --max-parallel: the caller asks for a limit and gets none.
func TestParseArgs_RejectsMalformedResourceBudgets(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"test", "--resource"},                        // no value at all
		{"test", "--resource", "db-connections"},      // no "="
		{"test", "--resource", "=400"},                // no name
		{"test", "--resource", "db-connections="},     // no units
		{"test", "--resource", "db-connections=lots"}, // non-numeric units
		{"test", "--resource", "db-connections=-1"},   // negative budget
	} {
		if parsed := ParseArgs(args, nil, nil); parsed.Err == nil {
			t.Errorf("ParseArgs(%v) should be a usage error", args)
		}
	}
}

// TestApplyResourceBudget_MessageNamesTheExpectedSpelling keeps the diagnostic
// actionable: a caller who typed the wrong shape needs the right one.
func TestApplyResourceBudget_MessageNamesTheExpectedSpelling(t *testing.T) {
	t.Parallel()
	err := applyResourceBudget(&GlobalFlags{}, "db-connections")
	if err == nil {
		t.Fatal("a value with no '=' should be a usage error")
	}
	if !strings.Contains(err.Error(), "<name>=<units>") {
		t.Errorf("message %q does not show the expected spelling", err.Error())
	}
}
