package cachekey

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The golden task key.
//
// Every digest below was computed outside Go: the byte layouts written out in
// the comments were hashed with Python's hashlib and checked with shasum, so no
// expected value comes from the code under test. A change that moves one of
// them moves the key of every task with the same shape, which is a whole-cache
// miss for every user, so it is made on purpose or not at all.
const (
	// sha256 of {"format":"wp1","kind":"project","project":{"path":"app","type":"library","tags":["golden"]}}
	goldenProbeDigest = "wp1:00bae6c724f9038b1e46b8fe99ad67cff0c774d8ea5ffccc2d233b4728ecda96"
	// sha256 of "wsid1" NUL, the probe digest NUL, "dependencyNames" NUL,
	// "@golden/lib" NUL, "providerMetadata" NUL, "@putnami/golden" NUL,
	// {"a":"x","b":1} NUL.
	goldenMetadataDigest = "wsid1:d60eeff555c9ba3f6181c69f6140a5f1bc246c6caf1e2b79a45be1267d308564"
	// sha256 of "mode" NUL "\"release\"" NUL.
	goldenParamsDigest = "98fb01f8ca3d250e36d23212120bbb77b47559bc020d1f52d1e41a6f5e00f5a9"
	// sha256 of "src/main.txt" NUL, the 32 raw bytes of sha256("hello\n"), NUL.
	goldenFilesDigest = "713a142deec1fc31c4c4e542918d5762b6b5e2387ba0b8b09111182921fb1d1a"
	// goldenToolchain is the Go release putnami.lock.json pins. The toolchain
	// field of a key is the running Go release, so the full key below is the
	// one a CLI built with it computes.
	goldenToolchain = "go1.26.9"
	// sha256 of goldenKeyTranscript(goldenToolchain).
	goldenKey = "923e388b74cb7fc227ab24fc047e1ff08dac3fa61cfd62dfd23be29dd84de6e7"
)

// goldenKeyTranscript is the byte layout of the golden task's key, one field
// per line, each followed by NUL. Only the toolchain varies, because it is the
// Go release that runs the test.
func goldenKeyTranscript(toolchain string) []byte {
	var transcript []byte
	for _, field := range []string{
		"v9",                  // store.cacheKeyVersion
		"@putnami/golden",     // extension
		"1.2.3",               // extension version: no implementation digest follows
		"",                    // implementation digest
		toolchain,             // toolchain
		"sourceState",         // the source-state marker
		"unmanaged",           // store.SourceStateUnmanaged
		"build~compile",       // task
		"tc1:golden-contract", // task contract digest
		"app",                 // project name
		goldenMetadataDigest,  // project metadata digest
		"",                    // embedded version
		goldenParamsDigest,    // params
		goldenFilesDigest,     // declared files
		"upstream-golden",     // the dependency's key
	} {
		transcript = append(transcript, field...)
		transcript = append(transcript, 0)
	}
	return transcript
}

// TestGoldenTaskKey pins one full task key: a library project with a tag, an
// unresolved dependency and provider metadata, an installed extension keyed by
// its version, a bound param, one declared file beside an undeclared one, a
// dependency's key, and the source state of a root Git does not manage. The OS
// class is left out, so the key is the same on every host. It replaces package
// variables, so it does not run in parallel.
func TestGoldenTaskKey(t *testing.T) {
	keyAsHost(t, "")
	keyAsSourceState(t, store.SourceStateUnmanaged)

	sum := sha256.Sum256(goldenKeyTranscript(goldenToolchain))
	if got := hex.EncodeToString(sum[:]); got != goldenKey {
		t.Fatalf("transcript digest = %s, want %s: the transcript no longer spells the golden key", got, goldenKey)
	}

	root := t.TempDir()
	for rel, content := range map[string]string{
		"app/src/main.txt": "hello\n",
		"app/README.md":    "not a declared input\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	project := &workspace.Project{
		ID: "/app", Name: "app", Path: "app", Type: "library",
		Tags:         []string{"golden"},
		Dependencies: []string{"@golden/lib"},
		Metadata:     map[string]json.RawMessage{"@putnami/golden": json.RawMessage(`{"b":1,"a":"x"}`)},
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "golden"}, []*workspace.Project{project})
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(t.TempDir(), "store")))
	job := &jobmodel.ScheduledJob{
		Project: project,
		Extension: &extension.ExtensionDescription{
			Name:    "@putnami/golden",
			Version: "1.2.3",
			Tasks: map[string]extension.TaskDefinition{
				"compile": {Declares: &extension.TaskDeclaration{}},
			},
		},
		JobDef: &extension.JobDefinition{
			Name:           "build~compile",
			CommandName:    "build",
			ExtensionName:  "@putnami/golden",
			Cache:          true,
			ContractDigest: "tc1:golden-contract",
			BoundParams:    extension.ParamMap{"mode": "release"},
			TaskCachePolicy: &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{
				Files: []string{"src/*.txt"},
			}},
		},
		Step:      &extension.PipelineStep{Task: "compile"},
		DependsOn: []string{"/lib:build"},
	}

	// The parts first, so a moved key names the input that moved it.
	if got := wsproto.ProbeProjectDigest(workspace.ProbeViewOf(project, ws.DependencyPathResolver())); got != goldenProbeDigest {
		t.Errorf("probe digest = %s, want %s", got, goldenProbeDigest)
	}
	if got := ws.MetadataDigestFor(project); got != goldenMetadataDigest {
		t.Errorf("metadata digest = %s, want %s", got, goldenMetadataDigest)
	}
	files, err := cache.HashFiles(filepath.Join(root, "app"), []string{"src/*.txt"}, nil, JobConfigScope(ws, job))
	if err != nil {
		t.Fatal(err)
	}
	if files != goldenFilesDigest {
		t.Errorf("files digest = %s, want %s", files, goldenFilesDigest)
	}

	got, err := ComputeJobCacheHash(ws, job, nil, nil, cache, map[string]string{"/lib:build": "upstream-golden"})
	if err != nil {
		t.Fatal(err)
	}
	sum = sha256.Sum256(goldenKeyTranscript(runtime.Version()))
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("task key = %s, want %s, the golden transcript with toolchain %s", got, want, runtime.Version())
	}
	if runtime.Version() == goldenToolchain && got != goldenKey {
		t.Fatalf("task key = %s, want %s", got, goldenKey)
	}
}
