package lockfile

// Compatibility budget, executable.
//
// The other lock tests in this package build their inputs with today's writer.
// That proves the writer and the reader agree, which they always will — they
// ship together. It cannot prove that a lock a user committed six weeks ago
// still loads.
//
// These tests read RECOVERED bytes: each fixture under
// testdata/prior-releases/ is a putnami.lock.json exactly as it was committed at
// the commit that shipped its format version. provenance.json records the source
// commit and path for every one, and the digests below make silently editing
// one a test failure rather than a fix.
//
// The budget these tests certify is documented in
// ../../doc/21-compatibility-and-migration.md and decided in
// ../../doc/adr/0010-compatibility-budget.md.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// priorReleaseCorpusDigest is SHA-256 over "<file>\x00<sha256>\n" for every
// fixture, in file-name order.
//
// It exists because a Go test task's declared cache inputs are its .go files —
// testdata is not among them, so a run whose only change is a fixture edit can
// be served from cache and never execute the guard below. This constant puts
// the corpus's identity inside a file the cache DOES key on: changing a fixture
// without changing this line leaves the two disagreeing the next time the
// package is tested, and changing this line is a reviewable act.
const priorReleaseCorpusDigest = "8ea9ba9497f21eb5e17178e2e79580f78bdf20552dc6cdd631d9d5f9ada86db7"

// priorReleaseDir is the corpus root, relative to this package.
var priorReleaseDir = filepath.Join("testdata", "prior-releases")

// priorReleaseFixture mirrors one record in provenance.json. Every field is
// required: a fixture whose provenance a reviewer cannot follow is not evidence.
type priorReleaseFixture struct {
	File          string `json:"file"`
	FormatVersion int    `json:"formatVersion"`
	SourceCommit  string `json:"sourceCommit"`
	SourcePath    string `json:"sourcePath"`
	Committed     string `json:"committed"`
	SHA256        string `json:"sha256"`
	Why           string `json:"why"`
}

type priorReleaseGap struct {
	Shape  string `json:"shape"`
	Reason string `json:"reason"`
}

type priorReleaseCorpus struct {
	Fixtures []priorReleaseFixture `json:"fixtures"`
	Gaps     []priorReleaseGap     `json:"gaps"`
}

func loadPriorReleaseCorpus(t *testing.T) priorReleaseCorpus {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(priorReleaseDir, "provenance.json"))
	if err != nil {
		t.Fatalf("read prior-release provenance: %v", err)
	}
	var corpus priorReleaseCorpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatalf("parse prior-release provenance: %v", err)
	}
	if len(corpus.Fixtures) == 0 {
		t.Fatal("the prior-release corpus is empty; every test below would pass vacuously")
	}
	return corpus
}

func priorReleaseBytes(t *testing.T, fixture priorReleaseFixture) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(priorReleaseDir, fixture.File))
	if err != nil {
		t.Fatalf("read fixture %s: %v", fixture.File, err)
	}
	return data
}

// TestPriorReleaseFixturesAreImmutable is the guard that keeps the corpus
// evidence rather than decoration.
//
// When a reader change breaks a fixture the cheap fix is to update the fixture
// and the correct one is to ask whether the reader just broke a lock somebody
// has committed. This test removes the cheap fix: bytes, file set and provenance
// must all agree, and the corpus digest must match the constant above.
func TestPriorReleaseFixturesAreImmutable(t *testing.T) {
	corpus := loadPriorReleaseCorpus(t)

	// Non-vacuity: the corpus must actually span the format history it claims
	// to. A guard over an accidentally-empty or single-version corpus reports a
	// protected invariant that nothing protects.
	versions := make(map[int]bool)
	for _, fixture := range corpus.Fixtures {
		versions[fixture.FormatVersion] = true
	}
	for want := 1; want < MinSupportedVersion; want++ {
		if !versions[want] {
			t.Errorf("no prior-release fixture for lock format v%d; the corpus must cover every "+
				"version this reader has an opinion about", want)
		}
	}
	if !versions[MinSupportedVersion-1] {
		t.Error("without a below-floor fixture the rejection arm is untested")
	}

	recorded := make(map[string]priorReleaseFixture, len(corpus.Fixtures))
	for _, fixture := range corpus.Fixtures {
		if fixture.File == "" || fixture.SourceCommit == "" || fixture.SourcePath == "" ||
			fixture.Committed == "" || fixture.SHA256 == "" || fixture.Why == "" ||
			fixture.FormatVersion == 0 {
			t.Errorf("provenance record is incomplete, so the fixture cannot be re-derived: %+v", fixture)
			continue
		}
		if _, duplicate := recorded[fixture.File]; duplicate {
			t.Errorf("provenance records %s twice", fixture.File)
		}
		recorded[fixture.File] = fixture

		data := priorReleaseBytes(t, fixture)
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != fixture.SHA256 {
			t.Errorf("fixture %s changed: sha256 = %s, provenance records %s.\n"+
				"A prior-release artifact is immutable. If a reader no longer accepts it, change the "+
				"reader or move the fixture's expected verdict — never the bytes.\n"+
				"Re-derive with: git show %s:%s",
				fixture.File, got, fixture.SHA256, fixture.SourceCommit, fixture.SourcePath)
		}

		// The declared format version must be what the bytes actually say, or
		// every expectation keyed on it below tests the wrong row.
		var declared struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal(data, &declared); err != nil {
			t.Errorf("fixture %s is not JSON: %v", fixture.File, err)
			continue
		}
		if declared.Version != fixture.FormatVersion {
			t.Errorf("fixture %s declares version %d but provenance says v%d",
				fixture.File, declared.Version, fixture.FormatVersion)
		}
	}

	// A fixture added without provenance is a fixture nobody can re-derive.
	entries, err := os.ReadDir(priorReleaseDir)
	if err != nil {
		t.Fatalf("read corpus directory: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == "provenance.json" || name == "README.md" {
			continue
		}
		if _, ok := recorded[name]; !ok {
			t.Errorf("%s is in the corpus but has no provenance record", name)
		}
	}

	// Gaps are recorded, not implied: a corpus that silently omitted the shapes
	// this repository never committed would read as complete.
	if len(corpus.Gaps) == 0 {
		t.Error("provenance records no gaps; the corpus claims to cover every shape in the budget")
	}
	for _, gap := range corpus.Gaps {
		if gap.Shape == "" || gap.Reason == "" {
			t.Errorf("gap record is incomplete: %+v", gap)
		}
	}

	if got := priorReleaseDigest(corpus); got != priorReleaseCorpusDigest {
		t.Errorf("corpus digest = %s, want %s.\nThe fixture set changed. Update "+
			"priorReleaseCorpusDigest in the same commit, so the cache sees the change.", got, priorReleaseCorpusDigest)
	}
}

func priorReleaseDigest(corpus priorReleaseCorpus) string {
	fixtures := make([]priorReleaseFixture, len(corpus.Fixtures))
	copy(fixtures, corpus.Fixtures)
	sort.Slice(fixtures, func(i, j int) bool { return fixtures[i].File < fixtures[j].File })
	hash := sha256.New()
	for _, fixture := range fixtures {
		fmt.Fprintf(hash, "%s\x00%s\n", fixture.File, fixture.SHA256)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// TestPriorReleaseLocksMeetTheCompatibilityBudget drives the real reader over
// the recovered bytes and asserts exactly what the budget document promises: a
// below-floor lock is refused with a typed error naming the remedy and is left
// untouched, an in-window lock loads at the version it records, and neither
// reader nor writer promotes a version behind the workspace's back.
func TestPriorReleaseLocksMeetTheCompatibilityBudget(t *testing.T) {
	for _, fixture := range loadPriorReleaseCorpus(t).Fixtures {
		t.Run(fixture.File, func(t *testing.T) {
			data := priorReleaseBytes(t, fixture)
			dir := t.TempDir()
			path := filepath.Join(dir, LockFilename)
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatalf("seed lock: %v", err)
			}

			if fixture.FormatVersion < MinSupportedVersion {
				lf, err := ReadLockFile(dir)
				if lf != nil {
					t.Error("a refused lock must not be returned; a caller could act on it")
				}
				var outdated *OutdatedVersionError
				if !errors.As(err, &outdated) {
					t.Fatalf("ReadLockFile = %v, want *OutdatedVersionError", err)
				}
				if outdated.Min != MinSupportedVersion {
					t.Errorf("Min = %d, want %d", outdated.Min, MinSupportedVersion)
				}
				// The remedy is the only part of the failure a user sees.
				for _, want := range []string{
					LockFilename,
					fmt.Sprintf("version %d", fixture.FormatVersion),
					fmt.Sprintf("version %d", MinSupportedVersion),
					"putnami install",
				} {
					if !strings.Contains(outdated.Error(), want) {
						t.Errorf("refusal %q does not name %q", outdated.Error(), want)
					}
				}

				// Refusing must not rewrite the file: a reader that converted a
				// lock it refused would migrate a workspace as a side effect of
				// an unrelated command.
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read back: %v", err)
				}
				if string(after) != string(data) {
					t.Error("refusing a below-floor lock modified it on disk")
				}
				return
			}

			lf, err := ReadLockFile(dir)
			if err != nil {
				t.Fatalf("ReadLockFile(v%d): %v", fixture.FormatVersion, err)
			}
			if lf.Version != fixture.FormatVersion {
				t.Errorf("Version = %d, want %d — an ordinary read must not promote a lock",
					lf.Version, fixture.FormatVersion)
			}

			// An ordinary write preserves the workspace's version too, so
			// `putnami install` on a v4 workspace never emits v5 vocabulary.
			if err := WriteLockFile(dir, lf); err != nil {
				t.Fatalf("WriteLockFile: %v", err)
			}
			rewritten, err := ReadLockFile(dir)
			if err != nil {
				t.Fatalf("re-read after write: %v", err)
			}
			if rewritten.Version != fixture.FormatVersion {
				t.Errorf("rewriting promoted the lock from v%d to v%d",
					fixture.FormatVersion, rewritten.Version)
			}
		})
	}
}

// rawDigestInventory walks the lock's JSON and returns every field a verifier
// depends on, keyed by its path: per-platform archive digests and manifest
// hashes. The legacy scalar `integrity` is deliberately excluded — the
// canonical form drops it when a per-platform map exists, which
// TestMarshalLockFileOmitsLegacyExtensionScalarIntegrity covers on its own.
func rawDigestInventory(t *testing.T, data []byte) map[string]string {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse lock bytes: %v", err)
	}
	inventory := make(map[string]string)

	collect := func(section, name string, raw json.RawMessage) {
		var entry struct {
			Integrities  map[string]string `json:"integrities"`
			ManifestHash string            `json:"manifestHash"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatalf("parse %s.%s: %v", section, name, err)
		}
		for platform, digest := range entry.Integrities {
			inventory[section+"."+name+".integrities."+platform] = digest
		}
		if entry.ManifestHash != "" {
			inventory[section+"."+name+".manifestHash"] = entry.ManifestHash
		}
	}

	if raw, ok := doc["cli"]; ok {
		collect("cli", "", raw)
	}
	for _, section := range []string{"toolchains", "extensions", "templates", "agentArtifacts"} {
		raw, ok := doc[section]
		if !ok {
			continue
		}
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			t.Fatalf("parse %s: %v", section, err)
		}
		for name, entry := range entries {
			collect(section, name, entry)
		}
	}
	return inventory
}

// v4Golden is a synthetic v4 lock: a
// two-platform CLI pin, an extension pinned by manifest hash alone, and
// a scalar-plus-map extension. The recovered corpus holds no in-window lock and
// no CLI pin, which testdata/prior-releases/provenance.json records as gaps.
const v4Golden = `{
  "version": 4,
  "cli": {
    "version": "1.4.2",
    "integrities": {
      "darwin/arm64": "climachash",
      "linux/amd64": "clilinuxhash"
    },
    "source": "https://put.putnami.dev/putnami/cli/download?channel=1.4.2"
  },
  "extensions": {
    "@putnami/go": {
      "version": "2.0.0",
      "manifestHash": "gomanifest",
      "taskContract": 2
    },
    "@putnami/typescript": {
      "version": "3.1.0",
      "integrity": "legacydigest",
      "integrities": {
        "darwin/arm64": "tsmachash"
      },
      "manifestHash": "tsmanifest",
      "source": "https://put.putnami.dev/putnami/typescript/download?channel=3.1.0",
      "taskContract": 3
    }
  },
  "templates": {
    "typescript-web": {
      "version": "1.0.0"
    }
  }
}
`

// TestMigrationPreservesEveryPlatformDigest is the security row of the budget.
//
// A lock's `integrities` maps are the only thing between a download and
// execution, and the CLI verifies them fail-closed: the CLI pin before the
// launcher execs a downloaded binary, an extension pin before install. A
// projection that rebuilt a map from the machine it ran on would drop every
// other platform's digest, and nothing would say so until a teammate on that
// platform ran a command. The inventory is read from the RAW bytes rather than
// through the typed reader, so a reader that silently dropped a field cannot
// hide behind itself. The input is the synthetic v4Golden.
func TestMigrationPreservesEveryPlatformDigest(t *testing.T) {
	source := rawDigestInventory(t, []byte(v4Golden))
	if source["cli..integrities.darwin/arm64"] == "" || source["cli..integrities.linux/amd64"] == "" {
		t.Fatal("v4Golden does not carry a two-platform CLI pin; this test would pass vacuously")
	}
	if source["extensions.@putnami/typescript.integrities.darwin/arm64"] == "" {
		t.Fatal("v4Golden does not carry an extension platform digest; this test would pass vacuously")
	}

	parsed, err := ParseLockFile([]byte(v4Golden))
	if err != nil {
		t.Fatalf("ParseLockFile(v4Golden): %v", err)
	}
	current, err := MigrateLockFileToCurrent(parsed)
	if err != nil {
		t.Fatalf("MigrateLockFileToCurrent: %v", err)
	}
	if current.Version != MaxSupportedVersion {
		t.Fatalf("migrated to v%d, want v%d", current.Version, MaxSupportedVersion)
	}
	currentBytes, err := MarshalLockFile(current)
	if err != nil {
		t.Fatalf("MarshalLockFile: %v", err)
	}
	assertDigestsSurvive(t, fmt.Sprintf("v4→v%d", MaxSupportedVersion), source, rawDigestInventory(t, currentBytes))
}

func assertDigestsSurvive(t *testing.T, step string, before, after map[string]string) {
	t.Helper()
	for key, digest := range before {
		got, ok := after[key]
		if !ok {
			t.Errorf("%s dropped the pin digest %s (%s); the CLI verifies it fail-closed",
				step, key, digest)
			continue
		}
		if got != digest {
			t.Errorf("%s changed the pin digest %s: %s → %s", step, key, digest, got)
		}
	}
}

// TestPriorReleaseNewerLockNamesTheUpgradeRemedy is the ceiling arm, built from
// a recovered fixture rather than a hand-written document: only the version
// member moves, so what the reader meets is a real lock from the future.
func TestPriorReleaseNewerLockNamesTheUpgradeRemedy(t *testing.T) {
	corpus := loadPriorReleaseCorpus(t)
	newest := corpus.Fixtures[0]
	for _, fixture := range corpus.Fixtures {
		if fixture.FormatVersion > newest.FormatVersion {
			newest = fixture
		}
	}
	future := strings.Replace(
		string(priorReleaseBytes(t, newest)),
		fmt.Sprintf(`"version": %d,`, newest.FormatVersion),
		fmt.Sprintf(`"version": %d,`, MaxSupportedVersion+1), 1)
	if !strings.Contains(future, fmt.Sprintf(`"version": %d,`, MaxSupportedVersion+1)) {
		t.Fatalf("could not restamp %s; the fixture's version member moved", newest.File)
	}

	_, err := ParseLockFile([]byte(future))
	var unsupported *UnsupportedVersionError
	if !errors.As(err, &unsupported) {
		t.Fatalf("ParseLockFile(future) = %v, want *UnsupportedVersionError", err)
	}
	if unsupported.Found != MaxSupportedVersion+1 || unsupported.Max != MaxSupportedVersion {
		t.Errorf("UnsupportedVersionError = %+v, want Found=%d Max=%d",
			unsupported, MaxSupportedVersion+1, MaxSupportedVersion)
	}
	if !strings.Contains(unsupported.Error(), "putnami upgrade") {
		t.Errorf("refusal %q does not name `putnami upgrade`", unsupported.Error())
	}
}

// TestCompatibilityBudgetDocumentMatchesTheCode closes the loop the issue is
// about: a promise nobody executes drifts from the code that keeps it.
//
// The budget document states its numbers in prose. This test rebuilds those
// sentences from the constants and requires the document to contain them, so
// moving a window without editing the document fails here, and the failure names
// the file to edit.
func TestCompatibilityBudgetDocumentMatchesTheCode(t *testing.T) {
	const docPath = "../../doc/21-compatibility-and-migration.md"
	data, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read the compatibility budget: %v", err)
	}
	doc := string(data)

	claims := []string{
		fmt.Sprintf("accepts lock format versions %d – %d and writes new locks at version %d",
			MinSupportedVersion, MaxSupportedVersion, DefaultWriteVersion),
		fmt.Sprintf("implements CLI ↔ extension contract %d", protocolcli.CurrentContract),
		fmt.Sprintf("carries machine result protocol version %d", protocolcli.ResultProtocolVersion),
		fmt.Sprintf("every version from 1 to the reader's own (currently %d)", protocolcli.CommandSurfaceVersion),
	}
	for _, claim := range claims {
		if !strings.Contains(doc, claim) {
			t.Errorf("doc/21-compatibility-and-migration.md does not state %q.\n"+
				"The budget and the code must move together; edit the document in this commit.", claim)
		}
	}

	// The remedies are the user-visible half of the budget, so the document must
	// name the same commands the errors do.
	for _, remedy := range []string{
		"putnami migrate vnext --apply",
		"putnami upgrade",
		"putnami extensions update",
		"putnami pin",
	} {
		if !strings.Contains(doc, remedy) {
			t.Errorf("the budget does not tell a user to run %q", remedy)
		}
	}
}
