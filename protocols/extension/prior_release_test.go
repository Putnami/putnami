package extension

// The cliContract exact-match rejection, proven against a RECOVERED manifest.
//
// TestLoadManifest_CompatibilityMatrix already renders every rung of the ladder
// against every surface kind, but it builds each manifest from a format string:
// it proves the loader is self-consistent, not that it still refuses a manifest
// somebody actually published. This test reads bytes recovered from the commit
// that last carried them, so the "absent stamp" arm is exercised by a real
// pre-registry manifest with real commands, flags and tasks.
//
// Intentional exact matching is the thing being preserved here. The budget in
// tooling/cli/doc/21-compatibility-and-migration.md says a manifest below the
// current contract does NOT load and the error names the side that has to move;
// contract 3 retired the older "ship adaptation for contract N-1" rule
// (ADR 0002 §1, ADR 0003).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

var priorReleaseManifestDir = filepath.Join("testdata", "prior-releases")

type priorReleaseManifest struct {
	File             string `json:"file"`
	DeclaredContract int    `json:"declaredContract"`
	SourceCommit     string `json:"sourceCommit"`
	SourcePath       string `json:"sourcePath"`
	Committed        string `json:"committed"`
	SHA256           string `json:"sha256"`
	Expect           string `json:"expect"`
	Why              string `json:"why"`
}

type priorReleaseManifestCorpus struct {
	Fixtures []priorReleaseManifest `json:"fixtures"`
	Gaps     []struct {
		Shape  string `json:"shape"`
		Reason string `json:"reason"`
	} `json:"gaps"`
}

func loadPriorReleaseManifests(t *testing.T) priorReleaseManifestCorpus {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(priorReleaseManifestDir, "provenance.json"))
	if err != nil {
		t.Fatalf("read prior-release provenance: %v", err)
	}
	var corpus priorReleaseManifestCorpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatalf("parse prior-release provenance: %v", err)
	}
	if len(corpus.Fixtures) == 0 {
		t.Fatal("the prior-release manifest corpus is empty; the tests below would pass vacuously")
	}
	return corpus
}

// TestPriorReleaseManifestsAreImmutable keeps the corpus evidence: bytes,
// provenance and the file set must agree. Editing a fixture to make a loader
// change pass is a compatibility break with the evidence deleted.
func TestPriorReleaseManifestsAreImmutable(t *testing.T) {
	corpus := loadPriorReleaseManifests(t)
	recorded := make(map[string]bool, len(corpus.Fixtures))

	for _, fixture := range corpus.Fixtures {
		if fixture.File == "" || fixture.SourceCommit == "" || fixture.SourcePath == "" ||
			fixture.Committed == "" || fixture.SHA256 == "" || fixture.Why == "" ||
			fixture.Expect == "" {
			t.Errorf("provenance record is incomplete, so the fixture cannot be re-derived: %+v", fixture)
			continue
		}
		recorded[fixture.File] = true

		data, err := os.ReadFile(filepath.Join(priorReleaseManifestDir, fixture.File))
		if err != nil {
			t.Errorf("read %s: %v", fixture.File, err)
			continue
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != fixture.SHA256 {
			t.Errorf("fixture %s changed: sha256 = %s, provenance records %s.\n"+
				"Re-derive with: git show %s:%s", fixture.File, got, fixture.SHA256,
				fixture.SourceCommit, fixture.SourcePath)
		}

		// The declared contract must be what the bytes say, or the expectation
		// below is keyed on the wrong rung.
		var declared struct {
			CLIContract *int `json:"cliContract"`
		}
		if err := json.Unmarshal(data, &declared); err != nil {
			t.Errorf("fixture %s is not JSON: %v", fixture.File, err)
			continue
		}
		stamp := 0
		if declared.CLIContract != nil {
			stamp = *declared.CLIContract
		}
		if stamp != fixture.DeclaredContract {
			t.Errorf("fixture %s declares cliContract %d but provenance says %d",
				fixture.File, stamp, fixture.DeclaredContract)
		}
	}

	entries, err := os.ReadDir(priorReleaseManifestDir)
	if err != nil {
		t.Fatalf("read corpus directory: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == "provenance.json" || name == "README.md" {
			continue
		}
		if !recorded[name] {
			t.Errorf("%s is in the corpus but has no provenance record", name)
		}
	}

	if len(corpus.Gaps) == 0 {
		t.Error("provenance records no gaps; the corpus claims to cover every rung of the ladder")
	}
}

// TestPriorReleaseManifestsAreRejectedWithTheDocumentedRemedy is the executable
// form of the budget's exact-match row. A rejection a user cannot act on is a
// different failure from a rejection, so the remediation text is asserted too.
func TestPriorReleaseManifestsAreRejectedWithTheDocumentedRemedy(t *testing.T) {
	corpus := loadPriorReleaseManifests(t)
	rejected := 0

	for _, fixture := range corpus.Fixtures {
		t.Run(fixture.File, func(t *testing.T) {
			path := filepath.Join(priorReleaseManifestDir, fixture.File)
			manifest, err := LoadManifest(path)

			if fixture.Expect != "reject" {
				if err != nil {
					t.Fatalf("fixture expected to load: %v", err)
				}
				return
			}
			rejected++
			if err == nil {
				t.Fatal("a manifest below the current contract must not load")
			}
			if manifest != nil {
				t.Error("a rejected manifest must not be returned; a caller could act on it")
			}

			// The rejection must be the CONTRACT rejection, not an incidental
			// structural failure that happens to also fail. Otherwise this test
			// would keep passing after the ladder was removed.
			for _, want := range []string{
				fmt.Sprintf("declares CLI contract %d", fixture.DeclaredContract),
				fmt.Sprintf("requires %d", protocolcli.CurrentContract),
				"re-package the extension",
				"putnami extensions update",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("rejection %q does not name %q", err.Error(), want)
				}
			}
		})
	}

	if rejected == 0 {
		t.Error("no recovered manifest exercised the rejection arm")
	}
}

// TestPriorReleaseManifestGovernsTheLadder pins WHY the recovered manifest is
// rejected rather than exempted: it declares a contract surface. The hook-only
// exemption is not a hole in the ladder, it is the absence of anything
// for the rule to govern, and the two must not be confused.
func TestPriorReleaseManifestGovernsTheLadder(t *testing.T) {
	for _, fixture := range loadPriorReleaseManifests(t).Fixtures {
		data, err := os.ReadFile(filepath.Join(priorReleaseManifestDir, fixture.File))
		if err != nil {
			t.Fatalf("read %s: %v", fixture.File, err)
		}
		var probe struct {
			Commands      map[string]json.RawMessage `json:"commands"`
			CommandGroups map[string]json.RawMessage `json:"commandGroups"`
			Tools         map[string]json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal(data, &probe); err != nil {
			t.Fatalf("parse %s: %v", fixture.File, err)
		}
		surfaces := len(probe.Commands) + len(probe.CommandGroups) + len(probe.Tools)
		if fixture.Expect == "reject" && surfaces == 0 {
			t.Errorf("%s is expected to be rejected but declares no contract surface; "+
				"the exemption would apply and the fixture would prove the opposite rule", fixture.File)
		}
	}
}
