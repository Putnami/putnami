package clicore

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// The status contract. Every `putnami cloud <entry> status` command
// and the `putnami cloud status` synthesis build a StatusNode, print it with
// one renderer, encode it as the data of one result envelope, and exit by one
// rule. A new fact is a new child or a new metric; the shape never changes.

// StatusState is the closed vocabulary every status node reports. It is the
// one scale `putnami cloud status` compares entries on.
type StatusState string

const (
	// StatusOK means what is declared exists and works.
	StatusOK StatusState = "ok"
	// StatusDegraded means the entry works with a gap that needs attention.
	StatusDegraded StatusState = "degraded"
	// StatusFailing means the entry does not work.
	StatusFailing StatusState = "failing"
	// StatusUnknown means the entry did not answer, or was not checked.
	StatusUnknown StatusState = "unknown"
)

// StatusNode is one checked fact. ID is stable for scripts, Title is for
// people, Detail says what was found, Fix is the one command or action that
// clears a state other than ok, Metrics are the measured values, Facts are the
// identifiers a script reads (a digest, a revision, a release id) that Detail
// only quotes, and Children carry the facts the state was derived from.
type StatusNode struct {
	ID       string            `json:"id"`
	Title    string            `json:"title"`
	State    StatusState       `json:"state"`
	Detail   string            `json:"detail,omitempty"`
	Fix      string            `json:"fix,omitempty"`
	Metrics  []StatusMetric    `json:"metrics,omitempty"`
	Facts    map[string]string `json:"facts,omitempty"`
	Children []StatusNode      `json:"children,omitempty"`
}

// Child returns the direct child with id, and whether it exists.
func (n StatusNode) Child(id string) (StatusNode, bool) {
	for _, child := range n.Children {
		if child.ID == id {
			return child, true
		}
	}
	return StatusNode{}, false
}

// StatusMetricKind says what a metric is for: usage could be billed, health
// says whether the entry works well.
type StatusMetricKind string

const (
	// MetricUsage is a quantity the workspace consumes: stored bytes, runs,
	// spend. It is the kind a bill or a quota would read.
	MetricUsage StatusMetricKind = "usage"
	// MetricHealth is a measure of how well the entry works: queue depth,
	// cache reuse, ready workloads.
	MetricHealth StatusMetricKind = "health"
)

// The units a metric value is expressed in.
const (
	UnitCount   = "count"
	UnitBytes   = "bytes"
	UnitPercent = "percent"
	UnitSeconds = "seconds"
	UnitEUR     = "eur"
)

// StatusMetric is one measured value. Limit is the quota or total the value
// is read against, when one applies. Window names the period a counted value
// covers ("7 days", "last run"); an empty window is the value now. Estimated
// marks a value computed from rates rather than measured.
type StatusMetric struct {
	ID        string           `json:"id"`
	Title     string           `json:"title"`
	Value     float64          `json:"value"`
	Unit      string           `json:"unit"`
	Limit     *float64         `json:"limit,omitempty"`
	Window    string           `json:"window,omitempty"`
	Kind      StatusMetricKind `json:"kind"`
	Estimated bool             `json:"estimated,omitempty"`
}

// CountMetric is a count measured now.
func CountMetric(id, title string, value int, kind StatusMetricKind) StatusMetric {
	return StatusMetric{ID: id, Title: title, Value: float64(value), Unit: UnitCount, Kind: kind}
}

// Of reads the metric against a limit: "311 of 645 (48%)".
func (m StatusMetric) Of(limit float64) StatusMetric {
	m.Limit = &limit
	return m
}

// Over sets the window the metric covers.
func (m StatusMetric) Over(window string) StatusMetric {
	m.Window = window
	return m
}

// FormatValue renders the value in its unit, with its limit when one is set.
func (m StatusMetric) FormatValue() string {
	value := formatMetricValue(m.Value, m.Unit, m.Estimated)
	if m.Limit == nil {
		return value
	}
	text := value + " of " + formatMetricValue(*m.Limit, m.Unit, false)
	if *m.Limit > 0 && m.Unit != UnitPercent {
		text += fmt.Sprintf(" (%d%%)", int(math.Round(m.Value*100 / *m.Limit)))
	}
	return text
}

func formatMetricValue(value float64, unit string, estimated bool) string {
	var text string
	switch unit {
	case UnitBytes:
		text = FormatBytes(int64(value))
	case UnitPercent:
		text = strconv.FormatFloat(math.Round(value*10)/10, 'f', -1, 64) + "%"
	case UnitSeconds:
		text = time.Duration(value * float64(time.Second)).Round(time.Second).String()
	case UnitEUR:
		text = fmt.Sprintf("€%.2f", value)
	default:
		text = groupDigits(int64(math.Round(value)))
	}
	if estimated {
		return "~" + text
	}
	return text
}

// FormatBytes renders a byte count in binary units: 512 B, 1.5 KiB, 212 GiB.
func FormatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit && exp < 5; n /= unit {
		div *= unit
		exp++
	}
	value := float64(bytes) / float64(div)
	precision := 1
	if value >= 10 {
		precision = 0
	}
	return strconv.FormatFloat(value, 'f', precision, 64) + " " + string("KMGTPE"[exp]) + "iB"
}

func groupDigits(value int64) string {
	sign := ""
	if value < 0 {
		sign, value = "-", -value
	}
	digits := strconv.FormatInt(value, 10)
	for index := len(digits) - 3; index > 0; index -= 3 {
		digits = digits[:index] + "," + digits[index:]
	}
	return sign + digits
}

// statusSeverity orders the states from best to worst. Unknown sits between
// degraded and failing: an entry that did not answer is not known to work,
// but it is not known to be broken either.
var statusSeverity = map[StatusState]int{
	StatusOK:       0,
	StatusDegraded: 1,
	StatusUnknown:  2,
	StatusFailing:  3,
}

// ValidStatusState reports whether state belongs to the closed vocabulary.
func ValidStatusState(state StatusState) bool {
	_, ok := statusSeverity[state]
	return ok
}

// WorstStatus returns the worst of states. An empty list is ok; a value
// outside the vocabulary counts as unknown.
func WorstStatus(states ...StatusState) StatusState {
	worst := StatusOK
	for _, state := range states {
		if !ValidStatusState(state) {
			state = StatusUnknown
		}
		if statusSeverity[state] > statusSeverity[worst] {
			worst = state
		}
	}
	return worst
}

// RollUp returns the worst state of the node and every descendant.
func (n StatusNode) RollUp() StatusState {
	states := make([]StatusState, 0, 1+len(n.Children))
	states = append(states, n.State)
	for _, child := range n.Children {
		states = append(states, child.RollUp())
	}
	return WorstStatus(states...)
}

// ChildStates returns the state of each direct child.
func (n StatusNode) ChildStates() []StatusState {
	states := make([]StatusState, 0, len(n.Children))
	for _, child := range n.Children {
		states = append(states, child.State)
	}
	return states
}

// UnknownStatus builds the node of an entry that did not answer. The first
// line of the error is the detail, so the reader sees why, and fix names the
// command that shows the entry in full.
func UnknownStatus(id, title string, err error, fix string) StatusNode {
	detail := "no answer"
	if err != nil {
		detail = strings.TrimSpace(err.Error())
		if first, _, found := strings.Cut(detail, "\n"); found {
			detail = strings.TrimSpace(first)
		}
	}
	return StatusNode{ID: id, Title: title, State: StatusUnknown, Detail: detail, Fix: fix}
}

func normalState(state StatusState) StatusState {
	if !ValidStatusState(state) {
		return StatusUnknown
	}
	return state
}

// RenderStatusTable renders the synthesis: a header, then one line per node
// with the state, the title, the detail, and a command. The command is the
// node's Fix when the state is not ok; otherwise, or when the node has no
// Fix, it is more(node), the command that shows the entry in full. Children
// and metrics are not printed: they are in the structured output, and the
// entry's own status command shows them.
func RenderStatusTable(nodes []StatusNode, more func(StatusNode) string) []string {
	rows := make([][]string, 0, len(nodes))
	for _, node := range nodes {
		state := normalState(node.State)
		command := node.Fix
		if (state == StatusOK || command == "") && more != nil {
			command = more(node)
		}
		rows = append(rows, []string{string(state), node.Title, node.Detail, command})
	}
	return renderColumns([]string{"STATE", "ENTRY", "DETAIL", "NEXT"}, rows, "")
}

// RenderStatusReport renders one entry's own status command: a header line
// with the title, the state and the detail; the metrics table; the checks
// table, with the fix of each check that is not ok under it and grandchildren
// indented under their parent; and the next command when the entry is not ok
// and no check names one.
func RenderStatusReport(node StatusNode) []string {
	state := normalState(node.State)
	header := node.Title + "  " + string(state)
	if node.Detail != "" {
		header += "  " + node.Detail
	}
	lines := []string{header}
	if len(node.Metrics) > 0 {
		rows := make([][]string, 0, len(node.Metrics))
		for _, metric := range node.Metrics {
			window := metric.Window
			if window == "" {
				window = "now"
			}
			rows = append(rows, []string{metric.Title, metric.FormatValue(), window})
		}
		lines = append(lines, "")
		lines = append(lines, renderColumns([]string{"METRIC", "VALUE", "WINDOW"}, rows, "  ")...)
	}
	childFix := false
	if len(node.Children) > 0 {
		var rows [][]string
		var fixes []string
		var walk func(children []StatusNode, depth int)
		walk = func(children []StatusNode, depth int) {
			for _, child := range children {
				childState := normalState(child.State)
				rows = append(rows, []string{string(childState), strings.Repeat("  ", depth) + child.Title, child.Detail})
				fix := ""
				if childState != StatusOK && child.Fix != "" {
					fix = child.Fix
					childFix = true
				}
				fixes = append(fixes, fix)
				walk(child.Children, depth+1)
			}
		}
		walk(node.Children, 0)
		table := renderColumns([]string{"STATE", "CHECK", "DETAIL"}, rows, "  ")
		lines = append(lines, "", table[0])
		indent := detailOffset(table[0])
		for index, line := range table[1:] {
			lines = append(lines, line)
			if fixes[index] != "" {
				lines = append(lines, strings.Repeat(" ", indent)+"fix: "+fixes[index])
			}
		}
	}
	if state != StatusOK && node.Fix != "" && !childFix {
		lines = append(lines, "", "next: "+node.Fix)
	}
	return lines
}

// detailOffset is the column where the DETAIL header starts.
func detailOffset(header string) int {
	if index := strings.Index(header, "DETAIL"); index >= 0 {
		return index
	}
	return 0
}

// renderColumns aligns rows under headers. The last column is not padded.
func renderColumns(headers []string, rows [][]string, indent string) []string {
	widths := make([]int, len(headers))
	for index, header := range headers {
		widths[index] = len(header)
	}
	for _, row := range rows {
		for index, cell := range row {
			widths[index] = max(widths[index], len([]rune(cell)))
		}
	}
	format := func(cells []string) string {
		var line strings.Builder
		line.WriteString(indent)
		for index, cell := range cells {
			if index > 0 {
				line.WriteString("  ")
			}
			line.WriteString(cell)
			if index < len(cells)-1 {
				line.WriteString(strings.Repeat(" ", widths[index]-len([]rune(cell))))
			}
		}
		return strings.TrimRight(line.String(), " ")
	}
	lines := make([]string, 0, len(rows)+1)
	lines = append(lines, format(headers))
	for _, row := range rows {
		lines = append(lines, format(row))
	}
	return lines
}

// WriteStatus prints one status and returns the exit decision every status
// command shares: success unless the state is failing, or, with --strict,
// unless it is ok. Structured output holds one envelope whose data is the
// node, on success and on failure alike.
func WriteStatus(params map[string]any, ioctx IO, node StatusNode) error {
	node.State = normalState(node.State)
	text := strings.Join(RenderStatusReport(node), "\n")
	return writeStatusDocument(params, ioctx, node.Title, node.State, node.Detail, node, text)
}

// WriteStatusSynthesis prints the synthesis table of `putnami cloud status`
// under the same exit rule as WriteStatus. The envelope data is the overall
// state and every entry node.
func WriteStatusSynthesis(params map[string]any, ioctx IO, nodes []StatusNode, more func(StatusNode) string) error {
	states := make([]StatusState, 0, len(nodes))
	var notOK []string
	for _, node := range nodes {
		state := normalState(node.State)
		states = append(states, state)
		if state != StatusOK {
			notOK = append(notOK, node.Title+" "+string(state))
		}
	}
	overall := WorstStatus(states...)
	text := strings.Join(RenderStatusTable(nodes, more), "\n")
	data := map[string]any{"state": overall, "entries": nodes}
	return writeStatusDocument(params, ioctx, "workspace", overall, strings.Join(notOK, ", "), data, text)
}

func writeStatusDocument(params map[string]any, ioctx IO, title string, state StatusState, detail string, data any, text string) error {
	failed := state == StatusFailing || (BoolParam(params, false, "strict") && state != StatusOK)
	if !failed {
		WriteResult(data, params, ioctx, text)
		return nil
	}
	if !StructuredOutput(params) && ioctx.Stdout != nil {
		ioctx.Stdout(text)
	}
	message := title + " is " + string(state)
	if detail != "" {
		message += ": " + detail
	}
	return WithResultData(NewError(message, ExitFailure), data)
}
