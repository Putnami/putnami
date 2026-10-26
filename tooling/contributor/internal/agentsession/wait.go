package agentsession

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
)

const (
	// sessionIdleWakeWindow is how long the Claude Code idle wake waits
	// after a turn. Its hook entry declares idleWakeTimeout.
	sessionIdleWakeWindow = 10 * time.Minute
	// sessionDecisionWait is how long a Codex Stop waits for the answer to an
	// open decision. Its hook entry declares codexStopTimeout.
	sessionDecisionWait = 180 * time.Second
	// idleWakeStopWait bounds how long the idle wake waits for the Stop hook
	// of the same event, which runs beside it, to decide.
	idleWakeStopWait = 15 * time.Second
	idleWakeStopPoll = 100 * time.Millisecond
	// transcriptWatchMax bounds the transcript bytes one look reads. A
	// session that wrote more since the last look is working.
	transcriptWatchMax = 4 << 20
)

// idleWake waits, after a Claude Code turn ended, for the owner's next
// message or answer, and answers its text, or "" when none came. Claude Code
// runs it as a wake Stop entry beside the Stop hook, so it first reads that
// Stop's decision: a turn the Stop continued has no idle time. It finds the
// decision of its own Stop by the Stop key, never by time.
func (e *Env) idleWake(ctx context.Context, engine string, input hookInput) (string, error) {
	if engine != sessionEngineClaudeCode || input.Event != hookEventStop {
		return "", errors.New("--idle-wake runs on the Claude Code Stop event only")
	}
	if input.PromptID == "" {
		return "", errors.New("the Stop payload names no prompt_id, so the idle wake cannot find its Stop: no idle wait")
	}
	// The transcript is watched from its length at this Stop: a user entry
	// past it is a turn that started after the Stop.
	turns := watchTurns(input.TranscriptPath)
	files := e.files(engine, input.SessionID)
	key := stopKey(input)
	start := e.Now()
	for {
		cache, err := files.read()
		if err != nil {
			return "", err
		}
		if cache != nil && cache.StopKey == key {
			if cache.StopContinued {
				return "", nil
			}
			return e.waitForOwner(ctx, files, ownerWait{window: sessionIdleWakeWindow, baseline: cache.StopDecidedAt, turns: turns})
		}
		if !e.Now().Before(start.Add(idleWakeStopWait)) {
			return "", errors.New("the Stop hook of this turn did not decide: no idle wait")
		}
		if err := e.Sleep(ctx, idleWakeStopPoll); err != nil {
			return "", err
		}
	}
}

// stopKey names one Stop event. The Stop hook and the idle wake read the same
// payload: the prompt the turn answers, whether a Stop hook continued the
// turn already, and the agent's last message. Two Stops of one prompt differ
// by the last two.
func stopKey(input hookInput) string {
	sum := sha256.Sum256([]byte(input.PromptID + "\x00" + strconv.FormatBool(input.StopHookActive) + "\x00" + input.LastMessage))
	return hex.EncodeToString(sum[:16])
}

// ownerWait is one wait for the owner: how long, after which Stop, and what
// ends it early. forDecision ends it once the mission has no open decision;
// turns ends it once a new turn starts.
type ownerWait struct {
	window      time.Duration
	baseline    time.Time
	forDecision bool
	turns       *turnWatch
}

// waitForOwner publishes the session as waiting until window ends and syncs
// at the pace the provider set, until a message or an answer arrives. It
// answers the delivered text, or "" when the wait ended without one.
//
// One wait runs per session: a newer wait takes the name in the cache, and
// an older one that finds another name ends without a write. A wait also ends
// once the session is active after the Stop it waits after, and, with a
// transcript to watch, once a new turn started, even one that runs no hooked
// tool: then it publishes the working state once, see workingAgain.
func (e *Env) waitForOwner(ctx context.Context, files sessionFiles, wait ownerWait) (string, error) {
	token, err := waiterToken()
	if err != nil {
		return "", err
	}
	until, baseline, forDecision := e.Now().Add(wait.window), wait.baseline, wait.forDecision
	started := false
	if err := lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
		if cache == nil || !sessionPublishable(cache) || cache.ActiveAt.After(baseline) {
			return nil, nil
		}
		setSessionState(cache, collab.SessionStateWaiting, sessionEventWait)
		cache.Waiter, cache.WaitUntil = token, until
		started = true
		return cache, nil
	}); err != nil || !started {
		return "", err
	}
	var firstErr error
	for {
		if wait.turns.started() {
			publish := false
			err := lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
				if cache == nil || cache.Waiter != token {
					return nil, nil
				}
				publish = workingAgain(cache)
				return cache, nil
			})
			if err == nil && publish {
				err = e.syncSession(ctx, files)
			}
			return "", errors.Join(firstErr, err)
		}
		syncErr := e.syncSession(ctx, files)
		if syncErr != nil && firstErr == nil {
			firstErr = syncErr
		}
		now := e.Now()
		var text string
		done, early, working := false, false, false
		interval := sessionSyncInterval
		if err := lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
			if cache == nil || cache.Waiter != token {
				done = true
				return nil, nil
			}
			interval = syncInterval(cache)
			if cache.ActiveAt.After(baseline) || wait.turns.started() {
				working, done = workingAgain(cache), true
				return cache, nil
			}
			if delivered := deliverInbox(cache, hookContextMax); len(delivered) > 0 {
				for _, message := range delivered {
					cache.ContinuedFor = appendBounded(cache.ContinuedFor, message.ID, sessionSeenMax)
				}
				text = deliveredText(delivered)
				cache.Waiter, done = "", true
				setSessionState(cache, collab.SessionStateWorking, sessionEventWait)
				return cache, nil
			}
			if (forDecision && syncErr == nil && len(cache.Mission.OpenDecisions) == 0) || !now.Before(until) {
				// A provider reads a wait whose end passed as handed back; a
				// wait that ends before is published.
				cache.Waiter, done, early = "", true, now.Before(until)
				setSessionState(cache, collab.SessionStateHandedBack, sessionEventWait)
			}
			return cache, nil
		}); err != nil {
			return "", errors.Join(firstErr, err)
		}
		if text != "" {
			// The acknowledgements and the working state go out before the
			// new turn starts.
			return text, errors.Join(firstErr, e.syncSession(ctx, files))
		}
		if early || working {
			return "", errors.Join(firstErr, e.syncSession(ctx, files))
		}
		if done {
			return "", firstErr
		}
		if err := e.Sleep(ctx, min(interval, until.Sub(now))); err != nil {
			return "", errors.Join(firstErr, err)
		}
	}
}

// workingAgain ends a wait because the session works again: a new turn
// started or one of its hooks ran. It reports whether the wait publishes the
// working state. A new prompt runs no hook and a tool call publishes
// nothing, so without that publish a provider reads the session as waiting
// until the wait's end. A hook that published since the session became
// active needs no repeat.
func workingAgain(cache *sessionCache) bool {
	cache.Waiter = ""
	if cache.State == collab.SessionStateWaiting {
		setSessionState(cache, collab.SessionStateWorking, sessionEventWait)
		return true
	}
	return cache.LastSyncAt.Before(cache.ActiveAt)
}

func waiterToken() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

// turnWatch tells whether a Claude Code session started a new turn since a
// Stop. A new turn, the owner's prompt or a continued one, adds a user entry
// to the transcript; Claude Code adds its own lines after a Stop too, so only
// a user entry counts. No transcript means no watch.
type turnWatch struct {
	path   string
	offset int64
}

func watchTurns(path string) *turnWatch {
	if path == "" || !filepath.IsAbs(path) {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	return &turnWatch{path: path, offset: info.Size()}
}

// started reads the whole lines written since the last look and reports
// whether one is a user entry. A transcript that shrank, or grew by more than
// transcriptWatchMax, counts as a new turn.
func (w *turnWatch) started() bool {
	if w == nil {
		return false
	}
	file, err := os.Open(w.path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return false
	}
	switch size := info.Size(); {
	case size < w.offset, size-w.offset > transcriptWatchMax:
		return true
	case size == w.offset:
		return false
	}
	added := make([]byte, info.Size()-w.offset)
	read, _ := file.ReadAt(added, w.offset)
	added = added[:read]
	end := bytes.LastIndexByte(added, '\n')
	if end < 0 {
		// A line still being written: the next look reads it whole.
		return false
	}
	w.offset += int64(end + 1)
	for _, line := range bytes.Split(added[:end], []byte("\n")) {
		var entry struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &entry) == nil && entry.Type == "user" {
			return true
		}
	}
	return false
}
