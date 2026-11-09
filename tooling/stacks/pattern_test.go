package stacks

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// withBundler is the sample profile with a .ruby-version style file on the
// toolchain and a further check, bundler, whose version Gemfile.lock states.
func withBundler(t *testing.T) Stack {
	t.Helper()
	doc := strings.Replace(sampleProfile, `"files": [{"path": "rust-toolchain.toml", "key": "toolchain.channel"}, {"path": "rust-toolchain"}]
  },`, `"files": [{"path": "rust-toolchain.toml", "key": "toolchain.channel"}, {"path": ".rust-version", "pattern": "^(?:rust-)?(?<version>\\S+)"}],
    "also": [{"command": ["bundle", "--version"], "pattern": "(?<version>[0-9]+\\.[0-9]+\\.[0-9]+\\S*)",
      "files": [{"path": "Gemfile.lock", "pattern": "(?m)^BUNDLED WITH\\s+(?<version>\\S+)"}]}]
  },`, 1)
	p, err := Decode([]byte(doc))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return Stack{Extension: "@putnami/rust", Profile: p, Detect: fakeDetector}
}

const lockfile = "GEM\n  specs:\n    rake (13.2.1)\n\nBUNDLED WITH\n   2.5.22\n"

func TestAVersionPatternReadsTheFile(t *testing.T) {
	root := t.TempDir()
	s := withBundler(t)
	writeFile(t, root, ".rust-version", "rust-1.82.0\n")
	writeFile(t, root, "Gemfile.lock", lockfile)
	got, err := s.Profile.Toolchain.ExpectedVersion(root, "crates/api", ".")
	if err != nil || got != (Expected{Version: "1.82.0", Source: ".rust-version"}) {
		t.Fatalf("toolchain = %+v, %v", got, err)
	}
	bundler := s.Profile.Toolchain.Checks()[1]
	got, err = bundler.ExpectedVersion(root, "crates/api", ".")
	if err != nil || got != (Expected{Version: "2.5.22", Source: "Gemfile.lock"}) {
		t.Fatalf("bundler = %+v, %v", got, err)
	}
	writeFile(t, root, "crates/api/Gemfile.lock", "GEM\n")
	if got, _ := bundler.ExpectedVersion(root, "crates/api", "."); got.Source != "Gemfile.lock" {
		t.Errorf("a file the pattern does not match states no version, the next one is read: %+v", got)
	}
}

func TestDecodeRefusesAnInvalidPattern(t *testing.T) {
	s := withBundler(t)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s.Profile); err != nil {
		t.Fatal(err)
	}
	base := buf.Bytes()
	for name, edit := range map[string][2]string{
		"no group":        {`(?m)^BUNDLED WITH\\s+(?<version>\\S+)`, `(?m)^BUNDLED WITH\\s+(\\S+)`},
		"bad expression":  {`(?m)^BUNDLED WITH\\s+(?<version>\\S+)`, `(?<version>[`},
		"key and pattern": {`"path":"Gemfile.lock",`, `"path":"Gemfile.lock","key":"x",`},
	} {
		doc := strings.Replace(string(base), edit[0], edit[1], 1)
		if doc == string(base) {
			t.Fatalf("%s: the edit %q matches nothing in %s", name, edit[0], base)
		}
		if _, err := Decode([]byte(doc)); err == nil {
			t.Errorf("%s: Decode accepted %s", name, doc)
		}
	}
}

func TestAFurtherCheckFileIsAnInputAndReachesTheMetadata(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Gemfile.lock", lockfile)
	s := withBundler(t)
	if project, rootFiles := s.Profile.TaskPatterns(TaskLint); !slices.Contains(project, "Gemfile.lock") || !slices.Contains(rootFiles, "Gemfile.lock") || !slices.Contains(rootFiles, ".rust-version") {
		t.Errorf("a further check's files are inputs of every task: %v %v", project, rootFiles)
	}
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"crates/api"}})
	if err != nil || len(result.Projects) != 1 {
		t.Fatalf("probe: %+v %v", result, err)
	}
	var meta Metadata
	if err := json.Unmarshal(result.Projects[0].Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if want := []Expected{{Version: "2.5.22", Source: "Gemfile.lock"}}; !slices.Equal(meta.Toolchains, want) {
		t.Errorf("toolchains = %+v", meta.Toolchains)
	}
	if !slices.Contains(result.Projects[0].WatchedFiles, "crates/api/Gemfile.lock") || !slices.Contains(result.Projects[0].WatchedFiles, "Gemfile.lock") {
		t.Errorf("the probe watches the further check's files: %v", result.Projects[0].WatchedFiles)
	}
}
