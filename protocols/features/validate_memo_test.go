package features

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// memoManifests is every manifest the fixtures and this project's own
// declarations parse to, by name.
func memoManifests(t *testing.T) map[string]*Manifest {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("fixtures", "*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, "putnami.features.json")
	manifests := map[string]*Manifest{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if manifest, _ := ParseManifest(data); manifest != nil {
			manifests[path] = manifest
		}
	}
	if len(manifests) < 10 {
		t.Fatalf("%d manifests parsed from the fixtures; the corpus moved", len(manifests))
	}
	return manifests
}

// TestAMemoizedManifestValidationAnswersWhatAFreshOneAnswers pins the memo's
// contract: over every fixture, a miss and a hit both return what a fresh
// validation returns, the same findings in the same order, nil when there
// are none.
func TestAMemoizedManifestValidationAnswersWhatAFreshOneAnswers(t *testing.T) {
	withErrors := 0
	for name, manifest := range memoManifests(t) {
		fresh := validateManifest(manifest)
		if diag.HasErrors(fresh) {
			withErrors++
		}
		for _, pass := range []string{"first", "second"} {
			if got := ValidateManifest(manifest); !reflect.DeepEqual(got, fresh) {
				t.Errorf("%s, %s call: findings = %v, want %v", name, pass, got, fresh)
			}
		}
	}
	if withErrors == 0 {
		t.Fatal("no fixture manifest has an error; the comparison proves nothing about findings")
	}
}

// TestAChangedManifestIsValidatedAgain changes one field at a time of a
// manifest the memo already answered. Each change gets its own answer, the
// one a fresh validation gives, and the original keeps its own.
func TestAChangedManifestIsValidatedAgain(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("fixtures", "valid", "features-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	original, diagnostics := ParseManifest(data)
	if original == nil || len(diagnostics) != 0 {
		t.Fatalf("parse the valid fixture: %v", diagnostics)
	}
	if findings := ValidateManifest(original); len(findings) != 0 {
		t.Fatalf("the valid fixture has findings: %v", findings)
	}
	changes := map[string]func(*Manifest){
		"namespace":                func(m *Manifest) { m.Namespace = "Other" },
		"protocol version":         func(m *Manifest) { m.ProtocolVersion = 99 },
		"features nil":             func(m *Manifest) { m.Features = nil },
		"feature owner bytes":      func(m *Manifest) { m.Features[0].Owner = "team\x00" },
		"duplicate feature":        func(m *Manifest) { m.Features = append(m.Features, m.Features[0]) },
		"requirement stage":        func(m *Manifest) { m.Features[0].Requirements[0].Stage = "unknown" },
		"requirement evidence nil": func(m *Manifest) { m.Features[0].Requirements[0].EvidenceKinds = nil },
	}
	for name, change := range changes {
		changed := cloneManifestForTest(t, data)
		change(changed)
		fresh := validateManifest(changed)
		if len(fresh) == 0 {
			t.Fatalf("%s: the change has no finding; it does not test a changed answer", name)
		}
		if got := ValidateManifest(changed); !reflect.DeepEqual(got, fresh) {
			t.Errorf("%s: findings = %v, want %v", name, got, fresh)
		}
	}
	if findings := ValidateManifest(original); len(findings) != 0 {
		t.Errorf("the original manifest now has findings: %v", findings)
	}
}

func cloneManifestForTest(t *testing.T, data []byte) *Manifest {
	t.Helper()
	manifest, diagnostics := ParseManifest(data)
	if manifest == nil || len(diagnostics) != 0 || len(manifest.Features) == 0 || len(manifest.Features[0].Requirements) == 0 {
		t.Fatalf("parse the valid fixture: %v", diagnostics)
	}
	return manifest
}

// TestAMemoizedAnswerIsTheCallersOwn pins that the memo hands out no shared
// state: a caller that rewrites its findings, as a caller that prefixes a
// field does, changes neither the memo's answer nor another caller's.
func TestAMemoizedAnswerIsTheCallersOwn(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("fixtures", "valid", "features-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := cloneManifestForTest(t, data)
	manifest.Namespace = "Shared"
	fresh := validateManifest(manifest)
	first := ValidateManifest(manifest)
	second := ValidateManifest(manifest)
	if len(first) == 0 || len(second) == 0 {
		t.Fatal("the manifest has no finding to rewrite")
	}
	first[0].Field = "rewritten." + first[0].Field
	if !reflect.DeepEqual(second, fresh) {
		t.Fatalf("a rewrite of one answer reached another: %v", second)
	}
	if third := ValidateManifest(manifest); !reflect.DeepEqual(third, fresh) {
		t.Fatalf("a rewrite of one answer reached the memo: %v", third)
	}
}

// TestConcurrentValidationsAnswerWhatAFreshOneAnswers runs workers over the
// same manifests at once, as the workspace feature validation does.
func TestConcurrentValidationsAnswerWhatAFreshOneAnswers(t *testing.T) {
	manifests := memoManifests(t)
	want := map[string][]diag.Diagnostic{}
	for name, manifest := range manifests {
		want[name] = validateManifest(manifest)
	}
	var wg sync.WaitGroup
	errs := make(chan string, 8*len(manifests))
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name, manifest := range manifests {
				if got := ValidateManifest(manifest); !reflect.DeepEqual(got, want[name]) {
					errs <- name
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for name := range errs {
		t.Errorf("%s: a concurrent validation answered differently", name)
	}
}

// TestTheManifestMemoIsBounded pins the bound: past its capacity the memo
// starts over instead of growing.
func TestTheManifestMemoIsBounded(t *testing.T) {
	memo := newFindingsMemo(2)
	for i := 0; i < 5; i++ {
		key, ok := contentKey("test", &Manifest{ProtocolVersion: i})
		if !ok {
			t.Fatal("a manifest has no content key")
		}
		memo.findings(key, func() []diag.Diagnostic { return nil })
		if held := len(memo.entries); held > 2 {
			t.Fatalf("the memo holds %d entries, capacity 2", held)
		}
	}
}

// TestTheContentKeyCoversEveryDifference pins what the key holds: each field,
// a nil apart from an empty list, the exact bytes of a string, an unexported
// field, and the type. A value it cannot encode has no key, so it is
// validated fresh.
func TestTheContentKeyCoversEveryDifference(t *testing.T) {
	type inner struct {
		hidden string
		List   []string
	}
	values := map[string]any{
		"empty":           &inner{},
		"hidden":          &inner{hidden: "x"},
		"nil list":        &inner{List: nil, hidden: "y"},
		"empty list":      &inner{List: []string{}, hidden: "y"},
		"replacement":     &inner{List: []string{"�"}},
		"invalid utf-8":   &inner{List: []string{"\xff"}},
		"split strings":   &inner{List: []string{"ab", "c"}},
		"joined strings":  &inner{List: []string{"a", "bc"}},
		"manifest":        &Manifest{},
		"feature surface": &FeatureSurface{},
	}
	seen := map[[32]byte]string{}
	for name, value := range values {
		key, ok := contentKey("test", value)
		if !ok {
			t.Fatalf("%s: no content key", name)
		}
		if other, taken := seen[key]; taken {
			t.Errorf("%s and %s share a content key", name, other)
		}
		seen[key] = name
	}
	if again, _ := contentKey("test", &inner{hidden: "x"}); seen[again] != "hidden" {
		t.Error("equal content keys differently")
	}
	if _, ok := contentKey("test", &struct{ M map[string]string }{M: map[string]string{}}); ok {
		t.Error("a map got a content key; its order is not encoded")
	}
	if _, ok := contentKey("test", &struct{ V any }{V: "x"}); ok {
		t.Error("an interface got a content key")
	}
}
