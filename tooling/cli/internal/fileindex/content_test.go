package fileindex

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func contentReader(path string) func() ([]byte, error) {
	return func() ([]byte, error) { return os.ReadFile(path) }
}

// contentRun opens the index, reads each path's content through it, fails
// unless each answers its file's bytes, saves, and returns the reads.
func (f *fixture) contentRun(want map[string]string) int64 {
	f.t.Helper()
	return f.contentRunAs("config", want)
}

// contentRunAs is contentRun under variant.
func (f *fixture) contentRunAs(variant string, want map[string]string) int64 {
	f.t.Helper()
	idx := Open(f.index, testIdentity)
	for path, content := range want {
		got, err := idx.Content(path, variant, contentReader(path))
		if err != nil {
			f.t.Fatal(err)
		}
		if string(got) != content {
			f.t.Fatalf("content of %s answered %q, want %q", path, got, content)
		}
	}
	if err := idx.Save(); err != nil {
		f.t.Fatal(err)
	}
	return idx.Reads()
}

func TestAnUnchangedFilesContentIsNotReadAgain(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("putnami.json", `{"name":"a"}`)
	files := map[string]string{a: `{"name":"a"}`}
	if reads := f.contentRun(files); reads != 1 {
		t.Fatalf("first run read %d files, want 1", reads)
	}
	for range 3 {
		if reads := f.contentRun(files); reads != 0 {
			t.Fatalf("a run of an unchanged file read it %d times, want 0", reads)
		}
	}
}

func TestAnAnsweredContentIsTheCallersOwn(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("putnami.json", "{}")
	f.contentRun(map[string]string{a: "{}"})
	idx := Open(f.index, testIdentity)
	got, err := idx.Content(a, "config", contentReader(a))
	if err != nil {
		t.Fatal(err)
	}
	got[0] = 'x'
	again, err := idx.Content(a, "config", contentReader(a))
	if err != nil || string(again) != "{}" {
		t.Fatalf("a caller's write reached the recorded content: %q, %v", again, err)
	}
}

func TestAChangedFilesContentIsReadAgain(t *testing.T) {
	cases := map[string]func(f *fixture, path string){
		"a rewrite in the same tick": nil, // the coarse clock below
		"a replacement by rename": func(f *fixture, path string) {
			staged := f.write("staged.json", `{"name":"b"}`)
			if err := os.Rename(staged, path); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if change == nil {
				coarse(t)
			} else {
				aged(t)
			}
			a := f.write("putnami.json", `{"name":"a"}`)
			if reads := f.contentRun(map[string]string{a: `{"name":"a"}`}); reads != 1 {
				t.Fatalf("first run read %d files, want 1", reads)
			}
			if change == nil {
				f.write("putnami.json", `{"name":"b"}`)
			} else {
				change(f, a)
			}
			if reads := f.contentRun(map[string]string{a: `{"name":"b"}`}); reads != 1 {
				t.Fatalf("the run after the change read %d files, want 1", reads)
			}
		})
	}
}

func TestALargeFilesContentIsNotRecorded(t *testing.T) {
	f := newFixture(t)
	aged(t)
	large := strings.Repeat("x", MaxContent+1)
	a := f.write("large.json", large)
	f.contentRun(map[string]string{a: large})
	if reads := f.contentRun(map[string]string{a: large}); reads != 1 {
		t.Fatalf("a file larger than MaxContent was read %d times, want 1", reads)
	}
}

func TestContentAndDigestNeverAnswerForEachOther(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("putnami.json", "{}")
	idx := Open(f.index, testIdentity)
	digest(t, idx, a, "{}")
	if _, err := idx.Content(a, "sha256", contentReader(a)); err != nil {
		t.Fatal(err)
	}
	if idx.Reads() != 2 {
		t.Fatalf("a digest and a content of one file read %d times, want 2", idx.Reads())
	}
}

func TestAContentEntryThatDisagreesWithItsDigestLoadsNothing(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("putnami.json", "{}")
	f.contentRun(map[string]string{a: "{}"})
	idx := Open(f.index, testIdentity)
	forged := map[key]entry{}
	for k, e := range idx.loaded {
		e.content = []byte("[]") // same size, other bytes than its digest
		forged[k] = e
	}
	if err := os.WriteFile(f.index, encode(file{identity: testIdentity, stamp: idx.clock, entries: forged}), 0o644); err != nil {
		t.Fatal(err)
	}
	if reads := f.contentRun(map[string]string{a: "{}"}); reads != 1 {
		t.Fatalf("a forged content entry answered: %d reads, want 1", reads)
	}
}

func TestADigestEntryCarriesNoContent(t *testing.T) {
	f := newFixture(t)
	forged := encode(file{identity: testIdentity, stamp: 1, entries: map[key]entry{
		{path: filepath.Join(f.dir, "a"), variant: "sha256"}: {content: []byte("x")},
	}})
	if _, err := decode(forged); err == nil {
		t.Fatal("a digest entry that carries content decoded")
	}
	s := sha256.Sum256(nil)
	empty := encode(file{identity: testIdentity, stamp: 1, entries: map[key]entry{
		{path: filepath.Join(f.dir, "a"), variant: contentVariant + "config"}: {digest: s},
	}})
	decoded, err := decode(empty)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range decoded.entries {
		if e.content == nil || !bytes.Equal(e.content, []byte{}) {
			t.Fatalf("an empty file's content entry decoded as %v", e.content)
		}
	}
}

func TestARunRecordsTheContentAReadOnlyIndexReadBeforeIt(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("putnami.json", `{"name":"a"}`)
	// The index file exists, as after any earlier run.
	f.contentRun(map[string]string{})

	early := OpenReadOnly(f.index, testIdentity)
	if _, err := early.Content(a, BytesVariant, contentReader(a)); err != nil {
		t.Fatal(err)
	}
	if early.Reads() != 1 {
		t.Fatalf("the read-only index read %d files, want 1", early.Reads())
	}
	run := Open(f.index, testIdentity)
	run.Adopt(early)
	if err := run.Save(); err != nil {
		t.Fatal(err)
	}
	if reads := f.contentRunAs(BytesVariant, map[string]string{a: `{"name":"a"}`}); reads != 0 {
		t.Fatalf("a run after the adopting run read %d files, want 0", reads)
	}

	// A read-only index adopts nothing.
	b := f.write("b.json", "{}")
	early = OpenReadOnly(f.index, testIdentity)
	if _, err := early.Content(b, BytesVariant, contentReader(b)); err != nil {
		t.Fatal(err)
	}
	OpenReadOnly(f.index, testIdentity).Adopt(early)
	if reads := f.contentRunAs(BytesVariant, map[string]string{b: "{}"}); reads != 1 {
		t.Fatalf("a file only read-only indexes read was read %d times, want 1", reads)
	}
}

func TestAMissingFileIsNoContentRead(t *testing.T) {
	f := newFixture(t)
	idx := Open(f.index, testIdentity)
	missingPath := filepath.Join(f.dir, "absent.json")
	before := ContentReads()
	if _, err := idx.Content(missingPath, BytesVariant, contentReader(missingPath)); !os.IsNotExist(err) {
		t.Fatalf("a missing file answered %v, want the read's not-exist error", err)
	}
	if ContentReads() != before || idx.Reads() != 0 {
		t.Fatal("looking for a missing file counted as a content read")
	}
}

func TestADigestNeverRecordsUnderAContentVariant(t *testing.T) {
	f := newFixture(t)
	aged(t)
	a := f.write("putnami.json", "{}")
	idx := Open(f.index, testIdentity)
	if _, err := idx.Digest(a, contentVariant+BytesVariant, reader(a)); err != nil {
		t.Fatal(err)
	}
	got, err := idx.Content(a, BytesVariant, contentReader(a))
	if err != nil || string(got) != "{}" {
		t.Fatalf("a digest under a content variant answered the file's bytes: %q, %v", got, err)
	}
}

func TestAdoptReadsOnlyWhatARunCanRecord(t *testing.T) {
	f := newFixture(t)
	aged(t)
	large := strings.Repeat("x", MaxContent+1)
	big := f.write("large.json", large)
	small := f.write("small.json", "{}")
	Open(f.index, testIdentity) // the index directory, as after any earlier run
	early := OpenReadOnly(f.index, testIdentity)
	for _, path := range []string{big, small} {
		if _, err := early.Content(path, BytesVariant, contentReader(path)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := early.Content(small, "other", contentReader(small)); err != nil {
		t.Fatal(err)
	}
	run := Open(f.index, testIdentity)
	run.Adopt(early)
	if run.Reads() != 1 {
		t.Fatalf("adopt read %d files, want only the small file under its bytes reading", run.Reads())
	}
	again := Open(f.index, testIdentity)
	again.Adopt(early)
	if again.Reads() != 0 {
		t.Fatalf("a second adopt read %d files again, want 0", again.Reads())
	}
}
