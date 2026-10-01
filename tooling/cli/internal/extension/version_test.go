package extension

import (
	"testing"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		input   string
		want    Version
		wantErr bool
	}{
		{"1.0.0", Version{1, 0, 0, ""}, false},
		{"2.3.4", Version{2, 3, 4, ""}, false},
		{"0.0.1", Version{0, 0, 1, ""}, false},
		{"v1.2.3", Version{1, 2, 3, ""}, false},
		{"1.0.0-alpha.1", Version{1, 0, 0, "alpha.1"}, false},
		{"1.0.0-beta", Version{1, 0, 0, "beta"}, false},
		{"1.0", Version{1, 0, 0, ""}, false},
		{"1", Version{1, 0, 0, ""}, false},
		{"", Version{}, true},
		{"abc", Version{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseVersion(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseVersion(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseVersion(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestVersionCompare(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "2.0.0", -1},
		{"2.0.0", "1.0.0", 1},
		{"1.0.0", "1.1.0", -1},
		{"1.1.0", "1.0.0", 1},
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0-alpha", "1.0.0", -1}, // prerelease < release
		{"1.0.0", "1.0.0-alpha", 1},  // release > prerelease
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha.2", -1},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_vs_"+tt.b, func(t *testing.T) {
			va, _ := ParseVersion(tt.a)
			vb, _ := ParseVersion(tt.b)
			got := va.Compare(vb)
			if got != tt.want {
				t.Errorf("Compare(%s, %s) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestParseConstraint(t *testing.T) {
	tests := []struct {
		constraint string
		version    string
		want       bool
	}{
		// Caret
		{"^1.0.0", "1.0.0", true},
		{"^1.0.0", "1.9.9", true},
		{"^1.0.0", "2.0.0", false},
		{"^1.2.3", "1.2.3", true},
		{"^1.2.3", "1.3.0", true},
		{"^1.2.3", "1.2.2", false},
		{"^0.2.3", "0.2.5", true},
		{"^0.2.3", "0.3.0", false},
		{"^0.0.3", "0.0.3", true},
		{"^0.0.3", "0.0.4", false},

		// Tilde
		{"~1.2.0", "1.2.0", true},
		{"~1.2.0", "1.2.9", true},
		{"~1.2.0", "1.3.0", false},

		// Exact
		{"1.2.3", "1.2.3", true},
		{"1.2.3", "1.2.4", false},

		// Ranges
		{">=1.0.0", "1.0.0", true},
		{">=1.0.0", "0.9.9", false},
		{">1.0.0", "1.0.1", true},
		{">1.0.0", "1.0.0", false},
		{">=1.0.0 <2.0.0", "1.5.0", true},
		{">=1.0.0 <2.0.0", "2.0.0", false},

		// Latest/wildcard
		{"latest", "1.0.0", true},
		{"latest", "99.0.0", true},
		{"*", "1.0.0", true},
		{"", "1.0.0", true},
	}

	for _, tt := range tests {
		t.Run(tt.constraint+"_matches_"+tt.version, func(t *testing.T) {
			c, err := ParseConstraint(tt.constraint)
			if err != nil {
				t.Fatalf("ParseConstraint(%q) error: %v", tt.constraint, err)
			}
			v, err := ParseVersion(tt.version)
			if err != nil {
				t.Fatalf("ParseVersion(%q) error: %v", tt.version, err)
			}
			got := c.Match(v)
			if got != tt.want {
				t.Errorf("Constraint(%q).Match(%q) = %v, want %v", tt.constraint, tt.version, got, tt.want)
			}
		})
	}
}

func TestSortVersions(t *testing.T) {
	versions := []string{"2.0.0", "1.0.0", "1.5.0", "0.1.0", "1.0.0-alpha"}
	SortVersions(versions)

	want := []string{"0.1.0", "1.0.0-alpha", "1.0.0", "1.5.0", "2.0.0"}
	for i, v := range versions {
		if v != want[i] {
			t.Errorf("SortVersions[%d] = %q, want %q", i, v, want[i])
		}
	}
}
