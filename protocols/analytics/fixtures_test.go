package analytics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// userAgentCase is one row of fixtures/user-agents.json. The Go side of the
// corpus checks only that the expected classification stays inside the closed
// enums and that the bot flag agrees with IsBotUserAgent; the classifier itself
// is TypeScript-only (there is no Go web layer), so its test runs the full row.
type userAgentCase struct {
	UA           string `json:"ua"`
	Browser      string `json:"browser"`
	BrowserMajor *int   `json:"browserMajor"`
	OS           string `json:"os"`
	DeviceType   string `json:"deviceType"`
	Bot          bool   `json:"bot"`
}

// referrerCase is one row of fixtures/referrers.json.
type referrerCase struct {
	Referrer string `json:"referrer"`
	Origin   string `json:"origin"`
	Expected struct {
		Referrer     string `json:"referrer"`
		ReferrerType string `json:"referrerType"`
	} `json:"expected"`
}

// TestUserAgentFixtures pins the parity corpus for the user-agent classifier.
// A row whose expected value is outside a closed enum would let the TypeScript
// classifier emit a value the database vocabulary forbids and still pass its own
// conformance run, so membership is checked here, on the owning side.
func TestUserAgentFixtures(t *testing.T) {
	var cases []userAgentCase
	readFixture(t, "user-agents.json", &cases)
	if len(cases) < 25 {
		t.Fatalf("user-agents.json has %d cases, want at least 25", len(cases))
	}

	seen := make(map[string]bool, len(cases))
	bots := 0
	for _, c := range cases {
		if seen[c.UA] {
			t.Errorf("user-agents.json repeats %q", c.UA)
		}
		seen[c.UA] = true

		if !IsBrowser(c.Browser) {
			t.Errorf("%q expects browser %q, which is not in Browsers", c.UA, c.Browser)
		}
		if !IsOS(c.OS) {
			t.Errorf("%q expects os %q, which is not in OperatingSystems", c.UA, c.OS)
		}
		if !IsDeviceType(c.DeviceType) {
			t.Errorf("%q expects deviceType %q, which is not in DeviceTypes", c.UA, c.DeviceType)
		}
		if c.BrowserMajor != nil && *c.BrowserMajor <= 0 {
			t.Errorf("%q expects browserMajor %d, want a positive integer or null", c.UA, *c.BrowserMajor)
		}
		if c.Browser == "other" && c.BrowserMajor != nil {
			t.Errorf("%q classifies as browser other but still claims a major version", c.UA)
		}
		if got := IsBotUserAgent(c.UA); got != c.Bot {
			t.Errorf("IsBotUserAgent(%q) = %t, fixture says %t", c.UA, got, c.Bot)
		}
		if c.Bot {
			bots++
		}
	}
	if bots < 4 {
		t.Errorf("user-agents.json declares %d bots, want at least 4 (the corpus must exercise the filter)", bots)
	}
}

// TestReferrerFixtures pins the parity corpus for referrer classification: every
// expected type is a closed-enum member and every stored referrer already
// respects the storage bound, so the TypeScript side has a truncation case that
// actually truncates.
func TestReferrerFixtures(t *testing.T) {
	var cases []referrerCase
	readFixture(t, "referrers.json", &cases)
	if len(cases) < 12 {
		t.Fatalf("referrers.json has %d cases, want at least 12", len(cases))
	}

	types := make(map[string]int, len(ReferrerTypes))
	truncations := 0
	for _, c := range cases {
		if c.Origin == "" {
			t.Errorf("referrer case %q declares no origin; internal detection compares against it", c.Referrer)
		}
		if !IsReferrerType(c.Expected.ReferrerType) {
			t.Errorf("%q expects referrerType %q, which is not in ReferrerTypes", c.Referrer, c.Expected.ReferrerType)
		}
		if len(c.Expected.Referrer) > MaxReferrerLen {
			t.Errorf("%q expects a stored referrer of %d bytes, over the %d bound",
				c.Referrer, len(c.Expected.Referrer), MaxReferrerLen)
		}
		if strings.ContainsAny(c.Expected.Referrer, "?#") {
			t.Errorf("%q expects a stored referrer that kept its query or fragment: %q", c.Referrer, c.Expected.Referrer)
		}
		if len(c.Referrer) > MaxReferrerLen {
			truncations++
		}
		types[c.Expected.ReferrerType]++
	}
	for _, want := range ReferrerTypes {
		if types[want] == 0 {
			t.Errorf("referrers.json has no case expecting referrerType %q", want)
		}
	}
	if truncations == 0 {
		t.Error("referrers.json has no over-long referrer, so the truncation rule is untested on both sides")
	}
}

// TestBotsFixture pins the shape of the embedded token list. Tokens are matched
// as lowercase substrings, so an uppercase entry would never match anything and
// would silently shrink the filter.
func TestBotsFixture(t *testing.T) {
	var document struct {
		Tokens []string `json:"tokens"`
	}
	if err := json.Unmarshal(BotsJSON, &document); err != nil {
		t.Fatalf("bots.json is not decodable: %v", err)
	}
	if len(document.Tokens) < 30 {
		t.Errorf("bots.json declares %d tokens, want at least 30", len(document.Tokens))
	}

	seen := make(map[string]bool, len(document.Tokens))
	for _, token := range document.Tokens {
		if token == "" {
			t.Error("bots.json declares an empty token, which matches every user agent")
		}
		if token != strings.ToLower(token) {
			t.Errorf("bot token %q is not lowercase, so the substring match can never hit it", token)
		}
		if seen[token] {
			t.Errorf("bots.json repeats %q", token)
		}
		seen[token] = true
	}
}

// TestIsBotUserAgentTreatsAbsenceAsAutomation pins the fail-shut default: a
// real browser always sends a User-Agent, so counting a missing one as human
// would make the cheapest forgery the one that inflates the audience.
func TestIsBotUserAgentTreatsAbsenceAsAutomation(t *testing.T) {
	for _, ua := range []string{"", " ", "\t\n"} {
		if !IsBotUserAgent(ua) {
			t.Errorf("IsBotUserAgent(%q) = false, want true", ua)
		}
	}
	if !IsBotUserAgent("Mozilla/5.0 (compatible; GOOGLEBOT/2.1)") {
		t.Error("IsBotUserAgent must match tokens case-insensitively")
	}
	if IsBotUserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/126.0.0.0 Safari/537.36") {
		t.Error("IsBotUserAgent must not match a plain desktop browser")
	}
}

// readFixture decodes a fixture file into out, rejecting unknown fields so a
// renamed column in the corpus fails here rather than silently decoding to a
// zero value that every assertion then accepts.
func readFixture(t *testing.T, name string, out any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}
