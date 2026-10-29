package agentsession

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	collab "go.putnami.dev/protocol/collaboration"
)

const (
	sessionEngineClaudeCode = "claude-code"
	sessionEngineCodex      = "codex"

	// The events a sync names, for display.
	sessionEventStart       = "session_start"
	sessionEventToolUse     = "post_tool_use"
	sessionEventStop        = "stop"
	sessionEventWait        = "wait"
	sessionEventStopFailure = "stop_failure"
	sessionEventEnd         = "session_end"
	sessionEventStage       = "stage"

	// The hook events of both engines; StopFailure and SessionEnd are Claude
	// Code's only.
	hookEventSessionStart = "SessionStart"
	hookEventPostToolUse  = "PostToolUse"
	hookEventStop         = "Stop"
	hookEventStopFailure  = "StopFailure"
	hookEventSessionEnd   = "SessionEnd"

	// ExitWake is the exit code of an idle wake that brings a message. The
	// hook entry the CLI writes turns it into the host's own wake code.
	ExitWake = 75

	// hookStdinMax bounds one hook payload. A tool response can hold a whole
	// file, so the bound is wide.
	hookStdinMax = 64 << 20
	// hookContextMax bounds every text a hook hands an engine, in UTF-8 bytes:
	// Claude Code takes 10,000 characters, and a byte count is never below a
	// count of UTF-16 units or code points. The last 500 bytes stay spare.
	hookContextMax = 9_500
	// messageTextMax is the longest message text a sync answers, in
	// characters.
	messageTextMax = collab.MaxInboxTextLength
	// hookLockWait is how long a hook that is not a tool call waits for the
	// session lock. A tool call never waits.
	hookLockWait = 500 * time.Millisecond
	// sessionPromptsMax bounds the delivered prompts that wait for a reply.
	sessionPromptsMax = 20
	// sessionInboxMax bounds the fetched messages that wait for delivery.
	sessionInboxMax = collab.MaxInboxMessages
	// transcriptTailMax bounds the transcript bytes read for the model name
	// and for the usage of a Codex session.
	transcriptTailMax = 256 << 10
	// promptReminderMax bounds the text of a prompt a reminder repeats.
	promptReminderMax = 200
)

var (
	// sessionIDPattern is the session id rule of missions.sync.
	sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	// missionIDPattern is the message and decision id rule of missions.
	missionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// terminalEscapePattern is one terminal escape sequence: a CSI, such as
	// a color, ESC [ or U+009B, its parameter and intermediate bytes and a
	// final byte; or an OSC, such as a link, ESC ] or U+009D, up to BEL, ESC
	// \, U+009C or the end of the text.
	terminalEscapePattern = regexp.MustCompile("(?:\x1b\\[|\u009b)[0-?]*[ -/]*[@-~]|(?:\x1b\\]|\u009d)[^\x07\x1b\u009c]*(?:\x07|\x1b\\\\|\u009c|$)")
	// ackLinePattern is one reply line of the agent's last message:
	// [ack <message id>] <reply>.
	ackLinePattern = regexp.MustCompile(`\[ack ([A-Za-z0-9_-]{1,64})\][ \t]*([^\r\n]*)`)
)

// inboxMessage and messageReply are the contract's documents, kept as they
// are in the session cache.
type (
	inboxMessage = collab.InboxMessage
	messageReply = collab.MessageReply
)

// hookInput is the part of a Claude Code or Codex hook payload the hook
// reads. Both engines send the same names; agent_id, error and error_details
// are Claude Code's.
type hookInput struct {
	SessionID      string          `json:"session_id"`
	Cwd            string          `json:"cwd"`
	Event          string          `json:"hook_event_name"`
	TranscriptPath string          `json:"transcript_path"`
	Model          string          `json:"model"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
	StopHookActive bool            `json:"stop_hook_active"`
	LastMessage    string          `json:"last_assistant_message"`
	// PromptID names the user prompt the turn answers: Claude Code's, absent
	// before the first prompt.
	PromptID string `json:"prompt_id"`
	// AgentID marks a hook that runs for a subagent's tool call, under the
	// parent's session id.
	AgentID      string `json:"agent_id"`
	Error        string `json:"error"`
	ErrorDetails string `json:"error_details"`
}

// hookOutput is what a hook prints. Claude Code and Codex read the same
// shapes: additional context, or a block with a reason for Codex's Stop.
type hookOutput struct {
	HookSpecificOutput *hookContext `json:"hookSpecificOutput,omitempty"`
	Decision           string       `json:"decision,omitempty"`
	Reason             string       `json:"reason,omitempty"`
}

type hookContext struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// Hook runs `agent hook`: it handles one event read on stdin and answers the
// exit code. It never fails a turn: a failure, a missing channel and an
// outcome other than ok print nothing, leave one line in hook.log and exit 0.
// The output is printed last, so a failure never leaves half a document on
// stdout. Only an idle wake that brings a message exits ExitWake, with the
// message on stderr. The messages the output holds count as delivered only
// once it is written (see pendingDelivery).
func Hook(ctx context.Context, e *Env, args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	engine, idleWake, argErr := parseHookArgs(args)
	event := ""
	defer func() {
		if recovered := recover(); recovered != nil {
			e.log(engine, event, fmt.Errorf("panic: %v", recovered))
			code = 0
		}
	}()
	if idleWake && e.Getenv("CLAUDE_CODE_ENTRYPOINT") == "sdk-cli" {
		// An SDK host never leaves a session idle for the owner: no wait.
		_, _ = io.Copy(io.Discard, stdin)
		return 0
	}
	input, err := readHookInput(stdin)
	if input != nil {
		event = input.Event
	}
	if err == nil {
		err = argErr
	}
	if err == nil && engine != sessionEngineClaudeCode && engine != sessionEngineCodex {
		err = fmt.Errorf("unknown engine %q: use claude-code or codex", engine)
	}
	if err != nil {
		e.log(engine, event, err)
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, eventBudget(engine, input.Event, idleWake))
	defer cancel()
	if idleWake {
		text, delivery, err := e.idleWake(ctx, engine, *input)
		e.log(engine, "idle_wake", err)
		if text == "" {
			return 0
		}
		if _, err := io.WriteString(stderr, text+"\n"); err != nil {
			e.log(engine, "idle_wake", fmt.Errorf("write the wake text: %w", err))
			return 0
		}
		e.log(engine, "idle_wake", delivery.confirm())
		return ExitWake
	}
	output, delivery, err := e.handleHookEvent(ctx, engine, *input)
	if err != nil {
		e.log(engine, event, err)
		return 0
	}
	if output == nil {
		return 0
	}
	data, err := json.Marshal(output)
	if err != nil {
		e.log(engine, event, err)
		return 0
	}
	if _, err := stdout.Write(append(data, '\n')); err != nil {
		e.log(engine, event, fmt.Errorf("write the hook output: %w", err))
		return 0
	}
	e.log(engine, event, delivery.confirm())
	return 0
}

// pendingDelivery is the messages a hook's output hands the agent. The host
// reads that output only when the hook exits, and drops it when the hook runs
// past its timeout. So a message counts as delivered only once the hook wrote
// it: confirm then takes it from the inbox, and the next sync reports it. A
// hook stopped before that leaves the message for the next hook, which gives
// it again: a message reaches the agent at least once. Until that sync,
// another session of the mission may receive it too. The one case left is a
// host that stops the hook after the write, while the launcher ends; each
// event's budget leaves the launcher launcherMargin for that.
type pendingDelivery struct {
	files    sessionFiles
	messages []inboxMessage
	// continues reports that the messages continue or start a turn: none of
	// them continues another one.
	continues bool
}

// confirm records the messages as delivered. A message another hook
// confirmed first stays as that hook recorded it.
func (d pendingDelivery) confirm() error {
	if len(d.messages) == 0 {
		return nil
	}
	return lockedUpdate(d.files, func(cache *sessionCache) (*sessionCache, error) {
		if cache == nil {
			return nil, nil
		}
		for _, message := range d.messages {
			cache.Inbox = withoutMessage(cache.Inbox, message.ID)
			if !containsString(cache.Seen, message.ID) {
				markDelivered(cache, message)
			}
			if d.continues {
				cache.ContinuedFor = appendBounded(cache.ContinuedFor, message.ID, sessionSeenMax)
			}
		}
		return cache, nil
	})
}

// withoutMessage is inbox without the message id, in order.
func withoutMessage(inbox []inboxMessage, id string) []inboxMessage {
	kept := make([]inboxMessage, 0, len(inbox))
	for _, message := range inbox {
		if message.ID != id {
			kept = append(kept, message)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// parseHookArgs reads `--engine <claude-code|codex>` and `--idle-wake`.
func parseHookArgs(args []string) (engine string, idleWake bool, err error) {
	for len(args) > 0 {
		raw := args[0]
		args = args[1:]
		name, value, hasValue := strings.Cut(raw, "=")
		switch name {
		case "--idle-wake":
			if hasValue && value != "true" {
				return engine, idleWake, fmt.Errorf("%s takes no value", name)
			}
			idleWake = true
		case "--engine":
			if !hasValue {
				if len(args) == 0 {
					return engine, idleWake, errors.New("--engine needs claude-code or codex")
				}
				value, args = args[0], args[1:]
			}
			engine = value
		default:
			return engine, idleWake, fmt.Errorf("unexpected hook argument %q", boundedLine(raw, 64))
		}
	}
	return engine, idleWake, nil
}

// readHookInput reads one payload. It reads stdin to its end even past the
// bound, so the engine never writes into a closed pipe.
func readHookInput(r io.Reader) (*hookInput, error) {
	data, err := io.ReadAll(io.LimitReader(r, hookStdinMax+1))
	if err != nil {
		return nil, fmt.Errorf("read the hook payload: %w", err)
	}
	if len(data) > hookStdinMax {
		_, _ = io.Copy(io.Discard, r)
		return nil, errors.New("the hook payload exceeds 64 MiB")
	}
	var input hookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return nil, fmt.Errorf("decode the hook payload: %w", err)
	}
	if !sessionIDPattern.MatchString(input.SessionID) {
		return &input, errors.New("the hook payload names no valid session_id")
	}
	return &input, nil
}

func sessionKey(engine, session string) string {
	return engine + ":" + session
}

func (e *Env) handleHookEvent(ctx context.Context, engine string, input hookInput) (*hookOutput, pendingDelivery, error) {
	switch input.Event {
	case hookEventSessionStart:
		return e.hookSessionStart(ctx, engine, input)
	case hookEventPostToolUse:
		return nil, pendingDelivery{}, e.hookPostToolUse(engine, input)
	case hookEventStop:
		return e.hookStop(ctx, engine, input)
	case hookEventStopFailure:
		return nil, pendingDelivery{}, e.hookStopFailure(ctx, engine, input)
	case hookEventSessionEnd:
		return nil, pendingDelivery{}, e.hookSessionEnd(ctx, engine, input)
	}
	return nil, pendingDelivery{}, nil
}

// sessionIdentity is what a session reads from its checkout: the workspace
// root, the lowercase owner/name of origin and the branch. Reading it runs
// git, so only SessionStart, Stop and StopFailure read it, never a tool call.
type sessionIdentity struct {
	root, repository, branch string
}

func (e *Env) readSessionIdentity(ctx context.Context, cwd string) sessionIdentity {
	root := canonicalRoot(sessionWorkspaceRoot(cwd, e.Root))
	identity := sessionIdentity{root: root}
	if root == "" {
		return identity
	}
	identity.repository = e.originRepository(ctx, root)
	identity.branch = e.checkoutBranch(ctx, root)
	return identity
}

// peekIdentity reads the identity only when the cache has none yet: Stop and
// StopFailure keep the one SessionStart read.
func (e *Env) peekIdentity(ctx context.Context, files sessionFiles, input hookInput) *sessionIdentity {
	if peek, _ := files.read(); peek != nil && peek.Root != "" {
		return nil
	}
	identity := e.readSessionIdentity(ctx, input.Cwd)
	return &identity
}

// openSession returns the cache of the session the payload names, with the
// identity read at this event.
func (e *Env) openSession(cache *sessionCache, engine string, input hookInput, identity *sessionIdentity) *sessionCache {
	if cache == nil || cache.Engine != engine || cache.Session != input.SessionID {
		cache = &sessionCache{Engine: engine, Session: input.SessionID}
	}
	if input.Cwd != "" && len(input.Cwd) <= sessionPathMax {
		cache.Cwd = input.Cwd
	}
	if input.TranscriptPath != "" && len(input.TranscriptPath) <= sessionPathMax {
		cache.Transcript = input.TranscriptPath
	}
	if model := boundedLine(input.Model, collab.MaxModelLength); model != "" {
		cache.Model = model
	}
	if identity != nil && len(identity.root) <= sessionPathMax && len(identity.repository) <= sessionPathMax && len(identity.branch) <= sessionPathMax {
		cache.Root, cache.Repository, cache.Branch = identity.root, identity.repository, identity.branch
	}
	refreshMissionKey(cache, e.sessionsDir())
	return cache
}

// refreshMissionKey reads the mission declared for the cached checkout and
// branch: a file read, no git. A new key starts a new mission.
func refreshMissionKey(cache *sessionCache, dir string) {
	key := ""
	if cache.Root != "" {
		key = sessionMissionKey(dir, cache.Root, cache.Branch)
	}
	if key != cache.Mission.Key {
		cache.Mission = sessionMissionState{Key: key}
	}
}

// setSessionState sets the state the next sync publishes. The wait end goes
// with waiting only, and the error with failed only.
func setSessionState(cache *sessionCache, state collab.SessionState, event string) {
	cache.State, cache.LastEvent = state, event
	if state != collab.SessionStateWaiting {
		cache.WaitUntil = time.Time{}
	}
	if state != collab.SessionStateFailed {
		cache.Error = ""
	}
}

// hookSessionStart opens the session and gives the agent its mission sheet
// and, in full, the waiting messages that fit after it (sessionStartText).
// It syncs every time, and reads the mission's checkpoint when the workspace
// binds memory; when the sync fails, the cached sheet answers with its date.
// The messages count as delivered once Hook wrote the text.
func (e *Env) hookSessionStart(ctx context.Context, engine string, input hookInput) (*hookOutput, pendingDelivery, error) {
	files := e.files(engine, input.SessionID)
	identity := e.readSessionIdentity(ctx, input.Cwd)
	now := e.Now()
	if err := lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
		cache = e.openSession(cache, engine, input, &identity)
		setSessionState(cache, collab.SessionStateWorking, sessionEventStart)
		cache.ActiveAt = now
		return cache, nil
	}); err != nil {
		return nil, pendingDelivery{}, err
	}
	syncErr := e.syncSession(ctx, files)
	e.log(engine, input.Event, syncErr)
	e.log(engine, input.Event, e.readCheckpoint(ctx, files))
	var text string
	var messages []inboxMessage
	err := lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
		if cache == nil {
			return nil, nil
		}
		text, messages = sessionStartText(cache, missionSheet(cache, syncErr, now))
		return cache, nil
	})
	if err != nil || text == "" {
		return nil, pendingDelivery{}, err
	}
	return contextOutput(hookEventSessionStart, text), pendingDelivery{files: files, messages: messages}, nil
}

// lockedUpdate reads the cache under the lock, applies change and writes
// what it answers. A nil answer writes nothing. It waits at most
// hookLockWait, so a tool call never uses it.
func lockedUpdate(files sessionFiles, change func(*sessionCache) (*sessionCache, error)) error {
	lock, err := files.lockWithin(hookLockWait)
	if err != nil {
		return err
	}
	defer lock.release()
	cache, err := files.read()
	if err != nil {
		// A cache this build cannot read starts over: keeping it would stop
		// the session for good.
		cache = nil
	}
	spooled, paths := files.readSpool()
	updated, err := change(cache)
	if err != nil || updated == nil {
		return err
	}
	applySpooled(updated, spooled)
	if err := files.write(updated); err != nil {
		return err
	}
	removeFiles(paths)
	return nil
}

// applySpooled applies spooled tool calls in the order they happened.
func applySpooled(cache *sessionCache, spooled []spooledToolUse) {
	for _, event := range spooled {
		observeToolUse(cache, event.ToolName, event.ToolInput, event.ToolResponse)
	}
}

// hookPostToolUse records a plan change. The hook entry runs it on the plan
// tools only, and it ignores any other tool at once. It runs no git and calls
// no provider: what it records goes with the next sync, at the end of the
// turn. It never delivers a message: the models ignore or refuse a message
// that follows a tool call. When another hook holds the lock, it spools what
// it observed and returns at once.
func (e *Env) hookPostToolUse(engine string, input hookInput) error {
	if !isPlanTool(input.ToolName) {
		return nil
	}
	if input.AgentID != "" {
		// Claude Code runs a subagent's hooks under the parent's session id:
		// a subagent's plan is not the session's plan.
		return nil
	}
	now := e.Now()
	files := e.files(engine, input.SessionID)
	own := spooledToolUse{At: now.UnixNano(), ToolName: input.ToolName, ToolInput: input.ToolInput, ToolResponse: input.ToolResponse}
	cache, err := files.read()
	if err != nil {
		return err
	}
	if cache == nil {
		// No SessionStart opened this session yet. The next hook that opens
		// it applies the spool.
		return files.spool(own)
	}
	lock, err := files.tryLock()
	if errors.Is(err, errSessionBusy) {
		return files.spool(own)
	}
	if err != nil {
		return err
	}
	defer lock.release()
	if cache, err = files.read(); err != nil || cache == nil {
		if err == nil {
			err = files.spool(own)
		}
		return err
	}
	spooled, paths := files.readSpool()
	spooled = append(spooled, own)
	sort.SliceStable(spooled, func(i, j int) bool { return spooled[i].At < spooled[j].At })
	applySpooled(cache, spooled)
	setSessionState(cache, collab.SessionStateWorking, sessionEventToolUse)
	cache.ActiveAt = now
	if err := files.write(cache); err != nil {
		return err
	}
	removeFiles(paths)
	return nil
}

// hookStop publishes the end of the turn with the agent's last message, its
// plan, the messages it received and its `[ack <id>]` replies, in one sync,
// then decides whether the turn continues:
//
//  1. a message that was never delivered continues the turn with that
//     message;
//  2. else a delivered prompt without a reply continues the turn once with a
//     reminder, unless the engine already continues a Stop;
//  3. no message ever continues a turn twice.
//
// Claude Code continues with additional context, because a block shows
// "Stop hook error occurred"; Codex continues with a block. With nothing to
// deliver and an open decision this session asked, Codex waits for the
// answer; another session's decision never holds it. When the sync answers a
// message that continues the turn, a second sync publishes that the session
// works again. The messages count as delivered once Hook wrote the reason, and
// the next sync reports them.
func (e *Env) hookStop(ctx context.Context, engine string, input hookInput) (*hookOutput, pendingDelivery, error) {
	files := e.files(engine, input.SessionID)
	identity := e.peekIdentity(ctx, files, input)
	now := e.Now()
	dir := e.sessionsDir()
	var reason string
	var messages []inboxMessage
	if err := lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
		cache = e.openSession(cache, engine, input, identity)
		if cache.Model == "" || engine == sessionEngineClaudeCode {
			if model := transcriptModel(cache.Transcript); model != "" {
				cache.Model = model
			}
		}
		cache.LastMessage = boundedText(input.LastMessage, collab.MaxLastMessageLength)
		// A reply `agent reply` recorded comes first: the provider keeps the
		// first reply of a message and refuses another.
		closeRepliedPrompts(cache, dir)
		recordAckLines(cache, input.LastMessage, dir)
		cache.ActiveAt = now
		reason, messages = stopReason(cache, input.StopHookActive, false)
		if reason != "" {
			setSessionState(cache, collab.SessionStateWorking, sessionEventStop)
		} else {
			setSessionState(cache, collab.SessionStateHandedBack, sessionEventStop)
		}
		return cache, nil
	}); err != nil {
		return nil, pendingDelivery{}, err
	}
	syncErr := e.syncSession(ctx, files)
	e.log(engine, input.Event, syncErr)
	decidedAt := e.Now()
	republish, wait := false, false
	if err := lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
		if cache == nil {
			return nil, nil
		}
		if reason == "" {
			// The sync answered new messages: the turn continues with them.
			if reason, messages = stopReason(cache, input.StopHookActive, true); reason != "" {
				setSessionState(cache, collab.SessionStateWorking, sessionEventStop)
				republish = true
			}
		}
		cache.ActiveAt, cache.StopDecidedAt, cache.StopContinued = decidedAt, decidedAt, reason != ""
		cache.StopKey = stopKey(input)
		wait = engine == sessionEngineCodex && reason == "" && syncErr == nil && ownDecisionOpen(cache)
		return cache, nil
	}); err != nil {
		e.log(engine, input.Event, err)
	}
	if republish {
		e.log(engine, input.Event, e.syncSession(ctx, files))
	}
	if wait {
		// Codex holds the turn while its Stop hook runs, so no turn starts
		// during this wait: it watches no transcript.
		text, waited, err := e.waitForOwner(ctx, files, ownerWait{window: sessionDecisionWait, baseline: decidedAt, forDecision: true})
		e.log(engine, input.Event, err)
		if text != "" {
			reason, messages = text, waited
			err := lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
				if cache == nil {
					return nil, nil
				}
				cache.StopContinued = true
				return cache, nil
			})
			e.log(engine, input.Event, err)
		}
	}
	if reason == "" {
		return nil, pendingDelivery{}, nil
	}
	delivery := pendingDelivery{files: files, messages: messages, continues: true}
	if engine == sessionEngineClaudeCode {
		return contextOutput(hookEventStop, reason), delivery, nil
	}
	return &hookOutput{Decision: "block", Reason: reason}, delivery, nil
}

// ownDecisionOpen reports whether a decision this session asked is open in
// the mission's view.
func ownDecisionOpen(cache *sessionCache) bool {
	for _, decision := range cache.Mission.OpenDecisions {
		if containsString(cache.Asked, decision.ID) {
			return true
		}
	}
	return false
}

// hookStopFailure publishes that the turn ended on an API error, with the
// error. Claude Code ignores its output and exit code.
func (e *Env) hookStopFailure(ctx context.Context, engine string, input hookInput) error {
	files := e.files(engine, input.SessionID)
	identity := e.peekIdentity(ctx, files, input)
	now := e.Now()
	if err := lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
		cache = e.openSession(cache, engine, input, identity)
		setSessionState(cache, collab.SessionStateFailed, sessionEventStopFailure)
		cache.Error = stopFailureError(input)
		cache.ActiveAt = now
		return cache, nil
	}); err != nil {
		return err
	}
	return e.syncSession(ctx, files)
}

// hookSessionEnd publishes that the session ended, with its usage. It ends
// the wait of an idle wake that runs beside it: that wait finds another
// waiter name and stops without a write. Claude Code ignores its output and
// exit code.
func (e *Env) hookSessionEnd(ctx context.Context, engine string, input hookInput) error {
	files := e.files(engine, input.SessionID)
	identity := e.peekIdentity(ctx, files, input)
	now := e.Now()
	if err := lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
		cache = e.openSession(cache, engine, input, identity)
		setSessionState(cache, collab.SessionStateEnded, sessionEventEnd)
		cache.Waiter = ""
		cache.ActiveAt = now
		return cache, nil
	}); err != nil {
		return err
	}
	return e.syncSession(ctx, files)
}

// stopFailureError is the error type and its details, at most
// collab.MaxErrorLength bytes.
func stopFailureError(input hookInput) string {
	text := firstString(strings.TrimSpace(input.Error), "unknown")
	if details := strings.TrimSpace(input.ErrorDetails); details != "" {
		text += ": " + details
	}
	return boundedBytes(strings.ReplaceAll(text, "\x00", ""), collab.MaxErrorLength)
}

// recordAckLines queues the `[ack <id>] <reply>` lines of the agent's last
// message as replies. Only a message delivered to this session counts, once,
// and the first line for an id wins. A reply `agent reply` recorded wins
// over a line: the provider keeps the first reply of a message and refuses
// another, so a line for a message whose marker is there queues nothing.
func recordAckLines(cache *sessionCache, message, dir string) {
	for _, match := range ackLinePattern.FindAllStringSubmatch(message, -1) {
		id, reply := match[1], boundedText(match[2], collab.MaxReplyLength)
		if reply == "" || !containsString(cache.Seen, id) || containsString(cache.Replied, id) || replyQueued(cache.Replies, id) {
			continue
		}
		if _, err := os.Stat(repliedMarkerPath(dir, id)); err == nil {
			cache.Replied = appendBounded(cache.Replied, id, sessionSeenMax)
			closePrompt(cache, id)
			continue
		}
		if len(cache.Replies) >= collab.MaxSyncReplies {
			break
		}
		cache.Replies = append(cache.Replies, messageReply{Message: id, Reply: reply})
		closePrompt(cache, id)
	}
}

func replyQueued(replies []messageReply, id string) bool {
	for _, reply := range replies {
		if reply.Message == id {
			return true
		}
	}
	return false
}

// closeRepliedPrompts closes the prompts `agent reply` replied to, and
// removes their markers.
func closeRepliedPrompts(cache *sessionCache, dir string) {
	for _, prompt := range append([]pendingPrompt(nil), cache.Prompts...) {
		marker := repliedMarkerPath(dir, prompt.ID)
		if _, err := os.Stat(marker); err != nil {
			continue
		}
		closePrompt(cache, prompt.ID)
		cache.Replied = appendBounded(cache.Replied, prompt.ID, sessionSeenMax)
		_ = os.Remove(marker)
	}
}

func closePrompt(cache *sessionCache, id string) {
	kept := cache.Prompts[:0]
	for _, prompt := range cache.Prompts {
		if prompt.ID != id {
			kept = append(kept, prompt)
		}
	}
	cache.Prompts = kept
}

// repliedMarkerPath is the marker `agent reply` leaves for one message id;
// the id matches missionIDPattern, so it is a plain file name.
func repliedMarkerPath(dir, message string) string {
	return filepath.Join(dir, repliedMarkersDir, message)
}

// repliedMarkersDir holds the replied markers, under the session store.
const repliedMarkersDir = "replied"

// stopReason applies the Stop continuation rules to the cache and answers the
// text to continue with, or "" to let the turn end, with the waiting messages
// the text gives. newOnly limits it to rule 1, after a sync answered.
// Reminders go in order while their text stays within hookContextMax; the
// first always goes, and fits, since a prompt keeps at most promptReminderMax
// characters. A prompt whose reminder does not fit waits for the next turn
// end.
func stopReason(cache *sessionCache, stopHookActive, newOnly bool) (string, []inboxMessage) {
	if messages := nextMessages(cache, hookContextMax); len(messages) > 0 {
		return deliveredText(messages), messages
	}
	if newOnly || stopHookActive {
		return "", nil
	}
	var reminders []string
	size := len(sessionReplyWays)
	for _, prompt := range cache.Prompts {
		if containsString(cache.ContinuedFor, prompt.ID) {
			continue
		}
		reminder := fmt.Sprintf("The owner's message %s (\"%s\") has no recorded reply yet.", prompt.ID, prompt.Text)
		if size += len(reminder) + len("\n\n"); len(reminders) > 0 && size > hookContextMax {
			break
		}
		cache.ContinuedFor = appendBounded(cache.ContinuedFor, prompt.ID, sessionSeenMax)
		reminders = append(reminders, reminder)
	}
	if len(reminders) == 0 {
		return "", nil
	}
	return joinParagraphs(strings.Join(reminders, "\n\n"), sessionReplyWays), nil
}

// sessionReplyWays names both ways a reply reaches the owner. A text the hook
// gives the agent states it once, at its end: after the SessionStart context,
// and after the messages or reminders of a turn end that hold a prompt.
const sessionReplyWays = `The command "putnami agent reply --message <message id> --reply <text>" records a reply to a message id; a line "[ack <message id>] <reply>" in the final message of a turn also records it.`

// deliveredText is the text that delivers messages at the end of a turn or
// during a wait: each message, then the ways a reply reaches the owner once
// when one of them is a prompt.
func deliveredText(messages []inboxMessage) string {
	text := renderMessages(messages)
	for _, message := range messages {
		if message.Kind == collab.InboxKindPrompt {
			return joinParagraphs(text, sessionReplyWays)
		}
	}
	return text
}

// observeToolUse records what one plan call changes in the session, and
// answers whether it changed anything to publish.
func observeToolUse(cache *sessionCache, toolName string, input, response json.RawMessage) bool {
	if !isPlanTool(toolName) {
		return false
	}
	next, ok := applyPlanTool(toolName, input, response, cache.Plan)
	if !ok {
		return false
	}
	digest := planDigest(next)
	if digest == cache.PlanDigest {
		return false
	}
	cache.Plan, cache.PlanDigest = next, digest
	return true
}

// nextMessages answers the waiting messages a text gives, in order, while
// their text, deliveredText of them, stays within budget bytes. The first
// message always goes: renderMessage keeps one message within
// messageRenderMax, so it fits hookContextMax, the budget of a turn end, with
// the reply ways. They stay in the inbox until pendingDelivery.confirm.
func nextMessages(cache *sessionCache, budget int) []inboxMessage {
	var messages []inboxMessage
	text, prompt := "", false
	for _, message := range cache.Inbox {
		next, nextPrompt := joinParagraphs(text, renderMessage(message)), prompt || message.Kind == collab.InboxKindPrompt
		whole := next
		if nextPrompt {
			whole = joinParagraphs(next, sessionReplyWays)
		}
		if len(messages) > 0 && len(whole) > budget {
			break
		}
		text, prompt = next, nextPrompt
		messages = append(messages, message)
	}
	return messages
}

// sessionStartText is the SessionStart context: the sheet, then each waiting
// message in full, rendered as at the end of a turn, then the ways a reply
// reaches the owner, with the messages it gives. The sheet carries no
// message, so a message is given only when this text holds the whole of it.
// Messages go in order while the whole text stays within hookContextMax; the
// first one that does not fit, even the first message, stays in the inbox
// with the ones after it, for the end of the turn.
func sessionStartText(cache *sessionCache, sheet string) (string, []inboxMessage) {
	text := sheet
	var messages []inboxMessage
	for _, message := range cache.Inbox {
		next := joinParagraphs(text, renderMessage(message))
		if len(joinParagraphs(next, sessionReplyWays)) > hookContextMax {
			break
		}
		messages = append(messages, message)
		text = next
	}
	if text == "" {
		return "", nil
	}
	return joinParagraphs(text, sessionReplyWays), messages
}

func markDelivered(cache *sessionCache, message inboxMessage) {
	cache.Seen = appendBounded(cache.Seen, message.ID, sessionSeenMax)
	cache.Acks = appendBounded(cache.Acks, message.ID, sessionSeenMax)
	if message.Kind == collab.InboxKindPrompt {
		cache.Prompts = append(cache.Prompts, pendingPrompt{ID: message.ID, Text: boundedText(message.Text, promptReminderMax)})
		if len(cache.Prompts) > sessionPromptsMax {
			cache.Prompts = cache.Prompts[len(cache.Prompts)-sessionPromptsMax:]
		}
	}
}

// renderMessages states the delivered messages, one paragraph each.
func renderMessages(messages []inboxMessage) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		parts = append(parts, renderMessage(message))
	}
	return strings.Join(parts, "\n\n")
}

// messageCutNote ends an owner's text the hook cut.
const messageCutNote = " (cut: the message is longer)"

// messageRenderMax bounds one rendered message, in bytes: with the reply
// ways after it, it fits hookContextMax.
const messageRenderMax = hookContextMax - len("\n\n"+sessionReplyWays)

// renderMessage states one message with its id and time. It reports what the
// owner sent; it never tells the agent what to do, and the text that delivers
// it states how a reply reaches the owner. A rendering stays within
// messageRenderMax bytes, so a turn end always delivers the first waiting
// message. The owner's text, the answer of a decision or else the message
// text, is cut on a character boundary and ends with messageCutNote when it
// holds more than messageTextMax characters or when the rendering would pass
// that bound.
func renderMessage(message inboxMessage) string {
	raw := message.Text
	if message.Kind == collab.InboxKindAnswer && message.Decision != nil && boundedText(message.Decision.Answer, messageTextMax) != "" {
		raw = message.Decision.Answer
	}
	text := boundedText(raw, messageTextMax)
	cut := utf8.RuneCountInString(strings.TrimSpace(raw)) > messageTextMax
	rendered := renderMessageText(message, text, cut)
	for len(rendered) > messageRenderMax && text != "" {
		text = boundedBytes(text, max(len(text)-(len(rendered)-messageRenderMax), 0))
		cut = true
		rendered = renderMessageText(message, text, cut)
	}
	return rendered
}

// renderMessageText states message with the owner's text text, which ends
// with messageCutNote when cut.
func renderMessageText(message inboxMessage, text string, cut bool) string {
	if cut {
		text += messageCutNote
	}
	at := ""
	if created, err := time.Parse(time.RFC3339Nano, message.CreatedAt); err == nil {
		at = " at " + created.UTC().Format("15:04") + " UTC"
	}
	switch {
	case message.Kind == collab.InboxKindAnswer && message.Decision != nil:
		// An agent session wrote the question, and the label of the chosen
		// option that the answer starts with, so their lines are quoted: none
		// of them can pass for a line of the hook.
		return fmt.Sprintf("The owner answered the open decision %s%s (message id %s).\nQuestion:\n%s\nAnswer:\n%s",
			message.Decision.ID, at, message.ID, quoteLines(boundedText(message.Decision.Question, collab.MaxQuestionLength)), quoteLines(text))
	case message.Kind == collab.InboxKindPrompt:
		return fmt.Sprintf("The owner sent this message%s (message id %s): \"%s\"", at, message.ID, text)
	default:
		return fmt.Sprintf("The owner sent this information%s (message id %s): \"%s\"", at, message.ID, text)
	}
}

// lineBreaks turns every character a reader may take for a line break into
// "\n".
var lineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n", "\u0085", "\n", " ", "\n", " ", "\n")

// quoteLines starts every line of text with "> ".
func quoteLines(text string) string {
	lines := strings.Split(lineBreaks.Replace(text), "\n")
	for i, line := range lines {
		lines[i] = "> " + line
	}
	return strings.Join(lines, "\n")
}

func contextOutput(event, text string) *hookOutput {
	return &hookOutput{HookSpecificOutput: &hookContext{HookEventName: event, AdditionalContext: text}}
}

func joinParagraphs(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "\n\n")
}

// boundedText is value with valid UTF-8 and no NUL, trimmed and cut to at
// most limit runes.
func boundedText(value string, limit int) string {
	value = strings.TrimSpace(strings.ReplaceAll(strings.ToValidUTF8(value, ""), "\x00", ""))
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	return string([]rune(value)[:limit])
}

// lineText is value on one line, the rule missions.sync holds a one-line
// member to: valid UTF-8, its whitespace collapsed to single spaces, and no
// other control character, such as ESC, DEL or a C1 control. A terminal
// escape sequence goes whole, so a color leaves no "[31m" behind.
func lineText(value string) string {
	value = terminalEscapePattern.ReplaceAllString(strings.ToValidUTF8(value, ""), "")
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

// boundedLine is lineText cut to at most limit runes.
func boundedLine(value string, limit int) string {
	value = lineText(value)
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	return strings.TrimSpace(string([]rune(value)[:limit]))
}

// boundedBytes cuts value to at most limit bytes, on a character boundary.
func boundedBytes(value string, limit int) string {
	value = strings.ToValidUTF8(value, "")
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// transcriptModel reads the model of the last assistant turn from the tail
// of a Claude Code transcript, which its hook payloads do not carry.
func transcriptModel(path string) string {
	lines := bytes.Split(transcriptTail(path), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if !bytes.Contains(lines[i], []byte(`"assistant"`)) {
			continue
		}
		var line struct {
			Type    string `json:"type"`
			Message struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		if json.Unmarshal(lines[i], &line) != nil || line.Type != "assistant" {
			continue
		}
		if model := boundedLine(line.Message.Model, collab.MaxModelLength); model != "" && !strings.HasPrefix(model, "<") {
			return model
		}
	}
	return ""
}
