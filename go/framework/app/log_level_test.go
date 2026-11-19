package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/logger"
	"go.putnami.dev/protocol/features/spectest"
)

func writeLoggerConf(t *testing.T, dir, level string) {
	t.Helper()
	body := "logger:\n  level: " + level + "\n  refreshSeconds: 1\n"
	if err := os.WriteFile(filepath.Join(dir, "conf", ".env.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write conf: %v", err)
	}
}

func TestStartedApplicationWatchesTheLogLevel(t *testing.T) {
	spectest.Proves(t, "go/structured-logging", "live-level", "the-application-watches-the-level")
	saved := logger.CurrentLevel()
	t.Cleanup(func() { logger.SetLevel(saved) })
	t.Setenv(logger.LevelEnv, "")
	t.Setenv("CONFIG_DATA", "")

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "conf"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeLoggerConf(t, dir, "warn")
	t.Chdir(dir)

	a, _, _ := newReadinessApp("log-level")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	if got := logger.CurrentLevel(); got != logger.LevelWarn {
		t.Fatalf("level after start = %v, want warn from logger.level", got)
	}

	writeLoggerConf(t, dir, "debug")
	deadline := time.Now().Add(10 * time.Second)
	for logger.CurrentLevel() != logger.LevelDebug {
		if time.Now().After(deadline) {
			t.Fatalf("level = %v, want debug after the conf file changed", logger.CurrentLevel())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Stop ends the watcher: a later change is not applied.
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	writeLoggerConf(t, dir, "error")
	time.Sleep(1500 * time.Millisecond)
	if got := logger.CurrentLevel(); got != logger.LevelDebug {
		t.Fatalf("level = %v after Stop, want debug: the watcher outlived the application", got)
	}
}

func TestStopDoesNotWaitPastItsContextForAReadInFlight(t *testing.T) {
	saved := logger.CurrentLevel()
	t.Cleanup(func() { logger.SetLevel(saved) })

	reading := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	calls := 0
	read := func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return "info", nil // the baseline read
		}
		if calls == 2 {
			close(reading)
		}
		<-release // a config read that hangs
		return "debug", nil
	}

	a := New("stop-bound")
	a.startLevelWatch(time.Millisecond, read)
	select {
	case <-reading:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never re-read")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	returned := make(chan struct{})
	go func() {
		a.stopWatchingLogLevel(ctx)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("stopWatchingLogLevel waited past its context for a hung read")
	}
}

func TestReplacingTheWatcherDoesNotWaitPastItsBoundForAReadInFlight(t *testing.T) {
	saved := logger.CurrentLevel()
	t.Cleanup(func() { logger.SetLevel(saved) })
	savedWait := replaceLevelWatchWait
	replaceLevelWatchWait = 50 * time.Millisecond
	t.Cleanup(func() { replaceLevelWatchWait = savedWait })

	reading := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	calls := 0
	hung := func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return "info", nil // the baseline read
		}
		if calls == 2 {
			close(reading)
		}
		<-release // a config read that hangs
		return "debug", nil
	}

	a := New("replace-bound")
	a.startLevelWatch(time.Millisecond, hung)
	select {
	case <-reading:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never re-read")
	}

	returned := make(chan struct{})
	go func() {
		a.startLevelWatch(time.Hour, func(context.Context) (string, error) { return "warn", nil })
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("startLevelWatch waited past its bound for the replaced watcher's hung read")
	}
	a.stopWatchingLogLevel(context.Background())
}
