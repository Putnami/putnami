package runcredential

import (
	"strconv"
	"strings"
	"testing"
)

// The help of Flag advertises the custody level, and the level
// a help advertises reads back as the CLI's CustodyLevel. A line that lists
// the flag with no level, as an older build prints it, reads as level 0.
func TestAdvertisedCustodyLevel(t *testing.T) {
	t.Parallel()
	if !strings.HasSuffix(FlagDescription, " custody level "+strconv.Itoa(CustodyLevel)) {
		t.Fatalf("FlagDescription = %q, want it to end with the custody level %d", FlagDescription, CustodyLevel)
	}
	line := func(description string) string {
		return "  " + Flag + " <n>          " + description + "\n"
	}
	for _, tc := range []struct {
		name       string
		help       string
		wantLevel  int
		wantListed bool
	}{
		{name: "this CLI's help", help: "Usage: putnami\n" + line(FlagDescription) + "  --no-cache\n", wantLevel: CustodyLevel, wantListed: true},
		{name: "the flag with no level", help: line("Read the run credential from descriptor <n>; keeps it out of every environment and file"), wantListed: true},
		{name: "a higher level", help: line("Read the credential; custody level 9"), wantLevel: 9, wantListed: true},
		{name: "a level that is not a number", help: line("custody level two"), wantListed: true},
		{name: "a negative level", help: line("custody level -3"), wantListed: true},
		{name: "the words with nothing after them", help: line("custody level"), wantListed: true},
		{name: "a level on another line", help: line("Read the credential") + "  --other  custody level 9\n", wantListed: true},
		{name: "the flag mentioned inside a line", help: "  --other   like " + Flag + "; custody level 9\n"},
		{name: "a longer flag", help: "  " + Flag + "s <n>  custody level 9\n"},
		{name: "no help", help: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			level, listed := AdvertisedCustodyLevel([]byte(tc.help))
			if level != tc.wantLevel || listed != tc.wantListed {
				t.Errorf("AdvertisedCustodyLevel(%q) = %d, %t, want %d, %t", tc.help, level, listed, tc.wantLevel, tc.wantListed)
			}
		})
	}
}
