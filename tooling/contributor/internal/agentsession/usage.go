package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
)

const (
	// usageReadMax bounds the transcript bytes one hook reads for the usage.
	// What is left goes to the next hook.
	usageReadMax = 16 << 20
	// usageLineMax is the longest transcript line the usage waits for. A
	// longer line is skipped, so it never blocks the lines after it.
	usageLineMax = 4 << 20
	// usageFilesMax bounds the transcripts the usage of a session follows:
	// its own and its subagents'. A subagent past the bound is not counted.
	usageFilesMax = 512
	// usageWalkMax bounds the directory entries one hook visits to find the
	// subagent transcripts, and usageWalkDepth how deep it looks.
	usageWalkMax   = 4096
	usageWalkDepth = 3
	// usagePathMax bounds the path of a subagent transcript, relative to the
	// subagents directory, and usageIDMax a message id the cache keeps.
	usagePathMax = 256
	usageIDMax   = 128
)

// usageState is the token usage read so far from a session's transcripts,
// and how far each transcript was read.
type usageState struct {
	Totals collab.SessionUsage `json:"totals"`
	Files  []transcriptCursor  `json:"files,omitempty"`
}

// transcriptCursor is how far one Claude Code transcript was read. Path is ""
// for the session's own transcript, else the path of a subagent's transcript
// relative to the subagents directory beside it. Offset is the byte after the
// last line read. LastID is the message id of the last usage counted: Claude
// Code writes one message as adjacent lines that repeat its id and usage, so
// a message counts once. Partial reports that Offset falls inside a line
// longer than usageLineMax, whose rest is skipped.
type transcriptCursor struct {
	Path    string `json:"path,omitempty"`
	Offset  int64  `json:"offset"`
	LastID  string `json:"last_id,omitempty"`
	Partial bool   `json:"partial,omitempty"`
}

// refreshUsage reads the usage the session's transcript added since the last
// read. A transcript that cannot be read leaves the usage as it is.
func refreshUsage(cache *sessionCache) {
	switch cache.Engine {
	case sessionEngineClaudeCode:
		refreshClaudeUsage(cache)
	case sessionEngineCodex:
		if usage, ok := codexUsage(cache.Transcript); ok {
			cache.Usage = &usageState{Totals: usage}
		}
	}
}

// refreshClaudeUsage adds the usage of the assistant messages that Claude
// Code wrote since the last read: in the session's transcript
// <dir>/<session>.jsonl, then in each subagent's transcript under
// <dir>/<session>/subagents, at any depth up to usageWalkDepth. It reads at
// most usageReadMax bytes in all.
func refreshClaudeUsage(cache *sessionCache) {
	main := cache.Transcript
	if main == "" || !filepath.IsAbs(main) || filepath.Ext(main) != ".jsonl" {
		return
	}
	state := cache.Usage
	if state == nil {
		state = &usageState{}
	}
	subagents := filepath.Join(strings.TrimSuffix(main, ".jsonl"), "subagents")
	budget := int64(usageReadMax)
	opened := false
	for _, relative := range append([]string{""}, subagentTranscripts(subagents)...) {
		if budget <= 0 {
			break
		}
		index := -1
		for i, cursor := range state.Files {
			if cursor.Path == relative {
				index = i
				break
			}
		}
		if index < 0 {
			if len(state.Files) >= usageFilesMax {
				continue
			}
			state.Files = append(state.Files, transcriptCursor{Path: relative})
			index = len(state.Files) - 1
		}
		path := main
		if relative != "" {
			path = filepath.Join(subagents, filepath.FromSlash(relative))
		}
		read, ok := readTranscriptUsage(path, &state.Files[index], &state.Totals, budget)
		budget -= read
		opened = opened || ok
	}
	if opened {
		cache.Usage = state
	}
}

// subagentTranscripts lists the subagent transcripts under dir, agent-*.jsonl
// files, as slash paths relative to dir, in lexical order.
func subagentTranscripts(dir string) []string {
	var found []string
	visited := 0
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				return err
			}
			return nil
		}
		if visited++; visited > usageWalkMax {
			return fs.SkipAll
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil || relative == "." {
			return nil
		}
		if entry.IsDir() {
			if strings.Count(filepath.ToSlash(relative), "/")+1 >= usageWalkDepth {
				return fs.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		if relative = filepath.ToSlash(relative); len(relative) <= usagePathMax {
			found = append(found, relative)
		}
		return nil
	})
	return found
}

// readTranscriptUsage reads the whole lines of one transcript past its cursor,
// at most budget bytes, and adds the usage of each assistant message to
// totals. It answers the bytes it read, and whether it could open the file.
func readTranscriptUsage(path string, cursor *transcriptCursor, totals *collab.SessionUsage, budget int64) (int64, bool) {
	file, err := os.Open(path) // #nosec G304 -- the engine names its own transcript.
	if err != nil {
		return 0, false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return 0, false
	}
	if info.Size() <= cursor.Offset {
		return 0, true
	}
	chunk := make([]byte, min(info.Size()-cursor.Offset, budget))
	n, err := file.ReadAt(chunk, cursor.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, true
	}
	chunk = chunk[:n]
	start := 0
	if cursor.Partial {
		newline := bytes.IndexByte(chunk, '\n')
		if newline < 0 {
			cursor.Offset += int64(len(chunk))
			return int64(len(chunk)), true
		}
		start, cursor.Partial = newline+1, false
	}
	end := bytes.LastIndexByte(chunk, '\n')
	if end < start {
		// No whole line: a line still being written, or one longer than
		// usageLineMax, which is skipped.
		if len(chunk)-start >= usageLineMax {
			cursor.Offset += int64(len(chunk))
			cursor.Partial = true
		} else {
			cursor.Offset += int64(start)
		}
		return int64(len(chunk)), true
	}
	for _, line := range bytes.Split(chunk[start:end], []byte("\n")) {
		addLineUsage(line, cursor, totals)
	}
	cursor.Offset += int64(end + 1)
	return int64(len(chunk)), true
}

// addLineUsage adds the usage of one transcript line when it is an assistant
// message other than the one counted last.
func addLineUsage(line []byte, cursor *transcriptCursor, totals *collab.SessionUsage) {
	if !bytes.Contains(line, []byte(`"usage"`)) || !bytes.Contains(line, []byte(`"assistant"`)) {
		return
	}
	var entry struct {
		Type    string `json:"type"`
		Message struct {
			ID    string `json:"id"`
			Usage *struct {
				Input      int64 `json:"input_tokens"`
				Output     int64 `json:"output_tokens"`
				CacheWrite int64 `json:"cache_creation_input_tokens"`
				CacheRead  int64 `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Type != "assistant" || entry.Message.Usage == nil {
		return
	}
	if id := boundedBytes(entry.Message.ID, usageIDMax); id != "" {
		if id == cursor.LastID {
			return
		}
		cursor.LastID = id
	}
	usage := entry.Message.Usage
	addUsage(totals, collab.SessionUsage{InputTokens: usage.Input, OutputTokens: usage.Output, CacheWriteTokens: usage.CacheWrite, CacheReadTokens: usage.CacheRead})
}

// addUsage adds usage to totals, each count kept within 0 and
// collab.MaxUsageTokens.
func addUsage(totals *collab.SessionUsage, usage collab.SessionUsage) {
	add := func(total *int64, value int64) {
		*total = min(*total+tokenCount(value), collab.MaxUsageTokens)
	}
	add(&totals.InputTokens, usage.InputTokens)
	add(&totals.OutputTokens, usage.OutputTokens)
	add(&totals.CacheWriteTokens, usage.CacheWriteTokens)
	add(&totals.CacheReadTokens, usage.CacheReadTokens)
}

// tokenCount is value within 0 and collab.MaxUsageTokens.
func tokenCount(value int64) int64 {
	return min(max(value, 0), collab.MaxUsageTokens)
}

// codexUsage reads the session's usage from the last token_count event in
// the tail of a Codex rollout. Codex counts the cached input and the input it
// wrote to the cache inside input_tokens, and the reasoning inside
// output_tokens; its total is cumulative for the thread. A subagent thread
// writes a rollout of its own, which this does not read.
func codexUsage(path string) (collab.SessionUsage, bool) {
	tail := transcriptTail(path)
	lines := bytes.Split(tail, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if !bytes.Contains(lines[i], []byte(`"token_count"`)) {
			continue
		}
		var event struct {
			Payload struct {
				Type string `json:"type"`
				Info *struct {
					Total *struct {
						Input      int64 `json:"input_tokens"`
						Cached     int64 `json:"cached_input_tokens"`
						CacheWrite int64 `json:"cache_write_input_tokens"`
						Output     int64 `json:"output_tokens"`
					} `json:"total_token_usage"`
				} `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(lines[i], &event) != nil || event.Payload.Type != "token_count" || event.Payload.Info == nil || event.Payload.Info.Total == nil {
			continue
		}
		total := event.Payload.Info.Total
		cached, written := tokenCount(total.Cached), tokenCount(total.CacheWrite)
		return collab.SessionUsage{
			InputTokens:      tokenCount(tokenCount(total.Input) - cached - written),
			OutputTokens:     tokenCount(total.Output),
			CacheWriteTokens: written,
			CacheReadTokens:  cached,
		}, true
	}
	return collab.SessionUsage{}, false
}

// transcriptTail reads the last transcriptTailMax bytes of a transcript the
// engine names, or nil.
func transcriptTail(path string) []byte {
	if path == "" || !filepath.IsAbs(path) {
		return nil
	}
	file, err := os.Open(path) // #nosec G304 -- the engine names its own transcript.
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	offset := max(info.Size()-transcriptTailMax, 0)
	tail := make([]byte, info.Size()-offset)
	if _, err := file.ReadAt(tail, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	return tail
}
