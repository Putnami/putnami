package machineproc

import (
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Journal is what changed in one worktree since a reader's sequence number
// (CLI ADR 0116 §7). A path the journal names was written, created, removed
// or renamed; a broken subtree may have changed in any way, and every path
// below it is taken by stat. A path neither named nor below a broken subtree
// has held since the sequence the reader gave, on a watch that never broke.
type Journal struct {
	// Seq is the sequence number to read from next time.
	Seq uint64
	// Changed are the paths that changed, sorted, absolute.
	Changed []string
	// Broken are the subtrees whose events may have been lost, sorted,
	// absolute. The worktree's root among them breaks every path.
	Broken []string
}

// Holds reports whether path held since the journal's start: no change named
// it and no broken subtree holds it.
func (j Journal) Holds(path string) bool {
	path = filepath.Clean(path)
	for _, b := range j.Broken {
		if within(path, b) {
			return false
		}
	}
	i := sort.SearchStrings(j.Changed, path)
	return i >= len(j.Changed) || j.Changed[i] != path
}

// within reports whether path is dir or lies below it.
func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// journalEntry is one record: a path that changed, or a subtree that broke.
type journalEntry struct {
	seq    uint64
	path   string
	broken bool
}

// journal keeps a worktree's records. It keeps at most maxJournal entries;
// a reader whose sequence lies before the oldest kept one reads the whole
// worktree as broken.
type journal struct {
	mu      sync.Mutex
	root    string
	seq     uint64
	entries []journalEntry
	// floor is the sequence before which entries were dropped.
	floor uint64
}

// maxJournal bounds the records a journal keeps between two reads.
const maxJournal = 1 << 16

func newJournal(root string) *journal {
	return &journal{root: filepath.Clean(root)}
}

func (j *journal) record(path string, broken bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seq++
	j.entries = append(j.entries, journalEntry{seq: j.seq, path: filepath.Clean(path), broken: broken})
	if len(j.entries) > maxJournal {
		drop := len(j.entries) - maxJournal
		j.floor = j.entries[drop-1].seq
		j.entries = append([]journalEntry(nil), j.entries[drop:]...)
	}
}

// current is the journal's last sequence number.
func (j *journal) current() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.seq
}

// since is the journal from seq, exclusive, to now.
func (j *journal) since(seq uint64) Journal {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := Journal{Seq: j.seq}
	if seq < j.floor {
		out.Broken = []string{j.root}
		return out
	}
	changed, broken := map[string]bool{}, map[string]bool{}
	for _, e := range j.entries {
		if e.seq <= seq {
			continue
		}
		if e.broken {
			broken[e.path] = true
		} else {
			changed[e.path] = true
		}
	}
	out.Changed = sortedKeys(changed)
	out.Broken = sortedKeys(broken)
	return out
}

func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
