package output

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.putnami.dev/cli/model/jobs"
)

// taskProgress is the per-job bookkeeping behind the global progress bar. The
// weight is the job's expected wall-clock cost — the EMA the scheduler learns
// in .putnami/stats and resolves onto every planned job before the run — so a
// long build counts for more of the bar than a quick lint. An in-flight job
// with history also earns partial credit as its expected duration elapses, so
// the bar creeps through a long pole instead of stalling on it.
type taskProgress struct {
	weight     float64 // share of the bar; ExpectedWallMs, or the fallback when cold
	expectedMs float64 // historical duration for partial credit (0 = no history)
	startTime  time.Time
	done       bool
}

// initProgress builds the progress accumulator from the tuned plan. Jobs the
// stats store has never seen fall back to the median known cost — or an equal
// unit share when the whole store is cold, which degrades the bar to a plain
// task-count fraction rather than misweighting unknown work.
func (r *LiveRenderer) initProgress(planned []*jobs.ScheduledJob) {
	r.taskProg = make(map[string]*taskProgress, len(planned))

	known := make([]float64, 0, len(planned))
	for _, job := range planned {
		if job != nil && job.ExpectedWallMs > 0 {
			known = append(known, float64(job.ExpectedWallMs))
		}
	}
	fallback := medianFloat(known)
	if fallback <= 0 {
		fallback = 1 // cold store: every task weighs the same → count fraction
	}

	var total float64
	for _, job := range planned {
		if job == nil {
			continue
		}
		tp := &taskProgress{weight: fallback}
		if job.ExpectedWallMs > 0 {
			tp.weight = float64(job.ExpectedWallMs)
			tp.expectedMs = float64(job.ExpectedWallMs)
		}
		r.taskProg[job.Key()] = tp
		total += tp.weight
	}
	r.totalWeight = total
}

// progressJobStarted stamps a job's start so an in-flight task with history can
// accrue partial credit.
func (r *LiveRenderer) progressJobStarted(key string, now time.Time) {
	if tp := r.taskProg[key]; tp != nil && tp.startTime.IsZero() {
		tp.startTime = now
	}
}

// progressJobDone marks a job's full weight as earned. Cache hits and skips
// count too: their work is over, so the bar should reflect that immediately
// (a fully cached run races to 100%).
func (r *LiveRenderer) progressJobDone(key string) {
	if tp := r.taskProg[key]; tp != nil {
		tp.done = true
	}
}

// sessionProgress returns the fraction of expected work completed in [0,1] and
// the estimated time remaining. The raw ETA is the larger of a throughput
// estimate (remaining work over the realized completion rate) and the longest
// single remaining task, so it stays realistic as the parallel tail narrows
// instead of trending optimistic. It is then clamped to a finish deadline that
// only ever moves earlier, so the displayed countdown never jumps back up.
func (r *LiveRenderer) sessionProgress(now time.Time) (fraction float64, eta time.Duration, hasETA bool) {
	if r.totalWeight <= 0 {
		return 0, 0, false
	}

	var earned, completed, longestRemainingMs float64
	for _, tp := range r.taskProg {
		if tp.done {
			earned += tp.weight
			completed += tp.weight
			continue
		}

		var remainingMs float64
		switch {
		case tp.expectedMs > 0 && !tp.startTime.IsZero():
			elapsedMs := float64(now.Sub(tp.startTime).Milliseconds())
			if frac := elapsedMs / tp.expectedMs; frac > 0 {
				if frac > 0.99 {
					frac = 0.99 // never let an overrunning task claim its full weight
				}
				earned += frac * tp.weight
			}
			remainingMs = max(tp.expectedMs-elapsedMs, 0)
		case tp.expectedMs > 0:
			remainingMs = tp.expectedMs // queued task with history
		}
		longestRemainingMs = max(longestRemainingMs, remainingMs)
	}

	fraction = earned / r.totalWeight
	if fraction > 1 {
		fraction = 1
	}
	if fraction >= 1 {
		return fraction, 0, false
	}

	// Raw estimate: the larger of two terms, which keeps it honest rather than
	// optimistic:
	//   - throughput: remaining work at the realized completion rate. The rate
	//     counts only *completed* work, so in-flight partial credit can't make
	//     it look faster than tasks are actually finishing.
	//   - critical path: the single longest unfinished task. The run cannot end
	//     before its longest remaining job does, which dominates the tail once
	//     parallelism narrows to a few long poles.
	etaSeconds := longestRemainingMs / 1000
	if elapsed := now.Sub(r.start).Seconds(); completed > 0 && elapsed > 0.5 {
		if rate := completed / elapsed; rate > 0 {
			etaSeconds = max(etaSeconds, (r.totalWeight-completed)/rate)
		}
	}
	if etaSeconds <= 0 {
		return fraction, 0, false
	}

	// Monotonic clamp: project a finish deadline that can only move earlier. The
	// total task set is fixed at Start, so over the run we only learn we are
	// going *faster* — the countdown should revise down, never bounce back up.
	candidate := now.Add(time.Duration(etaSeconds * float64(time.Second)))
	if r.etaDeadline.IsZero() || candidate.Before(r.etaDeadline) {
		r.etaDeadline = candidate
	}
	if remaining := r.etaDeadline.Sub(now); remaining > 0 {
		return fraction, remaining, true
	}
	// Under-ran the estimate: just show the bar rather than a stuck "0s".
	return fraction, 0, false
}

const miniBarWidth = 14
const miniBarMin = 6

// renderMiniProgress builds the compact progress bar appended to the header
// line: a short weighted bar and the ETA, no percentage, trimmed to the width
// budget. Returns "" when there isn't room for even a small bar.
func (r *LiveRenderer) renderMiniProgress(now time.Time, budget int) string {
	if budget < miniBarMin {
		return ""
	}
	fraction, eta, hasETA := r.sessionProgress(now)

	etaStr := ""
	if hasETA {
		etaStr = "~" + formatETA(eta)
	}
	barW := min(miniBarWidth, budget)
	if etaStr != "" {
		// Shrink the bar to make room for the ETA; drop the ETA if too tight.
		if room := budget - utf8.RuneCountInString(etaStr) - 1; room >= miniBarMin {
			barW = min(miniBarWidth, room)
		} else {
			etaStr = ""
		}
	}
	filled := max(0, min(int(fraction*float64(barW)), barW))
	// Neutral, not an accent: the filled blocks ride the default foreground (a
	// gentle step above the dim header) and the empty blocks are dim. The █/░
	// density carries the progress, so it informs without pulling focus.
	bar := strings.Repeat("█", filled) + colorize(strings.Repeat("░", barW-filled), Dim)
	if etaStr != "" {
		return bar + " " + colorize(etaStr, Dim)
	}
	return bar
}

// formatETA renders a remaining-time estimate, rounded to coarse buckets so the
// countdown reads steadily instead of jittering every frame.
func formatETA(d time.Duration) string {
	secs := int(d.Seconds() + 0.5)
	switch {
	case secs < 1:
		return "<1s"
	case secs < 60:
		if r := roundUpTo(secs, 5); r < 60 {
			return fmt.Sprintf("%ds", r)
		}
		return "1m"
	case secs < 3600:
		m := secs / 60
		s := roundTo(secs%60, 5)
		if s >= 60 {
			m++
			s = 0
		}
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%dh%02dm", secs/3600, (secs%3600)/60)
	}
}

func roundUpTo(v, step int) int {
	if v <= step {
		return step
	}
	return ((v + step - 1) / step) * step
}

func roundTo(v, step int) int {
	return ((v + step/2) / step) * step
}

// medianFloat returns the median of values (0 for an empty slice).
func medianFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	s := make([]float64, len(values))
	copy(s, values)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}
