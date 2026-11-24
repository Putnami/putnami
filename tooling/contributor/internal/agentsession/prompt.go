package agentsession

import (
	"regexp"
	"slices"
	"strings"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
)

// promptLockWait is how long the prompt hook waits for the session lock. The
// host runs the hook in the background, so the wait delays nothing.
const promptLockWait = 5 * time.Second

// deliveredMessagePattern is the id a message rendering names (renderMessage):
// "(message id <id>)".
var deliveredMessagePattern = regexp.MustCompile(`\(message id ([A-Za-z0-9_-]{1,64})\)`)

// hookUserPromptSubmit records the person's request, so the next sync carries
// it as lastPrompt (collaboration ADR 0013). Claude Code runs it in the
// background, before the turn starts. It replaces the request recorded
// before, and runs no git and no sync. The request recorded already changes
// nothing, so a second hook source that sends it again never sends it twice.
// An empty request, and a request that renders a message this session
// delivered, record nothing. Codex sends no request.
func (e *Env) hookUserPromptSubmit(engine string, input hookInput) error {
	if engine != sessionEngineClaudeCode {
		return nil
	}
	prompt := boundedText(input.Prompt, collab.MaxLastPromptLength)
	if prompt == "" {
		return nil
	}
	files := e.files(engine, input.SessionID)
	return lockedUpdateWithin(files, promptLockWait, func(cache *sessionCache) (*sessionCache, error) {
		cache = e.openSession(cache, engine, input, nil)
		if prompt == cache.LastPrompt || rendersDeliveredMessage(cache, input.Prompt) {
			return nil, nil
		}
		cache.LastPrompt, cache.PromptSent = prompt, false
		return cache, nil
	})
}

// rendersDeliveredMessage reports whether prompt holds the rendering of a
// message the session delivered: the hook started that turn with the owner's
// message, which is not the person's request.
func rendersDeliveredMessage(cache *sessionCache, prompt string) bool {
	if !strings.Contains(prompt, "(message id ") {
		return false
	}
	for _, match := range deliveredMessagePattern.FindAllStringSubmatch(prompt, -1) {
		if slices.Contains(cache.Seen, match[1]) {
			return true
		}
	}
	return false
}

// addLastPrompt puts the recorded request on a sync request until a sync
// carried it to an ok answer. The snapshot keeps what the request carried, so
// that answer records it as sent, unless a newer request replaced it.
func addLastPrompt(request *collab.MissionsSyncInput, snapshot *syncSnapshot, cache *sessionCache) {
	if cache.LastPrompt == "" || cache.PromptSent {
		return
	}
	prompt := cache.LastPrompt
	request.LastPrompt = prompt
	snapshot.lastPrompt = &prompt
}

// lastPromptRefused answers how the log names a request a sync went without.
// A refusal that names the member, as lastPrompt from a CLI that does not know
// it or last_prompt from a provider's own check, keeps the request recorded as
// sent, so the syncs do not all go twice. A refusal for another member forgets
// it, so the next sync carries it again.
func lastPromptRefused(snapshot *syncSnapshot, refusal error) string {
	if text := refusal.Error(); strings.Contains(text, collab.MissionMemberLastPrompt) || strings.Contains(text, "last_prompt") {
		return "the person's request, which goes again with a new request"
	}
	snapshot.lastPrompt = nil
	return "the person's request, which goes again with the next sync"
}
