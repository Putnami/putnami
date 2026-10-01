package sessions

import (
	"encoding/json"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// SessionsSummary prints one line per recorded session of the current worktree:
// what ran, over which selection, how many tasks, what it cost, and how it
// ended.
//
// It exists because that reduction was a hand-written `python3` one-liner over
// session.json, rewritten by every consumer that needed the weekly ledger or a
// benchmark, and wrong twice: it counted a nested run (a task that invokes the
// CLI again) as a second gate, and it counted one session twice when a worktree
// reached the store through a symlink. Both are properties of the READ, so the
// fix belongs here rather than in each consumer — the ordering and the
// deduplication come from recordedSessionStream, the same code `sessions export`
// streams, and nesting is read from the record's own parentSessionId.
//
// A nested run is LISTED and MARKED in the unfiltered listing, not dropped. Its
// CPU is real, and a reader reconciling a machine's cost against the ledger
// needs to see it. What must never happen is counting it as a run of its own,
// which is why --command selects top-level runs only; see summaryCommandFilter.
//
// With byDigest the same selected sessions are regrouped by the input digest
// their task records carry; see summarizeByDigest.
func SessionsSummary(wsRoot string, since string, commands string, byDigest bool, outputFormat string) error {
	filter, err := parseSummaryCommandFilter(commands)
	if err != nil {
		return err
	}
	documents, err := recordedSessionStream(wsRoot, since)
	if err != nil {
		return err
	}

	rows := make([]sessionSummaryRow, 0, len(documents))
	var tasks []summaryTaskRecord
	for _, document := range documents {
		row, records, ok := summaryRowFrom(document)
		if !ok {
			continue
		}
		if !filter.matches(row) {
			continue
		}
		rows = append(rows, row)
		for i := range records {
			tasks = append(tasks, summaryTaskRecord{sessionID: row.SessionID, record: records[i]})
		}
	}

	structured := outputFormat == "json" || outputFormat == "jsonl"
	if byDigest {
		digests, undigested := summarizeByDigest(tasks)
		if structured {
			return printSummaryRowsJSON(digests)
		}
		return printDigestTable(digests, len(tasks), undigested)
	}
	if structured {
		return printSummaryRowsJSON(rows)
	}
	return printSummaryTable(rows)
}

// sessionSummaryRow is one session reduced to the facts a ledger row carries.
// It is the `--output=json` contract: the rows travel as the shared result
// envelope's `data` array, in the same stream order `sessions export` uses and
// aggregated from the same records.
type sessionSummaryRow struct {
	SessionID string `json:"sessionId"`
	// ParentSessionID names the session whose task spawned this run, absent for
	// a top-level run and for every record written before the member existed.
	ParentSessionID string `json:"parentSessionId,omitempty"`
	// Nested is ParentSessionID stated as the predicate a consumer filters on.
	// It is false — not unknown — for a record that carries no parent: the
	// member is optional and absent means top-level, which is what an older
	// record also means, since older CLIs recorded nested runs the same way.
	Nested    bool     `json:"nested"`
	StartTime string   `json:"startTime,omitempty"`
	Commands  []string `json:"commands"`
	// Selection is how the run chose its projects, absent for a record that
	// states none.
	Selection *protocolcli.SessionSelection `json:"selection,omitempty"`
	Tasks     sessionSummaryTasks           `json:"tasks"`
	// CPUActualMs is run.cpu.actualMs: the CPU the run's subprocesses actually
	// consumed. It is a POINTER because absent and zero are different answers —
	// a run that spawned nothing, or a producer that captured no runner
	// environment, records no CPU block at all, and reporting 0 would invent a
	// measurement.
	CPUActualMs *int64 `json:"cpuActualMs,omitempty"`
	WallMs      int64  `json:"wallMs"`
	Outcome     string `json:"outcome,omitempty"`
}

// sessionSummaryTasks is the task histogram, split the way a cost reader needs
// it: executed is what the machine paid for, reused is what the cache saved.
type sessionSummaryTasks struct {
	Total int `json:"total"`
	// Executed is Total minus Reused. It is derived rather than recorded
	// because the contract's histograms are the verdict and the provenance, and
	// deriving keeps executed+reused equal to total by construction.
	Executed int `json:"executed"`
	// Reused sums run.reuse: local cache, remote cache and in-run coalescing.
	Reused int `json:"reused"`
}

// summaryTaskRecord is one recorded task record and the session that recorded
// it.
type summaryTaskRecord struct {
	sessionID string
	record    protocolcli.TaskRecord
}

// digestSummaryRow is one input digest reduced across the selected sessions:
// how many task records were keyed on it, and what each run did with it. It is
// the `--by-digest --output=json` contract, carried as the envelope's `data`
// array like the session rows.
type digestSummaryRow struct {
	InputDigest string `json:"inputDigest"`
	// Tasks are the distinct task keys recorded at this digest, sorted. The key
	// is computed over the task and its project, so one key is the expected case.
	Tasks   []string `json:"tasks"`
	Records int      `json:"records"`
	// Executed is Records minus Reused, the same derivation the session rows use:
	// a replayed failure spawned nothing but reused no result either.
	Executed int `json:"executed"`
	// Reused counts records served from the local cache, the remote cache or an
	// in-run coalesced result.
	Reused int `json:"reused"`
	// Failed counts failed records, executed or replayed, so "red at this digest
	// before my change" reads off one number.
	Failed int `json:"failed"`
	// Occurrences are the records themselves, in stream order: which session
	// recorded each one, and whether it executed or reused.
	Occurrences []digestOccurrence `json:"occurrences"`
}

// digestOccurrence is one task record observed at a digest.
type digestOccurrence struct {
	SessionID string `json:"sessionId"`
	Task      string `json:"task"`
	Status    string `json:"status"`
	Reuse     string `json:"reuse"`
}

// summarizeByDigest groups task records by the input digest they carry, in the
// order each digest was first recorded — oldest session first, then record
// order — so the grouping is as deterministic as the stream it reads.
//
// A record without a digest is counted apart and never grouped: a skipped task,
// a task with no cache identity, and every record an older CLI wrote carry none,
// and folding them under an empty digest would make unrelated tasks look like
// observations of one key.
func summarizeByDigest(tasks []summaryTaskRecord) ([]digestSummaryRow, int) {
	rows := make([]digestSummaryRow, 0)
	index := make(map[string]int)
	undigested := 0
	for i := range tasks {
		record := &tasks[i].record
		if record.InputDigest == "" {
			undigested++
			continue
		}
		position, seen := index[record.InputDigest]
		if !seen {
			position = len(rows)
			index[record.InputDigest] = position
			rows = append(rows, digestSummaryRow{InputDigest: record.InputDigest})
		}
		row := &rows[position]
		row.Records++
		if record.Reuse != "" && record.Reuse != protocolcli.TaskReuseNone {
			row.Reused++
		}
		if record.Status == protocolcli.TaskStatusFailed {
			row.Failed++
		}
		row.Occurrences = append(row.Occurrences, digestOccurrence{
			SessionID: tasks[i].sessionID,
			Task:      record.Identity.Key,
			Status:    record.Status,
			Reuse:     record.Reuse,
		})
		if !slices.Contains(row.Tasks, record.Identity.Key) {
			row.Tasks = append(row.Tasks, record.Identity.Key)
		}
	}
	for i := range rows {
		rows[i].Executed = rows[i].Records - rows[i].Reused
		sort.Strings(rows[i].Tasks)
	}
	return rows, undigested
}

// summaryRowFrom reduces one recorded document to a row, and returns the task
// records a version-2 document carries so --by-digest reads the same decode.
//
// It reads both recorded contracts, exactly as readSessionMeta does, because a
// workspace that upgraded across the version-2 flip still holds records written
// by the older CLI and a ledger that silently skipped them would understate the
// period. A version-1 record simply carries none of the members version 2 added:
// no CPU, no selection, no parent, so those columns read as absent rather than
// as zero.
func summaryRowFrom(document recordedSessionDocument) (sessionSummaryRow, []protocolcli.TaskRecord, bool) {
	data := []byte(document.line)
	if recordedProtocolVersion(data) >= 2 {
		var doc protocolcli.SessionFile
		if err := json.Unmarshal(data, &doc); err != nil {
			return sessionSummaryRow{}, nil, false
		}
		reused := doc.Run.Reuse.Sum()
		row := sessionSummaryRow{
			SessionID:       doc.SessionID,
			ParentSessionID: doc.ParentSessionID,
			Nested:          doc.ParentSessionID != "",
			StartTime:       doc.StartTime,
			Commands:        doc.Commands,
			Selection:       doc.Selection,
			Tasks: sessionSummaryTasks{
				Total:    doc.Run.Counts.Total,
				Executed: doc.Run.Counts.Total - reused,
				Reused:   reused,
			},
			WallMs:  doc.Run.DurationMs,
			Outcome: doc.Run.Outcome,
		}
		if doc.Run.CPU != nil {
			actual := doc.Run.CPU.ActualMs
			row.CPUActualMs = &actual
		}
		return row, doc.Tasks, true
	}

	var meta workspace_state.SessionMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return sessionSummaryRow{}, nil, false
	}
	row := sessionSummaryRow{
		SessionID: meta.ID,
		StartTime: meta.StartTime,
		Commands:  meta.Commands,
		WallMs:    meta.Duration,
	}
	if row.SessionID == "" {
		row.SessionID = document.dir
	}
	if meta.Stats != nil {
		reused := meta.Stats.Cached + meta.Stats.Coalesced
		row.Tasks = sessionSummaryTasks{
			Total:    meta.Stats.Total,
			Executed: meta.Stats.Total - reused,
			Reused:   reused,
		}
		// Version 1 recorded no run outcome, only the counters the verdict was
		// derived from. Recover the same strict reduction version 2 states
		// rather than leaving the column blank.
		row.Outcome = protocolcli.RunOutcomeSuccess
		if meta.Stats.Failed > 0 {
			row.Outcome = protocolcli.RunOutcomeFailure
		}
	}
	return row, nil, true
}

// summaryCommandFilter is the parsed --command value.
type summaryCommandFilter struct {
	active bool
	names  map[string]bool
}

// parseSummaryCommandFilter parses --command as a comma-separated command list.
//
// The match is EXACT SET EQUALITY, order-insensitive: a session matches when the
// commands it recorded are exactly the ones named, whatever order the user typed
// them in. Subset matching would also return every bare `build`, including the
// one a gate's own validation spawns. Order is ignored because a run's command
// order is an execution detail, not an identity: `test,lint` and `lint,test` are
// the same shape of run to a ledger.
//
// A NESTED RUN NEVER MATCHES, whatever it ran. --command answers "which runs of
// this shape happened", and a run a task spawned is not a run of its own — the
// same sentence the table footer states. Leaving nesting to the command set
// alone would have made the filter's correctness depend on an assumption nothing
// enforces: that no task ever invokes the CLI with the command set being asked
// for. A validation guard that ran the full gate would then be returned AS a
// gate and double-count in every ledger, which is the exact miscount this
// command exists to remove. The assumption is not hypothetical at any other
// width either — `--command build` today matches only the builds that
// `validate~clientgen-guard` spawned.
//
// A reader who wants the nested runs lists without --command, where they are
// listed and marked, or filters `nested` in --output=json.
func parseSummaryCommandFilter(raw string) (summaryCommandFilter, error) {
	if strings.TrimSpace(raw) == "" {
		return summaryCommandFilter{}, nil
	}
	names := make(map[string]bool)
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			return summaryCommandFilter{}, cmderr.Usagef(
				"invalid --command value %q: expected a comma-separated command list such as lint,test,build,validate", raw)
		}
		names[name] = true
	}
	return summaryCommandFilter{active: true, names: names}, nil
}

func (f summaryCommandFilter) matches(row sessionSummaryRow) bool {
	if !f.active {
		return true
	}
	if row.Nested {
		return false
	}
	seen := make(map[string]bool, len(row.Commands))
	for _, command := range row.Commands {
		if !f.names[command] {
			return false
		}
		seen[command] = true
	}
	return len(seen) == len(f.names)
}

// printSummaryRowsJSON emits the rows — session rows, or digest rows under
// --by-digest — as ONE JSON document: an array, always, including when it is
// empty.
//
// Emitting them as a JSONL stream instead — one object per line, which is what
// `sessions export` does — would put the rows under `ResultV2.data` in a shape
// that changes with their COUNT. The shared capture in internal/cli collects the
// documents a structured command printed and unwraps a stream of exactly one
// (`structuredPayload`), so a summary with no sessions lands as `data: null` and
// a summary with exactly one session lands as a bare object. Both break the
// `.data[]` a ledger is documented to read, and the single-session case is the
// ordinary shape of `--command lint,test,build,validate` on a fresh worktree.
// One array document is unwrapped to itself, so `data` is an array at every
// count.
func printSummaryRowsJSON[Row sessionSummaryRow | digestSummaryRow](rows []Row) error {
	// Never nil: a nil slice marshals to `null`, which is the shape this function
	// exists to rule out.
	if rows == nil {
		rows = []Row{}
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	iox.Fprintln(os.Stdout, string(data))
	return nil
}

func printSummaryTable(rows []sessionSummaryRow) error {
	if len(rows) == 0 {
		iox.Fprintln(os.Stdout, "  No sessions match.")
		return nil
	}

	iox.Fprintln(os.Stdout)
	header := "  %-22s %-19s %-24s %-28s %-14s %-10s %-9s %s\n"
	iox.Fprintf(os.Stdout, header, "SESSION", "START", "COMMANDS", "SELECTION", "TASKS", "CPU", "WALL", "OUTCOME")
	iox.Fprintf(os.Stdout, header, "-------", "-----", "--------", "---------", "-----", "---", "----", "-------")

	nested := 0
	for _, row := range rows {
		outcome := row.Outcome
		if outcome == "" {
			outcome = "unknown"
		}
		if row.Nested {
			nested++
			// The marker travels with the outcome rather than in its own column:
			// it is rare, and a column that is blank on almost every line costs
			// every line its width.
			outcome += " (nested)"
		}
		iox.Fprintf(os.Stdout, header,
			truncateSummaryCell(row.SessionID, 22),
			formatTimestamp(row.StartTime),
			truncateSummaryCell(strings.Join(row.Commands, ","), 24),
			truncateSummaryCell(formatSummarySelection(row.Selection), 28),
			formatSummaryTasks(row.Tasks),
			formatSummaryCPU(row.CPUActualMs),
			formatDurationMs64(row.WallMs),
			outcome,
		)
	}

	iox.Fprintf(os.Stdout, "\n  %d session%s", len(rows), plural(len(rows)))
	if nested > 0 {
		iox.Fprintf(os.Stdout, " · %d nested (spawned by a task of another session, not a run of their own)", nested)
	}
	iox.Fprintf(os.Stdout, "\n\n")
	return nil
}

// printDigestTable renders --by-digest: one line per digest, then how many of
// the selected records carried none, so a reader knows what the grouping left
// out rather than reading its absence as zero.
func printDigestTable(rows []digestSummaryRow, records int, undigested int) error {
	if len(rows) == 0 && records == 0 {
		iox.Fprintln(os.Stdout, "  No sessions match.")
		return nil
	}

	iox.Fprintln(os.Stdout)
	header := "  %-21s %-44s %-8s %-9s %-7s %s\n"
	iox.Fprintf(os.Stdout, header, "DIGEST", "TASK", "RECORDS", "EXECUTED", "REUSED", "FAILED")
	iox.Fprintf(os.Stdout, header, "------", "----", "-------", "--------", "------", "------")
	for _, row := range rows {
		task := strings.Join(row.Tasks, ",")
		iox.Fprintf(os.Stdout, header,
			truncateSummaryCell(row.InputDigest, 21),
			truncateSummaryCell(task, 44),
			strconv.Itoa(row.Records),
			strconv.Itoa(row.Executed),
			strconv.Itoa(row.Reused),
			strconv.Itoa(row.Failed),
		)
	}

	iox.Fprintf(os.Stdout, "\n  %d digest%s over %d record%s", len(rows), plural(len(rows)), records-undigested, plural(records-undigested))
	if undigested > 0 {
		iox.Fprintf(os.Stdout, " · %d record%s without a digest (skipped, no cache identity, or recorded by an older CLI)",
			undigested, plural(undigested))
	}
	iox.Fprintf(os.Stdout, "\n\n")
	return nil
}

// formatSummarySelection renders the selection the way the user would have
// spelled it, so a ledger row is readable without knowing the wire vocabulary.
// A record that states no selection renders as "-": it is unknown, not "all".
func formatSummarySelection(selection *protocolcli.SessionSelection) string {
	if selection == nil {
		return "-"
	}
	switch selection.Mode {
	case protocolcli.SessionSelectionModeImpacted:
		return "--impacted (" + strconv.Itoa(len(selection.Projects)) + ")"
	case protocolcli.SessionSelectionModeAll:
		if selection.Scoped {
			// `all` with a scope is the filtered whole-workspace projection
			// (--filter-tag, --exclude): every project was considered, not every
			// project was selected, and the count is the only honest summary.
			return "--all filtered (" + strconv.Itoa(len(selection.Projects)) + ")"
		}
		return "--all"
	default:
		if len(selection.Projects) == 0 {
			return "--projects"
		}
		return "--projects " + strings.Join(selection.Projects, ",")
	}
}

func formatSummaryTasks(tasks sessionSummaryTasks) string {
	return strconv.Itoa(tasks.Total) + "/" + strconv.Itoa(tasks.Executed) + "/" + strconv.Itoa(tasks.Reused)
}

// formatSummaryCPU renders run.cpu.actualMs. An absent measurement renders as
// "-" rather than as 0, because a run that spawned nothing and a run whose
// runner environment was never captured both record none, and printing a zero
// would put a fabricated measurement into a ledger.
func formatSummaryCPU(actualMs *int64) string {
	if actualMs == nil {
		return "-"
	}
	return formatDurationMs64(*actualMs)
}

// plural is the table footer's "1 session" / "n sessions" suffix.
func plural(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func truncateSummaryCell(value string, width int) string {
	if value == "" {
		return "-"
	}
	if len(value) <= width {
		return value
	}
	return value[:width-3] + "..."
}
