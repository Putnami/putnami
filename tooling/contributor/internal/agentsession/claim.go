package agentsession

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// An agent host may send one event twice: the workspace and the user's hook
// files both declare the hook (CLI ADR 0098), and Codex, or a handler written
// with another launcher, runs both. The first hook claims the event, and the
// second does nothing (ADR 0016).

// duplicateEventWindow is how long a claim holds: identical events from two
// hook sources start together, and a later identical event is a new one.
const duplicateEventWindow = 60 * time.Second

// claimsDir holds one marker per claimed event, named by its digest.
const claimsDir = "events"

// claimEvent reports whether this hook handles the event: it creates the
// marker of the event's digest, exclusively, so two hooks that run at once
// agree without the session lock. A marker younger than the window means
// another hook claimed the event first. A claim that cannot be recorded is
// logged, and the event is handled: a duplicate costs one more sync, a lost
// event a report.
func (e *Env) claimEvent(engine string, idleWake bool, payload []byte) bool {
	window := e.DuplicateWindow
	if window <= 0 {
		return true
	}
	dir := filepath.Join(e.sessionsDir(), claimsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.log(engine, "claim", fmt.Errorf("record the event claim: %w", err))
		return true
	}
	now := e.Now()
	pruneClaims(dir, now, window)
	path := filepath.Join(dir, eventDigest(engine, idleWake, payload))
	for attempt := 0; attempt < 2; attempt++ {
		created, err := createClaim(path, now)
		if created {
			return true
		}
		if err != nil {
			e.log(engine, "claim", fmt.Errorf("record the event claim: %w", err))
			return true
		}
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue // pruned meanwhile: claim it again
		}
		if err != nil {
			e.log(engine, "claim", fmt.Errorf("read the event claim: %w", err))
			return true
		}
		if now.Sub(info.ModTime()) < window {
			return false
		}
		// A stale claim from an earlier identical event: this one is new.
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			e.log(engine, "claim", fmt.Errorf("remove a stale event claim: %w", err))
			return true
		}
	}
	// Another hook took the stale claim between the removal and the creation.
	return false
}

// createClaim creates the marker at path, dated now. It answers false and no
// error when the marker exists.
func createClaim(path string, now time.Time) (bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := file.Close(); err != nil {
		return true, nil
	}
	_ = os.Chtimes(path, now, now)
	return true, nil
}

// pruneClaims removes the markers older than the window, so they do not pile
// up. A marker another hook removes first is no error.
func pruneClaims(dir string, now time.Time, window time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) < window {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}

// eventDigest names an event: the engine, the idle-wake flag, which runs
// another handler on the same event, and the payload's exact bytes.
func eventDigest(engine string, idleWake bool, payload []byte) string {
	sum := sha256.New()
	sum.Write([]byte(engine))
	sum.Write([]byte{0})
	sum.Write([]byte(strconv.FormatBool(idleWake)))
	sum.Write([]byte{0})
	sum.Write(payload)
	return hex.EncodeToString(sum.Sum(nil))
}
