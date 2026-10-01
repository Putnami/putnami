package output

import (
	"strings"

	"go.putnami.dev/cli/model/jobs"
)

const verboseDurationWidth = 6

func (r *TextRenderer) computeVerboseLayout(planned []*jobs.ScheduledJob) {
	nameW := 6
	statusW := len(statusBlocked)
	for _, job := range planned {
		if l := len([]rune(job.Project.Name)); l > nameW {
			nameW = l
		}
		if l := len([]rune(stripStepSuffix(job.JobDef.Name))); l > statusW {
			statusW = l
		}
	}
	if nameW > 24 {
		nameW = 24
	}
	if statusW > 10 {
		statusW = 10
	}
	r.nameWidth = nameW
	r.statusWidth = statusW
}

func (r *TextRenderer) verboseLine(job *jobs.ScheduledJob, icon seg, duration string, detail ...seg) string {
	noColor := !ColorsEnabled()
	var b strings.Builder
	b.WriteString("  ")
	b.WriteString(r.verbosePrefix(job, icon, duration, noColor))
	if len(detail) > 0 {
		b.WriteString("  ")
		for _, s := range detail {
			if noColor || s.color == "" {
				b.WriteString(s.text)
			} else {
				b.WriteString(colorize(s.text, s.color))
			}
		}
	}
	return strings.TrimRight(b.String(), " ")
}

func (r *TextRenderer) verbosePrefix(job *jobs.ScheduledJob, icon seg, duration string, noColor bool) string {
	name := truncate(job.Project.Name, r.nameWidth)
	status := truncate(stripStepSuffix(job.JobDef.Name), r.statusWidth)
	if duration == "" {
		duration = strings.Repeat(" ", verboseDurationWidth)
	}

	var b strings.Builder
	b.WriteString(padTextCol(noColor, 1, icon))
	b.WriteByte(' ')
	b.WriteString(padTextCol(noColor, r.nameWidth, seg{name, Dim}))
	b.WriteByte(' ')
	b.WriteString(padTextCol(noColor, r.statusWidth, seg{status, ""}))
	b.WriteByte(' ')
	b.WriteString(padTextCol(noColor, verboseDurationWidth, seg{truncate(duration, verboseDurationWidth), Dim}))
	return b.String()
}

func verboseStepName(job *jobs.ScheduledJob) string {
	if step := stepName(job.JobDef.Name); step != "" {
		return step
	}
	return stripStepSuffix(job.JobDef.Name)
}

func padTextCol(noColor bool, width int, segs ...seg) string {
	var b strings.Builder
	for _, s := range segs {
		if noColor || s.color == "" {
			b.WriteString(s.text)
		} else {
			b.WriteString(colorize(s.text, s.color))
		}
	}
	if pad := width - segWidth(segs); pad > 0 {
		b.WriteString(strings.Repeat(" ", pad))
	}
	return b.String()
}

func verboseRunningIcon() seg {
	return seg{"●", Green}
}

func verboseSuccessIcon() seg {
	return seg{"✓", Green}
}

func verboseCachedIcon() seg {
	return seg{"✓", Dim}
}

func verboseFailedIcon() seg {
	return seg{"✗", Red}
}
