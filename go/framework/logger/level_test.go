package logger

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// restoreLiveLevel puts the live level back after a test that changes it.
func restoreLiveLevel(t *testing.T) {
	t.Helper()
	saved := CurrentLevel()
	t.Cleanup(func() { SetLevel(saved) })
}

func TestResolveLevelReadsTheConfigKeyThenLogLevel(t *testing.T) {
	spectest.Proves(t, "go/structured-logging", "live-level", "the-level-reads-the-config-key")

	t.Setenv(LevelEnv, "warn")
	if got := ResolveLevel("debug"); got != LevelDebug {
		t.Fatalf("logger.level=debug: got %v, want debug (the key wins over LOG_LEVEL)", got)
	}
	if got := ResolveLevel(""); got != LevelWarn {
		t.Fatalf("no key, LOG_LEVEL=warn: got %v, want warn", got)
	}
	if got := ResolveLevel("verbose"); got != LevelInfo {
		t.Fatalf("unknown value: got %v, want info", got)
	}
	t.Setenv(LevelEnv, "")
	if got := ResolveLevel(""); got != LevelInfo {
		t.Fatalf("no key, no LOG_LEVEL: got %v, want info", got)
	}
}

func TestSetLevelAppliesToTheNextRecord(t *testing.T) {
	spectest.Proves(t, "go/structured-logging", "live-level", "set-level-applies-to-the-next-record")
	restoreLiveLevel(t)

	var buf bytes.Buffer
	log := newLogger("live", liveLevel, NewJSONSinkWriter(&buf)).Named("child").With("k", "v")

	SetLevel(LevelInfo)
	log.Debug("hidden")
	if buf.Len() != 0 {
		t.Fatalf("debug record emitted at info: %s", buf.String())
	}

	SetLevel(LevelDebug)
	log.Debug("shown")
	if !strings.Contains(buf.String(), `"message":"shown"`) {
		t.Fatalf("debug record not emitted after SetLevel(debug): %q", buf.String())
	}
	if CurrentLevel() != LevelDebug {
		t.Fatalf("CurrentLevel = %v, want debug", CurrentLevel())
	}

	// A logger New builds keeps its own level.
	var fixedBuf bytes.Buffer
	fixed := New("fixed", LevelWarn, NewJSONSinkWriter(&fixedBuf))
	fixed.Info("hidden")
	if fixedBuf.Len() != 0 {
		t.Fatalf("New(LevelWarn) followed the live level: %s", fixedBuf.String())
	}
}

func TestErrorRecordIgnoresTheLevel(t *testing.T) {
	spectest.Proves(t, "go/structured-logging", "errors-always-emitted", "an-error-record-ignores-the-level")

	var buf bytes.Buffer
	// A minimum above error, as a directive could set, still lets errors pass.
	log := New("floor", LevelError+4, NewJSONSinkWriter(&buf))
	log.Warn("hidden")
	log.Error("kept", errors.New("boom"))
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Fatalf("warn record emitted above its level: %s", out)
	}
	if !strings.Contains(out, `"message":"kept"`) {
		t.Fatalf("error record dropped: %q", out)
	}
}

func TestRecordsAreCountedPerLevelAndOutcome(t *testing.T) {
	spectest.Proves(t, "go/structured-logging", "records-counted", "records-are-counted-per-level-and-outcome")

	counts := func() map[string]uint64 {
		m := map[string]uint64{}
		for _, c := range RecordCounts() {
			m[c.Level+"/"+c.Outcome] = c.Count
		}
		return m
	}
	if got := len(RecordCounts()); got != 8 {
		t.Fatalf("RecordCounts has %d entries, want 8", got)
	}
	before := counts()

	log := New("count", LevelInfo, NewMemorySink())
	log.Debug("d")
	log.Info("i")
	log.InfoCtx(context.Background(), "i")
	log.Warn("w")
	log.Error("e", nil)

	after := counts()
	want := map[string]uint64{
		"debug/dropped": 1, "debug/emitted": 0,
		"info/emitted": 2, "info/dropped": 0,
		"warn/emitted": 1, "warn/dropped": 0,
		"error/emitted": 1, "error/dropped": 0,
	}
	for key, delta := range want {
		if got := after[key] - before[key]; got != delta {
			t.Errorf("%s grew by %d, want %d", key, got, delta)
		}
	}
}

func TestWatchLevelAppliesAChangedLevel(t *testing.T) {
	spectest.Proves(t, "go/structured-logging", "live-level", "the-watcher-applies-a-changed-level")
	restoreLiveLevel(t)
	t.Setenv(LevelEnv, "")

	var mu sync.Mutex
	value := "info"
	reads := make(chan struct{}, 64)
	read := func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		reads <- struct{}{}
		return value, nil
	}
	set := func(v string) {
		mu.Lock()
		value = v
		mu.Unlock()
	}
	waitReads := func(n int) {
		for range n {
			select {
			case <-reads:
			case <-time.After(5 * time.Second):
				t.Fatal("watcher stopped reading")
			}
		}
	}

	// A level set since start stays while the config does not change.
	SetLevel(LevelWarn)
	ctx, cancel := context.WithCancel(context.Background())
	done := WatchLevel(ctx, time.Millisecond, read)
	waitReads(3)
	if CurrentLevel() != LevelWarn {
		t.Fatalf("an unchanged config overrode SetLevel: %v", CurrentLevel())
	}

	set("debug")
	waitReads(3)
	if CurrentLevel() != LevelDebug {
		t.Fatalf("changed logger.level not applied: %v", CurrentLevel())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchLevel did not return after its context ended")
	}

	// A zero interval turns the watcher off: no read, a closed channel.
	for len(reads) > 0 {
		<-reads
	}
	select {
	case <-WatchLevel(context.Background(), 0, read):
	default:
		t.Fatal("WatchLevel(interval 0) returned an open channel")
	}
	if len(reads) != 0 {
		t.Fatal("WatchLevel(interval 0) read the configuration")
	}
}

func TestWatchLevelAppliesNothingOnceItsContextEnds(t *testing.T) {
	restoreLiveLevel(t)
	SetLevel(LevelWarn)

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	read := func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return "warn", nil
		}
		// The watcher stops while this read is in flight; its changed value
		// must not apply.
		cancel()
		return "debug", nil
	}
	done := WatchLevel(ctx, time.Millisecond, read)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchLevel did not stop after its context ended")
	}
	if CurrentLevel() != LevelWarn {
		t.Fatalf("a read that ended with the context applied %v", CurrentLevel())
	}
}

func TestTheLimitRaisesTheLevelOfEveryLogger(t *testing.T) {
	spectest.Proves(t, "go/structured-logging", "level-limit", "the-limit-raises-the-level-of-every-logger")
	restoreLiveLevel(t)
	t.Cleanup(func() { SetLevelLimit(nil) })

	SetLevel(LevelDebug)
	var live, fixed bytes.Buffer
	derived := newLogger("live", liveLevel, NewJSONSinkWriter(&live)).Named("child")
	built := New("fixed", LevelDebug, NewJSONSinkWriter(&fixed))

	limit := LevelWarn
	SetLevelLimit(func() Level { return limit })
	derived.Info("hidden-live")
	built.Info("hidden-fixed")
	derived.Warn("shown-live")
	if strings.Contains(live.String(), "hidden-live") || strings.Contains(fixed.String(), "hidden-fixed") {
		t.Fatalf("an info record passed a warn limit: %q %q", live.String(), fixed.String())
	}
	if !strings.Contains(live.String(), "shown-live") {
		t.Fatalf("a warn record did not pass a warn limit: %q", live.String())
	}
	if CurrentLevel() != LevelDebug {
		t.Fatalf("the limit changed the live level to %v", CurrentLevel())
	}

	// The limit is read on each record: once it ends, the level is the
	// logger's own again.
	limit = LevelDebug
	derived.Debug("shown-again")
	if !strings.Contains(live.String(), "shown-again") {
		t.Fatalf("a debug record did not pass once the limit ended: %q", live.String())
	}

	// A limit below a logger's own level never lowers it.
	SetLevel(LevelWarn)
	derived.Info("still-hidden")
	SetLevelLimit(nil)
	derived.Info("still-hidden")
	if strings.Contains(live.String(), "still-hidden") {
		t.Fatalf("a limit lowered the level: %q", live.String())
	}
}

func TestAnErrorPassesTheLimit(t *testing.T) {
	spectest.Proves(t, "go/structured-logging", "level-limit", "an-error-passes-the-limit")
	t.Cleanup(func() { SetLevelLimit(nil) })

	counts := func() map[string]uint64 {
		m := map[string]uint64{}
		for _, c := range RecordCounts() {
			m[c.Level+"/"+c.Outcome] = c.Count
		}
		return m
	}
	var buf bytes.Buffer
	log := New("limited", LevelDebug, NewJSONSinkWriter(&buf))
	SetLevelLimit(func() Level { return LevelError + 4 })
	before := counts()
	log.Warn("hidden")
	log.Error("kept", errors.New("boom"))
	after := counts()
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), `"message":"kept"`) {
		t.Fatalf("the limit did not keep only the error: %q", buf.String())
	}
	if after["warn/dropped"]-before["warn/dropped"] != 1 || after["error/emitted"]-before["error/emitted"] != 1 {
		t.Fatalf("counts moved by warn/dropped %d, error/emitted %d, want 1 and 1",
			after["warn/dropped"]-before["warn/dropped"], after["error/emitted"]-before["error/emitted"])
	}
}

func TestSkipCountsADroppedRecordOnce(t *testing.T) {
	spectest.Proves(t, "go/structured-logging", "records-counted", "a-skipped-record-counts-as-dropped")
	counts := func() map[string]uint64 {
		m := map[string]uint64{}
		for _, c := range RecordCounts() {
			m[c.Level+"/"+c.Outcome] = c.Count
		}
		return m
	}
	var buf bytes.Buffer
	log := New("skip", LevelInfo, NewJSONSinkWriter(&buf))
	before := counts()
	if !log.Skip(LevelDebug) {
		t.Fatal("Skip(debug) = false under an info logger, want true")
	}
	if log.Skip(LevelInfo) {
		t.Fatal("Skip(info) = true under an info logger, want false")
	}
	log.Info("kept")
	if log.Skip(LevelError) {
		t.Fatal("Skip(error) = true, want false: an error always passes")
	}
	after := counts()
	if got := after["debug/dropped"] - before["debug/dropped"]; got != 1 {
		t.Fatalf("debug/dropped moved by %d, want 1", got)
	}
	if got := after["info/emitted"] - before["info/emitted"]; got != 1 {
		t.Fatalf("info/emitted moved by %d, want 1: Skip must not count a record it lets pass", got)
	}
	if buf.Len() == 0 {
		t.Fatal("the admitted record was not written")
	}
}
