package store

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestReadingANewStoreIsEmpty(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "absent")).Read()
	if err != nil {
		t.Fatal(err)
	}
	if state.StoreID != "" || state.Source() != "" || len(state.Tasks) != 0 || state.Version != FormatVersion {
		t.Fatalf("state = %+v", state)
	}
}

func TestTheFirstWriteIssuesAStoreID(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "store"))
	written, err := s.Update(func(state *State) error {
		if state.StoreID == "" {
			t.Error("the store id is issued before the change runs")
		}
		state.Next.Task = 7
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	read, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if read.StoreID != written.StoreID || read.Next.Task != 7 || !strings.HasPrefix(read.Source(), "local:") {
		t.Fatalf("read %+v, written %+v", read, written)
	}
	again, err := s.Update(func(*State) error { return nil })
	if err != nil || again.StoreID != written.StoreID {
		t.Fatalf("the store id changed: %v %+v", err, again)
	}
	if s.Root() == "" {
		t.Error("root")
	}
}

func TestAFailedChangeWritesNothing(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "store"))
	if _, err := s.Update(func(state *State) error { state.Next.Task = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("refused")
	if _, err := s.Update(func(state *State) error { state.Next.Task = 99; return refusal }); !errors.Is(err, refusal) {
		t.Fatalf("err = %v", err)
	}
	state, _ := s.Read()
	if state.Next.Task != 1 {
		t.Fatalf("a refused change was written: %+v", state.Next)
	}
	leftovers, _ := filepath.Glob(filepath.Join(s.Root(), "*.tmp"))
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

// TestConcurrentWritersSerialize is the lock's contract: N writers that each
// read a counter and write it back incremented never lose an increment.
func TestConcurrentWritersSerialize(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "store"))
	const writers = 24
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Open(s.Root()).Update(func(state *State) error {
				state.Next.Review++
				return nil
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	state, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if state.Next.Review != writers {
		t.Fatalf("counter = %d after %d serialized writers", state.Next.Review, writers)
	}
}

func TestAnUnknownOrBrokenDocumentIsRefused(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, stateFile), []byte(`{"version":9}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root).Read(); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("err = %v", err)
	}
	if _, err := Open(root).Update(func(*State) error { return nil }); err == nil {
		t.Fatal("a write over an unknown format must be refused")
	} else {
		var write *WriteError
		if !errors.As(err, &write) || write.Landed {
			t.Errorf("err = %#v, want a write error that did not land", err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, stateFile), []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root).Read(); err == nil {
		t.Fatal("a torn document must be refused")
	}
	if err := os.Remove(filepath.Join(root, stateFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, stateFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root).Read(); err == nil {
		t.Fatal("an unreadable document must be refused")
	}
}

func TestAStoreThatCannotBeCreatedIsAnError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(file, "store")).Update(func(*State) error { return nil }); err == nil {
		t.Fatal("a store under a regular file cannot be created")
	}
	var write WriteError
	write.Err = errors.New("x")
	if write.Error() != "x" || write.Unwrap() == nil {
		t.Error("WriteError")
	}
}

// The store is its user's alone: its directory is 0700, and its files, the
// lock and every document or note written by rename, 0600.
func TestTheStoreIsReadableByItsUserOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX mode bits")
	}
	s := Open(filepath.Join(t.TempDir(), "store"))
	if _, err := s.Update(func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateMissions(func(m *Missions) error { m.Next.Message = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.Root(), MessagesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteRefusals(MessagesDir, map[string]string{"a.json": "a.json is not taken: a test"}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		s.Root():                                                     0o700,
		filepath.Join(s.Root(), lockFile):                            0o600,
		filepath.Join(s.Root(), stateFile):                           0o600,
		filepath.Join(s.Root(), missionsFile):                        0o600,
		filepath.Join(s.Root(), MessagesDir, "a.json"+RefusalSuffix): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %o, want %o", filepath.Base(path), got, want)
		}
	}
}

// A note is written once for each text, and removed once its file is no
// longer refused; other files of the directory stay as they are.
func TestRefusalNotesFollowTheRefusedFiles(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "store"))
	if err := s.WriteRefusals(AnswersDir, nil); err != nil {
		t.Fatalf("a directory that does not exist: %v", err)
	}
	dir := filepath.Join(s.Root(), MessagesDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"a.json": "{}", "keep.refused": "mine", "b.json" + RefusalSuffix: "old\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.WriteRefusals(MessagesDir, map[string]string{"a.json": "a.json is not taken: why"}); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			return "<none>"
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if got := read("a.json" + RefusalSuffix); got != "a.json is not taken: why\n" {
		t.Errorf("the note says %q", got)
	}
	if read("b.json"+RefusalSuffix) != "<none>" || read("keep.refused") != "mine" || read("a.json") != "{}" {
		t.Errorf("notes and files: b %q, keep %q, a %q", read("b.json"+RefusalSuffix), read("keep.refused"), read("a.json"))
	}
	if err := s.WriteRefusals(MessagesDir, nil); err != nil || read("a.json"+RefusalSuffix) != "<none>" {
		t.Errorf("the note of a file taken since stays: %v", err)
	}
}
