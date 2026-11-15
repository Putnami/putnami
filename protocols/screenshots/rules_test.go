package screenshots

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const (
	testBase = "1f0e2d3c4b5a69788796a5b4c3d2e1f001234567"
	testHead = "89abcdef0123456789abcdef0123456789abcdef"
)

// manifestOf builds a valid manifest of n pages both sides hold.
func manifestOf(n int) Manifest {
	manifest := Manifest{ProtocolVersion: ProtocolVersion, Project: "apps/web", BaseSHA: testBase, HeadSHA: testHead}
	for i := 0; i < n; i++ {
		page := Page{File: fmt.Sprintf("src/app/p%d/page.tsx", i), Route: fmt.Sprintf("/p%d", i), Path: fmt.Sprintf("/p%d", i), Before: true, After: true}
		manifest.Pages = append(manifest.Pages, page)
		for _, side := range Sides() {
			for _, viewport := range Viewports() {
				for _, scheme := range Schemes() {
					manifest.Captures = append(manifest.Captures, Capture{
						File:  fmt.Sprintf("%03d-%s-%s-%s.png", i, side, viewport.Name, scheme),
						Route: page.Route, Side: side, Viewport: viewport.Name, Scheme: scheme,
						SHA256: strings.Repeat("ab", 32), Status: 200, URL: "http://127.0.0.1:4100" + page.Path,
					})
				}
			}
		}
	}
	return manifest
}

// codes returns the codes of the findings, in order.
func codes(diags []diag.Diagnostic) []string {
	var got []string
	for _, finding := range diags {
		got = append(got, finding.Code)
	}
	return got
}

// assertRules validates each edited copy of a one-page manifest and expects
// exactly the one code its rule owns.
func assertRules(t *testing.T, rules map[string]func(*Manifest), want map[string]string) {
	t.Helper()
	for name, edit := range rules {
		t.Run(name, func(t *testing.T) {
			manifest := manifestOf(1)
			edit(&manifest)
			if got := codes(Validate(&manifest)); !reflect.DeepEqual(got, []string{want[name]}) {
				t.Fatalf("codes = %v, want [%s]", got, want[name])
			}
		})
	}
}

func TestAnUnknownDuplicateOrMissingMemberIsRefused(t *testing.T) {
	spectest.Proves(t, feature, "a-manifest-parses-strictly", "an-unknown-duplicate-or-missing-member-is-refused")
	cases := []struct{ fixture, code, field string }{
		{"unknown-member.json", ErrorCodeUnknownField, "generatedAt"},
		{"unknown-capture-member.json", ErrorCodeUnknownField, "width"},
		{"duplicate-member.json", ErrorCodeDuplicateField, "project"},
		{"missing-head-sha.json", ErrorCodeMissingField, "headSHA"},
		{"missing-page-after.json", ErrorCodeMissingField, "pages[0].after"},
		{"missing-capture-url.json", ErrorCodeMissingField, "captures[0].url"},
		{"null-captures.json", ErrorCodeMissingField, "captures"},
		{"trailing-data.json", ErrorCodeParseError, ""},
		{"not-json.json", ErrorCodeParseError, ""},
		{"status-as-text.json", ErrorCodeParseError, ""},
	}
	for _, c := range cases {
		manifest, diags := Parse(readFixture(t, "invalid", c.fixture))
		if manifest != nil || len(diags) != 1 || diags[0].Code != c.code || diags[0].Field != c.field {
			t.Errorf("%s: %v, want one %s at %q", c.fixture, diags, c.code, c.field)
		}
	}
	nested := []byte(`{"pages": [{"file": "a", "file": "b"}], "captures": [1, {"url": "a", "url": "b"}]}`)
	if _, diags := Parse(nested); len(diags) != 1 || diags[0].Field != "pages[0].file" {
		t.Errorf("a member listed twice in a page: %v", diags)
	}
	for _, text := range []string{`{"pages": [`, `{"pages"`, `{"pages": {"a": 1,`, ``} {
		if _, diags := Parse([]byte(text)); !hasCode(diags, ErrorCodeParseError) {
			t.Errorf("%q: %v, want a parse error", text, diags)
		}
	}
	if diags := Validate(nil); !hasCode(diags, ErrorCodeParseError) {
		t.Errorf("a nil manifest: %v", diags)
	}
}

func TestEachEnvelopeRuleHasAStableCode(t *testing.T) {
	spectest.Proves(t, feature, "a-manifest-parses-strictly", "each-envelope-rule-has-a-stable-code")
	want := map[string]string{
		"another version":          ErrorCodeInvalidProtocolVersion,
		"an absolute project":      ErrorCodeInvalidProject,
		"an escaping project":      ErrorCodeInvalidProject,
		"a backslash project":      ErrorCodeInvalidProject,
		"an empty project":         ErrorCodeInvalidProject,
		"a long project":           ErrorCodeTextTooLong,
		"an abbreviated base":      ErrorCodeInvalidSHA,
		"an uppercase head":        ErrorCodeInvalidSHA,
		"a head of another length": ErrorCodeInvalidSHA,
	}
	assertRules(t, map[string]func(*Manifest){
		"another version":          func(m *Manifest) { m.ProtocolVersion = 2 },
		"an absolute project":      func(m *Manifest) { m.Project = "/apps/web" },
		"an escaping project":      func(m *Manifest) { m.Project = "../web" },
		"a backslash project":      func(m *Manifest) { m.Project = `apps\web` },
		"an empty project":         func(m *Manifest) { m.Project = "" },
		"a long project":           func(m *Manifest) { m.Project = strings.Repeat("a", MaxTextBytes+1) },
		"an abbreviated base":      func(m *Manifest) { m.BaseSHA = testBase[:7] },
		"an uppercase head":        func(m *Manifest) { m.HeadSHA = strings.ToUpper(testHead) },
		"a head of another length": func(m *Manifest) { m.HeadSHA = testHead + "00" },
	}, want)
	for _, project := range []string{".", "web", "apps/web"} {
		manifest := manifestOf(1)
		manifest.Project = project
		if diags := Validate(&manifest); len(diags) > 0 {
			t.Errorf("the project %q: %v", project, diags)
		}
	}
	manifest := manifestOf(1)
	manifest.BaseSHA, manifest.HeadSHA = testBase+strings.Repeat("0", 24), testHead+strings.Repeat("f", 24)
	if diags := Validate(&manifest); len(diags) > 0 {
		t.Errorf("sha256 commits: %v", diags)
	}
}

func TestEachPageRuleHasAStableCode(t *testing.T) {
	spectest.Proves(t, feature, "a-page-is-well-formed", "each-page-rule-has-a-stable-code")
	want := map[string]string{
		"an absolute file":      ErrorCodeInvalidPageFile,
		"an escaping file":      ErrorCodeInvalidPageFile,
		"an empty file":         ErrorCodeInvalidPageFile,
		"a path without slash":  ErrorCodeInvalidPath,
		"a long path":           ErrorCodeTextTooLong,
		"a page on no side":     ErrorCodeInvalidPageSides,
		"a route listed twice":  ErrorCodeDuplicateRoute,
		"a route without slash": ErrorCodeInvalidRoute,
	}
	assertRules(t, map[string]func(*Manifest){
		"an absolute file":     func(m *Manifest) { m.Pages[0].File = "/src/page.tsx" },
		"an escaping file":     func(m *Manifest) { m.Pages[0].File = "../page.tsx" },
		"an empty file":        func(m *Manifest) { m.Pages[0].File = "" },
		"a path without slash": func(m *Manifest) { m.Pages[0].Path = "p0" },
		"a long path":          func(m *Manifest) { m.Pages[0].Path = "/" + strings.Repeat("a", MaxTextBytes) },
		"a page on no side": func(m *Manifest) {
			m.Pages[0].Before, m.Pages[0].After = false, false
			m.Captures = nil
		},
		"a route listed twice": func(m *Manifest) { m.Pages = append(m.Pages, m.Pages[0]) },
		"a route without slash": func(m *Manifest) {
			m.Pages[0].Route = "p0"
			for i := range m.Captures {
				m.Captures[i].Route = "p0"
			}
		},
	}, want)
}

func TestEachCaptureRuleHasAStableCode(t *testing.T) {
	spectest.Proves(t, feature, "a-capture-is-well-formed", "each-capture-rule-has-a-stable-code")
	want := map[string]string{
		"a file in a directory":      ErrorCodeInvalidCaptureFile,
		"a windows directory":        ErrorCodeInvalidCaptureFile,
		"a file that is no png":      ErrorCodeInvalidCaptureFile,
		"a bare extension":           ErrorCodeInvalidCaptureFile,
		"a long file name":           ErrorCodeInvalidCaptureFile,
		"a file listed twice":        ErrorCodeDuplicateCaptureFile,
		"a viewport outside the set": ErrorCodeInvalidViewport,
		"a scheme outside the set":   ErrorCodeInvalidScheme,
		"an uppercase sha256":        ErrorCodeInvalidSHA256,
		"a status below 100":         ErrorCodeInvalidStatus,
		"a status above 599":         ErrorCodeInvalidStatus,
		"a relative url":             ErrorCodeInvalidURL,
		"a url without host":         ErrorCodeInvalidURL,
		"a url that does not parse":  ErrorCodeInvalidURL,
		"a file url":                 ErrorCodeInvalidURL,
		"a long url":                 ErrorCodeTextTooLong,
	}
	assertRules(t, map[string]func(*Manifest){
		"a file in a directory": func(m *Manifest) { m.Captures[0].File = "shots/a.png" },
		"a windows directory":   func(m *Manifest) { m.Captures[0].File = `shots\a.png` },
		"a file that is no png": func(m *Manifest) { m.Captures[0].File = "a.jpg" },
		"a bare extension":      func(m *Manifest) { m.Captures[0].File = ".png" },
		"a long file name":      func(m *Manifest) { m.Captures[0].File = strings.Repeat("a", MaxFileNameBytes) + ".png" },
		"a file listed twice":   func(m *Manifest) { m.Captures[1].File = m.Captures[0].File },
		"a viewport outside the set": func(m *Manifest) {
			m.Captures[0].Viewport = "tablet"
			m.Captures = append(m.Captures, m.Captures[0])
			m.Captures[8].Viewport, m.Captures[8].File = ViewportDesktop, "again.png"
		},
		"a scheme outside the set": func(m *Manifest) {
			m.Captures[0].Scheme = "sepia"
			m.Captures = append(m.Captures, m.Captures[0])
			m.Captures[8].Scheme, m.Captures[8].File = SchemeLight, "again.png"
		},
		"an uppercase sha256":       func(m *Manifest) { m.Captures[0].SHA256 = strings.Repeat("AB", 32) },
		"a status below 100":        func(m *Manifest) { m.Captures[0].Status = 99 },
		"a status above 599":        func(m *Manifest) { m.Captures[0].Status = 600 },
		"a relative url":            func(m *Manifest) { m.Captures[0].URL = "/p0" },
		"a url without host":        func(m *Manifest) { m.Captures[0].URL = "http:///p0" },
		"a url that does not parse": func(m *Manifest) { m.Captures[0].URL = "http://127.0.0.1:port/" },
		"a file url":                func(m *Manifest) { m.Captures[0].URL = "file:///srv/index.html" },
		"a long url":                func(m *Manifest) { m.Captures[0].URL = "http://127.0.0.1/" + strings.Repeat("a", MaxTextBytes) },
	}, want)
	for status := range map[int]bool{100: true, 200: true, 304: true, 404: true, 599: true} {
		manifest := manifestOf(1)
		manifest.Captures[0].Status = status
		if diags := Validate(&manifest); len(diags) > 0 {
			t.Errorf("the status %d: %v", status, diags)
		}
	}
}

func TestAPageHoldsOneCapturePerSideViewportAndScheme(t *testing.T) {
	spectest.Proves(t, feature, "the-captures-cover-the-pages", "a-page-holds-one-capture-per-side-viewport-and-scheme")
	both := manifestOf(1)
	if len(both.Captures) != 8 || len(Validate(&both)) != 0 {
		t.Fatalf("a page both sides hold: %d captures, %v", len(both.Captures), Validate(&both))
	}
	after := manifestOf(1)
	after.Pages[0].Before = false
	after.Captures = after.Captures[4:]
	if diags := Validate(&after); len(diags) != 0 {
		t.Fatalf("a page the after side alone holds, with four captures: %v", diags)
	}
	// Each of the eight captures is needed: without it, the manifest names
	// the page and the capture it lacks.
	for i := range both.Captures {
		manifest := manifestOf(1)
		missing := manifest.Captures[i]
		manifest.Captures = append(manifest.Captures[:i:i], manifest.Captures[i+1:]...)
		diags := Validate(&manifest)
		want := fmt.Sprintf("/p0 has no %s %s %s capture", missing.Side, missing.Viewport, missing.Scheme)
		if len(diags) != 1 || diags[0].Code != ErrorCodeMissingCapture || diags[0].Field != "pages[0]" || diags[0].Message != want {
			t.Errorf("without capture %d: %v, want %q", i, diags, want)
		}
	}
	repeated := manifestOf(1)
	again := repeated.Captures[0]
	again.File = "again.png"
	repeated.Captures = append(repeated.Captures, again)
	if got := codes(Validate(&repeated)); !reflect.DeepEqual(got, []string{ErrorCodeDuplicateCapture}) {
		t.Errorf("a capture taken twice: %v", got)
	}
}

func TestACaptureNamesAPageAndASideThatHoldsIt(t *testing.T) {
	spectest.Proves(t, feature, "the-captures-cover-the-pages", "a-capture-names-a-page-and-a-side-that-holds-it")
	assertRules(t, map[string]func(*Manifest){
		"a route of no page": func(m *Manifest) {
			extra := m.Captures[0]
			extra.Route, extra.File = "/other", "other.png"
			m.Captures = append(m.Captures, extra)
		},
		"a side that does not hold the page": func(m *Manifest) {
			m.Pages[0].Before = false
			m.Captures = m.Captures[3:]
		},
		"a side outside the set": func(m *Manifest) {
			extra := m.Captures[0]
			extra.Side, extra.File = "during", "during.png"
			m.Captures = append(m.Captures, extra)
		},
		"a long route": func(m *Manifest) {
			extra := m.Captures[0]
			extra.Route, extra.File = "/"+strings.Repeat("a", MaxTextBytes), "long.png"
			m.Captures = append(m.Captures, extra)
		},
	}, map[string]string{
		"a route of no page":                 ErrorCodeUnknownRoute,
		"a side that does not hold the page": ErrorCodeUnexpectedSide,
		"a side outside the set":             ErrorCodeInvalidSide,
		"a long route":                       ErrorCodeTextTooLong,
	})
}

func TestAManifestPastABoundIsRefused(t *testing.T) {
	spectest.Proves(t, feature, "the-bounds-are-published", "a-manifest-past-a-bound-is-refused")
	if MaxCaptures != MaxPages*len(Sides())*len(Viewports())*len(Schemes()) {
		t.Fatalf("MaxCaptures = %d, not the captures of %d pages", MaxCaptures, MaxPages)
	}
	largest := manifestOf(MaxPages)
	if diags := Validate(&largest); len(diags) != 0 {
		t.Fatalf("a manifest of %d pages and %d captures: %v", MaxPages, len(largest.Captures), diags[0])
	}
	pages := manifestOf(MaxPages + 1)
	if got := codes(Validate(&pages)); !reflect.DeepEqual(got, []string{ErrorCodeTooManyPages}) {
		t.Errorf("%d pages: %v", MaxPages+1, got)
	}
	captures := manifestOf(MaxPages)
	captures.Captures = append(captures.Captures, captures.Captures[0])
	if got := codes(Validate(&captures)); !reflect.DeepEqual(got, []string{ErrorCodeTooManyCaptures}) {
		t.Errorf("%d captures: %v", MaxCaptures+1, got)
	}
	data, err := Marshal(manifestOf(1))
	if err != nil {
		t.Fatal(err)
	}
	padded := slices.Concat(data, bytes.Repeat([]byte(" "), MaxManifestBytes+1-len(data)))
	if _, diags := Decode(padded); !reflect.DeepEqual(codes(diags), []string{ErrorCodeTooLarge}) {
		t.Errorf("a manifest of %d bytes: %v", len(padded), diags)
	}
	if _, diags := Decode(padded[:MaxManifestBytes]); len(diags) != 0 {
		t.Errorf("a manifest of %d bytes: %v", MaxManifestBytes, diags)
	}
	if MaxManifestBytes != 4<<20 || MaxCaptureBytes != 64<<20 || MaxPages != 512 || MaxTextBytes != 2048 || MaxFileNameBytes != 255 {
		t.Error("a bound moved: a bound is part of the contract, and moving one needs a new protocolVersion")
	}
}

func TestMarshalRefusesAManifestPastTheByteBound(t *testing.T) {
	spectest.Proves(t, feature, "the-bounds-are-published", "marshal-refuses-a-manifest-past-the-byte-bound")
	manifest := manifestOf(MaxPages)
	for i := range manifest.Captures {
		manifest.Captures[i].URL = "http://127.0.0.1/" + strings.Repeat("a", MaxTextBytes-len("http://127.0.0.1/"))
	}
	if diags := Validate(&manifest); len(diags) != 0 {
		t.Fatalf("the manifest is valid but for its size: %v", diags[0])
	}
	if _, err := Marshal(manifest); err == nil || !strings.Contains(err.Error(), "more than the 4194304") {
		t.Fatalf("Marshal = %v, want a refusal that names the byte bound", err)
	}
}

func TestMarshalRefusesAnInvalidManifest(t *testing.T) {
	spectest.Proves(t, feature, "a-written-manifest-reads-back", "marshal-refuses-an-invalid-manifest")
	manifest := manifestOf(1)
	manifest.HeadSHA = "head"
	manifest.Captures = manifest.Captures[1:]
	data, err := Marshal(manifest)
	if data != nil || err == nil {
		t.Fatal("Marshal wrote an invalid manifest")
	}
	for _, code := range []string{ErrorCodeInvalidSHA, ErrorCodeMissingCapture} {
		if !strings.Contains(err.Error(), code) {
			t.Errorf("the refusal %q does not name %s", err, code)
		}
	}
}

func TestAMarshaledManifestParsesToTheSameValue(t *testing.T) {
	spectest.Proves(t, feature, "a-written-manifest-reads-back", "a-marshaled-manifest-parses-to-the-same-value")
	for _, manifest := range []Manifest{manifestOf(2), manifestOf(0)} {
		data, err := Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(data, []byte("{\n  \"protocolVersion\": 1,\n")) || !bytes.HasSuffix(data, []byte("\n}\n")) || bytes.HasSuffix(data, []byte("\n\n")) {
			t.Errorf("the manifest is not indented JSON with one trailing newline:\n%s", data)
		}
		got, diags := Decode(data)
		if len(diags) != 0 {
			t.Fatalf("the marshaled manifest does not decode: %v", diags)
		}
		want := manifest
		if want.Pages == nil {
			want.Pages, want.Captures = []Page{}, []Capture{}
		}
		if !reflect.DeepEqual(*got, want) {
			t.Errorf("the manifest read back differs:\n got %+v\nwant %+v", *got, want)
		}
		again, err := Marshal(*got)
		if err != nil || !bytes.Equal(again, data) {
			t.Errorf("a second Marshal differs: %v", err)
		}
	}
	// Every valid fixture survives a round trip unchanged in value.
	for _, name := range loadExpectations(t).Valid {
		first, diags := Decode(readFixture(t, "valid", name))
		if len(diags) != 0 {
			t.Fatal(diags)
		}
		data, err := Marshal(*first)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if second, _ := Decode(data); second == nil || !reflect.DeepEqual(first, second) {
			t.Errorf("%s does not survive a round trip", name)
		}
	}
}
