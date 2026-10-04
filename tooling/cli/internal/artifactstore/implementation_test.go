package artifactstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func admitEntry(t *testing.T, s *Store, name string) (string, string) {
	t.Helper()
	digest := hexDigest(name)
	dir, err := s.Admit(digest, func(stage string) error {
		return os.WriteFile(filepath.Join(stage, "putnami.extension.json"), []byte("{}"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return digest, dir
}

// Only the root of a published entry is an entry: a record is trusted for
// immutable store content alone, never for a directory a link or a lookalike
// path makes resemble one.
func TestEntryNamesOnlyAPublishedEntryRoot(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	digest, dir := admitEntry(t, s, "entry")

	if got, ok := s.Entry(dir); !ok || got != digest {
		t.Fatalf("Entry(entry dir) = %q, %v; want %q, true", got, ok, digest)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err == nil {
		if got, ok := s.Entry(link); !ok || got != digest {
			t.Errorf("Entry(link to entry) = %q, %v; want %q, true", got, ok, digest)
		}
	} else if runtime.GOOS != "windows" {
		t.Fatal(err)
	}

	sub := filepath.Join(dir, "bin")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	wrongShard := filepath.Join(root, shaDirName, "zz", hexDigest("other"))
	notHex := filepath.Join(root, shaDirName, "ab", "ab-not-a-digest")
	staged := filepath.Join(root, tmpDirName, hexDigest("staged"))
	outside := filepath.Join(t.TempDir(), shaDirName, digest[:2], digest)
	for _, path := range []string{sub, wrongShard, notHex, staged, outside} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if got, ok := s.Entry(path); ok {
			t.Errorf("Entry(%s) = %q, true; want false", path, got)
		}
	}
	if _, ok := s.Entry(filepath.Join(root, "missing")); ok {
		t.Error("Entry of a missing path reported an entry")
	}
}

func TestImplementationRecordsTheDerivedValueOnce(t *testing.T) {
	s := New(t.TempDir())
	digest, dir := admitEntry(t, s, "record")
	accept := func(v string) bool { return strings.HasPrefix(v, "v1 ") }
	derived := 0
	derive := func(got string) (string, error) {
		derived++
		if got != dir {
			t.Errorf("derive got dir %q, want %q", got, dir)
		}
		return "v1 value", nil
	}

	for i := 0; i < 2; i++ {
		value, err := s.Implementation(context.Background(), digest, accept, derive)
		if err != nil {
			t.Fatal(err)
		}
		if value != "v1 value" {
			t.Fatalf("Implementation = %q, want the derived value", value)
		}
	}
	if derived != 1 {
		t.Errorf("derive ran %d times, want 1: the record serves the second call", derived)
	}
	data, err := os.ReadFile(filepath.Join(dir, implementationFile))
	if err != nil || string(data) != "v1 value" {
		t.Errorf("record = %q, %v; want the derived value", data, err)
	}
}

// A record the reader refuses (another schema), one it cannot read whole, and
// one it cannot read at all are each derived again and replaced.
func TestImplementationReplacesARecordItCannotUse(t *testing.T) {
	for name, write := range map[string]func(t *testing.T, path string){
		"another schema": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("v0 stale"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"oversized": func(t *testing.T, path string) {
			big := "v1 " + strings.Repeat("a", maxImplementationRecord)
			if err := os.WriteFile(path, []byte(big), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"unreadable": func(t *testing.T, path string) {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(t.TempDir())
			digest, dir := admitEntry(t, s, name)
			write(t, filepath.Join(dir, implementationFile))
			derived := 0
			value, err := s.Implementation(context.Background(), digest,
				func(v string) bool { return strings.HasPrefix(v, "v1 ") && len(v) < 64 },
				func(string) (string, error) { derived++; return "v1 fresh", nil })
			if err != nil {
				t.Fatal(err)
			}
			if value != "v1 fresh" || derived != 1 {
				t.Errorf("Implementation = %q after %d derivations; want the fresh value from one", value, derived)
			}
		})
	}
}

func TestImplementationRecordsNothingForAFailedDerivation(t *testing.T) {
	s := New(t.TempDir())
	digest, dir := admitEntry(t, s, "failed")
	boom := errors.New("boom")
	_, err := s.Implementation(context.Background(), digest,
		func(string) bool { return true },
		func(string) (string, error) { return "partial", boom })
	if !errors.Is(err, boom) {
		t.Fatalf("Implementation error = %v, want the derivation error", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, implementationFile)); !os.IsNotExist(err) {
		t.Errorf("a failed derivation left a record (err=%v)", err)
	}
}

// Without the shared lock GC may remove the entry mid-write, so a caller that
// cannot take it in time still gets its value and records nothing.
func TestImplementationWithoutTheLockDerivesAndRecordsNothing(t *testing.T) {
	s := New(t.TempDir())
	digest, dir := admitEntry(t, s, "unlocked")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	value, err := s.Implementation(ctx, digest,
		func(string) bool { return true },
		func(string) (string, error) { return "v1 value", nil })
	if err != nil || value != "v1 value" {
		t.Fatalf("Implementation = %q, %v; want the derived value", value, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, implementationFile)); !os.IsNotExist(err) {
		t.Errorf("an unlocked derivation left a record (err=%v)", err)
	}
}

func TestImplementationRefusesAnInvalidDigest(t *testing.T) {
	s := New(t.TempDir())
	_, err := s.Implementation(context.Background(), "../escape",
		func(string) bool { return true },
		func(string) (string, error) {
			t.Error("derive ran for an invalid digest")
			return "", nil
		})
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Implementation error = %v, want os.ErrNotExist", err)
	}
}
