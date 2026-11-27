package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// GlobWitnessFilename is the file, under a workspace's `.putnami/`, that keeps
// the probe's recursive glob inputs with their witnesses between runs: a run
// whose witnesses hold validates those inputs with a stat per path instead of
// a walk and a digest per match (the directory rule of CLI ADR 0113 §2, under
// ADR 0009 §2 and §3 of protocols/workspace).
const GlobWitnessFilename = "probe-witness.json"

// globWitnessVersion is the file's format.
const globWitnessVersion = 2

// persistedWitnesses is the file: paths are shared by every input, which
// names them by index.
type persistedWitnesses struct {
	Version int `json:"version"`
	// Program is the identity of the CLI that wrote the file (fileindex
	// ExecutableIdentity): a file another build wrote is not read, since its
	// digests may follow another reading.
	Program string `json:"program"`
	// Stamp is the clock, in Unix nanoseconds, when the file was written: a
	// file whose stamp lies ahead of the clock is not read, since the clock
	// went back and a stat may repeat (ADR 0009 §4 of protocols/workspace).
	Stamp int64 `json:"stamp"`
	// Ignored is the git-ignored set the inputs' walks skipped.
	Ignored string             `json:"ignored"`
	Paths   []persistedPath    `json:"paths"`
	Inputs  []persistedPattern `json:"inputs"`
}

type persistedPath struct {
	Path string         `json:"p"`
	Stat fileindex.Stat `json:"s"`
}

type persistedPattern struct {
	Pattern string `json:"pattern"`
	Digest  string `json:"digest"`
	Paths   []int  `json:"paths"`
}

// witnessFiles tracks, per workspace root, whether its file was read and
// whether this process recorded a witness the file does not hold.
var witnessFiles sync.Map // root → *witnessFile

type witnessFile struct {
	once  sync.Once
	mu    sync.Mutex
	dirty bool
	// used are the patterns this process collected or answered: the file
	// keeps those only, so a pattern no longer read leaves it.
	used map[string]bool
}

func witnessFileOf(root string) *witnessFile {
	held, _ := witnessFiles.LoadOrStore(root, &witnessFile{})
	if f, ok := held.(*witnessFile); ok {
		return f
	}
	return &witnessFile{}
}

// loadGlobWitnesses reads root's file once per process into globWitnesses,
// leaving any witness this process already holds.
func loadGlobWitnesses(root string) {
	witnessFileOf(root).once.Do(func() {
		program := fileindex.ExecutableIdentity()
		if program == "" {
			return
		}
		data, err := os.ReadFile(filepath.Join(root, ".putnami", GlobWitnessFilename))
		if err != nil {
			return
		}
		var file persistedWitnesses
		if json.Unmarshal(data, &file) != nil || file.Version != globWitnessVersion || file.Program != program ||
			file.Stamp <= 0 || file.Stamp > time.Now().UnixNano() {
			return
		}
		for _, input := range file.Inputs {
			w := &globWitness{ignored: file.Ignored, digest: input.Digest, paths: make(map[string]fileindex.Stat, len(input.Paths))}
			valid, directory := true, false
			for _, i := range input.Paths {
				if i < 0 || i >= len(file.Paths) {
					valid = false
					break
				}
				p := file.Paths[i]
				w.paths[filepath.Join(root, filepath.FromSlash(p.Path))] = p.Stat
				directory = directory || p.Stat.Mode&modeType == modeDirectory
			}
			// A witness holds its walk's search root at least: one without a
			// directory holds nothing.
			if valid && directory {
				globWitnesses.LoadOrStore(root+"\x00"+input.Pattern, w)
			}
		}
	})
}

// modeType and modeDirectory are the file type bits of a stat's mode and
// their value for a directory.
const (
	modeType      = 0o170000
	modeDirectory = 0o040000
)

// markGlobWitnessesDirty records that root holds a witness its file does not.
func markGlobWitnessesDirty(root string) {
	f := witnessFileOf(root)
	f.mu.Lock()
	f.dirty = true
	f.mu.Unlock()
}

// useGlobWitness records that this process read pattern's input in root.
func useGlobWitness(root, pattern string) {
	f := witnessFileOf(root)
	f.mu.Lock()
	if f.used == nil {
		f.used = map[string]bool{}
	}
	f.used[pattern] = true
	f.mu.Unlock()
}

// SaveGlobWitnesses writes root's file when this process recorded a witness
// it did not hold: an unchanged run whose witnesses were read from it writes
// nothing. The file holds the witnesses of the git-ignored set this run read.
func SaveGlobWitnesses(root string, policy SnapshotWritePolicy) {
	f := witnessFileOf(root)
	f.mu.Lock()
	dirty := f.dirty
	f.dirty = false
	used := make(map[string]bool, len(f.used))
	for pattern := range f.used {
		used[pattern] = true
	}
	f.mu.Unlock()
	program := fileindex.ExecutableIdentity()
	if !dirty || !policy.Persists() || program == "" {
		return
	}
	ignored := ignoredFingerprint(root)
	file := persistedWitnesses{Version: globWitnessVersion, Program: program, Stamp: time.Now().UnixNano(), Ignored: ignored}
	held := map[string]*globWitness{}
	prefix := root + "\x00"
	globWitnesses.Range(func(key, value any) bool {
		name, _ := key.(string)
		w, ok := value.(*globWitness)
		if ok && strings.HasPrefix(name, prefix) && w.ignored == ignored && used[strings.TrimPrefix(name, prefix)] {
			held[strings.TrimPrefix(name, prefix)] = w
		}
		return true
	})
	patterns := make([]string, 0, len(held))
	for pattern := range held {
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)
	index := map[string]int{}
	for _, pattern := range patterns {
		if input, ok := persistWitness(root, pattern, held[pattern], index, &file); ok {
			file.Inputs = append(file.Inputs, input)
		}
	}
	data, err := json.Marshal(file)
	if err != nil {
		return
	}
	dir := filepath.Join(root, ".putnami")
	temp, err := os.CreateTemp(dir, "."+GlobWitnessFilename+".*")
	if err != nil {
		return
	}
	name := temp.Name()
	defer os.Remove(name) //nolint:errcheck // gone once renamed
	if _, err := temp.Write(data); err != nil {
		temp.Close() //nolint:errcheck,gosec // the write already failed
		return
	}
	if temp.Close() != nil {
		return
	}
	_ = os.Rename(name, filepath.Join(dir, GlobWitnessFilename))
}

// persistWitness is w as the file keeps it, its paths named by their index in
// file.Paths, which it extends in sorted order. A witness with a path outside
// root is not kept.
func persistWitness(root, pattern string, w *globWitness, index map[string]int, file *persistedWitnesses) (persistedPattern, bool) {
	rels := make(map[string]fileindex.Stat, len(w.paths))
	for abs, st := range w.paths {
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return persistedPattern{}, false
		}
		rels[filepath.ToSlash(rel)] = st
	}
	names := make([]string, 0, len(rels))
	for rel := range rels {
		names = append(names, rel)
	}
	sort.Strings(names)
	input := persistedPattern{Pattern: pattern, Digest: w.digest}
	for _, rel := range names {
		i, seen := index[rel]
		if !seen {
			i = len(file.Paths)
			index[rel] = i
			file.Paths = append(file.Paths, persistedPath{Path: rel, Stat: rels[rel]})
		}
		input.Paths = append(input.Paths, i)
	}
	sort.Ints(input.Paths)
	return input, true
}
