package agentsession

import (
	"regexp"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
)

// The variables Claude Code sets when a session has a web address: a cloud
// session names its remote session, and a local session with Remote Control
// on names its bridge session while the connection is active.
const (
	remoteSessionVariable = "CLAUDE_CODE_REMOTE_SESSION_ID"
	bridgeSessionVariable = "CLAUDE_CODE_BRIDGE_SESSION_ID"
	// claudeSessionAddress is the address of a Claude Code web session,
	// before its id.
	claudeSessionAddress = "https://claude.ai/code/"
	// remoteSessionPrefix is the prefix of a cloud session id that the web
	// address spells webSessionPrefix.
	remoteSessionPrefix = "cse_"
	webSessionPrefix    = "session_"
)

// webSessionIDPattern is an id the web address can carry as it is: letters,
// digits, _ and -, at most 256 of them, so the whole address stays within
// collab.MaxSessionURLLength. Any other id gives no address, so the address
// never carries a character the provider refuses.
var webSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// sessionWebAddress is the address where the owner opens the session on its
// host's web interface, or "" when the host names none. Only Claude Code
// names one: the cloud session id first, then the Remote Control one.
func sessionWebAddress(engine string, getenv func(string) string) string {
	if engine != sessionEngineClaudeCode || getenv == nil {
		return ""
	}
	if id := getenv(remoteSessionVariable); id != "" {
		if rest, ok := strings.CutPrefix(id, remoteSessionPrefix); ok {
			if rest == "" {
				return ""
			}
			id = webSessionPrefix + rest
		}
		return webAddress(id)
	}
	return webAddress(getenv(bridgeSessionVariable))
}

func webAddress(id string) string {
	if !webSessionIDPattern.MatchString(id) {
		return ""
	}
	return claudeSessionAddress + id
}

// addSessionURL puts the session's web address on a sync request when it
// differs from the last one a sync carried, and an empty one when the
// address that sync carried is gone. It leaves the member out when nothing
// changed or nothing is known. The snapshot keeps what the request carried,
// so an ok answer records it as sent.
func addSessionURL(request *collab.MissionsSyncInput, snapshot *syncSnapshot, cache *sessionCache, getenv func(string) string) {
	address := sessionWebAddress(cache.Engine, getenv)
	if address == cache.SessionURL {
		return
	}
	request.SessionURL = &address
	snapshot.sessionURL = &address
}

// sessionURLRefused answers how the log names a session address a sync went
// without. A refusal that names the member, as sessionUrl from a CLI that
// does not know it or session_url from a provider's own check, keeps the
// address recorded as sent until it changes, so the syncs do not all go
// twice. A refusal for another member forgets it, so the next sync carries
// it again.
func sessionURLRefused(snapshot *syncSnapshot, refusal error) string {
	if text := refusal.Error(); strings.Contains(text, collab.MissionMemberSessionURL) || strings.Contains(text, "session_url") {
		return "the session address, which goes again once it changes"
	}
	snapshot.sessionURL = nil
	return "the session address, which goes again with the next sync"
}
