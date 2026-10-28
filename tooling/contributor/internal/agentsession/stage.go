package agentsession

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
)

// loopOrder is the contribution loop, in the order a change runs it.
var loopOrder = []collab.LoopStage{
	collab.LoopStageTalk, collab.LoopStageDeclare, collab.LoopStageShow, collab.LoopStageCode,
	collab.LoopStageCheck, collab.LoopStageProve, collab.LoopStageValidate, collab.LoopStageMerge,
}

// sessionEnvironment names the variable each host sets, for the commands an
// agent runs, to the id of the session the hooks receive.
var sessionEnvironment = []struct{ engine, variable string }{
	{sessionEngineClaudeCode, "CLAUDE_CODE_SESSION_ID"},
	{sessionEngineCodex, "CODEX_THREAD_ID"},
}

const (
	// sessionScanMax bounds the session caches `agent stage` reads to find
	// the session of the checkout.
	sessionScanMax = 4096
)

// sessionCacheName is the name of a session cache in the session store.
var sessionCacheName = regexp.MustCompile(`^[0-9a-f]{64}\.json$`)

// Stage runs `agent stage <name>`: it records the loop stage of the current
// session's change in the session cache, so every later sync carries it, and
// syncs at once when the session reports to a mission. The current session is
// the one the host names in the environment, else the session of this
// checkout and branch that was active last. A usage error exits 2; no
// session, or a sync that fails, exits 1, and a stage it recorded still goes
// with the next sync.
func Stage(ctx context.Context, e *Env, args []string, stdout, stderr io.Writer) int {
	stage, err := parseStageArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agent stage: %v\n", err)
		return 2
	}
	files, inSession, found := e.currentSession(ctx)
	if !found {
		_, _ = fmt.Fprintln(stderr, "agent stage: no agent session found for this checkout: run it from a Claude Code or Codex session that runs the Putnami session hooks")
		return 1
	}
	now := e.Now()
	mission, publishable := "", false
	dir := e.sessionsDir()
	if err := lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
		if cache == nil {
			return nil, nil
		}
		cache.Stage = stage
		if inSession {
			// The host runs this command inside a turn of the session.
			setSessionState(cache, collab.SessionStateWorking, sessionEventStage)
			cache.ActiveAt = now
		} else {
			cache.LastEvent = sessionEventStage
		}
		refreshMissionKey(cache, dir)
		mission, publishable = cache.Mission.Key, sessionPublishable(cache)
		return cache, nil
	}); err != nil {
		_, _ = fmt.Fprintf(stderr, "agent stage: %v\n", err)
		return 1
	}
	if !publishable {
		_, _ = fmt.Fprintf(stdout, "The session is at stage %s. It reports to no mission yet; its first sync carries the stage.\n", stage)
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, hookTimeout-launcherMargin)
	defer cancel()
	if err := e.syncSession(ctx, files); err != nil {
		_, _ = fmt.Fprintf(stderr, "agent stage: the stage %s is recorded and goes with the next sync; this sync failed: %v\n", stage, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "The session is at stage %s, and its mission %s knows it.\n", stage, mission)
	return 0
}

// parseStageArgs reads the one stage name.
func parseStageArgs(args []string) (collab.LoopStage, error) {
	names := make([]string, 0, len(loopOrder))
	for _, stage := range loopOrder {
		names = append(names, string(stage))
	}
	usage := "give one stage: " + strings.Join(names, ", ")
	if len(args) != 1 {
		return "", errors.New(usage)
	}
	stage := collab.LoopStage(args[0])
	if !slices.Contains(loopOrder, stage) {
		return "", fmt.Errorf("unknown stage %q; %s", boundedLine(args[0], 64), usage)
	}
	return stage, nil
}

// currentSession finds the session `agent stage` runs in. A session the host
// names in the environment comes first, when the hooks opened its cache; it
// answers inSession. Else it is the session of this checkout and branch that
// was active last, as `agent attach` names a checkout.
func (e *Env) currentSession(ctx context.Context) (files sessionFiles, inSession, found bool) {
	var latest time.Time
	for _, named := range sessionEnvironment {
		id := e.Getenv(named.variable)
		if !sessionIDPattern.MatchString(id) {
			continue
		}
		candidate := e.files(named.engine, id)
		if cache, err := candidate.read(); err == nil && cache != nil && (!found || cache.ActiveAt.After(latest)) {
			files, inSession, found, latest = candidate, true, true, cache.ActiveAt
		}
	}
	if found {
		return files, inSession, found
	}
	root := canonicalRoot(e.Root)
	if root == "" {
		return files, false, false
	}
	branch := e.checkoutBranch(ctx, root)
	entries, err := os.ReadDir(e.sessionsDir())
	if err != nil {
		return files, false, false
	}
	scanned := 0
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !sessionCacheName.MatchString(entry.Name()) {
			continue
		}
		if scanned++; scanned > sessionScanMax {
			break
		}
		candidate := sessionFilesNamed(e.sessionsDir(), strings.TrimSuffix(entry.Name(), ".json"))
		cache, err := candidate.read()
		if err != nil || cache == nil || cache.Root != root || cache.Branch != branch {
			continue
		}
		if !found || cache.ActiveAt.After(latest) {
			files, found, latest = candidate, true, cache.ActiveAt
		}
	}
	return files, false, found
}
