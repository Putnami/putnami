package history

import (
	"context"
	"regexp"
	"strings"

	"go.putnami.dev/intelligence/agent-readiness/internal/gitrepo"
)

// MarkChangesReverted sets RevertedAt on the changes, newest first, that a
// revert of the window undoes, to the time of the first such revert. A
// revert undoes the change whose id it names, or the change that landed the
// commit it names: landing answers that change's id, or "" for an id git
// does not know. Only when git knows none of its ids does a revert fall back
// to the subject it quotes, and then it undoes the newest change with that
// subject or title that is not newer than itself. A revert older than the
// change it names, made on the branch before the change landed, undoes
// nothing. The comparison leaves out
// a forge's numbered squash suffix: GitHub's
// revert quotes the title without it.
func MarkChangesReverted(changes []Change, reverts []Revert, landing func(hash string) string) {
	mark := func(i int, at int64) {
		// A revert made on the branch before the change landed undoes
		// nothing that reached HEAD.
		if at <= changes[i].Time {
			return
		}
		if changes[i].RevertedAt == 0 || at < changes[i].RevertedAt {
			changes[i].RevertedAt = at
		}
	}
	find := func(hash string) int {
		for i := range changes {
			if strings.HasPrefix(changes[i].Hash, hash) {
				return i
			}
		}
		return -1
	}
	for _, revert := range reverts {
		named := false
		for _, hash := range revert.Hashes {
			if i := find(hash); i >= 0 {
				mark(i, revert.Time)
				named = true
				continue
			}
			landed := landing(hash)
			named = named || landed != ""
			if i := find(landed); landed != "" && i >= 0 {
				mark(i, revert.Time)
			}
		}
		if named {
			continue
		}
		for _, subject := range revert.Subjects {
			subject = withoutPullNumber(subject)
			for i := range changes {
				if (withoutPullNumber(changes[i].Subject) == subject || withoutPullNumber(changes[i].Title) == subject) && changes[i].Time <= revert.Time {
					mark(i, revert.Time)
					break
				}
			}
		}
	}
}

// pullSuffix matches the pull request number a forge appends to a squashed
// change's subject.
var pullSuffix = regexp.MustCompile(`\s*\(#\d+\)$`)

func withoutPullNumber(subject string) string {
	return pullSuffix.ReplaceAllString(subject, "")
}

// maxLandings bounds the reverted commits whose landing change the
// collector looks up. Each lookup costs about log2 of the window's changes
// git calls.
const maxLandings = 50

// Landing answers, for a commit id, the id of the window's change that
// landed it on HEAD's first-parent line: the merge that brought a branch
// commit in, or the commit itself when it is on that line. changes are the
// window's changes, newest first. It answers the id itself for a commit that
// landed before the window, and "" for an id git does not know or HEAD does
// not contain.
func Landing(ctx context.Context, repo *gitrepo.Repo, changes []Change) func(hash string) string {
	lookups := 0
	contains := func(change, hash string) bool {
		_, err := repo.Output(ctx, "merge-base", "--is-ancestor", hash, change)
		return err == nil
	}
	return func(hash string) string {
		if lookups >= maxLandings || len(changes) == 0 {
			return ""
		}
		lookups++
		if !contains(changes[0].Hash, hash) {
			return ""
		}
		// Once a change contains the commit, every newer change does: find
		// the oldest change that contains it.
		oldest, newest := len(changes)-1, 0
		if contains(changes[oldest].Hash, hash) {
			if contains(changes[oldest].Hash+"^1", hash) {
				return hash
			}
			return changes[oldest].Hash
		}
		for oldest-newest > 1 {
			middle := (oldest + newest) / 2
			if contains(changes[middle].Hash, hash) {
				newest = middle
			} else {
				oldest = middle
			}
		}
		return changes[newest].Hash
	}
}
