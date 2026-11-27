package fileindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests that follow replace statFile, so none of them runs in parallel.

// testIdentity is the program identity the tests open their indexes under.
const testIdentity = "fileindex test"

func requireStat(t *testing.T) {
	t.Helper()
	if !statDecoded {
		t.Skip("this platform's stat is not decoded: the index records nothing")
	}
}

// isClockProbe reports whether info is the file readClock creates.
func isClockProbe(info os.FileInfo) bool {
	return strings.HasPrefix(info.Name(), ".file-index-clock.")
}

// withStat replaces statFile with adjust applied to every file but the clock
// probe, for the duration of the test.
func withStat(t *testing.T, adjust func(Stat) Stat) {
	t.Helper()
	previous := statFile
	statFile = func(info os.FileInfo) (Stat, bool) {
		st, ok := statOf(info)
		if ok && !isClockProbe(info) {
			st = adjust(st)
		}
		return st, ok
	}
	t.Cleanup(func() { statFile = previous })
}

// aged makes every file but the clock probe read as written an hour earlier,
// as a file of a checkout older than the run is.
func aged(t *testing.T) {
	withStat(t, func(st Stat) Stat {
		st.ModNano -= int64(time.Hour)
		st.ChangeNano -= int64(time.Hour)
		return st
	})
}

// coarse makes every file, the clock probe included, keep times on a tick of
// one hour, as a filesystem with a coarse modification time does: writes in
// one hour share their timestamps.
func coarse(t *testing.T) {
	t.Helper()
	previous := statFile
	tick := int64(time.Hour)
	statFile = func(info os.FileInfo) (Stat, bool) {
		st, ok := statOf(info)
		st.ModNano -= st.ModNano % tick
		st.ChangeNano -= st.ChangeNano % tick
		return st, ok
	}
	t.Cleanup(func() { statFile = previous })
}

// laterTick waits until a write gets a later status-change time than the file
// at path has. The kernel stamps times from a coarse clock, one tick of up to
// 10 ms on many kernels, so two writes a test makes back to back share them.
func laterTick(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	was, _ := statOf(info)
	probe := filepath.Join(filepath.Dir(path), ".tick")
	defer os.Remove(probe) //nolint:errcheck // a probe file
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if err := os.WriteFile(probe, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(probe)
		if err != nil {
			t.Fatal(err)
		}
		if now, _ := statOf(info); now.ChangeNano > was.ChangeNano {
			return
		}
	}
	t.Fatal("the status-change clock did not move in 5 s")
}

type fixture struct {
	t     *testing.T
	dir   string
	index string
}

func newFixture(t *testing.T) *fixture {
	requireStat(t)
	dir := t.TempDir()
	return &fixture{t: t, dir: dir, index: filepath.Join(dir, ".putnami", Filename)}
}

func (f *fixture) write(name, content string) string {
	f.t.Helper()
	path := filepath.Join(f.dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func sum(content string) []byte {
	s := sha256.Sum256([]byte(content))
	return s[:]
}

func reader(path string) func() ([]byte, error) {
	return func() ([]byte, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		s := sha256.Sum256(data)
		return s[:], nil
	}
}

// digest reads path through idx and fails unless it answers want.
func digest(t *testing.T, idx *Index, path, want string) {
	t.Helper()
	got, err := idx.Digest(path, "sha256", reader(path))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sum(want)) {
		t.Fatalf("digest of %s answers for other content than %q", path, want)
	}
}

// run opens the index, reads paths through it, saves it, and returns the
// contents it read.
func (f *fixture) run(want map[string]string) int64 {
	f.t.Helper()
	idx := Open(f.index, testIdentity)
	if !idx.enabled() {
		f.t.Fatal("the index did not read its clock")
	}
	for path, content := range want {
		digest(f.t, idx, path, content)
	}
	if err := idx.Save(); err != nil {
		f.t.Fatal(err)
	}
	return idx.Reads()
}

func TestAnUnchangedFileIsNotReadAgain(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	b := f.write("b.txt", "bravo")
	files := map[string]string{a: "alpha", b: "bravo"}

	if reads := f.run(files); reads != 2 {
		t.Fatalf("first run read %d files, want 2", reads)
	}
	before := ContentReads()
	if reads := f.run(files); reads != 0 {
		t.Fatalf("a run of unchanged files read %d, want 0", reads)
	}
	if ContentReads() != before {
		t.Fatalf("the process counted %d content reads on an unchanged run", ContentReads()-before)
	}
}

func TestASameTickRewriteOfTheSameSizeIsReadAgain(t *testing.T) {
	f := newFixture(t)
	coarse(t)
	a := f.write("a.txt", "aaaa")
	if reads := f.run(map[string]string{a: "aaaa"}); reads != 1 {
		t.Fatalf("first run read %d files, want 1", reads)
	}
	// Same size, same tick: device, inode, mode, size and both times are what
	// the first run saw. Only the tick rule tells the index not to answer.
	f.write("a.txt", "bbbb")
	if reads := f.run(map[string]string{a: "bbbb"}); reads != 1 {
		t.Fatalf("the run after a same-tick rewrite read %d files, want 1", reads)
	}
}

func TestATickOlderThanTheIndexClockIsTrusted(t *testing.T) {
	// The control of the same-tick test: the same coarse clock, with the file
	// written a tick before the index's clock, is recorded and answers.
	f := newFixture(t)
	tick := int64(time.Hour)
	previous := statFile
	statFile = func(info os.FileInfo) (Stat, bool) {
		st, ok := statOf(info)
		st.ModNano -= st.ModNano % tick
		st.ChangeNano -= st.ChangeNano % tick
		if !isClockProbe(info) {
			st.ModNano -= tick
			st.ChangeNano -= tick
		}
		return st, ok
	}
	t.Cleanup(func() { statFile = previous })

	a := f.write("a.txt", "aaaa")
	f.run(map[string]string{a: "aaaa"})
	if reads := f.run(map[string]string{a: "aaaa"}); reads != 0 {
		t.Fatalf("a file a tick older than the index was read %d times, want 0", reads)
	}
}

func TestAChangedStatIsReadAgain(t *testing.T) {
	cases := map[string]func(f *fixture, path string){
		"content of another size": func(f *fixture, _ string) { f.write("a.txt", "a longer alpha") },
		"modification time set back": func(_ *fixture, path string) {
			old := time.Now().Add(-48 * time.Hour)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		},
		"modification time restored after a same-size rewrite": func(f *fixture, path string) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			// aged dates the first write an hour back; a real rewrite is at
			// least one kernel tick after it, as it is for a file the index
			// recorded, which is more than Margin old.
			laterTick(t, path)
			f.write("a.txt", "omega")
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		},
		"replaced by a rename with the same size and time": func(f *fixture, path string) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			other := f.write("other.txt", "omega")
			if err := os.Chtimes(other, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(other, path); err != nil {
				t.Fatal(err)
			}
		},
		"permissions changed": func(_ *fixture, path string) {
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			aged(t)
			a := f.write("a.txt", "alpha")
			f.run(map[string]string{a: "alpha"})
			change(f, a)
			content, err := os.ReadFile(a)
			if err != nil {
				t.Fatal(err)
			}
			if reads := f.run(map[string]string{a: string(content)}); reads != 1 {
				t.Fatalf("the run after the change read %d files, want 1", reads)
			}
		})
	}
}

func TestWhatTheIndexNeverRecords(t *testing.T) {
	cases := map[string]func(t *testing.T, f *fixture) string{
		"a symbolic link": func(t *testing.T, f *fixture) string {
			aged(t)
			target := f.write("target.txt", "alpha")
			link := filepath.Join(f.dir, "link.txt")
			if err := os.Symlink(target, link); err != nil {
				t.Skip("no symbolic links here:", err)
			}
			return link
		},
		"a file on another filesystem": func(t *testing.T, f *fixture) string {
			withStat(t, func(st Stat) Stat {
				st.Dev++
				st.ModNano -= int64(time.Hour)
				st.ChangeNano -= int64(time.Hour)
				return st
			})
			return f.write("a.txt", "alpha")
		},
		"a modification time ahead of the clock": func(t *testing.T, f *fixture) string {
			withStat(t, func(st Stat) Stat {
				st.ModNano += int64(time.Hour)
				st.ChangeNano -= int64(time.Hour)
				return st
			})
			return f.write("a.txt", "alpha")
		},
		"a status change within the margin": func(t *testing.T, f *fixture) string {
			withStat(t, func(st Stat) Stat {
				st.ModNano -= int64(time.Hour)
				return st
			})
			return f.write("a.txt", "alpha")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			path := setup(t, f)
			f.run(map[string]string{path: "alpha"})
			if reads := f.run(map[string]string{path: "alpha"}); reads != 1 {
				t.Fatalf("the second run read %d files, want 1", reads)
			}
		})
	}
}

func TestAFileWrittenWhileItIsReadIsNotRecorded(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	idx := Open(f.index, testIdentity)
	_, err := idx.Digest(a, "sha256", func() ([]byte, error) {
		s := sha256.Sum256([]byte("alpha"))
		f.write("a.txt", "a longer alpha")
		return s[:], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.fresh) != 0 {
		t.Fatalf("a file written while it was read was recorded")
	}
}

func TestADamagedIndexReadsEveryFile(t *testing.T) {
	damage := map[string]func([]byte) []byte{
		"a flipped byte":      func(b []byte) []byte { b[len(b)/2] ^= 0xff; return b },
		"a truncated file":    func(b []byte) []byte { return b[:len(b)-7] },
		"an empty file":       func([]byte) []byte { return nil },
		"another format":      func(b []byte) []byte { return append([]byte("PNFI\x09\x00\x00\x00"), b[8:]...) },
		"garbage":             func([]byte) []byte { return []byte("{\"not\": \"an index\"}") },
		"a trailing appendix": func(b []byte) []byte { return append(b, 0) },
	}
	for name, corrupt := range damage {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			aged(t)
			a := f.write("a.txt", "alpha")
			f.run(map[string]string{a: "alpha"})
			data, err := os.ReadFile(f.index)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.index, corrupt(data), 0o644); err != nil {
				t.Fatal(err)
			}
			if reads := f.run(map[string]string{a: "alpha"}); reads != 1 {
				t.Fatalf("a run on a damaged index read %d files, want 1", reads)
			}
		})
	}
}

func TestADamagedEntryIsNeverTrusted(t *testing.T) {
	// A checksum that matches a forged body is not damage the index can see,
	// but a body it cannot parse is: the index rejects it whole.
	f := newFixture(t)
	encoded := encode(file{identity: testIdentity, stamp: 1, entries: map[key]entry{{path: "x", variant: "sha256"}: {}}})
	body := encoded[:len(encoded)-DigestSize-1]
	s := sha256.Sum256(body)
	if err := os.MkdirAll(filepath.Dir(f.index), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.index, append(body, s[:]...), 0o644); err != nil {
		t.Fatal(err)
	}
	if idx := Open(f.index, testIdentity); len(idx.loaded) != 0 {
		t.Fatalf("an index with a short entry loaded %d entries", len(idx.loaded))
	}
}

func TestAClockThatWentBackDiscardsTheIndex(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	f.run(map[string]string{a: "alpha"})

	idx := Open(f.index, testIdentity)
	data := encode(file{identity: testIdentity, stamp: idx.clock + int64(time.Hour), entries: idx.loaded})
	if err := os.WriteFile(f.index, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if reads := f.run(map[string]string{a: "alpha"}); reads != 1 {
		t.Fatalf("a run after the clock went back read %d files, want 1", reads)
	}
}

func TestAClockThatWentBackDuringARunRecordsNothing(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	idx := Open(f.index, testIdentity)
	idx.clock += int64(time.Hour)
	digest(t, idx, a, "alpha")
	if err := idx.Save(); err != nil {
		t.Fatal(err)
	}
	if reads := f.run(map[string]string{a: "alpha"}); reads != 1 {
		t.Fatalf("a run after a run whose clock went back read %d files, want 1", reads)
	}
}

func TestVariantsNeverAnswerForEachOther(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	idx := Open(f.index, testIdentity)
	other := sha256.Sum256([]byte("decoded"))
	if _, err := idx.Digest(a, "decoded", func() ([]byte, error) { return other[:], nil }); err != nil {
		t.Fatal(err)
	}
	digest(t, idx, a, "alpha")
	if idx.Reads() != 2 {
		t.Fatalf("two variants of one file read %d times, want 2", idx.Reads())
	}
}

func TestConcurrentRunsShareOneIndex(t *testing.T) {
	f := newFixture(t)
	aged(t)
	files := map[string]string{}
	for i := range 40 {
		name := string(rune('a'+i%26)) + strings.Repeat("x", i/26) + ".txt"
		files[f.write(name, name)] = name
	}
	first, second := Open(f.index, testIdentity), Open(f.index, testIdentity)
	var wg sync.WaitGroup
	i := 0
	for path, content := range files {
		idx := first
		if i%2 == 1 {
			idx = second
		}
		i++
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := idx.Digest(path, "sha256", reader(path))
			if err != nil || !bytes.Equal(got, sum(content)) {
				t.Errorf("digest of %s = %x, %v", path, got, err)
			}
		}()
	}
	wg.Wait()
	var saves sync.WaitGroup
	for _, idx := range []*Index{first, second} {
		saves.Add(1)
		go func() {
			defer saves.Done()
			if err := idx.Save(); err != nil {
				t.Error(err)
			}
		}()
	}
	saves.Wait()
	if reads := f.run(files); reads != 0 {
		t.Fatalf("after two concurrent runs, a run read %d files, want 0", reads)
	}
}

func TestAnEntryThatWentStaleIsDropped(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	b := f.write("b.txt", "bravo")
	f.run(map[string]string{a: "alpha", b: "bravo"})
	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}
	idx := Open(f.index, testIdentity)
	if _, err := idx.Digest(b, "sha256", reader(b)); err == nil {
		t.Fatal("a removed file has a digest")
	}
	if err := idx.Save(); err != nil {
		t.Fatal(err)
	}
	if loaded := Open(f.index, testIdentity).loaded; len(loaded) != 1 {
		t.Fatalf("the index holds %d entries after one of two files was removed, want 1", len(loaded))
	}
}

func TestNoIndexReadsEveryFile(t *testing.T) {
	f := newFixture(t)
	a := f.write("a.txt", "alpha")
	before := ContentReads()
	for range 2 {
		got, err := Digest(a, "sha256", reader(a))
		if err != nil || !bytes.Equal(got, sum("alpha")) {
			t.Fatalf("Digest = %x, %v", got, err)
		}
	}
	if n := ContentReads() - before; n != 2 {
		t.Fatalf("with no index, two digests read %d files, want 2", n)
	}
}

func TestARelativePathIsNeverRecorded(t *testing.T) {
	f := newFixture(t)
	aged(t)
	f.write("a.txt", "alpha")
	t.Chdir(f.dir)
	idx := Open(f.index, testIdentity)
	digest(t, idx, "a.txt", "alpha")
	if len(idx.fresh) != 0 {
		t.Fatal("a relative path was recorded: it names another file from another directory")
	}
}

func TestAnIndexOfAnotherProgramLoadsEmpty(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	f.run(map[string]string{a: "alpha"})
	idx := Open(f.index, "another program")
	digest(t, idx, a, "alpha")
	if idx.Reads() != 1 {
		t.Fatalf("an index written by another program answered: %d reads, want 1", idx.Reads())
	}
}

func TestAReadOnlyIndexAnswersAndWritesNothing(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	idx := OpenReadOnly(f.index, testIdentity)
	digest(t, idx, a, "alpha")
	if err := idx.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(f.index)); !os.IsNotExist(err) {
		t.Fatalf("a read-only index created its directory: %v", err)
	}
	f.run(map[string]string{a: "alpha"})
	idx = OpenReadOnly(f.index, testIdentity)
	digest(t, idx, a, "alpha")
	if idx.Reads() != 0 || len(idx.fresh) != 0 {
		t.Fatalf("a read-only index read %d files and recorded %d, want an answer from the file", idx.Reads(), len(idx.fresh))
	}
	entries, err := os.ReadDir(filepath.Dir(f.index))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".file-index-clock.") {
			t.Fatalf("a read-only index left %s", entry.Name())
		}
	}
}

func TestAnotherFilesystemRecordsNothing(t *testing.T) {
	f := newFixture(t)
	aged(t)
	previous := onLocalFilesystem
	onLocalFilesystem = func(string) bool { return false }
	t.Cleanup(func() { onLocalFilesystem = previous })
	a := f.write("a.txt", "alpha")
	for range 2 {
		idx := Open(f.index, testIdentity)
		digest(t, idx, a, "alpha")
		if err := idx.Save(); err != nil {
			t.Fatal(err)
		}
		if idx.Reads() != 1 {
			t.Fatalf("an index on a filesystem off the list answered: %d reads, want 1", idx.Reads())
		}
	}
}

func TestUseRestoresOnlyItsOwnIndex(t *testing.T) {
	a, b := &Index{}, &Index{}
	restoreA := Use(a)
	restoreB := Use(b)
	restoreA()
	if Current() != b {
		t.Fatal("restoring an index that is no longer current replaced the current one")
	}
	restoreB()
	if Current() != a {
		t.Fatal("restoring the current index did not put back the one before it")
	}
	Use(nil)
}

func TestNoIdentityAnswersAndRecordsNothing(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	for _, open := range []func(string, string) *Index{Open, OpenReadOnly} {
		idx := open(f.index, "")
		digest(t, idx, a, "alpha")
		digest(t, idx, a, "alpha")
		if err := idx.Save(); err != nil {
			t.Fatal(err)
		}
		if idx.Reads() != 2 || idx.enabled() {
			t.Fatalf("an index with no identity answered: %d reads of 2", idx.Reads())
		}
	}
	if _, err := os.Stat(f.index); !os.IsNotExist(err) {
		t.Fatalf("an index with no identity wrote its file: %v", err)
	}
}

// frozen makes every file but the clock probe keep the same old times, so a
// change shows only in the other members of its stat.
func frozen(t *testing.T) {
	old := time.Now().Add(-time.Hour).UnixNano()
	withStat(t, func(st Stat) Stat {
		st.ModNano, st.ChangeNano = old, old
		return st
	})
}

// onDisk decodes the index file at path.
func onDisk(t *testing.T, path string) file {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// forged is an entry that answers for a's current stat with the digest of
// other content.
func forged(t *testing.T, a string) entry {
	t.Helper()
	st, ok := lstat(a)
	if !ok {
		t.Fatal("no stat")
	}
	e := entry{stat: st}
	copy(e.digest[:], sum("forged"))
	return e
}

func TestTheMarginIsTwoSeconds(t *testing.T) {
	for _, tc := range []struct {
		age   time.Duration
		reads int64
	}{
		{2*time.Second - 100*time.Millisecond, 1},
		{2*time.Second + 100*time.Millisecond, 0},
	} {
		t.Run(tc.age.String(), func(t *testing.T) {
			f := newFixture(t)
			withStat(t, func(st Stat) Stat {
				st.ModNano -= int64(tc.age)
				st.ChangeNano -= int64(tc.age)
				return st
			})
			a := f.write("a.txt", "alpha")
			f.run(map[string]string{a: "alpha"})
			if reads := f.run(map[string]string{a: "alpha"}); reads != tc.reads {
				t.Fatalf("a file written %v before the clock: %d reads on the next run, want %d", tc.age, reads, tc.reads)
			}
		})
	}
}

func TestAnInodeOrModeChangeAloneIsReadAgain(t *testing.T) {
	for name, change := range map[string]func(f *fixture, path string){
		"inode": func(f *fixture, path string) {
			other := f.write("other.txt", "omega")
			if err := os.Rename(other, path); err != nil {
				t.Fatal(err)
			}
		},
		"mode": func(_ *fixture, path string) {
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			frozen(t)
			a := f.write("a.txt", "alpha")
			f.run(map[string]string{a: "alpha"})
			change(f, a)
			content, err := os.ReadFile(a)
			if err != nil {
				t.Fatal(err)
			}
			if reads := f.run(map[string]string{a: string(content)}); reads != 1 {
				t.Fatalf("a %s change with the same times read %d files, want 1", name, reads)
			}
		})
	}
}

func TestASaveNeverMergesAnotherProgramsIndex(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	b := f.write("b.txt", "bravo")
	if err := os.MkdirAll(filepath.Dir(f.index), 0o755); err != nil {
		t.Fatal(err)
	}
	other := encode(file{identity: "another program", stamp: 1, entries: map[key]entry{{path: a, variant: "sha256"}: forged(t, a)}})
	if err := os.WriteFile(f.index, other, 0o644); err != nil {
		t.Fatal(err)
	}
	f.run(map[string]string{b: "bravo"})
	if _, ok := onDisk(t, f.index).entries[key{path: a, variant: "sha256"}]; ok {
		t.Fatal("a save merged an entry another program recorded")
	}
	f.run(map[string]string{a: "alpha"})
}

func TestASaveNeverMergesAnIndexStampedAheadOfItsClock(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	b := f.write("b.txt", "bravo")
	if err := os.MkdirAll(filepath.Dir(f.index), 0o755); err != nil {
		t.Fatal(err)
	}
	ahead := encode(file{identity: testIdentity, stamp: time.Now().Add(time.Hour).UnixNano(), entries: map[key]entry{{path: a, variant: "sha256"}: forged(t, a)}})
	if err := os.WriteFile(f.index, ahead, 0o644); err != nil {
		t.Fatal(err)
	}
	f.run(map[string]string{b: "bravo"})
	disk := onDisk(t, f.index)
	if _, ok := disk.entries[key{path: a, variant: "sha256"}]; ok {
		t.Fatal("a save merged an index whose stamp lies ahead of its clock")
	}
	if disk.stamp > time.Now().UnixNano() {
		t.Fatal("a save kept a stamp ahead of the clock")
	}
}

func TestAClockThatWentBackDuringARunSavesNoEntry(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	idx := Open(f.index, testIdentity)
	idx.clock += int64(time.Hour)
	digest(t, idx, a, "alpha")
	if err := idx.Save(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(f.index); err == nil {
		if disk, decodeErr := decode(data); decodeErr == nil && len(disk.entries) != 0 {
			t.Fatalf("a run whose clock went back saved %d entries", len(disk.entries))
		}
	}
}

func TestAnEntryNoRunReadsIsDropped(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	f.run(map[string]string{a: "alpha"})
	for i := range keptRuns + 1 {
		b := f.write("b.txt", strings.Repeat("b", i+1))
		f.run(map[string]string{b: strings.Repeat("b", i+1)})
	}
	if _, ok := onDisk(t, f.index).entries[key{path: a, variant: "sha256"}]; ok {
		t.Fatalf("an entry no run read for %d saving runs was kept", keptRuns+1)
	}
}

func TestAnEntryAnUnchangedRunReadsIsKept(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	for range 2 * keptRuns {
		f.run(map[string]string{a: "alpha"})
		b := f.write("b.txt", "bravo")
		f.run(map[string]string{b: "bravo"})
		if err := os.Remove(b); err != nil {
			t.Fatal(err)
		}
	}
	if reads := f.run(map[string]string{a: "alpha"}); reads != 0 {
		t.Fatalf("an entry every other run confirmed was read again: %d reads", reads)
	}
}

// head is the first bytes of a Mach-O executable whose Go build ID is id.
func head(id string) []byte {
	return []byte("\xcf\xfa\xed\xfe padding \xff Go build ID: \"" + id + "\"\n \xff more text")
}

// elfHead is the first bytes of an ELF executable whose Go build ID note
// holds id.
func elfHead(id string) []byte {
	var b bytes.Buffer
	b.WriteString("\x7fELF padding")
	_ = binary.Write(&b, binary.LittleEndian, []uint32{4, uint32(len(id)), 4})
	b.WriteString("Go\x00\x00")
	b.WriteString(id)
	b.WriteString("\x00\x00 more")
	return b.Bytes()
}

func TestTheBuildIDIsReadFromTheELFNoteOrTheTextMarker(t *testing.T) {
	for name, tc := range map[string]struct {
		head []byte
		want string
	}{
		"an ELF note":          {elfHead("a/b/c/d"), "a/b/c/d"},
		"a Mach-O text marker": {head("e/f/g/h"), "e/f/g/h"},
		"a redacted ELF note":  {elfHead("redacted"), ""},
		"an empty ELF note":    {elfHead(""), ""},
	} {
		if got := buildID(tc.head); got != tc.want {
			t.Errorf("the build ID of %s = %q, want %q", name, got, tc.want)
		}
	}
}

// note is an ELF note header in order, then the padded name "Go" and desc.
func note(order binary.ByteOrder, nameSize, typ uint32, desc string) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, order, []uint32{nameSize, uint32(len(desc)), typ})
	b.WriteString("Go\x00\x00")
	b.WriteString(desc)
	b.WriteString("\x00\x00")
	return b.Bytes()
}

// A note named "Go" whose type or name size is not the build ID note's is
// passed over, and the build ID is read from the note after it.
func TestOnlyTheBuildIDNoteIsRead(t *testing.T) {
	for name, decoy := range map[string][]byte{
		"another note type": note(binary.LittleEndian, 4, 3, "x1/y1/z1/w1"),
		"another name size": note(binary.LittleEndian, 5, 4, "x2/y2/z2/w2"),
	} {
		h := append([]byte("\x7fELF padding"), decoy...)
		h = append(h, note(binary.LittleEndian, 4, 4, "a/b/c/d")...)
		if got := buildID(h); got != "a/b/c/d" {
			t.Errorf("with a note of %s first, the build ID = %q, want the build ID note's a/b/c/d", name, got)
		}
	}
}

// A big-endian ELF executable (s390x) writes its note header big-endian, which
// the reading does not decode, and the Go linker writes no text marker into an
// ELF file: it has no build ID, and so uses no index (protocols/workspace ADR
// 0009, Consequences).
func TestABigEndianELFNoteIsNotDecoded(t *testing.T) {
	h := append([]byte("\x7fELF padding"), note(binary.BigEndian, 4, 4, "a/b/c/d")...)
	if got := buildID(h); got != "" {
		t.Fatalf("the build ID of a big-endian note = %q, want none", got)
	}
}

func TestTheIdentityHoldsTheBuildIDAndTheReadingScheme(t *testing.T) {
	requireStat(t)
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	one := identityOf(ReadingScheme, "/bin/putnami", info, head("a1/b1/c1/d1"), "(devel)")
	if one == "" {
		t.Fatal("an executable with a build ID has no identity")
	}
	for name, other := range map[string]string{
		"another build ID":       identityOf(ReadingScheme, "/bin/putnami", info, head("a2/b2/c2/d2"), "(devel)"),
		"another build info":     identityOf(ReadingScheme, "/bin/putnami", info, head("a1/b1/c1/d1"), "(devel) vcs.modified=true"),
		"another reading scheme": identityOf(ReadingScheme+"+1", "/bin/putnami", info, head("a1/b1/c1/d1"), "(devel)"),
	} {
		if other == one {
			t.Errorf("%s gives the same identity", name)
		}
	}
}

func TestAnExecutableWithNoUsableBuildIDUsesNoIndex(t *testing.T) {
	requireStat(t)
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, h := range map[string][]byte{
		"no build ID":         []byte("\x7fELF no marker here"),
		"an empty build ID":   head(""),
		"a redacted one":      head("redacted"),
		"one with no slash":   head("abcdef"),
		"an unterminated one": []byte("\xff Go build ID: \"a/b/c"),
		"a note larger than the head": func() []byte {
			var b bytes.Buffer
			b.WriteString("\x7fELF padding")
			_ = binary.Write(&b, binary.LittleEndian, []uint32{4, 0x7fffffff, 4})
			b.WriteString("Go\x00\x00a/b/c/d")
			return b.Bytes()
		}(),
	} {
		if id := identityOf(ReadingScheme, "/bin/putnami", info, h, "(devel)"); id != "" {
			t.Errorf("an executable with %s has the identity %q, want none", name, id)
		}
	}
}

func TestTheRunningExecutablesBuildIDLiesInItsHead(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h, err := readHead(exe)
	if err != nil {
		t.Fatal(err)
	}
	if buildID(h) == "" {
		t.Fatalf("the first %d bytes of the test binary hold no Go build ID", identityHead)
	}
	if requireStat(t); ExecutableIdentity() == "" {
		t.Fatal("the running test binary has no identity")
	}
}

func TestAnExecutableThatCannotBeReadUsesNoIndex(t *testing.T) {
	requireStat(t)
	dir := t.TempDir()
	if id := identityFromFile(ReadingScheme, "/bin/putnami", dir, "(devel)"); id != "" {
		t.Fatalf("an executable that cannot be read has the identity %q, want none", id)
	}
	if id := identityFromFile(ReadingScheme, "/bin/putnami", filepath.Join(dir, "missing"), "(devel)"); id != "" {
		t.Fatalf("a missing executable has the identity %q, want none", id)
	}
}

func TestAnEntryReadBetweenSavingRunsIsNeverReadAgain(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	f.run(map[string]string{a: "alpha"})
	var reads int64
	for i := range 3 * keptRuns {
		b := f.write("b.txt", strings.Repeat("b", i+1))
		f.run(map[string]string{b: strings.Repeat("b", i+1)})
		reads += f.run(map[string]string{a: "alpha"})
	}
	if reads != 0 {
		t.Fatalf("an entry confirmed between saving runs was read %d times over %d of them", reads, 3*keptRuns)
	}
}

func TestAnUnchangedFileIsNotReadOverManyRuns(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	f.run(map[string]string{a: "alpha"})
	for i := range 3 * keptRuns {
		if reads := f.run(map[string]string{a: "alpha"}); reads != 0 {
			t.Fatalf("run %d of an unchanged file read it %d times", i+2, reads)
		}
	}
}

func TestARunThatOnlyConfirmsLeavesTheIndexFileAsItWas(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	f.run(map[string]string{a: "alpha"})
	before, err := os.ReadFile(f.index)
	if err != nil {
		t.Fatal(err)
	}
	f.run(map[string]string{a: "alpha"})
	after, err := os.ReadFile(f.index)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a run that only confirmed an entry rewrote the index file")
	}
}

func TestAnIndexOfTheLastFormatLoadsEmpty(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("a.txt", "alpha")
	f.run(map[string]string{a: "alpha"})
	data, err := os.ReadFile(f.index)
	if err != nil {
		t.Fatal(err)
	}
	body := append([]byte(nil), data[:len(data)-DigestSize]...)
	binary.LittleEndian.PutUint32(body[len(magic):], 3)
	s := sha256.Sum256(body)
	if err := os.WriteFile(f.index, append(body, s[:]...), 0o644); err != nil {
		t.Fatal(err)
	}
	if reads := f.run(map[string]string{a: "alpha"}); reads != 1 {
		t.Fatalf("an index of format 3 answered: %d reads, want 1", reads)
	}
}

func TestTheIndexHoldsAtMostMaxEntries(t *testing.T) {
	f := newFixture(t)
	aged(t)
	previous := maxEntries
	maxEntries = 4
	t.Cleanup(func() { maxEntries = previous })
	old := map[string]string{}
	for i := range 4 {
		name := fmt.Sprintf("old%d.txt", i)
		old[f.write(name, name)] = name
	}
	f.run(old)
	recent := map[string]string{}
	for i := range 2 {
		name := fmt.Sprintf("new%d.txt", i)
		recent[f.write(name, name)] = name
	}
	f.run(recent)
	disk := onDisk(t, f.index)
	if len(disk.entries) != 2 {
		t.Fatalf("an index capped at 4 entries holds %d after a run recorded 2 over 4, want the 2 it recorded", len(disk.entries))
	}
}

// TestACoarseKernelClockStillSeesTheStatusChange holds that the status-change
// time is compared on a clock of 10 ms ticks, which kernels without
// fine-grained timestamps keep: a file recorded once it is more than Margin
// old, then rewritten with the same size and its modification time set back,
// is read again because its status-change time moved. It does not prove the
// rule that keeps a recently changed file out of the index: the file it
// rewrites is recorded long after its change.
func TestACoarseKernelClockStillSeesTheStatusChange(t *testing.T) {
	f := newFixture(t)
	tick := int64(10 * time.Millisecond)
	previous := statFile
	statFile = func(info os.FileInfo) (Stat, bool) {
		st, ok := statOf(info)
		st.ModNano -= st.ModNano % tick
		st.ChangeNano -= st.ChangeNano % tick
		return st, ok
	}
	t.Cleanup(func() { statFile = previous })

	a := f.write("a.txt", "alpha")
	time.Sleep(Margin + 100*time.Millisecond)
	if reads := f.run(map[string]string{a: "alpha"}); reads != 1 {
		t.Fatalf("the first run read %d files, want 1", reads)
	}
	info, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	f.write("a.txt", "omega")
	if err := os.Chtimes(a, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if reads := f.run(map[string]string{a: "omega"}); reads != 1 {
		t.Fatalf("a same-size rewrite with its modification time set back read %d files, want 1", reads)
	}
}
