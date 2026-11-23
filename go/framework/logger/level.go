package logger

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// LevelEnv is the variable that sets the minimum level when the logger.level
// config key is absent.
const LevelEnv = "LOG_LEVEL"

// Record outcomes, the values of the outcome label of RecordCounts.
const (
	OutcomeEmitted = "emitted"
	OutcomeDropped = "dropped"
)

// liveLevel is the process-wide minimum level the default logger and every
// logger derived from it read on each record. LOG_LEVEL seeds it.
var liveLevel = func() *slog.LevelVar {
	v := new(slog.LevelVar)
	v.Set(ParseLevel(os.Getenv(LevelEnv)))
	return v
}()

// SetLevel changes the minimum level of the default logger and of every
// logger derived from it. The next record follows it. A logger New builds
// keeps its own level.
//
// SetLevel is the runtime control of the level: a started application calls
// it when logger.level changes. The last call wins. An export directive
// tightens the level through SetLevelLimit instead, and leaves it alone.
func SetLevel(level Level) {
	liveLevel.Set(level)
}

// CurrentLevel returns the live minimum level.
func CurrentLevel() Level {
	return liveLevel.Level()
}

// ResolveLevel applies the level rule to the value of the logger.level config
// key: a non-empty value wins, then LOG_LEVEL, then info. An unknown value
// counts as info.
func ResolveLevel(configured string) Level {
	if configured != "" {
		return ParseLevel(configured)
	}
	return ParseLevel(os.Getenv(LevelEnv))
}

// WatchLevel reads the logger.level value with read once, then re-reads it
// every interval in a background goroutine and, when the value differs from
// the previous read, applies it with SetLevel through ResolveLevel. The first
// read happens before WatchLevel returns and only records the value, so a
// level set since start stays until the configuration changes, and a change
// made after WatchLevel returns is applied. A failed read keeps the level and
// the previous value. The goroutine ends when ctx ends; the returned channel
// closes then. With an interval of zero or less, WatchLevel reads nothing and
// returns a closed channel. Once the channel is closed, the watcher applies no
// further level.
func WatchLevel(ctx context.Context, interval time.Duration, read func(context.Context) (string, error)) <-chan struct{} {
	done := make(chan struct{})
	if interval <= 0 {
		close(done)
		return done
	}
	last, err := read(ctx)
	known := err == nil
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			value, err := read(ctx)
			if err != nil {
				continue
			}
			if ctx.Err() != nil {
				// Ended during the read: a stopped watcher applies nothing.
				return
			}
			if known && value == last {
				continue
			}
			last, known = value, true
			SetLevel(ResolveLevel(value))
		}
	}()
	return done
}

// levelLimit holds the function SetLevelLimit installed, or nil.
var levelLimit atomic.Pointer[func() Level]

// SetLevelLimit installs limit, a process-wide floor that every logger,
// whether derived from the default logger or built by New, consults on each
// record without changing its own level: a record passes when it passes both
// its logger's level and the level limit returns. A nil limit removes it.
//
// An export directive uses it to tighten the level for a while: limit returns
// the directive's level while the directive holds, and a level at or below
// debug once it ends. A record at error or above passes whatever the limit,
// and a record the limit filters counts as dropped.
func SetLevelLimit(limit func() Level) {
	if limit == nil {
		levelLimit.Store(nil)
		return
	}
	levelLimit.Store(&limit)
}

// admit reports whether a record at level passes this logger's minimum level
// and the level limit, and counts the record as emitted or dropped. A record
// at error or above always passes.
func (l *Logger) admit(level Level) bool {
	emitted := l.passes(level)
	countRecord(level, emitted)
	return emitted
}

// passes reports whether a record at level passes this logger's minimum level
// and the level limit. A record at error or above always passes.
func (l *Logger) passes(level Level) bool {
	return level >= LevelError || (level >= l.level.Level() && passesLimit(level))
}

// Skip reports whether a record at level would be dropped, and counts it as
// dropped when so. A caller that builds a costly record checks Skip first and
// builds nothing when it returns true; when it returns false, the record it
// then logs is admitted and counted as emitted.
func (l *Logger) Skip(level Level) bool {
	if l.passes(level) {
		return false
	}
	countRecord(level, false)
	return true
}

func passesLimit(level Level) bool {
	limit := levelLimit.Load()
	return limit == nil || level >= (*limit)()
}

// RecordCount is the number of records at one level with one outcome since
// the process started.
type RecordCount struct {
	// Level is debug, info, warn or error.
	Level string
	// Outcome is OutcomeEmitted or OutcomeDropped.
	Outcome string
	// Count is the cumulative number of records.
	Count uint64
}

var recordLevels = [...]string{"debug", "info", "warn", "error"}

// recordCounts holds the cumulative counts by level bucket, then outcome
// (0 emitted, 1 dropped).
var recordCounts [len(recordLevels)][2]atomic.Uint64

func levelBucket(level Level) int {
	switch {
	case level >= LevelError:
		return 3
	case level >= LevelWarn:
		return 2
	case level >= LevelInfo:
		return 1
	default:
		return 0
	}
}

func countRecord(level Level, emitted bool) {
	outcome := 1
	if emitted {
		outcome = 0
	}
	recordCounts[levelBucket(level)][outcome].Add(1)
}

// RecordCounts returns the cumulative count of the records every logger of
// the process received, by level and outcome: eight entries, debug to error,
// emitted before dropped. The telemetry plugin observes it as log.records.
func RecordCounts() []RecordCount {
	counts := make([]RecordCount, 0, 2*len(recordLevels))
	for bucket, level := range recordLevels {
		counts = append(counts,
			RecordCount{Level: level, Outcome: OutcomeEmitted, Count: recordCounts[bucket][0].Load()},
			RecordCount{Level: level, Outcome: OutcomeDropped, Count: recordCounts[bucket][1].Load()},
		)
	}
	return counts
}
