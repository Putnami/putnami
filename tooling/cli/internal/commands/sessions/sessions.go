package sessions

import (
	"fmt"
	"os"
	"strings"

	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// SessionsList lists recent sessions with timestamp, command, outcome, and duration.
func SessionsList(wsRoot string, outputFormat string) error {
	return SessionsListRevision(wsRoot, outputFormat, "")
}

// SessionsListRevision is SessionsList narrowed to the sessions whose recorded
// tree sat on the commit revision names (a lowercase hex prefix of its id), so
// a new CLI session can find what judged a given commit. A session that
// recorded no tree never matches a revision: it cannot say which commit it
// judged, and the listing does not guess. With a revision the rows also show
// the head commit and the actual placement, because a judged revision is only
// evidence together with where its verdict was produced.
func SessionsListRevision(wsRoot string, outputFormat string, revision string) error {
	if revision != "" {
		if err := ValidateRevision(revision); err != nil {
			return err
		}
	}
	store := workspace_state.NewSessionStore(wsRoot)
	all, err := store.List()
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	identities := make(map[string]sessionIdentity, len(all))
	sessions := make([]string, 0, len(all))
	for _, id := range all {
		identity := readSessionIdentity(store, id)
		if !matchesRevision(identity, revision) {
			continue
		}
		identities[id] = identity
		sessions = append(sessions, id)
	}

	if len(sessions) == 0 {
		if revision != "" {
			iox.Fprintf(os.Stdout, "  No recorded session judged revision %s.\n", revision)
		} else {
			iox.Fprintln(os.Stdout, "  No sessions found.")
		}
		return nil
	}

	latestID := store.LatestID()

	if outputFormat == "jsonl" {
		return sessionsListJSONL(store, sessions, latestID, identities)
	}

	iox.Fprintln(os.Stdout)
	if revision != "" {
		iox.Fprintf(os.Stdout, "  %-25s %-25s %-10s %-10s %-14s %-8s %s\n", "SESSION", "COMMANDS", "STATUS", "DURATION", "REVISION", "WHERE", "")
		iox.Fprintf(os.Stdout, "  %-25s %-25s %-10s %-10s %-14s %-8s %s\n", "-------", "--------", "------", "--------", "--------", "-----", "")
	} else {
		iox.Fprintf(os.Stdout, "  %-25s %-25s %-10s %-10s %s\n", "SESSION", "COMMANDS", "STATUS", "DURATION", "")
		iox.Fprintf(os.Stdout, "  %-25s %-25s %-10s %-10s %s\n", "-------", "--------", "------", "--------", "")
	}

	for _, id := range sessions {
		meta, err := readSessionMeta(store, id)
		if err != nil {
			iox.Fprintf(os.Stdout, "  %-25s (unreadable)\n", id)
			continue
		}

		cmds := strings.Join(meta.Commands, ",")
		if len(cmds) > 25 {
			cmds = cmds[:22] + "..."
		}

		status := "success"
		if meta.Stats != nil && meta.Stats.Failed > 0 {
			status = "failed"
		}

		dur := formatDurationMs64(meta.Duration)

		marker := ""
		if id == latestID {
			marker = "(latest)"
		}

		if revision != "" {
			identity := identities[id]
			head := identity.headSHA
			if len(head) > 12 {
				head = head[:12]
			}
			where := "-"
			if identity.placement != nil {
				where = identity.placement.Actual
			}
			iox.Fprintf(os.Stdout, "  %-25s %-25s %-10s %-10s %-14s %-8s %s\n", id, cmds, status, dur, head, where, marker)
			continue
		}
		iox.Fprintf(os.Stdout, "  %-25s %-25s %-10s %-10s %s\n", id, cmds, status, dur, marker)
	}

	iox.Fprintf(os.Stdout, "\n  %d sessions\n\n", len(sessions))
	return nil
}
