package store

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A workspace-wide digest keeps its entry across writes its patterns cannot
// select: a command output under .putnami, which the recursive walk skips, a
// generated tree its exclusion removes, and a file whose name no include
// matches. It is dropped by a write that adds, changes or removes a file it
// selects (tooling/cli ADR 0109).
func TestAWriteDropsAWitnessedDigestOnlyWhenItCanSelectTheWrite(t *testing.T) {
	patterns := []string{"**/putnami.json", "**/features/*.json", "putnami.workspace.json", "!**/.gen/**"}
	cases := []struct {
		name  string
		write func(t *testing.T, dir string) []string
		keeps bool
	}{
		{"a command output under .putnami", func(t *testing.T, dir string) []string {
			path := filepath.Join(dir, ".putnami", "out", "app", "~build", "putnami.json")
			writeRegionFile(t, path, "{}")
			return []string{filepath.Join(dir, ".putnami", "out", "app", "~build")}
		}, true},
		{"a generated tree the exclusion removes", func(t *testing.T, dir string) []string {
			writeRegionFile(t, filepath.Join(dir, "app", ".gen", "putnami.json"), "{}")
			return []string{filepath.Join(dir, "app", ".gen")}
		}, true},
		{"a file no include names", func(t *testing.T, dir string) []string {
			path := filepath.Join(dir, "app", "schema", "feature-evidence", "app.json")
			writeRegionFile(t, path, "{}")
			return []string{path}
		}, true},
		{"a removal of a tree that held no selected file", func(t *testing.T, dir string) []string {
			path := filepath.Join(dir, "app", "dist")
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			return []string{path}
		}, true},
		{"a new file an include names", func(t *testing.T, dir string) []string {
			path := filepath.Join(dir, "app", "sub", "putnami.json")
			writeRegionFile(t, path, "{}")
			return []string{filepath.Join(dir, "app", "sub")}
		}, false},
		{"a change to a selected file", func(t *testing.T, dir string) []string {
			path := filepath.Join(dir, "app", "putnami.json")
			writeRegionFile(t, path, `{"name":"changed"}`)
			return []string{path}
		}, false},
		{"a removal of a tree that held a selected file", func(t *testing.T, dir string) []string {
			path := filepath.Join(dir, "app", "features")
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			return []string{path}
		}, false},
		{"a link the write leaves", func(t *testing.T, dir string) []string {
			path := filepath.Join(dir, "app", "schema", "linked.txt")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "app", "putnami.json"), path); err != nil {
				t.Skip("symbolic links unavailable:", err)
			}
			return []string{path}
		}, false},
		{"a write through a link", func(t *testing.T, dir string) []string {
			link := filepath.Join(dir, "elsewhere")
			if err := os.Symlink(filepath.Join(dir, "app"), link); err != nil {
				t.Skip("symbolic links unavailable:", err)
			}
			path := filepath.Join(link, "notes.txt")
			writeRegionFile(t, path, "notes")
			return []string{path}
		}, false},
		{"a write above the digest's directory", func(t *testing.T, dir string) []string {
			return []string{filepath.Dir(dir)}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := witnessTree(t)
			cm := NewCacheManager(nil)
			before, err := cm.HashFiles(dir, patterns, nil, ProjectConfigScope{})
			if err != nil {
				t.Fatal(err)
			}
			cm.InvalidateFileHashesUnder(tc.write(t, dir))
			_, _, kept := cm.fileHashes.lookup(memoKey(dir, patterns))
			if kept != tc.keeps {
				t.Fatalf("kept its entry = %v, want %v", kept, tc.keeps)
			}
			after, err := cm.HashFiles(dir, patterns, nil, ProjectConfigScope{})
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := hashFiles(dir, patterns, nil, ProjectConfigScope{})
			if err != nil {
				t.Fatal(err)
			}
			if after != fresh {
				t.Fatalf("the memo answered %s after the write, a fresh digest is %s (was %s)", after, fresh, before)
			}
		})
	}
}

// A digest whose selection the names alone do not decide keeps the region
// rule: every write its region reaches drops it.
func TestADigestWithoutAWitnessKeepsTheRegionRule(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		prepare  func(t *testing.T, dir string)
	}{
		{"a pattern with a parent segment", []string{"app/../**/putnami.json"}, nil},
		{"no pattern", nil, nil},
		{"a selected file read through a link", []string{"**/*.json"}, func(t *testing.T, dir string) {
			if err := os.Symlink(filepath.Join(dir, "app", "putnami.json"), filepath.Join(dir, "app", "linked.json")); err != nil {
				t.Skip("symbolic links unavailable:", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := witnessTree(t)
			if tc.prepare != nil {
				tc.prepare(t, dir)
			}
			cm := NewCacheManager(nil)
			if _, err := cm.HashFiles(dir, tc.patterns, nil, ProjectConfigScope{}); err != nil {
				t.Fatal(err)
			}
			cm.InvalidateFileHashesUnder([]string{filepath.Join(dir, ".putnami", "out", "report.txt")})
			if _, _, kept := cm.fileHashes.lookup(memoKey(dir, tc.patterns)); kept {
				t.Fatal("a digest without a witness kept its entry across a write its region reaches")
			}
		})
	}
}

// Whatever a run writes, a digest the memo answers with after the write's
// invalidation is the digest a fresh read of the tree gives. Random trees,
// patterns and writes, each seed reproducible.
func TestAWitnessedMemoNeverAnswersAStaleDigest(t *testing.T) {
	patternSets := [][]string{
		{"**/putnami.json"},
		{"**/*.json", "!**/.gen/**"},
		{"src/**/*.ts", "!**/*_test.ts"},
		{"*.json", "docs/*.md"},
		{"**/features/*.json", "putnami.workspace.json"},
		{"out/**", "dist/*.js"},
		{".gen/**"},
		{"**/*"},
	}
	names := []string{"putnami.json", "a.json", "b.ts", "a_test.ts", "x.md", "features", "src", "docs", ".gen", "out", "dist", ".putnami", "node_modules", "vendor", "c.js"}
	for seed := int64(1); seed <= 60; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			dir := t.TempDir()
			randomPath := func() string {
				depth := 1 + rng.Intn(4)
				parts := []string{dir}
				for range depth {
					parts = append(parts, names[rng.Intn(len(names))])
				}
				return filepath.Join(parts...)
			}
			write := func(path, content string) bool {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return false
				}
				return os.WriteFile(path, []byte(content), 0o644) == nil
			}
			for range 40 {
				write(randomPath(), "seed")
			}
			cm := NewCacheManager(nil)
			for _, patterns := range patternSets {
				if _, err := cm.HashFiles(dir, patterns, nil, ProjectConfigScope{}); err != nil {
					t.Fatal(err)
				}
			}
			for step := range 30 {
				path := randomPath()
				var written string
				switch rng.Intn(4) {
				case 0: // a file
					if !write(path, fmt.Sprintf("step %d", step)) {
						continue
					}
					written = path
				case 1: // a tree, named by its root
					if !write(filepath.Join(path, names[rng.Intn(len(names))]), "tree") {
						continue
					}
					written = path
				case 2: // a removal
					if err := os.RemoveAll(path); err != nil {
						continue
					}
					written = path
				default: // a link
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						continue
					}
					if os.Symlink(randomPath(), path) != nil {
						continue
					}
					written = path
				}
				cm.InvalidateFileHashesUnder([]string{written})
				for _, patterns := range patternSets {
					memo, err := cm.HashFiles(dir, patterns, nil, ProjectConfigScope{})
					if err != nil {
						t.Fatal(err)
					}
					fresh, err := hashFiles(dir, patterns, nil, ProjectConfigScope{})
					if err != nil {
						t.Fatal(err)
					}
					if memo != fresh {
						t.Fatalf("step %d: after a write of %s the memo answered a stale digest for %v", step, written, patterns)
					}
				}
			}
		})
	}
}

// witnessTree writes a small workspace: two project configs, a feature, a
// generated tree and a distribution directory.
func witnessTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeRegionFile(t, filepath.Join(dir, "putnami.workspace.json"), "{}")
	writeRegionFile(t, filepath.Join(dir, "app", "putnami.json"), `{"name":"app"}`)
	writeRegionFile(t, filepath.Join(dir, "app", "features", "login.json"), "{}")
	writeRegionFile(t, filepath.Join(dir, "app", ".gen", "conf.yaml"), "a: 1")
	writeRegionFile(t, filepath.Join(dir, "app", "dist", "index.js"), "")
	writeRegionFile(t, filepath.Join(dir, "lib", "putnami.json"), `{"name":"lib"}`)
	return dir
}

// memoKey is the key lookupFileHash memoizes a digest of patterns under dir
// at, with the zero scope and no expander.
func memoKey(dir string, patterns []string) string {
	key := dir + "|"
	for i, pattern := range patterns {
		if i > 0 {
			key += ","
		}
		key += pattern
	}
	return key + "||"
}

// emulatedCut lists every regular file and link under dir but those below a
// directory named .gen or .putnami, as Git lists the candidates of a work tree
// that ignores both.
func emulatedCut(dir string) CandidateSource {
	return func(asked string) (CandidateCut, bool) {
		if asked != dir {
			return CandidateCut{}, false
		}
		read := CandidateCut{Modes: map[string]string{}, Unmerged: map[string]bool{}}
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if d.Name() == ".gen" || d.Name() == ".putnami" {
					return filepath.SkipDir
				}
				return nil
			}
			rel, _ := filepath.Rel(dir, p)
			rel = filepath.ToSlash(rel)
			read.Paths = append(read.Paths, rel)
			read.Modes[rel] = "100644"
			return nil
		})
		return read, true
	}
}

// With the run's cut, a digest of Git candidates a write leaves pending is
// confirmed only by a cut that lists what its own read listed: whatever the
// writes, the memo answers the digest a fresh read gives.
func TestAPendingCandidateDigestIsTheFreshDigest(t *testing.T) {
	patternSets := [][]string{{"git:**"}, {"git:**", "src/**"}, {"git:**", "!**/*.md"}}
	names := []string{"putnami.json", "a.md", "src", ".gen", ".putnami", "docs", "b.txt"}
	for seed := int64(1); seed <= 40; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			dir := t.TempDir()
			randomPath := func() string {
				parts := []string{dir}
				for range 1 + rng.Intn(3) {
					parts = append(parts, names[rng.Intn(len(names))])
				}
				return filepath.Join(parts...)
			}
			write := func(path string) bool {
				if os.MkdirAll(filepath.Dir(path), 0o755) != nil {
					return false
				}
				return os.WriteFile(path, []byte(fmt.Sprint(rng.Int())), 0o644) == nil
			}
			for range 20 {
				write(randomPath())
			}
			cm := NewCacheManager(nil)
			cm.SetCandidateSource(emulatedCut(dir))
			for _, patterns := range patternSets {
				if _, err := cm.HashFiles(dir, patterns, nil, ProjectConfigScope{}); err != nil {
					t.Fatal(err)
				}
			}
			for step := range 25 {
				path := randomPath()
				if rng.Intn(3) == 0 {
					if os.RemoveAll(path) != nil {
						continue
					}
				} else if !write(path) {
					continue
				}
				cm.InvalidateFileHashesUnder([]string{path})
				for _, patterns := range patternSets {
					memo, err := cm.HashFiles(dir, patterns, nil, ProjectConfigScope{})
					if err != nil {
						t.Fatal(err)
					}
					fresh := NewCacheManager(nil)
					fresh.SetCandidateSource(emulatedCut(dir))
					want, err := fresh.HashFiles(dir, patterns, nil, ProjectConfigScope{})
					if err != nil {
						t.Fatal(err)
					}
					if memo != want {
						t.Fatalf("step %d: after a write of %s the memo answered a stale digest for %v", step, path, patterns)
					}
				}
			}
		})
	}
}

// The shapes the review of ADR 0109 found a witness kept a digest a write
// moved: each write here moves the digest, and the memo answers the digest a
// fresh read gives.
func TestAWitnessMovesWithTheShapesItOnceMissed(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		prepare  func(t *testing.T, dir string)
		write    func(t *testing.T, dir string) string
		cut      func(dir string) CandidateSource
	}{
		{
			name:     "a tracked candidate deleted, then restored",
			patterns: []string{"git:**"},
			prepare: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, "docs", "a.md")); err != nil {
					t.Fatal(err)
				}
			},
			write: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "docs", "a.md")
				writeRegionFile(t, path, "restored")
				return path
			},
			cut: func(dir string) CandidateSource {
				return func(asked string) (CandidateCut, bool) {
					read := CandidateCut{Paths: []string{"docs/a.md", "keep.txt"},
						Modes: map[string]string{"docs/a.md": "100644", "keep.txt": "100644"}, Unmerged: map[string]bool{}}
					return read, asked == dir
				}
			},
		},
		{
			name:     "a tracked candidate Git lists, deleted, then restored",
			patterns: []string{"git:**/*.md"},
			prepare: func(t *testing.T, dir string) {
				for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}} {
					cmd := exec.Command("git", args...)
					cmd.Dir = dir
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Skipf("git %v: %v %s", args, err, out)
					}
				}
				if err := os.Remove(filepath.Join(dir, "docs", "a.md")); err != nil {
					t.Fatal(err)
				}
			},
			write: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "docs", "a.md")
				writeRegionFile(t, path, "restored")
				return path
			},
		},
		{
			name:     "a pattern whose static prefix is a link",
			patterns: []string{"lib/*.go"},
			prepare: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "real"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "lib")); err != nil {
					t.Skip("symbolic links unavailable:", err)
				}
			},
			write: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "real", "x.go")
				writeRegionFile(t, path, "package lib")
				return path
			},
		},
		{
			name:     "a glob before the last segment of a pattern Glob expands through a link",
			patterns: []string{"lnk*/x.go"},
			prepare: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "real"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "lnk1")); err != nil {
					t.Skip("symbolic links unavailable:", err)
				}
			},
			write: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "real", "x.go")
				writeRegionFile(t, path, "package lib")
				return path
			},
		},
		{
			name:     "a pattern that starts with ./",
			patterns: []string{"./src/**/*.ts"},
			write: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "src", "new.ts")
				writeRegionFile(t, path, "export {}")
				return path
			},
		},
		{
			name:     "a link to a file a later write creates",
			patterns: []string{"*.go"},
			prepare: func(t *testing.T, dir string) {
				if err := os.Symlink(filepath.Join(dir, "gen", "a.txt"), filepath.Join(dir, "a.go")); err != nil {
					t.Skip("symbolic links unavailable:", err)
				}
			},
			write: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "gen", "a.txt")
				writeRegionFile(t, path, "package gen")
				return path
			},
		},
		{
			name:     "a selected link whose target lies outside the region",
			patterns: []string{"src/**/*.go"},
			prepare: func(t *testing.T, dir string) {
				writeRegionFile(t, filepath.Join(dir, "gen", "a.txt"), "package gen")
				if err := os.Symlink(filepath.Join(dir, "gen", "a.txt"), filepath.Join(dir, "src", "a.go")); err != nil {
					t.Skip("symbolic links unavailable:", err)
				}
			},
			write: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "gen", "a.txt")
				writeRegionFile(t, path, "package gen // changed")
				return path
			},
		},
		{
			name:     "a glob that reaches a file through a directory link",
			patterns: []string{"src/*/*.go"},
			prepare: func(t *testing.T, dir string) {
				writeRegionFile(t, filepath.Join(dir, "gen", "a.go"), "package gen")
				if err := os.Symlink(filepath.Join(dir, "gen"), filepath.Join(dir, "src", "x")); err != nil {
					t.Skip("symbolic links unavailable:", err)
				}
			},
			write: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "gen", "a.go")
				writeRegionFile(t, path, "package gen // changed")
				return path
			},
		},
		{
			name:     "a .gitignore removed and named by its own path",
			patterns: []string{"git:**/*.md"},
			prepare: func(t *testing.T, dir string) {
				writeRegionFile(t, filepath.Join(dir, "docs", "sub", ".gitignore"), "x.md\n")
				writeRegionFile(t, filepath.Join(dir, "docs", "sub", "x.md"), "ignored until the rules go")
				cmd := exec.Command("git", "init", "-q")
				cmd.Dir = dir
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Skipf("git init: %v %s", err, out)
				}
			},
			write: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "docs", "sub", ".gitignore")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name:     "a glob that reaches a directory link whose target holds no match yet",
			patterns: []string{"src/*/*.go"},
			prepare: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "gen"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(dir, "gen"), filepath.Join(dir, "src", "x")); err != nil {
					t.Skip("symbolic links unavailable:", err)
				}
			},
			write: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "gen", "a.go")
				writeRegionFile(t, path, "package gen")
				return path
			},
		},
		{
			name:     "a file at the root of a recursive pattern",
			patterns: []string{"gen/**"},
			write: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "gen")
				writeRegionFile(t, path, "a file named gen")
				return path
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeRegionFile(t, filepath.Join(dir, "docs", "a.md"), "tracked")
			writeRegionFile(t, filepath.Join(dir, "keep.txt"), "kept")
			writeRegionFile(t, filepath.Join(dir, "src", "old.ts"), "export {}")
			if tc.prepare != nil {
				tc.prepare(t, dir)
			}
			cm := NewCacheManager(nil)
			if tc.cut != nil {
				cm.SetCandidateSource(tc.cut(dir))
			}
			before, err := cm.HashFiles(dir, tc.patterns, nil, ProjectConfigScope{})
			if err != nil {
				t.Fatal(err)
			}
			cm.InvalidateFileHashesUnder([]string{tc.write(t, dir)})
			got, err := cm.HashFiles(dir, tc.patterns, nil, ProjectConfigScope{})
			if err != nil {
				t.Fatal(err)
			}
			fresh := NewCacheManager(nil)
			if tc.cut != nil {
				fresh.SetCandidateSource(tc.cut(dir))
			}
			want, err := fresh.HashFiles(dir, tc.patterns, nil, ProjectConfigScope{})
			if err != nil {
				t.Fatal(err)
			}
			if got != want || got == before {
				t.Fatalf("after the write the memo answered %s, a fresh read gives %s (was %s)", got, want, before)
			}
		})
	}
}
