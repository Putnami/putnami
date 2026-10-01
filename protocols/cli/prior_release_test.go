package cli

// The machine-result compatibility budget, executable.
//
// The budget in tooling/cli/doc/21-compatibility-and-migration.md makes three
// promises about this contract, and each is a different kind of claim:
//
//  1. version 2 is what this build EMITS — every document carries
//     protocolVersion: 2;
//  2. version 1 is still READABLE — documents recorded by builds published
//     before the removal must keep decoding, which means the retained shape has
//     to still describe them;
//  3. there is no downgrade — nothing in this module can BUILD a versionless
//     document, so a second output contract cannot grow back one command at a
//     time.
//
// Promise 2 is the one a test written today cannot check against itself: today's
// types and today's expectations ship together. testdata/prior-releases/ holds
// the published v1 schema in its last released state, recovered from the final
// commit that changed it, so "the read shape has not drifted" is measured
// against bytes this repository can no longer produce.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

var priorReleaseResultDir = filepath.Join("testdata", "prior-releases")

type priorReleaseResultFixture struct {
	File            string `json:"file"`
	ProtocolVersion int    `json:"protocolVersion"`
	SourceCommit    string `json:"sourceCommit"`
	SourcePath      string `json:"sourcePath"`
	Committed       string `json:"committed"`
	SHA256          string `json:"sha256"`
	Why             string `json:"why"`
}

type priorReleaseResultCorpus struct {
	Fixtures []priorReleaseResultFixture `json:"fixtures"`
	Gaps     []struct {
		Shape  string `json:"shape"`
		Reason string `json:"reason"`
	} `json:"gaps"`
}

func loadPriorReleaseResults(t *testing.T) priorReleaseResultCorpus {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(priorReleaseResultDir, "provenance.json"))
	if err != nil {
		t.Fatalf("read prior-release provenance: %v", err)
	}
	var corpus priorReleaseResultCorpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatalf("parse prior-release provenance: %v", err)
	}
	if len(corpus.Fixtures) == 0 {
		t.Fatal("the prior-release result corpus is empty; the tests below would pass vacuously")
	}
	return corpus
}

// TestPriorReleaseResultArtifactsAreImmutable is the guard. A recovered artifact
// that can be edited is not evidence: the next time the retained v1 shape stops
// matching it, the cheap fix would be to edit the fixture and the correct one is
// to decide whether a recorded document just stopped decoding.
func TestPriorReleaseResultArtifactsAreImmutable(t *testing.T) {
	corpus := loadPriorReleaseResults(t)
	recorded := make(map[string]bool, len(corpus.Fixtures))

	for _, fixture := range corpus.Fixtures {
		if fixture.File == "" || fixture.SourceCommit == "" || fixture.SourcePath == "" ||
			fixture.Committed == "" || fixture.SHA256 == "" || fixture.Why == "" ||
			fixture.ProtocolVersion == 0 {
			t.Errorf("provenance record is incomplete, so the fixture cannot be re-derived: %+v", fixture)
			continue
		}
		recorded[fixture.File] = true

		data, err := os.ReadFile(filepath.Join(priorReleaseResultDir, fixture.File))
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
	}

	entries, err := os.ReadDir(priorReleaseResultDir)
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
		t.Error("provenance records no gaps; the corpus claims to cover every v1 artifact shape")
	}
}

// TestRetainedV1SchemaHasNotDriftedSinceItsLastRelease is promise 2.
//
// schemas/result.json is retained so a consumer can still decode a document a
// pre-removal build wrote. That is only true while the retained bytes still
// describe those documents — a "cleanup" of a schema nothing emits is invisible
// in every other test, because nothing in this repository writes against it any
// more.
func TestRetainedV1SchemaHasNotDriftedSinceItsLastRelease(t *testing.T) {
	corpus := loadPriorReleaseResults(t)
	compared := 0

	for _, fixture := range corpus.Fixtures {
		if fixture.SourcePath != "protocols/cli/schemas/result.json" {
			continue
		}
		compared++
		released, err := os.ReadFile(filepath.Join(priorReleaseResultDir, fixture.File))
		if err != nil {
			t.Fatalf("read %s: %v", fixture.File, err)
		}
		retained, err := os.ReadFile(filepath.Join("schemas", "result.json"))
		if err != nil {
			t.Fatalf("read the retained v1 schema: %v", err)
		}
		if !bytes.Equal(released, retained) {
			t.Errorf("schemas/result.json no longer matches the schema published at %s.\n"+
				"The v1 shape is READ-ONLY: documents recorded by pre-removal builds are still on "+
				"disk, so changing it changes what those documents mean. If the change is "+
				"deliberate, say so in tooling/cli/doc/21-compatibility-and-migration.md and move "+
				"the fixture's expectation — not its bytes.", fixture.SourceCommit)
		}
	}

	if compared == 0 {
		t.Fatal("no recovered schema was compared; this test would pass vacuously")
	}
}

// TestRecordedV1DocumentsStillDecodeAgainstTheReleasedSchema ties the literal
// documents in result_test.go to the recovered schema: every member they use
// must be a member the released schema declared, and every member the schema
// requires must be one the retained Go type carries. Without this, the two
// halves of the v1 read path could drift apart while each stayed self-consistent.
func TestRecordedV1DocumentsStillDecodeAgainstTheReleasedSchema(t *testing.T) {
	released := releasedV1Schema(t)
	envelope := schemaProperties(t, released, "properties")
	if len(envelope) == 0 {
		t.Fatal("the released v1 schema declares no properties; the assertions below are vacuous")
	}

	for name, document := range map[string]string{
		"success": v1SuccessDocument,
		"failure": v1FailureDocument,
	} {
		t.Run(name, func(t *testing.T) {
			var members map[string]json.RawMessage
			if err := json.Unmarshal([]byte(document), &members); err != nil {
				t.Fatalf("parse recorded document: %v", err)
			}
			for member := range members {
				if !envelope[member] {
					t.Errorf("recorded document uses %q, which the released v1 schema does not declare", member)
				}
			}
			if _, present := members["protocolVersion"]; present {
				t.Error("a v1 document must carry no protocolVersion member; its absence is the only version signal")
			}
		})
	}

	// The retained Go type must still round-trip every member the schema
	// requires, or "you can still decode it" is only true of the schema.
	var decoded Result
	if err := json.Unmarshal([]byte(v1FailureDocument), &decoded); err != nil {
		t.Fatalf("decode recorded v1 document: %v", err)
	}
	for _, required := range schemaRequired(t, released) {
		switch required {
		case "command":
			if decoded.Command == "" {
				t.Error("the retained Result type dropped `command`")
			}
		case "status":
			if decoded.Status == "" {
				t.Error("the retained Result type dropped `status`")
			}
		case "exitCode":
			if decoded.ExitCode == 0 {
				t.Error("the retained Result type dropped `exitCode`")
			}
		default:
			t.Errorf("the released v1 schema requires %q, which this test does not check", required)
		}
	}
}

// TestEmittedDocumentsCarryTheCurrentProtocolVersion is promise 1 and promise 3
// together: what this build writes is stamped, and the stamp is the version the
// budget names.
func TestEmittedDocumentsCarryTheCurrentProtocolVersion(t *testing.T) {
	var buffer bytes.Buffer
	if _, err := WriteResultV2(&buffer, OutputJSON, NewResultV2("build", map[string]any{"ok": true}, nil)); err != nil {
		t.Fatalf("WriteResultV2: %v", err)
	}
	var emitted map[string]json.RawMessage
	if err := json.Unmarshal(buffer.Bytes(), &emitted); err != nil {
		t.Fatalf("parse emitted document: %v", err)
	}
	stamp, present := emitted["protocolVersion"]
	if !present {
		t.Fatalf("an emitted document carries no protocolVersion:\n%s", buffer.Bytes())
	}
	if want := fmt.Sprintf("%d", ResultProtocolVersion); string(stamp) != want {
		t.Errorf("protocolVersion = %s, want %s", stamp, want)
	}

	// Version detection is the whole compatibility story for this contract, so
	// pin both arms of the predicate a consumer branches on.
	if resultProtocolVersionOf(t, buffer.Bytes()) != ResultProtocolVersion {
		t.Error("an emitted document does not read back as the current version")
	}
	if resultProtocolVersionOf(t, []byte(v1SuccessDocument)) != 1 {
		t.Error("a document with no protocolVersion member must read as version 1")
	}
}

// resultProtocolVersionOf is the consumer-side predicate the budget documents:
// the member's absence means version 1, and nothing else distinguishes them.
func resultProtocolVersionOf(t *testing.T, document []byte) int {
	t.Helper()
	var probe struct {
		ProtocolVersion *int `json:"protocolVersion"`
	}
	if err := json.Unmarshal(document, &probe); err != nil {
		t.Fatalf("parse document: %v", err)
	}
	if probe.ProtocolVersion == nil {
		return 1
	}
	return *probe.ProtocolVersion
}

func releasedV1Schema(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	for _, fixture := range loadPriorReleaseResults(t).Fixtures {
		if fixture.SourcePath != "protocols/cli/schemas/result.json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(priorReleaseResultDir, fixture.File))
		if err != nil {
			t.Fatalf("read %s: %v", fixture.File, err)
		}
		var schema map[string]json.RawMessage
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("parse %s: %v", fixture.File, err)
		}
		return schema
	}
	t.Fatal("the corpus carries no released v1 schema")
	return nil
}

func schemaProperties(t *testing.T, schema map[string]json.RawMessage, key string) map[string]bool {
	t.Helper()
	raw, ok := schema[key]
	if !ok {
		return nil
	}
	var properties map[string]json.RawMessage
	if err := json.Unmarshal(raw, &properties); err != nil {
		t.Fatalf("parse schema %s: %v", key, err)
	}
	names := make(map[string]bool, len(properties))
	for name := range properties {
		names[name] = true
	}
	return names
}

func schemaRequired(t *testing.T, schema map[string]json.RawMessage) []string {
	t.Helper()
	raw, ok := schema["required"]
	if !ok {
		t.Fatal("the released v1 schema declares no required members")
	}
	var required []string
	if err := json.Unmarshal(raw, &required); err != nil {
		t.Fatalf("parse schema required: %v", err)
	}
	if len(required) == 0 {
		t.Fatal("the released v1 schema requires nothing; the loop below is vacuous")
	}
	return required
}
