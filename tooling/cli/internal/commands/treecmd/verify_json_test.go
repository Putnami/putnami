package treecmd

// These tests pin the JavaScript semantics the verifier reads evidence with and
// the bytes it writes. The expected values were produced by Node.js 24 and
// Bun 1.4, which agree on every one of them.

import (
	"bytes"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestStringifyMatchesJSONStringify(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, source, compact, indented string
	}{
		{
			name:     "array index keys come first, ascending, then insertion order",
			source:   `{"b":1,"a":[],"c":{},"2":"x","10":"y","1":true,"-1":null,"01":0}`,
			compact:  `{"1":true,"2":"x","10":"y","b":1,"a":[],"c":{},"-1":null,"01":0}`,
			indented: "{\n  \"1\": true,\n  \"2\": \"x\",\n  \"10\": \"y\",\n  \"b\": 1,\n  \"a\": [],\n  \"c\": {},\n  \"-1\": null,\n  \"01\": 0\n}",
		},
		{
			name:    "numbers print as Number.prototype.toString, infinities as null",
			source:  `[1e21,1e-7,0.1,-0,123456789012345680000,5e-324,1.5,100,-2.5e-10,1.7976931348623157e308,1e400,-1e400,123e-20,0.000001]`,
			compact: `[1e+21,1e-7,0.1,0,123456789012345680000,5e-324,1.5,100,-2.5e-10,1.7976931348623157e+308,null,null,1.23e-18,0.000001]`,
		},
		{
			name:    "strings escape control characters and lone surrogates only",
			source:  `"\u0000\u001f\u007f<>&\u2028\u2029\b\f\n\r\t\"\\\/\ud800x\udc00\ud83d\ude00\u00e9"`,
			compact: "\"\\u0000\\u001f\x7f<>&\u2028\u2029\\b\\f\\n\\r\\t\\\"\\\\/\\ud800x\\udc00\U0001F600\u00e9\"",
		},
		{
			name:     "nested containers indent by level",
			source:   `{"nested":{"list":[{"k":[1,[2,[]]]},{}],"s":"v"}}`,
			compact:  `{"nested":{"list":[{"k":[1,[2,[]]]},{}],"s":"v"}}`,
			indented: "{\n  \"nested\": {\n    \"list\": [\n      {\n        \"k\": [\n          1,\n          [\n            2,\n            []\n          ]\n        ]\n      },\n      {}\n    ],\n    \"s\": \"v\"\n  }\n}",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			value, err := parseJSON([]byte(tc.source))
			if err != nil {
				t.Fatalf("parseJSON: %v", err)
			}
			if got := stringify(value, ""); got != tc.compact {
				t.Errorf("compact = %q, want %q", got, tc.compact)
			}
			if tc.indented != "" {
				if got := stringify(value, "  "); got != tc.indented {
					t.Errorf("indented = %q, want %q", got, tc.indented)
				}
			}
		})
	}
}

func TestStringifyOmitsUndefinedAndReplacesInvalidBytes(t *testing.T) {
	t.Parallel()
	value := obj([]string{"kept", "absent", "list", "bytes"}, 1.0, nil, []any{nil, math.NaN(), math.Inf(1)}, "a\xffb")
	if got, want := stringify(value, ""), "{\"kept\":1,\"list\":[null,null,null],\"bytes\":\"a\ufffdb\"}"; got != want {
		t.Fatalf("stringify = %q, want %q", got, want)
	}
}

func TestParseJSONRejectsWhatJSONParseRejects(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		"", " ", "01", "1.", ".5", "+1", "-", "1e", "NaN", "Infinity", "'a'", "tru", "nul",
		`{"a":1,}`, `[1,]`, `{a:1}`, `{"a" 1}`, "\u00a0{}", "\"a\tb\"", `"\x"`, `"\u12"`, `"abc`, `{} {}`,
		`{"a":1,"a":2`,
	} {
		if _, err := parseJSON([]byte(source)); err == nil {
			t.Errorf("parseJSON(%q) accepted invalid JSON", source)
		} else if err.Error() == "duplicate JSON member" {
			t.Errorf("parseJSON(%q) reported a duplicate before the syntax error", source)
		}
	}
}

func TestParseJSONRejectsDuplicateMembersAfterParsing(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "local-evidence-verification",
		"evidence-is-read-strictly-against-the-git-top-level")
	t.Parallel()
	for _, source := range []string{
		`{"version":2,"version":2}`,
		`{"outer":{"b":1,"b":2}}`,
		`[{"ok":1},{"a":1,"\u0061":2}]`,
	} {
		if _, err := parseJSON([]byte(source)); err == nil || err.Error() != "duplicate JSON member" {
			t.Errorf("parseJSON(%s) = %v, want duplicate JSON member", source, err)
		}
	}
	if _, err := parseJSON([]byte(`[{"a":1},{"a":1}]`)); err != nil {
		t.Errorf("one member name in two objects: %v", err)
	}
}

func TestParseJSONDecodesLikeAFatalTextDecoder(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "local-evidence-verification",
		"evidence-is-read-strictly-against-the-git-top-level")
	t.Parallel()
	value, err := parseJSON([]byte("\xef\xbb\xbf{\"a\":\"\\ud83d\\ude00\"}"))
	if err != nil {
		t.Fatalf("a leading byte order mark is dropped: %v", err)
	}
	if got := stringify(value, ""); got != "{\"a\":\"\U0001F600\"}" {
		t.Errorf("an escaped surrogate pair is one code point: %q", got)
	}
	if _, err := parseJSON([]byte("{\"a\":\"\xff\"}")); err == nil || err.Error() != invalidUTF8 {
		t.Errorf("invalid UTF-8 = %v, want %q", err, invalidUTF8)
	}
	if _, err := parseJSON([]byte(" \xef\xbb\xbf{}")); err == nil {
		t.Error("a byte order mark after the start is not white space")
	}
}

func TestParseJSONBoundsSizeAndDepth(t *testing.T) {
	t.Parallel()
	if _, err := parseJSON(bytes.Repeat([]byte(" "), maxEvidenceBytes+1)); err == nil || err.Error() != "record exceeds 32 MiB" {
		t.Errorf("oversized document = %v, want record exceeds 32 MiB", err)
	}
	deep := strings.Repeat("[", maxJSONDepth+1) + strings.Repeat("]", maxJSONDepth+1)
	if _, err := parseJSON([]byte(deep)); err == nil {
		t.Error("a document nested past the depth bound parsed")
	}
	fits := strings.Repeat("[", maxJSONDepth) + strings.Repeat("]", maxJSONDepth)
	if _, err := parseJSON([]byte(fits)); err != nil {
		t.Errorf("a document at the depth bound: %v", err)
	}
}

func TestJavaScriptHelpers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ input, want string }{
		{"\ufeff x \u0085", "x \u0085"},
		{"\u00a0\u1680\u2000\u200a\u2028\u2029\u202f\u205f\u3000\ufeffy\v\f", "y"},
		{"\u180ez\u200b", "\u180ez\u200b"},
	} {
		if got := jsTrim(tc.input); got != tc.want {
			t.Errorf("jsTrim(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
	loneSurrogate, err := parseJSON([]byte(`"\ud800"`))
	if err != nil {
		t.Fatal(err)
	}
	for input, want := range map[string]int{"\U0001F600": 2, "\u00e9": 1, loneSurrogate.(string): 1} {
		if got := utf16Length(input); got != want {
			t.Errorf("utf16Length(%q) = %d, want %d", input, got, want)
		}
	}
	if got := jsToString([]any{1.0, []any{2.0, jsNull{}}, nil, "a"}); got != "1,2,,,a" {
		t.Errorf("jsToString(array) = %q", got)
	}
	for _, tc := range []struct {
		list []any
		want bool
	}{
		{[]any{math.NaN(), math.NaN()}, true},
		{[]any{0.0, math.Copysign(0, -1)}, true},
		{[]any{nil, nil}, true},
		{[]any{"1", 1.0}, false},
		{[]any{[]any{}, []any{}}, false},
	} {
		if got := hasDuplicate(tc.list); got != tc.want {
			t.Errorf("hasDuplicate(%v) = %v, want %v", tc.list, got, tc.want)
		}
	}
	for value, want := range map[any]bool{float64(1<<53 - 1): true, float64(1 << 53): false, 1.5: false, "1": false} {
		if got := isSafeInteger(value); got != want {
			t.Errorf("isSafeInteger(%v) = %v, want %v", value, got, want)
		}
	}
}

// TestVerifierArgumentErrors pins the reasons the verifier gives before it
// reads Git, so none of these needs a repository.
func TestVerifierArgumentErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args   []string
		reason string
	}{
		{nil, chooseOneMode},
		{[]string{"--record"}, "missing --record value"},
		{[]string{"--record", "--base"}, "missing --record value"},
		{[]string{"--record", "  "}, "missing --record value"},
		{[]string{"--snapshot", "--snapshot"}, chooseOneMode},
		{[]string{"--ref", "a", "--gate", "b"}, chooseOneMode},
		{[]string{"--gate", "s"}, "--report goes with --gate, and --gate needs it"},
		{[]string{"--snapshot", "--report", "r"}, "--report goes with --gate, and --gate needs it"},
		{[]string{"--report"}, "missing --report value"},
		{[]string{"--base", "--x"}, "missing --base value"},
		{[]string{"--snapshot", "extra"}, "unknown argument: extra"},
		{[]string{"--snapshot"}, "git rev-parse --show-toplevel failed (128)"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			err := Verifier{Dir: t.TempDir(), Stdout: &stdout}.Run(tc.args)
			if !errors.Is(err, ErrNotVerified) {
				t.Fatalf("Run = %v, want ErrNotVerified", err)
			}
			want := `{"verdict":"not-verified","reason":"` + tc.reason + "\"}\n"
			if stdout.String() != want {
				t.Fatalf("stdout = %q, want %q", stdout.String(), want)
			}
		})
	}
}

func TestVerifierHelpWinsOverEveryOtherArgument(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	if err := (Verifier{Dir: t.TempDir(), Stdout: &stdout}).Run([]string{"--record", "--help", "--unknown"}); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if stdout.String() != VerifyUsage {
		t.Fatalf("stdout = %q, want the usage line", stdout.String())
	}
}

// verifyJSONRepo is a committed repository with AGENTS.md and f.txt, and an
// empty sub directory.
func verifyJSONRepo(t *testing.T) (root, head string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = root
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("config", "user.name", "Verifier Test")
	run("config", "user.email", "verifier@example.invalid")
	run("config", "commit.gpgSign", "false")
	run("config", "core.hooksPath", filepath.Join(root, ".git", "no-hooks"))
	for name, content := range map[string]string{"AGENTS.md": "# Agents\n", "f.txt": "hello\n", "sub/.keep": ""} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-q", "-m", "initial")
	return root, run("rev-parse", "HEAD")
}

// TestVerifierResolvesPathsAgainstTheTopLevel runs from a sub directory: the
// path on the command line names a file of the top level, and the output
// keeps the path as given.
func TestVerifierResolvesPathsAgainstTheTopLevel(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "local-evidence-verification",
		"evidence-is-read-strictly-against-the-git-top-level")
	t.Parallel()
	root, _ := verifyJSONRepo(t)
	calls := 0
	var stdout bytes.Buffer
	verifier := Verifier{Dir: filepath.Join(root, "sub"), Stdout: &stdout,
		Fingerprint: func(string) (string, error) { calls++; return strings.Repeat("a", 64), nil }}
	if err := verifier.Run([]string{"--ref", "f.txt"}); err != nil {
		t.Fatalf("Run: %v: %s", err, stdout.String())
	}
	want := "{\n  \"path\": \"f.txt\",\n  \"sha256\": \"5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03\"\n}\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if calls != 0 {
		t.Fatalf("--ref read the fingerprint %d times, want 0", calls)
	}
}

func TestVerifierSnapshotBytes(t *testing.T) {
	t.Parallel()
	root, head := verifyJSONRepo(t)
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	var stdout bytes.Buffer
	verifier := Verifier{Dir: root, Stdout: &stdout,
		Fingerprint: func(string) (string, error) { calls++; return strings.Repeat("a", 64), nil }}
	if err := verifier.Run([]string{"--snapshot", "--base", "HEAD"}); err != nil {
		t.Fatalf("Run: %v: %s", err, stdout.String())
	}
	want := "{\n  \"version\": 2,\n  \"binding\": {\n    \"fingerprint\": \"" + strings.Repeat("a", 64) + "\",\n" +
		"    \"headSHA\": \"" + head + "\",\n    \"baseSHA\": \"" + head + "\"\n  },\n" +
		"  \"changedFiles\": [\n    \"new.txt\"\n  ],\n" +
		"  \"policies\": [\n    {\n      \"path\": \"AGENTS.md\",\n" +
		"      \"sha256\": \"070a8ff8c31696dd57f1d0f8dfbfb1c9151ebd903c5d7e41d9dbf4133d54f84e\"\n    }\n  ],\n" +
		"  \"objective\": null,\n  \"scope\": null,\n  \"implementers\": [],\n  \"scopes\": [],\n" +
		"  \"review\": null,\n  \"gate\": null,\n" +
		"  \"qualification\": {\n    \"required\": [],\n    \"results\": [],\n    \"notApplicable\": null\n  },\n" +
		"  \"acceptance\": []\n}\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if calls != 1 {
		t.Fatalf("--snapshot read the fingerprint %d times, want 1", calls)
	}
}

// TestVerifierFailsClosedOnMalformedEvidence feeds dossiers JavaScript would
// throw on: each is a not-verified verdict, never a panic.
func TestVerifierFailsClosedOnMalformedEvidence(t *testing.T) {
	t.Parallel()
	root, _ := verifyJSONRepo(t)
	cases := []struct{ source, reason string }{
		{"null", "Cannot read properties of null (reading 'version')"},
		{"[]", "unsupported local dossier version; v2 scopes required"},
		{`"x"`, "unsupported local dossier version; v2 scopes required"},
		{"5", "unsupported local dossier version; v2 scopes required"},
		{`{"version":2}`, "Cannot read properties of undefined (reading 'fingerprint')"},
		{`{"version":2,"domains":[]}`, "legacy domains field is not supported in v2"},
		{strings.Repeat("[", 50000), "JSON Parse error: nesting deeper than 10000 levels"},
	}
	for index, tc := range cases {
		source, reason := tc.source, tc.reason
		name := filepath.Join(".context", "dossier-"+strings.Repeat("x", index+1)+".json")
		if err := os.MkdirAll(filepath.Join(root, ".context"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
		var stdout bytes.Buffer
		verifier := Verifier{Dir: root, Stdout: &stdout,
			Fingerprint: func(string) (string, error) { return strings.Repeat("a", 64), nil }}
		if err := verifier.Run([]string{"--record", name}); !errors.Is(err, ErrNotVerified) {
			t.Fatalf("Run(%.20q) = %v, want ErrNotVerified", source, err)
		}
		if want := `{"verdict":"not-verified","reason":"` + reason + "\"}\n"; stdout.String() != want {
			t.Errorf("Run(%.20q) wrote %q, want %q", source, stdout.String(), want)
		}
	}
}

func TestVerifierRefusesAFingerprintThatIsNotADigest(t *testing.T) {
	t.Parallel()
	root, _ := verifyJSONRepo(t)
	if err := os.WriteFile(filepath.Join(root, "dossier.json"), []byte(`{"version":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	verifier := Verifier{Dir: root, Stdout: &stdout,
		Fingerprint: func(string) (string, error) { return strings.Repeat("A", 64), nil }}
	if err := verifier.Run([]string{"--record", "dossier.json"}); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("Run = %v, want ErrNotVerified", err)
	}
	if want := "{\"verdict\":\"not-verified\",\"reason\":\"CLI returned no tree fingerprint\"}\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestVerifierWordsMissingFilesLikeNode(t *testing.T) {
	t.Parallel()
	root, _ := verifyJSONRepo(t)
	var stdout bytes.Buffer
	if err := (Verifier{Dir: root, Stdout: &stdout}).Run([]string{"--ref", "missing.json"}); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("Run = %v, want ErrNotVerified", err)
	}
	want := "{\"verdict\":\"not-verified\",\"reason\":\"ENOENT: no such file or directory, open 'missing.json'\"}\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}
