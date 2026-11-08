package stacks

import "testing"

func TestARangeIsSatisfiedByTheReleasesItNames(t *testing.T) {
	cases := []struct {
		rng     string
		machine string
		want    bool
	}{
		{">=8.4", "8.4.10", true},
		{">=8.4", "8.3.6", false},
		{">= 8.4", "8.4.0", true},
		{">8.3", "8.3.0", false},
		{">8.3", "8.3.1", true},
		{"<9", "8.99.1", true},
		{"<=8.3", "8.3.1", false},
		{"!=8.3.1", "8.3.1", false},
		{"<>8.3.1", "8.3.2", true},
		{"^8.2", "8.4.1", true},
		{"^8.2", "9.0.0", false},
		{"^8.2", "8.1.9", false},
		{"^0.3", "0.3.9", true},
		{"^0.3", "0.4.0", false},
		{"^0.0.3", "0.0.4", false},
		{"~8.2", "8.9.0", true},
		{"~8.2", "9.0.0", false},
		{"~8.2.1", "8.2.9", true},
		{"~8.2.1", "8.3.0", false},
		{"~8", "8.4.0", true},
		{"8.3.*", "8.3.6", true},
		{"8.3.*", "8.4.0", false},
		{"8.x", "8.4.0", true},
		{"*", "7.4.0", true},
		{">=8.1 <8.4", "8.3.6", true},
		{">=8.1,<8.4", "8.4.0", false},
		{"^7.4 || ^8.1", "8.3.6", true},
		{"^7.4|^8.1", "8.0.30", false},
		{"8.1 - 8.3", "8.3.9", true},
		{"8.1 - 8.3", "8.4.0", false},
		{"8.1 - 8.3.2", "8.3.3", false},
		{"=8.3", "8.3.6", false},
		{"=8.3", "8.3.0", true},
		{"8.3 || 8.4", "8.4.0", true},
		{"8.3 || 8.4", "8.4.1", false},
		{">=8.4@dev", "8.4.0", true},
		{">=8.4", "8.4.0RC1", true},
		{">=8.4", "PHP", false},
	}
	for _, c := range cases {
		r, ok := parseRange(c.rng)
		if !ok {
			t.Errorf("%q does not parse", c.rng)
			continue
		}
		if got := r.satisfiedBy(c.machine); got != c.want {
			t.Errorf("%q satisfied by %s = %v, want %v", c.rng, c.machine, got, c.want)
		}
	}
	for _, s := range []string{"stable", "nightly-2024-01-01", "", ">=", "^8.*", "8.1 -", "a || b"} {
		if _, ok := parseRange(s); ok {
			t.Errorf("%q parses as a range", s)
		}
	}
}

func TestCompareChecksAStatedRange(t *testing.T) {
	tc := mustProfile(t).Toolchain
	if v := tc.Compare(Expected{Version: ">=8.4", Source: "composer.json"}, "8.4.10"); v.Code != "" || v.Fail {
		t.Errorf("a satisfied range passes with no diagnostic: %+v", v)
	}
	if v := tc.Compare(Expected{Version: ">=8.4", Source: "composer.json"}, "8.3.6"); v.Code != CodeToolchainMismatch || !v.Fail {
		t.Errorf("a version outside the range fails: %+v", v)
	}
	tc.Check = CheckWarn
	if v := tc.Compare(Expected{Version: "^8.4", Source: "composer.json"}, "8.3.6"); v.Code != CodeToolchainMismatch || v.Fail {
		t.Errorf("warn does not fail on a range: %+v", v)
	}
}
