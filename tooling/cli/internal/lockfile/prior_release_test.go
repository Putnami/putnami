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
	for _, want := range []int{FormatVersionV1, FormatVersionV2, FormatVersionV3} {
		if !versions[want] {
			t.Errorf("no prior-release fixture for lock format v%d; the corpus must cover every "+
				"version this reader has an opinion about", want)
		}
	}
	if !versions[FormatVersionV1] {
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
		if effectiveVersion(declared.Version) != fixture.FormatVersion {
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

// TestPriorReleaseLocksMeetTheCompatibilityBudget drives the real readers over
// the recovered bytes and asserts exactly what the budget document promises: a
// below-floor lock is refused with a typed error naming the migration, an
// in-window lock loads at the version it records, and neither reader nor writer
// promotes a version behind the workspace's back.
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
					"putnami migrate vnext --apply",
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

				// The migration path is the single exception, and it is the
				// reason the refusal is actionable at all.
				migratable, err := ReadMigratableLockFile(dir)
				if err != nil {
					t.Fatalf("ReadMigratableLockFile: %v", err)
				}
				if migratable.Version != declaredVersionMember(fixture) {
					t.Errorf("migration reader promoted the version: got %d, want %d",
						migratable.Version, declaredVersionMember(fixture))
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
			// `putnami install` on a v2 workspace never emits v4 vocabulary.
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

// declaredVersionMember is the raw `version` member a v1 file carries: 1 when it
// records one, 0 when it records none. Both spell v1.
func declaredVersionMember(fixture priorReleaseFixture) int {
	if fixture.FormatVersion == FormatVersionV1 {
		return FormatVersionV1
	}
	return fixture.FormatVersion
}

// TestPriorReleaseMigrationPreservesEveryPlatformDigest is the security row of
// the budget.
//
// A lock's `integrities` maps are the only thing between a download and
// execution, and the CLI verifies them fail-closed. A migration that rebuilt the
// map from the machine it ran on would drop every other platform's digest, and
// nothing would say so until a teammate on that platform ran a command. The
// inventory below is read from the RAW bytes rather than through the typed
// reader, so a reader that silently dropped a field cannot hide behind itself.
func TestPriorReleaseMigrationPreservesEveryPlatformDigest(t *testing.T) {
	for _, fixture := range loadPriorReleaseCorpus(t).Fixtures {
		t.Run(fixture.File, func(t *testing.T) {
			data := priorReleaseBytes(t, fixture)
			before := rawDigestInventory(t, data)
			if len(before) == 0 {
				t.Fatal("fixture records no digests, so this test would pass vacuously")
			}

			migrated := migratePriorRelease(t, fixture, data)
			after := rawDigestInventory(t, migrated)

			for key, digest := range before {
				got, ok := after[key]
				if !ok {
					t.Errorf("migration dropped %s (%s)", key, digest)
					continue
				}
				if got != digest {
					t.Errorf("migration changed %s: %s → %s", key, digest, got)
				}
			}
		})
	}
}

// migratePriorRelease runs the migration the budget promises for this fixture's
// version and returns the migrated bytes.
func migratePriorRelease(t *testing.T, fixture priorReleaseFixture, data []byte) []byte {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LockFilename), data, 0o644); err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	if fixture.FormatVersion < MinSupportedVersion {
		// v1 → v2: the `putnami migrate vnext --apply` seam.
		lf, err := ReadMigratableLockFile(dir)
		if err != nil {
			t.Fatalf("ReadMigratableLockFile: %v", err)
		}
		if err := WriteLockFileV2(dir, lf); err != nil {
			t.Fatalf("WriteLockFileV2: %v", err)
		}
		migrated, err := os.ReadFile(filepath.Join(dir, LockFilename))
		if err != nil {
			t.Fatalf("read migrated lock: %v", err)
		}
		if got, err := ParseLockFile(migrated); err != nil || got.Version != FormatVersionV2 {
			t.Fatalf("migrated lock does not read at v2: version=%v err=%v", got, err)
		}
		return migrated
	}

	// v2/v3 → v4: the explicit projection onto the current format.
	lf, err := ReadLockFile(dir)
	if err != nil {
		t.Fatalf("ReadLockFile: %v", err)
	}
	current, err := MigrateLockFileToCurrent(lf)
	if err != nil {
		t.Fatalf("MigrateLockFileToCurrent: %v", err)
	}
	if current.Version != MaxSupportedVersion {
		t.Fatalf("migrated to v%d, want v%d", current.Version, MaxSupportedVersion)
	}
	migrated, err := MarshalLockFile(current)
	if err != nil {
		t.Fatalf("MarshalLockFile: %v", err)
	}
	return migrated
}

// rawDigestInventory walks the lock's JSON and returns every field a verifier
// depends on, keyed by its path: per-platform archive digests and manifest
// hashes. The legacy scalar `integrity` is deliberately excluded — the v2
// canonicalization drops it when a per-platform map exists, which
// TestPriorReleaseScalarIntegrityRuleIsDeliberate covers on its own.
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

// TestPriorReleaseScalarIntegrityRuleIsDeliberate pins the one field a migration
// is allowed to drop, and the case where dropping it would be a security
// regression instead of a cleanup.
//
// An extension archive is per-os/arch, so the legacy scalar only ever matched
// the platform that wrote it: keeping it beside a per-platform map makes an
// otherwise complete lock platform-dependent. An entry that has ONLY the scalar
// is different — that scalar is its sole verification datum, and dropping it
// would leave the entry unverifiable.
func TestPriorReleaseScalarIntegrityRuleIsDeliberate(t *testing.T) {
	corpus := loadPriorReleaseCorpus(t)
	sawScalarOnly, sawBoth := false, false

	for _, fixture := range corpus.Fixtures {
		if fixture.FormatVersion >= MinSupportedVersion {
			continue // the rule applies to what the v1→v2 migration writes
		}
		data := priorReleaseBytes(t, fixture)
		migrated := migratePriorRelease(t, fixture, data)

		for name, source := range rawExtensionEntries(t, data) {
			result, ok := rawExtensionEntries(t, migrated)[name]
			if !ok {
				t.Fatalf("%s: migration dropped extension %s", fixture.File, name)
			}
			switch {
			case source.Integrity != "" && len(source.Integrities) == 0:
				sawScalarOnly = true
				if result.Integrity != source.Integrity {
					t.Errorf("%s: scalar-only entry %s lost its only verification datum: %q → %q",
						fixture.File, name, source.Integrity, result.Integrity)
				}
			case source.Integrity != "" && len(source.Integrities) > 0:
				sawBoth = true
				if result.Integrity != "" {
					t.Errorf("%s: entry %s kept the platform-dependent scalar %q beside a per-platform map",
						fixture.File, name, result.Integrity)
				}
				if len(result.Integrities) != len(source.Integrities) {
					t.Errorf("%s: entry %s lost per-platform digests: %v → %v",
						fixture.File, name, source.Integrities, result.Integrities)
				}
			}
		}
	}

	if !sawScalarOnly {
		t.Error("no recovered fixture exercises the scalar-only entry; the preservation arm is unproven")
	}
	if !sawBoth {
		t.Error("no recovered fixture exercises scalar + map; the drop arm is unproven")
	}
}

type rawExtensionEntry struct {
	Integrity   string            `json:"integrity"`
	Integrities map[string]string `json:"integrities"`
}

func rawExtensionEntries(t *testing.T, data []byte) map[string]rawExtensionEntry {
	t.Helper()
	var doc struct {
		Extensions map[string]rawExtensionEntry `json:"extensions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse lock bytes: %v", err)
	}
	return doc.Extensions
}

// TestMigrationPreservesTheCLIPinDigests covers the one shape the recovered
// corpus cannot: no lock ever committed to this repository pins the CLI (this
// workspace builds the CLI from source), which testdata/prior-releases/
// provenance.json records as an explicit gap. The input is therefore the
// package's synthetic v1Golden — the same bytes the rest of this package uses —
// and what it proves is the field the launcher verifies fail-closed before
// exec'ing a downloaded binary.
func TestMigrationPreservesTheCLIPinDigests(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LockFilename), []byte(v1Golden), 0o644); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	source := rawDigestInventory(t, []byte(v1Golden))
	if source["cli..integrities.darwin/arm64"] == "" || source["cli..integrities.linux/amd64"] == "" {
		t.Fatal("v1Golden no longer carries a two-platform CLI pin; this test would pass vacuously")
	}

	lf, err := ReadMigratableLockFile(dir)
	if err != nil {
		t.Fatalf("ReadMigratableLockFile: %v", err)
	}
	if err := WriteLockFileV2(dir, lf); err != nil {
		t.Fatalf("WriteLockFileV2: %v", err)
	}
	v2Bytes, err := os.ReadFile(filepath.Join(dir, LockFilename))
	if err != nil {
		t.Fatalf("read migrated lock: %v", err)
	}
	assertCLIDigestsSurvive(t, "v1→v2", source, rawDigestInventory(t, v2Bytes))

	// …and again across the explicit projection onto the current format, so a
	// workspace that migrates twice does not lose a platform on the second hop.
	parsed, err := ParseLockFile(v2Bytes)
	if err != nil {
		t.Fatalf("ParseLockFile(migrated): %v", err)
	}
	current, err := MigrateLockFileToCurrent(parsed)
	if err != nil {
		t.Fatalf("MigrateLockFileToCurrent: %v", err)
	}
	v4Bytes, err := MarshalLockFile(current)
	if err != nil {
		t.Fatalf("MarshalLockFile: %v", err)
	}
	assertCLIDigestsSurvive(t, "v2→v4", source, rawDigestInventory(t, v4Bytes))
}

func assertCLIDigestsSurvive(t *testing.T, step string, before, after map[string]string) {
	t.Helper()
	for key, digest := range before {
		if !strings.HasPrefix(key, "cli.") {
			continue
		}
		got, ok := after[key]
		if !ok {
			t.Errorf("%s dropped the CLI pin digest %s (%s); the launcher verifies it fail-closed",
				step, key, digest)
			continue
		}
		if got != digest {
			t.Errorf("%s changed the CLI pin digest %s: %s → %s", step, key, digest, got)
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
