package agentsession

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
)

// CommandIO is what `agent ask` and `agent reply` read and write.
type CommandIO struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// JSON prints the operation's result document instead of the text lines.
	JSON bool
	// ResolvePath resolves a path argument as the user typed it. Nil keeps
	// the argument as it is.
	ResolvePath func(string) string
}

// Ask runs `agent ask --input <file|->`: it asks the owner of the session's
// mission a decision through missions.ask and records the decision's id in
// the session cache, so the next sync binds it to the session. The input is
// a missions.ask document without its mission, which the session supplies;
// without an idempotency key the command derives one from the session and
// the content, so the same ask run again replays. A usage error exits 2 and
// a failure exits 1; an ask whose outcome is unknown is safe to run again.
func Ask(ctx context.Context, e *Env, args []string, cmd CommandIO) int {
	request, err := readAskRequest(args, cmd)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.Stderr, "agent ask: %v\n", err)
		return 2
	}
	files, inSession, found := e.currentSession(ctx)
	if !found {
		_, _ = fmt.Fprintln(cmd.Stderr, "agent ask: no agent session found for this checkout: run it from a Claude Code or Codex session that runs the Putnami session hooks")
		return 1
	}
	var mission collab.MissionName
	var session string
	publishable := false
	now := e.Now()
	dir := e.sessionsDir()
	if err := lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
		if cache == nil {
			return nil, nil
		}
		refreshMissionKey(cache, dir)
		if publishable = sessionPublishable(cache); !publishable {
			return nil, nil
		}
		mission = collab.MissionName{Repository: cache.Repository, Key: cache.Mission.Key}
		if !cache.Mission.Known {
			mission.Title = boundedLine(missionTitle(cache.Mission.Key), collab.MaxTitleLength)
		}
		session = sessionKey(cache.Engine, cache.Session)
		if inSession {
			// The host runs this command inside a turn of the session.
			setSessionState(cache, collab.SessionStateWorking, sessionEventToolUse)
			cache.ActiveAt = now
		}
		return cache, nil
	}); err != nil {
		_, _ = fmt.Fprintf(cmd.Stderr, "agent ask: %v\n", err)
		return 1
	}
	if !publishable {
		_, _ = fmt.Fprintln(cmd.Stderr, "agent ask: the session reports to no mission: it needs a repository with an origin remote and a branch, or `putnami agent attach --issue <n>`")
		return 1
	}
	if request.Mission.Repository != "" && (request.Mission.Repository != mission.Repository || request.Mission.Key != mission.Key) {
		_, _ = fmt.Fprintf(cmd.Stderr, "agent ask: the input names mission %s of %s; this session reports to %s of %s: leave the mission out\n",
			boundedLine(request.Mission.Key, 64), boundedLine(request.Mission.Repository, 64), mission.Key, mission.Repository)
		return 2
	}
	request.Mission = mission
	if request.IdempotencyKey == "" {
		request.IdempotencyKey = askIdempotencyKey(session, request)
	}
	ctx, cancel := context.WithTimeout(ctx, hookTimeout-launcherMargin)
	defer cancel()
	envelope, err := e.call(ctx, collab.ContractMissions, collab.OperationAsk, request)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.Stderr, "agent ask: %v; the decision may be recorded: run the same command again, which replays it\n", err)
		return 1
	}
	if envelope.Outcome != collab.OutcomeOK {
		_, _ = fmt.Fprintf(cmd.Stderr, "agent ask: %v\n", outcomeError(envelope))
		return 1
	}
	var answer collab.MissionsAskResult
	if err := json.Unmarshal(envelope.Result, &answer); err != nil || !missionIDPattern.MatchString(answer.Decision.ID) {
		_, _ = fmt.Fprintln(cmd.Stderr, "agent ask: the provider answered a result that names no valid decision")
		return 1
	}
	if err := lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
		if cache == nil {
			return nil, nil
		}
		recordAskedDecision(cache, answer.Decision.ID)
		return cache, nil
	}); err != nil {
		_, _ = fmt.Fprintf(cmd.Stderr, "agent ask: decision %s is recorded, and the session could not note it: %v; run the same command again, which replays it\n", answer.Decision.ID, err)
		return 1
	}
	return printResult(cmd, answer, func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "DECISION_ID=%s\nDECISION_STATE=%s\n", answer.Decision.ID, answer.Decision.State)
		if answer.Decision.URL != "" {
			_, _ = fmt.Fprintf(w, "DECISION_URL=%s\n", answer.Decision.URL)
		}
	})
}

// recordAskedDecision notes a decision the session asked: Asked keeps it for
// a Codex Stop that waits for the answer, and Decisions queues it for the
// next sync, which binds it to the session.
func recordAskedDecision(cache *sessionCache, id string) {
	cache.Asked = appendBounded(cache.Asked, id, collab.MaxOpenDecisions)
	cache.Decisions = appendBounded(cache.Decisions, id, collab.MaxSyncDecisions)
}

// askIdempotencyKey derives the key of an ask that gave none from the
// session and the content, so the same ask run again replays. The mission
// title is not content: it is there until a sync answers, and the key does
// not change with it.
func askIdempotencyKey(session string, request collab.MissionsAskInput) string {
	request.IdempotencyKey, request.Mission.Title = "", ""
	content, _ := json.Marshal(request)
	sum := sha256.Sum256(append([]byte(session+"\n"), content...))
	return "ask-" + hex.EncodeToString(sum[:16])
}

// readAskRequest reads `--input <file|->`, a file or standard input, as a
// missions.ask document. Unknown members are refused.
func readAskRequest(args []string, cmd CommandIO) (collab.MissionsAskInput, error) {
	var request collab.MissionsAskInput
	path := ""
	for len(args) > 0 {
		raw := args[0]
		args = args[1:]
		name, inline, hasValue := strings.Cut(raw, "=")
		if name != "--input" {
			return request, fmt.Errorf("unexpected argument %q; use --input <file|->", boundedLine(raw, 64))
		}
		if path != "" {
			return request, errors.New("--input given twice")
		}
		if hasValue {
			path = inline
		} else if len(args) > 0 {
			path, args = args[0], args[1:]
		}
		if path == "" {
			return request, errors.New("--input needs a file, or - for standard input")
		}
	}
	if path == "" {
		return request, errors.New("give the decision as --input <file|->: a missions.ask document without its mission")
	}
	reader := cmd.Stdin
	if path != "-" {
		if cmd.ResolvePath != nil {
			path = cmd.ResolvePath(path)
		}
		file, err := os.Open(path)
		if err != nil {
			return request, fmt.Errorf("read the input: %w", err)
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, collab.MaxDocumentBytes+1))
	if err != nil {
		return request, fmt.Errorf("read the input: %w", err)
	}
	if len(data) > collab.MaxDocumentBytes {
		return request, fmt.Errorf("the input exceeds %d bytes", collab.MaxDocumentBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("the input is not a missions.ask document: %w", err)
	}
	if decoder.More() {
		return request, errors.New("the input holds more than one document")
	}
	return request, nil
}

// Reply runs `agent reply --message <id> --reply <text>`: it records the
// reply to an owner's message through missions.reply and leaves the replied
// marker, so the Stop of the session that holds the prompt sends no
// `[ack <id>]` line for that message. A message that already holds a reply
// leaves the marker too and exits 1, since this reply was not written. A
// usage error exits 2 and a failure exits 1.
func Reply(ctx context.Context, e *Env, args []string, cmd CommandIO) int {
	request, err := parseReplyArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.Stderr, "agent reply: %v\n", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, hookTimeout-launcherMargin)
	defer cancel()
	envelope, err := e.call(ctx, collab.ContractMissions, collab.OperationReply, request)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.Stderr, "agent reply: %v; the reply may be recorded: run the same command again\n", err)
		return 1
	}
	closed := envelope.Outcome == collab.OutcomeConflict && envelope.Error != nil && envelope.Error.Reason == collab.ReasonMessageClosed
	if envelope.Outcome != collab.OutcomeOK && !closed {
		_, _ = fmt.Fprintf(cmd.Stderr, "agent reply: %v\n", outcomeError(envelope))
		return 1
	}
	if err := writeFileAtomic(repliedMarkerPath(e.sessionsDir(), request.Message), []byte(e.Now().UTC().Format(time.RFC3339Nano))); err != nil {
		_, _ = fmt.Fprintf(cmd.Stderr, "agent reply: the reply to %s is recorded, and the session could not note it: %v\n", request.Message, err)
		return 1
	}
	if closed {
		_, _ = fmt.Fprintf(cmd.Stderr, "agent reply: message %s already holds a reply, so this one was not written\n", request.Message)
		return 1
	}
	var answer collab.MissionsReplyResult
	if err := json.Unmarshal(envelope.Result, &answer); err != nil {
		_, _ = fmt.Fprintf(cmd.Stderr, "agent reply: the provider answered an unreadable result: %v\n", err)
		return 1
	}
	return printResult(cmd, answer, func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "MESSAGE_ID=%s\nMESSAGE_STATE=%s\n", answer.Message.ID, answer.Message.State)
	})
}

// parseReplyArgs reads `--message <id>` and `--reply <text>`, each also as
// `--name=value`.
func parseReplyArgs(args []string) (collab.MissionsReplyInput, error) {
	usage := "use --message <message id> --reply <text>"
	var request collab.MissionsReplyInput
	seen := map[string]bool{}
	for len(args) > 0 {
		raw := args[0]
		args = args[1:]
		name, value, hasValue := strings.Cut(raw, "=")
		if name != "--message" && name != "--reply" {
			return request, fmt.Errorf("unexpected argument %q; %s", boundedLine(raw, 64), usage)
		}
		if seen[name] {
			return request, fmt.Errorf("%s given twice", name)
		}
		seen[name] = true
		if !hasValue {
			if len(args) == 0 {
				return request, fmt.Errorf("%s needs a value", name)
			}
			value, args = args[0], args[1:]
		}
		if name == "--message" {
			request.Message = value
		} else {
			request.Reply = value
		}
	}
	if !missionIDPattern.MatchString(request.Message) {
		return request, fmt.Errorf("--message must be a message id; %s", usage)
	}
	if strings.TrimSpace(request.Reply) == "" {
		return request, fmt.Errorf("--reply must not be empty; %s", usage)
	}
	return request, nil
}

// printResult prints the result document in JSON mode, else the text lines.
func printResult(cmd CommandIO, result any, text func(io.Writer)) int {
	if cmd.JSON {
		data, err := json.Marshal(result)
		if err != nil {
			_, _ = fmt.Fprintf(cmd.Stderr, "%v\n", err)
			return 1
		}
		_, _ = fmt.Fprintln(cmd.Stdout, string(data))
		return 0
	}
	text(cmd.Stdout)
	return 0
}
