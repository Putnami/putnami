package output

import (
	"fmt"
	"sort"
	"time"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// reserved summary-cell widths per command, including the command label and a
// status glyph. Fixed widths keep columns aligned and prevent jitter as
// counters update; longer content is truncated to fit.
var commandCellWidth = map[string]int{
	"generate": 14,
	"lint":     15,
	"format":   15,
	// test carries the coverage percentage alongside the pass counts
	// ("test ✓ 285/285 80.8%"), so it reserves more room than the other cells.
	"test":    22,
	"build":   16,
	"package": 15,
	"deploy":  13,
	"publish": 15,
	"serve":   18,
}

const defaultCellWidth = 14
const minProjectNameWidth = 6

// liveRedrawInterval paces the redraw loop. Differential repainting (see paint)
// rewrites only changed lines in a single write, so a fast cadence no longer
// flickers — and it matches the spinner step so every spinner frame is drawn,
// giving smooth rotation on running rows.
const liveRedrawInterval = 80 * time.Millisecond
const liveDoneRetention = 3 * time.Second

// Start is called when job execution begins. It builds one row per project,
// assigns stable colors, records the cross-project dependency graph, and
// computes the column layout — all of which stay fixed for the run.
func (r *LiveRenderer) Start(planned []*jobs.ScheduledJob) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.start = time.Now()
	r.stopped = false
	r.reset()

	// Build rows and commands in first-seen plan order.
	colorIdx := 0
	cmdSeen := make(map[string]bool)
	for _, job := range planned {
		r.jobToProj[job.Key()] = job.Project.ID
		cacheable := jobs.CanUseCache(job)
		r.jobCacheable[job.Key()] = cacheable

		row := r.rowByID[job.Project.ID]
		if row == nil {
			row = &projectRow{
				id:       job.Project.ID,
				name:     job.Project.Name,
				color:    projectColorPalette[colorIdx%len(projectColorPalette)],
				order:    len(r.rows),
				commands: make(map[string]*commandState),
				deps:     make(map[string]struct{}),
			}
			colorIdx++
			r.rows = append(r.rows, row)
			r.rowByID[job.Project.ID] = row
		}

		cmd := commandName(job)
		cs := row.commands[cmd]
		if cs == nil {
			cs = &commandState{name: cmd, metrics: make(map[string]float64)}
			row.commands[cmd] = cs
			row.commandOrder = append(row.commandOrder, cmd)
		}
		cs.totalSteps++
		if cacheable {
			cs.cacheableSteps++
		} else {
			cs.alwaysRunSteps++
			if cs.alwaysRunSteps == 1 {
				cs.alwaysRunStep = stepName(job.JobDef.Name)
			}
		}
		cmdSeen[cmd] = true
	}
	for projectID, replay := range r.coverageReplay {
		row := r.rowByID[projectID]
		if row == nil {
			continue
		}
		cmd := row.commands["test"]
		if cmd == nil {
			continue
		}
		copy := replay
		cmd.replayedCoverage = &copy
	}

	r.sortCommandOrders()
	r.buildDependencyGraph(planned)
	r.computeLayout(cmdSeen)
	r.initProgress(planned)

	iox.Fprint(r.errOut, HideCursor)
	iox.Fprint(r.errOut, "\n")

	r.stopCh = make(chan struct{})
	r.doneCh = make(chan struct{})
	r.ticker = time.NewTicker(liveRedrawInterval)
	go r.redrawLoop()
}

// sortCommandOrders sorts each project's command list into canonical order.
func (r *LiveRenderer) sortCommandOrders() {
	for _, row := range r.rows {
		order := row.commandOrder
		sort.SliceStable(order, func(i, j int) bool {
			ri, rj := commandRank(order[i]), commandRank(order[j])
			if ri != rj {
				return ri < rj
			}
			return order[i] < order[j]
		})
	}
}

// buildDependencyGraph records, per project, the set of other projects it
// depends on. These cross-project edges drive the blocked/blocked-by display.
// Both functional dependencies and write-serialization edges count: each is a
// reason a project's job cannot start until another project's job finishes.
func (r *LiveRenderer) buildDependencyGraph(planned []*jobs.ScheduledJob) {
	for _, job := range planned {
		row := r.rowByID[job.Project.ID]
		if row == nil {
			continue
		}
		record := func(dep string) {
			depProj, ok := r.jobToProj[dep]
			if !ok || depProj == job.Project.ID {
				return
			}
			row.deps[depProj] = struct{}{}
		}
		for _, dep := range job.DependsOn {
			record(dep)
		}
		for _, dep := range job.SerializeAfter {
			record(dep)
		}
	}
}

// computeLayout fixes the column widths and summary column order for the run.
func (r *LiveRenderer) computeLayout(cmdSeen map[string]bool) {
	// Project-name column: widest name, later constrained by terminal width.
	nameW := minProjectNameWidth
	for _, row := range r.rows {
		if l := len([]rune(row.name)); l > nameW {
			nameW = l
		}
	}

	// Status/phase column: widest of the status words and command names.
	statusW := len(statusCoalesced)
	for cmd := range cmdSeen {
		if l := len(cmd); l > statusW {
			statusW = l
		}
	}
	if statusW > 10 {
		statusW = 10
	}
	r.statusWidth = statusW

	// Summary columns: one per command in canonical order. Coverage has no column
	// of its own — it renders inside the test cell (see successValue) so it cannot
	// be dropped while the test counts survive.
	cmds := make([]string, 0, len(cmdSeen))
	for cmd := range cmdSeen {
		cmds = append(cmds, cmd)
	}
	sort.SliceStable(cmds, func(i, j int) bool {
		ri, rj := commandRank(cmds[i]), commandRank(cmds[j])
		if ri != rj {
			return ri < rj
		}
		return cmds[i] < cmds[j]
	})

	r.colOrder = nil
	for _, cmd := range cmds {
		r.colOrder = append(r.colOrder, cmd)
		r.colWidth[cmd] = cellWidth(cmd)
		extraWidth := 0
		for _, row := range r.rows {
			state := row.commands[cmd]
			if state == nil || state.cacheableSteps == 0 || state.alwaysRunSteps == 0 {
				continue
			}
			// Mixed cached/always-run commands need room for provenance without
			// sacrificing the restored metric the base width already reserves.
			label := state.alwaysRunStep
			if state.alwaysRunSteps != 1 || label == "" {
				label = fmt.Sprintf("%d always-run", state.alwaysRunSteps)
			}
			note := fmt.Sprintf("(+%s %s)", label, formatSessionDuration(999*time.Millisecond))
			if extra := len(" ") + len(note); extra > extraWidth {
				extraWidth = extra
			}
		}
		r.colWidth[cmd] += extraWidth
	}

	r.nameWidth = r.adaptiveNameWidth(nameW)
}

func cellWidth(cmd string) int {
	if w, ok := commandCellWidth[cmd]; ok {
		return w
	}
	w := len(cmd) + 1 + 9
	if w < defaultCellWidth {
		w = defaultCellWidth
	}
	return w
}

func (r *LiveRenderer) adaptiveNameWidth(natural int) int {
	if natural < minProjectNameWidth {
		natural = minProjectNameWidth
	}
	if r.width <= 0 {
		return natural
	}

	// Choose the widest project-name column that preserves the summary columns
	// that fit at the minimum name width. Wide terminals get full project names;
	// narrow terminals spend scarce width on status summaries first.
	budgetAtMinName := r.width - r.leadingWidthForName(minProjectNameWidth)
	_, summaryWidth := r.visibleColumnsForBudget(budgetAtMinName)
	available := r.width - r.fixedLeadingWidthWithoutName() - summaryWidth
	switch {
	case available >= natural:
		return natural
	case available >= minProjectNameWidth:
		return available
	default:
		return minProjectNameWidth
	}
}

// redrawLoop refreshes the live table at a fixed cadence so elapsed clocks and
// counters advance smoothly. The display itself is static between updates
// (no per-row animation) to keep the view calm.
func (r *LiveRenderer) redrawLoop() {
	defer close(r.doneCh)
	for {
		select {
		case <-r.ticker.C:
			r.mu.Lock()
			r.redraw()
			r.mu.Unlock()
		case <-r.stopCh:
			return
		}
	}
}

// stopRedraw halts the redraw loop and blocks until the goroutine has fully
// exited, so callers can touch renderer state and write output without racing
// the background redraw. Safe to call more than once.
func (r *LiveRenderer) stopRedraw() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	if r.ticker != nil {
		r.ticker.Stop()
	}
	if r.stopCh != nil {
		close(r.stopCh)
	}
	done := r.doneCh
	r.mu.Unlock()

	if done != nil {
		<-done
	}
}
