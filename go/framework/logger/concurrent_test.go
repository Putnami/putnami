package logger

import (
	"fmt"
	"log/slog"
	"sync"
	"testing"
)

func TestLogger_ConcurrentInfo(t *testing.T) {
	sink := NewMemorySink()
	log := New("test", LevelDebug, sink)

	const goroutines = 50
	const msgsPerGoroutine = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func() {
			defer wg.Done()
			for j := range msgsPerGoroutine {
				log.Info(fmt.Sprintf("msg-%d-%d", i, j),
					slog.String("goroutine", fmt.Sprintf("%d", i)),
				)
			}
		}()
	}

	wg.Wait()

	expected := goroutines * msgsPerGoroutine
	if got := sink.Len(); got != expected {
		t.Errorf("entries = %d, want %d", got, expected)
	}
}

func TestLogger_ConcurrentMixedLevels(t *testing.T) {
	sink := NewMemorySink()
	log := New("test", LevelDebug, sink)

	const goroutines = 30
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func() {
			defer wg.Done()
			switch i % 4 {
			case 0:
				log.Debug("debug msg")
			case 1:
				log.Info("info msg")
			case 2:
				log.Warn("warn msg")
			case 3:
				log.Error("error msg", fmt.Errorf("test error %d", i))
			}
		}()
	}

	wg.Wait()

	if got := sink.Len(); got != goroutines {
		t.Errorf("entries = %d, want %d", got, goroutines)
	}
}

func TestMemorySink_ConcurrentWriteAndLen(t *testing.T) {
	sink := NewMemorySink()

	const goroutines = 30
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Writers
	for i := range goroutines {
		go func() {
			defer wg.Done()
			sink.Write(LogEntry{
				Level:   LevelInfo,
				Message: fmt.Sprintf("msg-%d", i),
			})
		}()
	}

	// Readers (Len + Last)
	for range goroutines {
		go func() {
			defer wg.Done()
			_ = sink.Len()
			_ = sink.Last()
		}()
	}

	wg.Wait()

	if got := sink.Len(); got != goroutines {
		t.Errorf("entries = %d, want %d", got, goroutines)
	}
}
