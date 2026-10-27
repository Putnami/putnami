package agentsession

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

func TestTheCacheIsWrittenWhole(t *testing.T) {
	spectest.Proves(t, sessionFeature, failOpenRequirement, "every-failure-exits-0-with-a-log-line")
	dir := t.TempDir()
	files := sessionFilesFor(dir, sessionKey(claude, claudeSession))
	if strings.Contains(files.cache, claudeSession) {
		t.Errorf("the engine's session id names a path: %s", files.cache)
	}
	if cache, err := files.read(); cache != nil || err != nil {
		t.Fatalf("a missing cache answered %+v, %v", cache, err)
	}
	if err := files.write(&sessionCache{Engine: claude, Session: claudeSession, Seen: []string{promptID}}); err != nil {
		t.Fatal(err)
	}
	cache, err := files.read()
	if err != nil || cache == nil || cache.Version != sessionCacheVersion || cache.Seen[0] != promptID {
		t.Fatalf("read %+v, %v", cache, err)
	}
	// A cache of another version starts over; a broken one is an error.
	if err := os.WriteFile(files.cache, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cache, err := files.read(); cache != nil || err != nil {
		t.Errorf("another version answered %+v, %v", cache, err)
	}
	if err := os.WriteFile(files.cache, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := files.read(); err == nil {
		t.Error("a broken cache read")
	}
	if err := files.write(&sessionCache{LastMessage: strings.Repeat("x", sessionCacheMaxBytes)}); err == nil {
		t.Error("an oversized cache was written")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("a write left %d files", len(entries))
	}
}

func TestTheSessionLockIsExclusive(t *testing.T) {
	spectest.Proves(t, sessionFeature, toolRequirement, "a-busy-session-spools-the-tool-call")
	files := sessionFilesFor(t.TempDir(), "k")
	held, err := files.tryLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := files.tryLock(); !errors.Is(err, errSessionBusy) {
		t.Errorf("a second lock answered %v", err)
	}
	started := time.Now()
	if _, err := files.lockWithin(50 * time.Millisecond); !errors.Is(err, errSessionBusy) || time.Since(started) < 50*time.Millisecond {
		t.Errorf("lockWithin answered %v after %v", err, time.Since(started))
	}
	held.release()
	again, err := files.tryLock()
	if err != nil {
		t.Fatalf("the released lock answered %v", err)
	}
	again.release()
	(*sessionLock)(nil).release()
}

func TestTheSpoolKeepsTheOrderOfTheToolCalls(t *testing.T) {
	spectest.Proves(t, sessionFeature, toolRequirement, "a-busy-session-spools-the-tool-call")
	files := sessionFilesFor(t.TempDir(), "k")
	for _, at := range []int64{30, 10, 20} {
		if err := files.spool(spooledToolUse{At: at, ToolName: "T" + strconv.FormatInt(at, 10)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(files.spoolDir, "partial.json-123.tmp"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	events, paths := files.readSpool()
	if len(events) != 3 || len(paths) != 3 || events[0].ToolName != "T10" || events[2].ToolName != "T30" {
		t.Errorf("events %+v, paths %v", events, paths)
	}
}

func TestRepliedMarkersArePruned(t *testing.T) {
	spectest.Proves(t, sessionFeature, toolRequirement, "a-recorded-reply-wins-over-an-ack-line")
	dir := t.TempDir()
	now := time.Now()
	for i := range repliedMarkersMax + 3 {
		path := filepath.Join(dir, "msg-"+strconv.Itoa(i))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-time.Duration(i) * time.Minute)
		if i == 0 {
			at = now.Add(-2 * repliedMarkerMaxAge)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	pruneRepliedMarkers(dir, now)
	entries, _ := os.ReadDir(dir)
	if len(entries) != repliedMarkersMax {
		t.Errorf("%d markers, want %d", len(entries), repliedMarkersMax)
	}
	for _, gone := range []string{"msg-0", "msg-" + strconv.Itoa(repliedMarkersMax+2)} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s stays", gone)
		}
	}
}

func TestAppendBoundedKeepsTheNewest(t *testing.T) {
	spectest.Proves(t, sessionFeature, stopRequirement, "a-waiting-message-continues-the-turn-once")
	values := appendBounded([]string{"a", "b"}, "b", 2)
	values = appendBounded(values, "c", 2)
	if strings.Join(values, ",") != "b,c" {
		t.Errorf("values %v", values)
	}
}
