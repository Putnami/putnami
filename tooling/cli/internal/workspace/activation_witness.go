package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// ActivationWitnessFilename is the file, under a workspace's `.putnami/`, that
// keeps the plan's step activation answers with their witnesses between runs:
// a run whose witnesses hold answers a step's gate with a stat per path
// instead of a walk and a read per file (the directory rule of CLI ADR 0113
// §2, the probe witness's rule, applied to activation by CLI ADR 0132).
const ActivationWitnessFilename = "activation-witness.json"

// activationWitnessVersion is the file's format.
const activationWitnessVersion = 1

type persistedActivations struct {
	Version int `json:"version"`
	// Program is the identity of the CLI that wrote the file: a file another
	// build wrote is not read, since its answers may follow another reading.
	Program string `json:"program"`
	// Stamp is the clock, in Unix nanoseconds, when the file was written: a
	// file whose stamp lies ahead of the clock is not read.
	Stamp   int64                 `json:"stamp"`
	Paths   []persistedPath       `json:"paths"`
	Answers []persistedActivation `json:"answers"`
}

type persistedActivation struct {
	Key    string `json:"key"`
	Active bool   `json:"active"`
	Paths  []int  `json:"paths"`
}

// ActivationProbe collects what one activation answer read: the directories
// it listed, the files whose content it read, and the nearest existing
// directory of each path it found absent. A probe that met anything a stat
// cannot answer for (a link, another file system, an unreadable path) holds
// nothing, and its answer is not kept.
type ActivationProbe struct {
	root    string
	dev     uint64
	trusted bool
	start   time.Time
	paths   map[string]fileindex.Stat
}

// NewActivationProbe starts a probe for an answer read under the workspace
// root. The clock is read before the first stat: a path whose time lies within
// the file index's margin of it may change again with an equal stat.
func NewActivationProbe(root string) *ActivationProbe {
	p := &ActivationProbe{root: root, paths: map[string]fileindex.Stat{}, start: time.Now().Round(0)}
	st, ok := fileindex.LstatAny(root)
	p.trusted = ok && root != "" && fileindex.TrustsStats(root)
	p.dev = st.Dev
	return p
}

// Dir records a directory the answer listed.
func (p *ActivationProbe) Dir(dir string) {
	if p == nil || !p.trusted {
		return
	}
	st, ok := fileindex.LstatAny(dir)
	if !ok || st.Dev != p.dev || st.Mode&modeType != modeDirectory {
		p.trusted = false
		return
	}
	p.paths[filepath.Clean(dir)] = st
}

// File records a file the answer read, or a path it found absent, by the
// stat of the nearest directory that exists.
func (p *ActivationProbe) File(path string) {
	if p == nil || !p.trusted {
		return
	}
	st, ok := fileindex.LstatAny(path)
	if !ok {
		p.Absent(path)
		return
	}
	if st.Dev != p.dev || st.Mode&modeType == modeLink {
		p.trusted = false
		return
	}
	p.paths[filepath.Clean(path)] = st
}

// Absent records that path does not exist, by the stat of its nearest
// existing ancestor directory: creating the path changes that directory.
func (p *ActivationProbe) Absent(path string) {
	if p == nil || !p.trusted {
		return
	}
	dir := filepath.Dir(filepath.Clean(path))
	for {
		if st, ok := fileindex.LstatAny(dir); ok {
			if st.Mode&modeType != modeDirectory {
				// A file where a directory would be: the path cannot be
				// created without changing it, which its stat records.
				p.File(dir)
				return
			}
			p.Dir(dir)
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir || !strings.HasPrefix(dir, p.root) {
			p.trusted = false
			return
		}
		dir = parent
	}
}

// Untrusted marks the probe as unable to hold its answer.
func (p *ActivationProbe) Untrusted() {
	if p != nil {
		p.trusted = false
	}
}

// modeLink is the file type bits of a symbolic link.
const modeLink = 0o120000

type activationWitness struct {
	active bool
	paths  map[string]fileindex.Stat
}

func (w *activationWitness) holds() bool {
	for path, recorded := range w.paths {
		if now, ok := fileindex.LstatAny(path); !ok || now != recorded {
			return false
		}
	}
	return true
}

type activationFile struct {
	once    sync.Once
	mu      sync.Mutex
	dirty   bool
	held    map[string]*activationWitness
	used    map[string]bool
	program string
}

var activationFiles sync.Map // root → *activationFile

func activationFileOf(root string) *activationFile {
	held, _ := activationFiles.LoadOrStore(root, &activationFile{})
	f, _ := held.(*activationFile)
	f.once.Do(func() { f.load(root) })
	return f
}

func (f *activationFile) load(root string) {
	f.held = map[string]*activationWitness{}
	f.used = map[string]bool{}
	f.program = fileindex.ExecutableIdentity()
	if f.program == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(root, ".putnami", ActivationWitnessFilename)) //nolint:gosec // the workspace's own state file
	if err != nil {
		return
	}
	var file persistedActivations
	if json.Unmarshal(data, &file) != nil || file.Version != activationWitnessVersion || file.Program != f.program ||
		file.Stamp <= 0 || file.Stamp > time.Now().UnixNano() {
		return
	}
	for _, answer := range file.Answers {
		w := &activationWitness{active: answer.Active, paths: make(map[string]fileindex.Stat, len(answer.Paths))}
		valid := len(answer.Paths) > 0
		for _, i := range answer.Paths {
			if i < 0 || i >= len(file.Paths) {
				valid = false
				break
			}
			p := file.Paths[i]
			w.paths[filepath.Join(root, filepath.FromSlash(p.Path))] = p.Stat
		}
		if valid {
			f.held[answer.Key] = w
		}
	}
}

// HeldActivation answers key from its witness when every stat it recorded
// still holds; ok is false otherwise, and the caller evaluates the gate.
func HeldActivation(root, key string) (active, ok bool) {
	if root == "" {
		return false, false
	}
	f := activationFileOf(root)
	f.mu.Lock()
	w := f.held[key]
	f.used[key] = true
	f.mu.Unlock()
	if w == nil || !w.holds() {
		return false, false
	}
	activationHolds.Add(1)
	return w.active, true
}

// activationHolds counts the answers read from their witnesses, for tests.
var activationHolds atomic.Int64

// ActivationHolds is the number of activation answers this process read from
// their witnesses.
func ActivationHolds() int64 { return activationHolds.Load() }

// RecordActivation keeps key's answer with the witness probe collected, when
// the probe holds it: every path it recorded still has its stat, and lies more
// than the file index's margin before the probe's start.
func RecordActivation(root, key string, active bool, probe *ActivationProbe) {
	if root == "" || probe == nil || !probe.trusted || len(probe.paths) == 0 {
		return
	}
	if time.Now().Round(0).Before(probe.start) {
		return
	}
	limit := probe.start.Add(-fileindex.Margin).UnixNano()
	for path, recorded := range probe.paths {
		now, ok := fileindex.LstatAny(path)
		if !ok || now != recorded || recorded.ModNano >= limit || recorded.ChangeNano >= limit {
			return
		}
	}
	w := &activationWitness{active: active, paths: probe.paths}
	f := activationFileOf(root)
	f.mu.Lock()
	f.held[key] = w
	f.used[key] = true
	f.dirty = true
	f.mu.Unlock()
}

// SaveActivationWitnesses writes root's file when this process recorded an
// answer the file did not hold, keeping only the answers this process used:
// an unchanged run whose answers all held writes nothing.
func SaveActivationWitnesses(root string, policy SnapshotWritePolicy) {
	if root == "" || !policy.Persists() {
		return
	}
	held, ok := activationFiles.Load(root)
	if !ok {
		return
	}
	f, _ := held.(*activationFile)
	f.mu.Lock()
	dirty := f.dirty
	f.dirty = false
	keys := make([]string, 0, len(f.used))
	for key := range f.used {
		if f.held[key] != nil {
			keys = append(keys, key)
		}
	}
	answers := make(map[string]*activationWitness, len(keys))
	for _, key := range keys {
		answers[key] = f.held[key]
	}
	f.mu.Unlock()
	if !dirty || f.program == "" {
		return
	}
	sort.Strings(keys)
	file := persistedActivations{Version: activationWitnessVersion, Program: f.program, Stamp: time.Now().UnixNano()}
	index := map[string]int{}
	for _, key := range keys {
		answer, ok := persistActivation(root, key, answers[key], index, &file)
		if ok {
			file.Answers = append(file.Answers, answer)
		}
	}
	data, err := json.Marshal(file)
	if err != nil {
		return
	}
	dir := filepath.Join(root, ".putnami")
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the workspace's state directory
		return
	}
	temp, err := os.CreateTemp(dir, "."+ActivationWitnessFilename+".*")
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
	_ = os.Rename(name, filepath.Join(dir, ActivationWitnessFilename))
}

func persistActivation(root, key string, w *activationWitness, index map[string]int, file *persistedActivations) (persistedActivation, bool) {
	rels := make(map[string]fileindex.Stat, len(w.paths))
	for abs, st := range w.paths {
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return persistedActivation{}, false
		}
		rels[filepath.ToSlash(rel)] = st
	}
	names := make([]string, 0, len(rels))
	for rel := range rels {
		names = append(names, rel)
	}
	sort.Strings(names)
	answer := persistedActivation{Key: key, Active: w.active}
	for _, rel := range names {
		i, seen := index[rel]
		if !seen {
			i = len(file.Paths)
			index[rel] = i
			file.Paths = append(file.Paths, persistedPath{Path: rel, Stat: rels[rel]})
		}
		answer.Paths = append(answer.Paths, i)
	}
	sort.Ints(answer.Paths)
	return answer, true
}

// forgetActivationWitnesses drops every answer this process holds, for tests.
func forgetActivationWitnesses() {
	activationFiles.Range(func(key, _ any) bool {
		activationFiles.Delete(key)
		return true
	})
}
