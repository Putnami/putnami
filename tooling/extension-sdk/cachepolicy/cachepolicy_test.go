package cachepolicy

import (
	"go.putnami.dev/protocol/features/spectest"

	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path string, size int, modTime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// TestCollect_EvictsOldestToLowWatermark is the core arithmetic: eviction stops
// at 80% of the budget rather than at the budget, and the oldest entries go
// first.
func TestCollect_EvictsOldestToLowWatermark(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "cache-eviction-policy", "collection-evicts-oldest-first-to-the-low-watermark")
	root := t.TempDir()
	now := time.Now()
	candidates := make([]Entry, 0, 10)
	for i := range 10 {
		path := filepath.Join(root, "entry-"+string(rune('a'+i)))
		age := time.Duration(10-i) * time.Hour
		writeFile(t, path, 100, now.Add(-age))
		candidates = append(candidates, Entry{Path: path, Size: 100, LastUsed: now.Add(-age)})
	}

	res, err := Collect(root, 1000, candidates, Options{MaxBytes: 500, Now: now})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// Low watermark is 400; 1000 → 400 needs 6 entries of 100 bytes gone.
	if res.EvictedEntries != 6 {
		t.Fatalf("evicted %d entries, want 6 (1000 → 400 low watermark)", res.EvictedEntries)
	}
	if res.FreedBytes != 600 || res.RemainingBytes != 400 {
		t.Fatalf("freed %d / remaining %d, want 600 / 400", res.FreedBytes, res.RemainingBytes)
	}
	// The six oldest (a..f) are gone; the four newest survive.
	for i := range 10 {
		path := filepath.Join(root, "entry-"+string(rune('a'+i)))
		_, err := os.Stat(path)
		if i < 6 && err == nil {
			t.Errorf("%s survived but is among the six oldest", path)
		}
		if i >= 6 && err != nil {
			t.Errorf("%s was evicted but is among the four newest", path)
		}
	}
}

// TestCollect_GraceProtectsRecentEntries is the invariant that keeps a
// background collection from breaking a running build: an entry inside the
// window is never removed, even when that leaves the cache over budget.
func TestCollect_GraceProtectsRecentEntries(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "cache-eviction-policy", "entries-inside-the-grace-window-are-protected")
	root := t.TempDir()
	now := time.Now()
	candidates := make([]Entry, 0, 10)
	for i := range 10 {
		path := filepath.Join(root, "fresh-"+string(rune('a'+i)))
		writeFile(t, path, 100, now.Add(-time.Minute))
		candidates = append(candidates, Entry{Path: path, Size: 100, LastUsed: now.Add(-time.Minute)})
	}

	res, err := Collect(root, 1000, candidates, Options{MaxBytes: 500, Grace: time.Hour, Now: now})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.EvictedEntries != 0 || res.FreedBytes != 0 {
		t.Fatalf("evicted %d entries (%d bytes) inside the grace window", res.EvictedEntries, res.FreedBytes)
	}
	if !res.OverBudget(500) {
		t.Fatal("a cache left over budget by grace protection must report OverBudget")
	}
}

// TestSelectVictims_FloorSurvivesAnUnevictableMajority is the pathological case
// the floor exists for: unevictable bytes alone exceed the low watermark, so an
// unfloored target is negative and every candidate is selected.
func TestSelectVictims_FloorSurvivesAnUnevictableMajority(t *testing.T) {
	now := time.Now()
	candidates := make([]Entry, 0, 10)
	for i := range 10 {
		candidates = append(candidates, Entry{
			Path:     filepath.Join("/cache", "e"+string(rune('a'+i))),
			Size:     100,
			LastUsed: now.Add(-time.Duration(100-i) * time.Hour),
		})
	}
	// 9000 unevictable bytes, 1000 evictable, budget 1000 (low watermark 800).
	unfloored := SelectVictims(candidates, 10000, Options{MaxBytes: 1000, Now: now})
	if len(unfloored) != 10 {
		t.Fatalf("without a floor %d victims were selected, want all 10 (the case the floor exists for)",
			len(unfloored))
	}
	floored := SelectVictims(candidates, 10000, Options{MaxBytes: 1000, FloorBytes: 200, Now: now})
	if len(floored) != 8 {
		t.Fatalf("with a 200-byte floor %d victims were selected, want 8", len(floored))
	}
}

// TestSelectVictims_Deterministic pins the tie-break: two runs over the same
// cache must choose the same victims, so a collection is reviewable.
func TestSelectVictims_Deterministic(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "cache-eviction-policy", "victim-selection-is-deterministic-for-identical-inputs")
	now := time.Now()
	same := now.Add(-48 * time.Hour)
	candidates := []Entry{
		{Path: "/cache/z", Size: 100, LastUsed: same},
		{Path: "/cache/a", Size: 100, LastUsed: same},
		{Path: "/cache/m", Size: 100, LastUsed: same},
	}
	first := SelectVictims(candidates, 300, Options{MaxBytes: 100, Now: now})
	second := SelectVictims(candidates, 300, Options{MaxBytes: 100, Now: now})
	if len(first) != len(second) {
		t.Fatalf("victim counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Path != second[i].Path {
			t.Fatalf("victim %d differs: %q vs %q", i, first[i].Path, second[i].Path)
		}
	}
	if len(first) > 0 && first[0].Path != "/cache/a" {
		t.Fatalf("first victim = %q, want the lexicographically smallest tied path", first[0].Path)
	}
	// SelectVictims must not reorder the caller's slice underneath it.
	if candidates[0].Path != "/cache/z" {
		t.Fatalf("caller's slice was reordered: %q", candidates[0].Path)
	}
}

// TestCollect_NoBudgetIsNotAZeroBudget: an unset budget must collect NOTHING.
func TestCollect_NoBudgetIsNotAZeroBudget(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "cache-eviction-policy", "an-absent-budget-is-unbounded-not-zero")
	root := t.TempDir()
	path := filepath.Join(root, "entry")
	writeFile(t, path, 100, time.Now().Add(-100*time.Hour))
	candidates := []Entry{{Path: path, Size: 100, LastUsed: time.Now().Add(-100 * time.Hour)}}

	res, err := Collect(root, 100, candidates, Options{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.EvictedEntries != 0 {
		t.Fatal("an unset budget collected entries")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("entry removed under an unset budget: %v", err)
	}
}

func TestCollect_MissingRootIsNotAnError(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "cache-eviction-policy", "a-missing-cache-root-is-success")
	res, err := Collect(filepath.Join(t.TempDir(), "absent"), 0, nil, Options{MaxBytes: 1})
	if err != nil {
		t.Fatalf("Collect on a missing root: %v", err)
	}
	if res == nil || res.EvictedEntries != 0 {
		t.Fatalf("unexpected result %+v", res)
	}
}

// TestCollect_NonBlockingSkipsAContendedRoot proves the lock is real and that a
// contended pass reports Skipped rather than failing.
func TestCollect_NonBlockingSkipsAContendedRoot(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "cache-eviction-policy", "a-root-another-collector-holds-is-skipped-not-blocked-on")
	root := t.TempDir()
	held, err := Acquire(root, false)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = held.Release() }()

	path := filepath.Join(root, "entry")
	writeFile(t, path, 100, time.Now().Add(-100*time.Hour))
	candidates := []Entry{{Path: path, Size: 100, LastUsed: time.Now().Add(-100 * time.Hour)}}

	res, err := Collect(root, 1000, candidates, Options{MaxBytes: 100, NonBlocking: true})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !res.Skipped {
		t.Fatal("a contended non-blocking pass must report Skipped")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a skipped pass removed an entry: %v", err)
	}
}

func TestRemoveTreeAndTreeSize(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tree")
	writeFile(t, filepath.Join(root, "a", "one"), 10, time.Now())
	writeFile(t, filepath.Join(root, "b", "two"), 20, time.Now())

	if got := TreeSize(root); got != 30 {
		t.Fatalf("TreeSize = %d, want 30", got)
	}
	if got := RemoveTree(root); got != 30 {
		t.Fatalf("RemoveTree = %d, want 30", got)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("tree survived RemoveTree: %v", err)
	}
	if got := RemoveTree(root); got != 0 {
		t.Fatalf("RemoveTree on a missing tree = %d, want 0", got)
	}
}

func TestPruneEmptyDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b", "c"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(root, "keep", "file"), 1, time.Now())

	PruneEmptyDirs(root)

	if _, err := os.Stat(filepath.Join(root, "a")); !os.IsNotExist(err) {
		t.Fatalf("empty subtree survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "keep", "file")); err != nil {
		t.Fatalf("non-empty subtree was pruned: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("root itself was pruned: %v", err)
	}
}

// TestWriteSummary pins the wire format core reads freed bytes back from.
func TestWriteSummary(t *testing.T) {
	var out bytes.Buffer
	if err := WriteSummary(&out, 4096); err != nil {
		t.Fatalf("WriteSummary: %v", err)
	}
	line := strings.TrimSpace(out.String())
	var event struct {
		V    int            `json:"v"`
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		t.Fatalf("summary is not JSON: %v (%q)", err, line)
	}
	if event.V != 1 || event.Type != "summary" {
		t.Fatalf("summary envelope = v%d/%q, want v1/summary", event.V, event.Type)
	}
	if got, ok := event.Data["freedBytes"].(float64); !ok || int64(got) != 4096 {
		t.Fatalf("freedBytes = %v, want 4096", event.Data["freedBytes"])
	}
}

func TestEnvHelpers(t *testing.T) {
	env := map[string]string{
		"SET":     "2048",
		"BAD":     "not-a-number",
		"ZERO":    "0",
		"DUR":     "45m",
		"ZERODUR": "0s",
		"BADDUR":  "later",
		"NEGDUR":  "-5m",
	}
	getenv := func(name string) string { return env[name] }

	if got := EnvInt64(getenv, "SET", 10); got != 2048 {
		t.Errorf("EnvInt64(SET) = %d, want 2048", got)
	}
	for _, name := range []string{"BAD", "ZERO", "ABSENT"} {
		if got := EnvInt64(getenv, name, 10); got != 10 {
			t.Errorf("EnvInt64(%s) = %d, want the fallback 10", name, got)
		}
	}
	if got := EnvDuration(getenv, "DUR", time.Hour); got != 45*time.Minute {
		t.Errorf("EnvDuration(DUR) = %v, want 45m", got)
	}
	if got := EnvDuration(getenv, "ZERODUR", time.Hour); got != 0 {
		t.Errorf("EnvDuration(ZERODUR) = %v, want 0 (an explicit opt-out)", got)
	}
	for _, name := range []string{"BADDUR", "NEGDUR", "ABSENT"} {
		if got := EnvDuration(getenv, name, time.Hour); got != time.Hour {
			t.Errorf("EnvDuration(%s) = %v, want the fallback 1h", name, got)
		}
	}
	if got := EnvInt64(nil, "SET", 7); got != 7 {
		t.Errorf("EnvInt64 with a nil getenv = %d, want 7", got)
	}
	if got := EnvDuration(nil, "DUR", time.Hour); got != time.Hour {
		t.Errorf("EnvDuration with a nil getenv = %v, want 1h", got)
	}
}
