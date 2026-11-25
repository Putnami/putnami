//go:build linux || darwin

package artifactstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// runtimeEntry publishes an entry holding an executable at runtime/tool and
// returns the store and the executable's path.
func runtimeEntry(t *testing.T) (*Store, string) {
	t.Helper()
	s := New(t.TempDir())
	const digest = "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	dir, err := s.Admit(digest, func(stageDir string) error {
		writeFile(t, filepath.Join(stageDir, "runtime", "tool"), "#!/bin/sh\n", 0o755)
		return nil
	})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	return s, filepath.Join(dir, "runtime", "tool")
}

func TestARecordedRuntimeInfoIsReadForTheSameExecutable(t *testing.T) {
	s, executable := runtimeEntry(t)
	if _, ok := recorded(s, executable); ok {
		t.Fatal("an entry with no record answered")
	}
	record(t, s, executable, `{"extension":"x"}`)
	got, ok := recorded(s, executable)
	if !ok || string(got) != `{"extension":"x"}` {
		t.Fatalf("RecordedRuntimeInfo = %s, %v", got, ok)
	}
	entries, err := os.ReadDir(filepath.Dir(filepath.Dir(executable)))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "runtime" && !IsBookkeeping(entry.Name()) {
			t.Errorf("the record left %s, which IsBookkeeping does not cover", entry.Name())
		}
	}
}

func TestARuntimeInfoRecordIsNotReadForAChangedExecutable(t *testing.T) {
	s, executable := runtimeEntry(t)
	record(t, s, executable, `{"extension":"x"}`)
	info, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	// The same size and modification time: the change time still moves.
	if err := os.WriteFile(executable, []byte("#!/bin/bash"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(executable, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got, ok := recorded(s, executable); ok {
		t.Fatalf("a record was read for a rewritten executable: %s", got)
	}
}

func TestARuntimeInfoRecordIsNotReadForAnotherExecutableOfTheEntry(t *testing.T) {
	s, executable := runtimeEntry(t)
	record(t, s, executable, `{"extension":"x"}`)
	other := filepath.Join(filepath.Dir(executable), "other")
	writeFile(t, other, "#!/bin/sh\n", 0o755)
	if got, ok := recorded(s, other); ok {
		t.Fatalf("a record was read for another executable: %s", got)
	}
}

func TestNoRuntimeInfoIsRecordedOutsideTheEntries(t *testing.T) {
	s, _ := runtimeEntry(t)
	outside := filepath.Join(t.TempDir(), "tool")
	writeFile(t, outside, "#!/bin/sh\n", 0o755)
	record(t, s, outside, `{"extension":"x"}`)
	if _, ok := recorded(s, outside); ok {
		t.Fatal("a record was read for an executable outside the store")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(outside), runtimeInfoFile)); !os.IsNotExist(err) {
		t.Fatalf("a record was written outside the store: %v", err)
	}
}

func TestAMalformedRuntimeInfoRecordIsNotRead(t *testing.T) {
	s, executable := runtimeEntry(t)
	entry := filepath.Dir(filepath.Dir(executable))
	for _, content := range []string{"", "{", `{"schema":2,"executable":"runtime/tool","info":{}}`} {
		if err := os.WriteFile(filepath.Join(entry, runtimeInfoFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, ok := recorded(s, executable); ok {
			t.Errorf("record %q was read: %s", content, got)
		}
	}
}

func record(t *testing.T, s *Store, executable, info string) {
	t.Helper()
	key, _ := s.RuntimeInfoKeyOf(executable)
	s.RecordRuntimeInfo(context.Background(), executable, key, []byte(info))
}

func recorded(s *Store, executable string) (json.RawMessage, bool) {
	key, ok := s.RuntimeInfoKeyOf(executable)
	if !ok {
		return nil, false
	}
	return s.RecordedRuntimeInfo(key)
}

func TestNoRuntimeInfoIsRecordedForAnExecutableThatChangedDuringTheHandshake(t *testing.T) {
	s, executable := runtimeEntry(t)
	key, ok := s.RuntimeInfoKeyOf(executable)
	if !ok {
		t.Fatal("no key for an executable inside an entry")
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.RecordRuntimeInfo(context.Background(), executable, key, []byte(`{"extension":"x"}`))
	if got, ok := recorded(s, executable); ok {
		t.Fatalf("an answer was recorded for an executable rewritten during its handshake: %s", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(executable)), runtimeInfoFile)); !os.IsNotExist(err) {
		t.Fatalf("a record was written: %v", err)
	}
}
