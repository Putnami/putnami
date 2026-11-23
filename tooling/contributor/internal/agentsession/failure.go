package agentsession

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
)

// syncNoticeAfter is how many syncs in a row fail before a hook says so in
// the session's context, once: one failure can be a blip, and a session
// whose syncs keep failing stops moving its mission while it works.
const syncNoticeAfter = 3

// timeoutReason is the reason code missedReason gives a sync call that timed
// out.
const timeoutReason = "timeout"

// retiredNotices reads the workspace at root and answers the notice of each
// retired extension it binds (wsproto.Config.RetiredBindings). A workspace
// the hook cannot read binds none.
func retiredNotices(root string) []string {
	if root == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(root, workspaceManifest)) // #nosec G304 -- the workspace manifest of the session's checkout.
	if err != nil {
		return nil
	}
	var config wsproto.Config
	if json.Unmarshal(data, &config) != nil {
		return nil
	}
	var notices []string
	for _, retired := range config.RetiredBindings() {
		notices = append(notices, retired.Notice())
	}
	return notices
}

// syncFailureAction is the action that fixes a failed sync, from the reason
// missedReason gave it: the notice of each retired extension the checkout
// binds, since the provider behind it refuses every call; else signing in
// for a sign-in refusal; else nothing for a timeout, which the next sync
// retries; else the binding to check.
func syncFailureAction(reason string, retired []string) string {
	if len(retired) > 0 {
		return strings.Join(retired, "\n")
	}
	code, _, _ := strings.Cut(reason, ":")
	switch {
	case slices.Contains(signInReasons, code):
		return "Run `putnami cloud login`, then start a new session."
	case code == timeoutReason:
		return "The next sync tries again."
	}
	return "Check the missions binding under options.collaboration in putnami.workspace.json; `putnami doctor` names the last hook failure."
}

// syncFailureNote names the cause of a failed sync, quoted since the
// provider wrote it, and the action that fixes it.
func syncFailureNote(reason string, retired []string) string {
	return "The provider answered:\n" + quoteLines(reason) + "\n" + syncFailureAction(reason, retired)
}

// failedSyncsNotice is the notice of the syncs that failed in a row, once
// syncNoticeAfter of them did and no hook gave it yet; "" otherwise, and in
// a workspace that binds no missions, where every sync is refused by design.
func failedSyncsNotice(cache *sessionCache) string {
	if cache.Missed == nil || cache.Missed.Count < syncNoticeAfter || cache.MissedNoticed {
		return ""
	}
	if missions, _ := hookConditions(cache.Root); !missions {
		return ""
	}
	return fmt.Sprintf("Putnami mission reporting: the last %d syncs of this session failed, so its mission does not show its progress. Tell the person. ", cache.Missed.Count) +
		syncFailureNote(cache.Missed.Reason, retiredNotices(cache.Root))
}
