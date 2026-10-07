package history

import (
	"regexp"
	"strings"
)

// agentEmails maps the exact addresses agent tools write in a Co-Authored-By
// trailer, or as a commit's author, to the agent they name.
var agentEmails = map[string]string{
	"noreply@anthropic.com":   "claude",
	"cursoragent@cursor.com":  "cursor",
	"noreply@aider.chat":      "aider",
	"codex@openai.com":        "codex",
	"noreply@openai.com":      "codex",
	"amp@ampcode.com":         "amp",
	"openhands@all-hands.dev": "openhands",
}

// agentBots maps the GitHub account slugs of agent tools to the agent they
// name. GitHub writes them as <id>+<slug>@users.noreply.github.com, with or
// without the [bot] suffix.
var agentBots = map[string]string{
	"copilot":                 "copilot",
	"copilot-swe-agent":       "copilot",
	"claude":                  "claude",
	"cursor":                  "cursor",
	"devin-ai-integration":    "devin",
	"google-labs-jules":       "jules",
	"gemini-code-assist":      "gemini",
	"chatgpt-codex-connector": "codex",
	"openhands-agent":         "openhands",
}

// botOnly are the slugs a person can hold as a GitHub login: they name an
// agent only with the [bot] suffix.
var botOnly = map[string]bool{"claude": true, "cursor": true}

var githubNoreply = regexp.MustCompile(`^(?:\d+\+)?([a-z0-9-]+)(\[bot\])?@users\.noreply\.github\.com$`)

// Agent returns the agent tool an identity names, or "" for a person. The
// identity is a trailer value such as "Claude <noreply@anthropic.com>", or a
// bare email. Only the address decides: a name alone never does, because
// Claude is also a person's first name.
func Agent(identity string) string {
	email := Email(identity)
	if agent, ok := agentEmails[email]; ok {
		return agent
	}
	if match := githubNoreply.FindStringSubmatch(email); match != nil && (match[2] != "" || !botOnly[match[1]]) {
		return agentBots[match[1]]
	}
	return ""
}

// Email returns the lowercase address of an identity such as
// "Name <email>", or of a bare email.
func Email(identity string) string {
	email := strings.ToLower(strings.TrimSpace(identity))
	if start := strings.LastIndex(email, "<"); start >= 0 {
		email = strings.TrimSuffix(email[start+1:], ">")
	}
	return strings.TrimSpace(email)
}

// coAuthor returns the address of a Co-Authored-By trailer that credits a
// person, or "" for an agent, a bot or an empty value.
func coAuthor(identity string) string {
	email := Email(identity)
	if email == "" || Agent(email) != "" || strings.Contains(email, "[bot]") {
		return ""
	}
	return email
}

// revertedHash matches the line git writes in a revert's message.
var revertedHash = regexp.MustCompile(`This reverts commit ([0-9a-f]{7,64})`)

// revertedSubject matches the subject git and the forges write for a revert:
// Revert "<subject>", with the squash suffix " (#123)" a forge may append.
var revertedSubject = regexp.MustCompile(`^Revert "(.+)"(?:\s*\(#\d+\))?$`)

// Revert is one revert commit of the window and what its message undoes.
type Revert struct {
	// Time is the revert's committer time in Unix seconds.
	Time int64
	// Hashes are the commit ids, full or abbreviated, that it names.
	Hashes []string
	// Subjects are the subjects it quotes.
	Subjects []string
}

// ReadRevert adds what one line of the revert's message undoes.
func (r *Revert) ReadRevert(line string) {
	for _, match := range revertedHash.FindAllStringSubmatch(line, -1) {
		r.Hashes = append(r.Hashes, match[1])
	}
	if match := revertedSubject.FindStringSubmatch(strings.TrimSpace(line)); match != nil {
		r.Subjects = append(r.Subjects, match[1])
	}
}

// MarkReverted sets Reverted on the commits, newest first, that the reverts
// undo. A revert undoes the commits whose id it names and, when an id names a
// merge, the branch commits that merge brought in. branch lists what an id
// brought in: the commit itself, or a merge's branch, and nothing for an id
// git does not know. Only when git knows none of its ids does a revert fall
// back to the subject it quotes, and then it undoes the newest commit with
// that subject that is not newer than itself.
func MarkReverted(commits []Commit, reverts []Revert, branch func(hash string) []string) {
	mark := func(hash string) bool {
		for i := range commits {
			if strings.HasPrefix(commits[i].Hash, hash) {
				commits[i].Reverted = true
				return true
			}
		}
		return false
	}
	for _, revert := range reverts {
		named := false
		for _, hash := range revert.Hashes {
			if mark(hash) {
				named = true
				continue
			}
			// An id git knows names the commit the revert undoes, even
			// outside the window: its subject must not match a later one.
			brought := branch(hash)
			named = named || len(brought) > 0
			for _, commit := range brought {
				mark(commit)
			}
		}
		if named {
			continue
		}
		for _, subject := range revert.Subjects {
			for i := range commits {
				if commits[i].Subject == subject && commits[i].Time <= revert.Time {
					commits[i].Reverted = true
					break
				}
			}
		}
	}
}
