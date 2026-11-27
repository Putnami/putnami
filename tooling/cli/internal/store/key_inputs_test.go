package store

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// keyInputWitnesses names, for each field of CacheKey, how a run has it
// (tooling/cli ADR 0113 §5): "computed" by the run that uses it, with the
// reader that computes it, or "recorded" in the input record under a witness.
// A field this table does not name fails TestEveryKeyInputHasAWitness: a new
// key input lands with its row, so nobody records a value whose input no
// witness covers.
var keyInputWitnesses = map[string]string{
	"Extension":                     "computed: the extension's manifest",
	"ExtensionVersion":              "computed: the extension's manifest",
	"ExtensionImplementationDigest": "computed: the installed tree's digest kept per store entry, or the runtime sync's digest (ADR 0102)",
	"ToolchainVersion":              "computed: the CLI's Go version and each toolchain's lock identity and recorded probes (ADR 0102)",
	"RuntimeIdentity":               "computed: the host platform, the release baseline (ADR 0110) and the tree facts, read once per run",
	"OSClass":                       "computed: the host",
	"SourceState":                   "computed: whether the workspace is managed by Git, read once per run",
	"EmptiedOutputs":                "computed: the task's declared outputs",
	"Task":                          "computed: the plan",
	"TaskContractDigest":            "computed: the extension's manifest",
	"Project":                       "computed: the plan",
	"ProjectMetadataDigest":         "computed: the workspace snapshot, which the file index validates (ADR 0009)",
	"EmbeddedVersion":               "computed: the tree's version, before any key (D-010)",
	"SelectedProjects":              "computed: the run's selection",
	"Params":                        "computed: the manifests, the configuration and the command line",
	"FilePatterns":                  "recorded: the project files' digest, under the witness of ADR 0113 §1",
	"ConfigScope":                   "recorded: part of the digest's memo key, which keys the record",
	"InputExpander":                 "recorded: its identity is part of the digest's memo key; selector digests are collected until their witness lands",
	"ProjectRoot":                   "recorded: part of the digest's memo key",
	"WorkspaceFilePatterns":         "recorded: the workspace files' digest, under the witness of ADR 0113 §1",
	"WorkspaceRoot":                 "recorded: part of the digest's memo key",
	"EnvVars":                       "computed: the run's environment",
	"ExtraFiles":                    "computed: collected every run, with no witness",
	"UpstreamHashes":                "computed: the dependencies' keys, composed by the run",
}

func TestEveryKeyInputHasAWitness(t *testing.T) {
	fields := reflect.TypeOf(CacheKey{})
	named := map[string]bool{}
	for i := range fields.NumField() {
		field := fields.Field(i)
		if !field.IsExported() {
			continue
		}
		named[field.Name] = true
		if _, ok := keyInputWitnesses[field.Name]; !ok {
			t.Errorf("CacheKey.%s has no row in keyInputWitnesses: say how a run has it, computed or recorded under a witness (ADR 0113 §5)", field.Name)
		}
	}
	var stale []string
	for name := range keyInputWitnesses {
		if !named[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		t.Errorf("keyInputWitnesses names %s, which CacheKey no longer has", name)
	}
}

// TestEachWayToCollectFilesIsRecordedOrNot pins, for each way a key collects
// its files, whether the input record keeps its digest. A new way lands with
// its row: recorded under a witness this store takes, or collected every run.
func TestEachWayToCollectFilesIsRecordedOrNot(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "p")
	writeRecordTree(t, project, map[string]string{"src/a.go": "a", "README.md": "r", "x.json": "{}"})
	runGit(t, project, "init", "-q")
	cases := []struct {
		name     string
		patterns []string
		recorded bool
	}{
		{"a recursive walk", []string{"src/**/*.go"}, true},
		{"a glob in the last segment", []string{"*.json"}, true},
		{"a literal name", []string{"README.md"}, true},
		{"an exclusion", []string{"**/*", "!README.md"}, true},
		{"a missing prefix", []string{"missing/**"}, true},
		{"the empty pattern set (the default walk)", nil, false},
		{"Git candidates Git listed (ADR 0113 §3: its fallback)", []string{"git:**"}, false},
		{"an input selector (ADR 0113 §1: after its basis witness)", []string{"go:sources"}, false},
		{"a glob before the last segment", []string{"*/a.go"}, false},
		{"a character class", []string{"[sx]*.json"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			record := fileindex.OpenRecordAt(fileindex.RecordPath(t.TempDir()), "test", time.Now().Add(time.Hour).UnixNano())
			if !record.Recordable(statOf(t, project)) {
				t.Skip("this platform or filesystem keeps no input record")
			}
			recorded := false
			if key, ok := recordKey("k", c.patterns); ok {
				hash, witness, err := witnessedHashFiles(nil, project, c.patterns, nil, ProjectConfigScope{})
				if err != nil {
					t.Fatal(err)
				}
				recordInputDigest(record, key, hash, witness)
				_, _, recorded = record.Answer(InputDigestTable, key, record.Hold)
			}
			if recorded != c.recorded {
				t.Fatalf("recorded = %v, want %v", recorded, c.recorded)
			}
		})
	}
}

func statOf(t *testing.T, path string) fileindex.Stat {
	t.Helper()
	st, ok := fileindex.LstatAny(path)
	if !ok {
		if _, err := os.Lstat(path); err != nil {
			t.Fatal(err)
		}
		t.Skip("this platform's stat is not decoded")
	}
	return st
}
