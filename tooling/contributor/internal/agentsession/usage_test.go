package agentsession

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

// assistantLine is one Claude Code transcript line of an assistant message.
func assistantLine(id string, input, output, cacheWrite, cacheRead int64) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"id":%q,"model":"example-model","content":[{"type":"text","text":"usage"}],`+
		`"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}}}`,
		id, input, output, cacheWrite, cacheRead)
}

// appendLines appends text to a file, creating it and its directory.
func appendLines(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func withTranscript(path string) func(map[string]any) {
	return func(payload map[string]any) { payload["transcript_path"] = path }
}

func usageOf(t *testing.T, sync collab.MissionsSyncInput) collab.SessionUsage {
	t.Helper()
	if sync.Usage == nil {
		t.Fatalf("the %s sync carries no usage", sync.Event)
	}
	return *sync.Usage
}

func TestTheUsageSumsTheSessionAndItsSubagents(t *testing.T) {
	spectest.Proves(t, sessionFeature, usageRequirement, "the-usage-sums-each-message-of-the-session-and-its-subagents-once")
	h := newHarness(t)
	dir := t.TempDir()
	main := filepath.Join(dir, claudeSession+".jsonl")
	subagents := filepath.Join(dir, claudeSession, "subagents")
	appendLines(t, main, `{"type":"user","message":{"role":"user","content":"Write a.txt"}}`+"\n"+
		assistantLine("msg_1", 10, 5, 100, 1000)+"\n"+
		// One message is written as several lines with its id and usage.
		assistantLine("msg_1", 10, 5, 100, 1000)+"\n"+
		assistantLine("msg_2", 1, 2, 3, 4)+"\n")
	appendLines(t, filepath.Join(subagents, "agent-a1.jsonl"), assistantLine("msg_3", 20, 0, 0, 0)+"\n")
	appendLines(t, filepath.Join(subagents, "agent-a1.meta.json"), `{"agentType":"Explore"}`)
	appendLines(t, filepath.Join(subagents, "workflows", "wf_1", "agent-b2.jsonl"), assistantLine("msg_4", 0, 7, 0, 0)+"\n")
	// Only agent-*.jsonl files are transcripts.
	appendLines(t, filepath.Join(subagents, "workflows", "wf_1", "journal.jsonl"), assistantLine("msg_5", 500, 500, 500, 500)+"\n")

	h.hook(claude, "session-start", claudeSession, withTranscript(main))
	if got, want := usageOf(t, h.provider.lastSync(t)), (collab.SessionUsage{InputTokens: 31, OutputTokens: 14, CacheWriteTokens: 103, CacheReadTokens: 1004}); got != want {
		t.Fatalf("usage %+v, want %+v", got, want)
	}

	// The next hook reads only what was added: a repeat of the last message
	// counts nothing, and a line still being written waits for its end.
	appendLines(t, main, assistantLine("msg_2", 1, 2, 3, 4)+"\n"+assistantLine("msg_6", 100, 0, 0, 0)+"\n"+assistantLine("msg_7", 0, 0, 0, 9)[:40])
	appendLines(t, filepath.Join(subagents, "agent-c3.jsonl"), assistantLine("msg_8", 0, 0, 50, 0)+"\n")
	h.hook(claude, "stop", claudeSession, withTranscript(main))
	if got, want := usageOf(t, h.provider.lastSync(t)), (collab.SessionUsage{InputTokens: 131, OutputTokens: 14, CacheWriteTokens: 153, CacheReadTokens: 1004}); got != want {
		t.Fatalf("usage %+v, want %+v", got, want)
	}
	appendLines(t, main, assistantLine("msg_7", 0, 0, 0, 9)[40:]+"\n")
	h.hook(claude, "stop", claudeSession, withTranscript(main))
	if got, want := usageOf(t, h.provider.lastSync(t)), (collab.SessionUsage{InputTokens: 131, OutputTokens: 14, CacheWriteTokens: 153, CacheReadTokens: 1013}); got != want {
		t.Fatalf("usage %+v, want %+v", got, want)
	}
	// The cache keeps a cursor per transcript, not the transcripts.
	if files := h.cache(claude, claudeSession).Usage.Files; len(files) != 4 || files[0].Path != "" || files[0].LastID != "msg_7" {
		t.Errorf("cursors %+v", files)
	}
}

func TestAHookReadsAtMostItsShareOfTheTranscript(t *testing.T) {
	spectest.Proves(t, sessionFeature, usageRequirement, "each-hook-reads-a-bounded-part-of-the-transcripts")
	path := filepath.Join(t.TempDir(), "session.jsonl")
	first, second := assistantLine("msg_1", 1, 0, 0, 0)+"\n", assistantLine("msg_2", 2, 0, 0, 0)+"\n"
	appendLines(t, path, first+second)
	var cursor transcriptCursor
	var totals collab.SessionUsage
	// A budget that ends inside the second line counts the first only.
	if read, ok := readTranscriptUsage(path, &cursor, &totals, int64(len(first)+10)); !ok || read != int64(len(first)+10) || totals.InputTokens != 1 || cursor.Offset != int64(len(first)) {
		t.Fatalf("read %d, totals %+v, cursor %+v", read, totals, cursor)
	}
	if _, ok := readTranscriptUsage(path, &cursor, &totals, usageReadMax); !ok || totals.InputTokens != 3 || cursor.Offset != int64(len(first+second)) {
		t.Fatalf("totals %+v, cursor %+v", totals, cursor)
	}
	// A line longer than usageLineMax is skipped, and the lines after it count.
	appendLines(t, path, `{"type":"user","message":{"content":"`+strings.Repeat("x", usageLineMax)+`"}}`+"\n"+assistantLine("msg_3", 4, 0, 0, 0)+"\n")
	for range 3 {
		readTranscriptUsage(path, &cursor, &totals, usageLineMax)
	}
	if totals.InputTokens != 7 || cursor.Partial {
		t.Fatalf("totals %+v, cursor %+v", totals, cursor)
	}
	// A count past the bound stays at the bound.
	totals = collab.SessionUsage{InputTokens: collab.MaxUsageTokens - 1}
	addUsage(&totals, collab.SessionUsage{InputTokens: 5, OutputTokens: -5})
	if totals.InputTokens != collab.MaxUsageTokens || totals.OutputTokens != 0 {
		t.Fatalf("totals %+v", totals)
	}
}

func TestCodexReportsTheUsageOfItsRollout(t *testing.T) {
	spectest.Proves(t, sessionFeature, usageRequirement, "codex-reports-the-usage-of-its-rollout")
	h := newHarness(t)
	rollout := filepath.Join(t.TempDir(), "rollout-"+codexSession+".jsonl")
	tokenCount := func(input, cached, written, output int64) string {
		return fmt.Sprintf(`{"timestamp":"2026-10-21T10:00:00Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":`+
			`{"input_tokens":%d,"cached_input_tokens":%d,"cache_write_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":3,"total_tokens":%d}}}}`,
			input, cached, written, output, input+output) + "\n"
	}
	appendLines(t, rollout, tokenCount(1000, 600, 300, 50)+`{"type":"response_item","payload":{"type":"message"}}`+"\n")
	h.hook(codex, "session-start", codexSession, withTranscript(rollout))
	// Codex counts the cached and the written input inside input_tokens.
	if got, want := usageOf(t, h.provider.lastSync(t)), (collab.SessionUsage{InputTokens: 100, OutputTokens: 50, CacheWriteTokens: 300, CacheReadTokens: 600}); got != want {
		t.Fatalf("usage %+v, want %+v", got, want)
	}
	appendLines(t, rollout, tokenCount(5000, 4000, 300, 80))
	h.hook(codex, "stop", codexSession, withTranscript(rollout))
	if got, want := usageOf(t, h.provider.lastSync(t)), (collab.SessionUsage{InputTokens: 700, OutputTokens: 80, CacheWriteTokens: 300, CacheReadTokens: 4000}); got != want {
		t.Fatalf("usage %+v, want %+v", got, want)
	}
}

func TestSessionEndPublishesTheEndWithTheUsage(t *testing.T) {
	spectest.Proves(t, sessionFeature, endRequirement, "session-end-publishes-the-ended-state-with-the-usage")
	h := newHarness(t)
	main := filepath.Join(t.TempDir(), claudeSession+".jsonl")
	appendLines(t, main, assistantLine("msg_1", 10, 5, 100, 1000)+"\n")
	h.hook(claude, "session-start", claudeSession, withTranscript(main))
	h.hook(claude, "stop-failure", claudeSession, withTranscript(main))
	h.seed(claude, claudeSession, func(cache *sessionCache) { cache.Waiter = "an-idle-wake" })
	appendLines(t, main, assistantLine("msg_2", 1, 1, 1, 1)+"\n")
	gitCalls := len(h.gitCalls)
	if out := h.hook(claude, "session-end", claudeSession, withTranscript(main)); out != nil {
		t.Errorf("output %+v", out)
	}
	sync := h.provider.lastSync(t)
	if sync.State != collab.SessionStateEnded || sync.Event != sessionEventEnd || sync.Error != "" || sync.WaitUntil != "" {
		t.Errorf("sync %+v", sync)
	}
	if got, want := usageOf(t, sync), (collab.SessionUsage{InputTokens: 11, OutputTokens: 6, CacheWriteTokens: 101, CacheReadTokens: 1001}); got != want {
		t.Errorf("usage %+v, want %+v", got, want)
	}
	// The end runs no git once the session started, and ends a wait.
	if len(h.gitCalls) != gitCalls {
		t.Errorf("the end ran git: %v", h.gitCalls[gitCalls:])
	}
	if cache := h.cache(claude, claudeSession); cache.Waiter != "" || cache.State != collab.SessionStateEnded {
		t.Errorf("waiter %q, state %s", cache.Waiter, cache.State)
	}
	if h.hookLog() != nil {
		t.Errorf("hook.log %v", h.hookLog())
	}
}
