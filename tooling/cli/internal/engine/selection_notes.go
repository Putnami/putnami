package engine

import (
	"io"
	"strconv"
	"strings"

	"go.putnami.dev/cli/model/workspace"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// This file isolates the presentation of the auto-project-selection note
// (the "· impacted vs abc1234 — 3 projects" line) from the selection stage in
// selection.go. It is pure formatting: no scheduling or workspace mutation
// happens here.

func printAutoProjectSelectionNote(w io.Writer, note autoProjectSelectionNote, count int) {
	switch note.mode {
	case workspace.AutoSelectionAll:
		detail := "all projects"
		switch note.reason {
		case workspace.AutoSelectionReasonFirstBuild:
			detail = "all projects (first build on " + note.branch + ")"
		case workspace.AutoSelectionReasonNoRepository:
			detail = "all projects (no git repository)"
		}
		iox.Fprintf(w, "  · %s — %s\n", detail, projectCountSummary(count))
	case workspace.AutoSelectionImpacted:
		detail := "impacted"
		if note.baseline != "" {
			detail += " vs " + autoSelectionBaselineLabel(note.baseline)
		}
		switch note.reason {
		case workspace.AutoSelectionReasonLastBuild:
			detail += " (last successful " + commandSetLabel(note.commands) + ")"
		case workspace.AutoSelectionReasonTrunk:
			detail += " (trunk)"
		}
		iox.Fprintf(w, "  · %s — %s\n", detail, projectCountSummary(count))
	}
}

func autoSelectionBaselineLabel(baseline string) string {
	if len(baseline) >= 12 && isHexString(baseline) {
		return baseline[:7]
	}
	return baseline
}

func isHexString(s string) bool {
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return s != ""
}

func commandSetLabel(commands []string) string {
	var normalized []string
	for _, cmd := range commands {
		cmd = strings.TrimSpace(cmd)
		if cmd != "" {
			normalized = append(normalized, cmd)
		}
	}
	if len(normalized) == 0 {
		return "run"
	}
	return strings.Join(normalized, ",")
}

func projectCountSummary(count int) string {
	if count == 0 {
		return "nothing to do"
	}
	if count == 1 {
		return "1 project"
	}
	return strconv.Itoa(count) + " projects"
}
