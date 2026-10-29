package agentsession

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
)

// The bounds of the mission sheet, in UTF-8 bytes. The sheet stays within
// sessionSheetMax, so the SessionStart context keeps room for the waiting
// messages. A plan and the open decisions each take a share; the checkpoint
// takes what is left, and goes only when sheetMinContent bytes of it fit.
const (
	sessionSheetMax   = 4 << 10
	sheetLineMax      = 240
	sheetPlanMax      = 1280
	sheetDecisionsMax = 768
	sheetMinContent   = 32
)

// sheetHeader starts the sheet.
const sheetHeader = "Putnami mission sheet:"

// missionSheet is the sheet the SessionStart context opens with, rendered
// from the mission view the last sync answered and the checkpoint
// memory.mission answered. It names the mission's repository and key, which
// putnami agent ask takes. Every text an agent session or the owner wrote is
// quoted with "> ", so none of it can pass for a line of the hook. started is
// when this session start began: a view older than it is stale, and the sheet
// says so with its date. A session that names no mission, or a workspace
// that binds no missions provider, has no sheet.
func missionSheet(cache *sessionCache, syncErr error, started time.Time) string {
	if !sessionPublishable(cache) || unbound(syncErr) {
		return ""
	}
	mission := cache.Mission
	identity, _ := json.Marshal(map[string]string{"repository": cache.Repository, "key": mission.Key})
	parts := []string{
		sheetHeader,
		fmt.Sprintf("Mission %s of repository %s.\nputnami agent ask takes this mission as %s.", mission.Key, cache.Repository, identity),
	}
	note := ""
	switch {
	case !mission.Known:
		note = "The missions provider did not answer at this session start."
	case mission.SheetAt.Before(started):
		note = "The missions provider did not answer at this session start; the sheet dates from " + mission.SheetAt.UTC().Format(time.RFC3339) + "."
	}
	if mission.Known {
		if mission.Title != "" {
			parts = append(parts, "Title:\n"+quoteLines(truncateBytes(mission.Title, sheetLineMax)))
		}
		if plan := sheetPlan(mission.Plan); plan != "" {
			parts = append(parts, plan)
		}
		if decisions := sheetDecisions(mission.OpenDecisions); decisions != "" {
			parts = append(parts, decisions)
		}
	}
	room := sessionSheetMax - len(joinParagraphs(append(append([]string(nil), parts...), note)...)) - len("\n\n")
	if checkpoint := sheetCheckpoint(mission.Checkpoint, room); checkpoint != "" {
		parts = append(parts, checkpoint)
	}
	return joinParagraphs(append(parts, note)...)
}

// unbound reports a sync the missions contract could not route: the
// workspace binds no missions provider.
func unbound(err error) bool {
	var failure *outcomeFailure
	return errors.As(err, &failure) && failure.envelope.Outcome == collab.OutcomeUnsupported
}

// sheetPlan is the plan as the sessions published it, within sheetPlanMax.
func sheetPlan(plan []planStep) string {
	if len(plan) == 0 {
		return ""
	}
	text := "Plan:"
	for i, step := range plan {
		line := "\n" + quoteLines(truncateBytes(fmt.Sprintf("- [%s] %s", step.Status, step.Text), sheetLineMax))
		rest := len(plan) - i
		if len(text)+len(line) > sheetPlanMax-len(moreNote(rest, "step")) {
			return text + "\n" + moreNote(rest, "step")
		}
		text += line
	}
	return text
}

// sheetDecisions is the mission's open decisions, within sheetDecisionsMax.
func sheetDecisions(decisions []collab.OpenDecision) string {
	if len(decisions) == 0 {
		return ""
	}
	text := "Open decisions, waiting for the owner's answer:"
	for i, decision := range decisions {
		entry := fmt.Sprintf("\n- %s:\n%s", decision.ID, quoteLines(truncateBytes(decision.Question, sheetLineMax)))
		rest := len(decisions) - i
		if len(text)+len(entry) > sheetDecisionsMax-len(moreNote(rest, "open decision")) {
			return text + "\n" + moreNote(rest, "open decision")
		}
		text += entry
	}
	return text
}

// sheetCheckpoint is the mission's checkpoint within room bytes, its content
// cut to fit.
func sheetCheckpoint(checkpoint *missionCheckpoint, room int) string {
	if checkpoint == nil || strings.TrimSpace(checkpoint.Content) == "" {
		return ""
	}
	head := "Checkpoint"
	if checkpoint.Revision != "" {
		head += " revision " + truncateBytes(checkpoint.Revision, 64)
	}
	if at, err := time.Parse(time.RFC3339Nano, checkpoint.UpdatedAt); err == nil {
		head += ", recorded at " + at.UTC().Format(time.RFC3339)
	}
	head += ":\n"
	room -= len(head)
	if room < sheetMinContent {
		return ""
	}
	// Quoting adds two bytes a line, so the quoted content is never shorter
	// than the content: the cut starts at room bytes and removes the excess
	// until the quoted content fits.
	content := strings.TrimSpace(checkpoint.Content)
	limit := min(len(content), room)
	cut := truncateBytes(content, limit)
	for len(quoteLines(cut)) > room && limit > 0 {
		limit = max(limit-(len(quoteLines(cut))-room), 0)
		cut = truncateBytes(content, limit)
	}
	if strings.TrimSpace(cut) == "" || len(quoteLines(cut)) > room {
		return ""
	}
	return head + quoteLines(cut)
}

func moreNote(count int, noun string) string {
	if count == 1 {
		return "1 more " + noun + " is not shown."
	}
	return fmt.Sprintf("%d more %ss are not shown.", count, noun)
}

// truncateBytes cuts value to at most limit bytes on a character boundary,
// and ends a cut value with "…".
func truncateBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	const mark = "…"
	if limit < len(mark) {
		return ""
	}
	return boundedBytes(value, limit-len(mark)) + mark
}
