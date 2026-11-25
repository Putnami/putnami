package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/level3"
)

const previewReport = "https://putnami.app/w/acme/changes/acme%2Fshop%2342"

func preview(t *testing.T, raw string) *level3.Preview {
	t.Helper()
	p := level3.Parse(json.RawMessage(raw))
	if p == nil {
		t.Fatalf("preview %s does not parse", raw)
	}
	return p
}

// mixedPreview holds two signals, two known zeros and one unknown.
const mixedPreview = `{"level":3,"baseBranch":"main","holds":2,
	"brokenRules":{"state":"known","count":1},
	"coverageLost":{"state":"known","count":2},
	"deletedDecisions":{"state":"known","count":0},
	"untestedSpecs":{"state":"known","count":0},
	"vanishedTests":{"state":"unknown","reason":"no run of ` + "`main`" + ` to compare"}}`

// allKnown builds a preview whose five signals are known with the counts.
func allKnown(counts ...int) string {
	members := []string{`"level":3`, `"baseBranch":"main"`}
	for i, name := range level3.Names {
		members = append(members, `"`+name+`":{"state":"known","count":`+string(rune('0'+counts[i]))+`}`)
	}
	return "{" + strings.Join(members, ",") + "}"
}

// previewLine returns the line of the note that holds the preview.
func previewLine(t *testing.T, note string) string {
	t.Helper()
	for _, line := range strings.Split(note, "\n") {
		if strings.HasPrefix(line, "**Level 3 preview**") {
			return line
		}
	}
	t.Fatalf("the note holds no preview line:\n%s", note)
	return ""
}

func TestTheNoteSaysWhatLevel3WouldHaveHeld(t *testing.T) {
	spectest.Proves(t, feature, "publish-level-3-preview", "the-note-says-what-level-3-would-have-held")
	golden, err := os.ReadFile(filepath.Join("testdata", "note-github-level3.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Note(pullRequest(), previewReport, preview(t, mixedPreview)); got != string(golden) {
		t.Fatalf("note\n%s\nwant\n%s", got, golden)
	}
	for counts, want := range map[[5]int]string{
		{0, 0, 0, 0, 0}: "level 3 would have held nothing on this change.",
		{3, 0, 1, 0, 0}: "level 3 would have held this change on 3 broken rules and 1 deleted decision.",
		{1, 1, 1, 1, 1}: "level 3 would have held this change on 1 broken rule, 1 component that lost coverage, 1 deleted decision, 1 untested spec and 1 vanished test.",
	} {
		line := previewLine(t, Note(pullRequest(), "", preview(t, allKnown(counts[:]...))))
		if !strings.HasPrefix(line, "**Level 3 preview** against `main`: "+want) {
			t.Errorf("counts %v: line %q, want it to say %q", counts, line, want)
		}
		if strings.Contains(line, "as far as the evidence tells") || strings.Contains(line, "unknown") {
			t.Errorf("counts %v: every signal is known, yet %q", counts, line)
		}
	}
}

func TestAnUnknownSignalIsNeverAZero(t *testing.T) {
	spectest.Proves(t, feature, "publish-level-3-preview", "an-unknown-signal-is-never-a-zero")
	for name, c := range map[string]struct{ raw, want string }{
		"one signal known": {
			`{"level":3,"brokenRules":{"state":"unknown","reason":"no lint report"},"coverageLost":{"state":"known","count":0}}`,
			"**Level 3 preview**: as far as the evidence tells, no known level 3 signal would have held this change; " +
				"unknown: broken rules (`no lint report`), deleted decisions (`the reporter answered no state for it`), " +
				"untested specs (`the reporter answered no state for it`), vanished tests (`the reporter answered no state for it`).",
		},
		"no signal known": {
			`{"level":3}`,
			"**Level 3 preview**: the evidence does not tell what level 3 would have held on this change; unknown: broken rules",
		},
		"a signal partly compared": {
			allKnown(0, 0, 0, 0, 0)[:len(allKnown(0, 0, 0, 0, 0))-2] + `,"notCompared":["apps/web","apps/api"]}}`,
			"**Level 3 preview** against `main`: as far as the evidence tells, no level 3 signal would have held this change on the components the evidence compared; " +
				"partly compared: vanished tests (2 components not compared).",
		},
	} {
		t.Run(name, func(t *testing.T) {
			line := previewLine(t, Note(pullRequest(), "", preview(t, c.raw)))
			if !strings.HasPrefix(line, c.want) {
				t.Fatalf("line\n%s\nwant it to start with\n%s", line, c.want)
			}
			for _, zero := range []string{" 0 ", "nothing"} {
				if strings.Contains(line, zero) {
					t.Errorf("line %q reads what it does not know as %q", line, zero)
				}
			}
		})
	}
	// A reason the reporter wrote renders as text, never as Markdown, HTML, a
	// link or a mention.
	line := previewLine(t, Note(pullRequest(), "", preview(t, `{"brokenRules":{"state":"unknown","reason":"@octocat <img src=x> [a](https://x) *b*"}}`)))
	if !strings.Contains(line, "broken rules (`@octocat <img src=x> [a](https://x) *b*`)") {
		t.Fatalf("a reason outside a code span: %s", line)
	}
}

func TestTheNoteNamesTheNextStep(t *testing.T) {
	spectest.Proves(t, feature, "publish-level-3-preview", "the-note-names-the-next-step")
	for name, c := range map[string]struct {
		raw, report, want string
	}{
		"an unknown signal":        {mixedPreview, previewReport, "Next step to level 3: make vanished tests known; [the report](" + previewReport + ") shows how."},
		"every signal known":       {allKnown(1, 0, 0, 0, 0), previewReport, "Next step to level 3: let level 3 hold the merge on these signals; [the report](" + previewReport + ") shows how."},
		"no report URL":            {allKnown(0, 0, 0, 0, 0), "", "Next step to level 3: let level 3 hold the merge on these signals."},
		"a signal partly compared": {allKnown(0, 0, 0, 0, 0)[:len(allKnown(0, 0, 0, 0, 0))-2] + `,"notCompared":["apps/web"]}}`, "", "Next step to level 3: compare vanished tests on every component."},
	} {
		t.Run(name, func(t *testing.T) {
			if line := previewLine(t, Note(pullRequest(), c.report, preview(t, c.raw))); !strings.HasSuffix(line, c.want) {
				t.Fatalf("line\n%s\nwant it to end with\n%s", line, c.want)
			}
		})
	}
}

func TestTheStatusSaysWhatLevel3WouldHaveHeld(t *testing.T) {
	spectest.Proves(t, feature, "publish-level-3-status", "the-status-says-what-level-3-would-have-held")
	o := pullRequest()
	p := preview(t, mixedPreview)
	note := Note(o, previewReport, p)
	r := load(t, "github-level3.json", note)
	var out bytes.Buffer
	diags := Run(context.Background(), Request{Observation: o, Upload: Uploaded, ReportURL: previewReport, Preview: p, Getenv: envOf(githubEnv), Client: &http.Client{Transport: r}, Output: &out})
	r.done()
	if len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}
	base := Description(o)
	for raw, want := range map[string]string{
		allKnown(0, 0, 0, 0, 0): "level 3 would have held nothing",
		allKnown(1, 0, 2, 0, 0): "level 3 would have held it on 2 signals",
		allKnown(0, 0, 0, 0, 1): "level 3 would have held it on 1 signal",
		`{"brokenRules":{"state":"known","count":0},"coverageLost":{"state":"known","count":0},"deletedDecisions":{"state":"known","count":0},"untestedSpecs":{"state":"known","count":0}}`: "no known level 3 signal would have held it, 1 unknown",
		`{}`: "level 3 preview unknown",
		allKnown(0, 0, 0, 0, 2)[:len(allKnown(0, 0, 0, 0, 2))-2] + `,"notCompared":["a"]}}`: "level 3 would have held it on 1 signal",
		allKnown(0, 0, 0, 0, 0)[:len(allKnown(0, 0, 0, 0, 0))-2] + `,"notCompared":["a"]}}`: "level 3 preview: 0 held, 1 partial",
	} {
		if got := StatusDescription(o, preview(t, raw)); got != base+"; "+want {
			t.Errorf("status %q, want %q", got, base+"; "+want)
		}
	}
	// A description that has no room left keeps its own words.
	long := pullRequest()
	long.Analysis.Components = nil
	for i := range 2000 {
		impact := evidence.ImpactChanged
		if i%2 == 1 {
			impact = evidence.ImpactDependent
		}
		long.Analysis.Components = append(long.Analysis.Components, evidence.Component{ID: fmt.Sprint(i), Impact: impact})
	}
	long.Analysis.ChangedLineCoverage = &evidence.ChangedLineCoverage{CoveredLines: 1, TotalLines: 1}
	long.Analysis.DependencyChanges = make([]evidence.DependencyChange, 1000)
	none := preview(t, `{"vanishedTests":{"state":"known","count":0,"notCompared":["a"]}}`)
	want := Description(long)
	if len([]rune(want+"; "+level3Compact(*none))) <= maxDescription {
		t.Fatalf("the long description %q leaves room for the preview", want)
	}
	if got := StatusDescription(long, none); got != want {
		t.Fatalf("status %q, want the description alone %q", got, want)
	}
}

// notCompared lists n components as not compared on vanished tests, whose
// count is vanished, the other signals known and zero.
func notCompared(vanished, n int) string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("%q", fmt.Sprintf("apps/c%d", i))
	}
	full := allKnown(0, 0, 0, 0, vanished)
	return full[:len(full)-2] + `,"notCompared":[` + strings.Join(names, ",") + `]}}`
}

func TestACappedListReadsAtLeast50(t *testing.T) {
	spectest.Proves(t, feature, "publish-level-3-partly-compared", "a-capped-list-reads-at-least-50")
	for n, want := range map[int]string{
		1:                         "partly compared: vanished tests (1 component not compared).",
		level3.MaxNotCompared - 1: "partly compared: vanished tests (49 components not compared).",
		level3.MaxNotCompared:     "partly compared: vanished tests (at least 50 components not compared).",
	} {
		line := previewLine(t, Note(pullRequest(), "", preview(t, notCompared(0, n))))
		if !strings.Contains(line, want) {
			t.Errorf("%d not compared: line\n%s\nwant it to hold\n%s", n, line, want)
		}
		if !strings.Contains(line, "as far as the evidence tells") || strings.Contains(line, "nothing") {
			t.Errorf("%d not compared: a partly compared signal reads as known whole: %s", n, line)
		}
	}
}

func TestTheStatusCountsPartlyComparedSignalsApart(t *testing.T) {
	spectest.Proves(t, feature, "publish-level-3-partly-compared", "the-status-counts-partly-compared-signals-apart")
	o := pullRequest()
	base := Description(o)
	// Broken rules hold the change, lost coverage is unknown and vanished
	// tests are partly compared: three signals, counted apart.
	apart := strings.Replace(notCompared(0, 3), `"brokenRules":{"state":"known","count":0}`, `"brokenRules":{"state":"known","count":1}`, 1)
	apart = strings.Replace(apart, `"coverageLost":{"state":"known","count":0}`, `"coverageLost":{"state":"unknown","reason":"no-coverage"}`, 1)
	for raw, want := range map[string]string{
		notCompared(0, 2): "level 3 preview: 0 held, 1 partial",
		// A partly compared signal that holds the change counts as held:
		// what it holds is known.
		notCompared(2, level3.MaxNotCompared): "level 3 would have held it on 1 signal",
		apart:                                 "level 3 preview: 1 held, 1 unknown, 1 partial",
	} {
		got := StatusDescription(o, preview(t, raw))
		if got != base+"; "+want {
			t.Errorf("status %q, want %q", got, base+"; "+want)
		}
	}
	// With room for it, the status says it in full.
	for raw, want := range map[string]string{
		apart:             "level 3 would have held it on 1 signal, 1 unknown, 1 partly compared",
		notCompared(0, 2): "no known level 3 signal would have held it, 1 partly compared",
	} {
		if got := level3Short(*preview(t, raw)); got != want {
			t.Errorf("short form %q, want %q", got, want)
		}
	}
	if got, want := level3Compact(*preview(t, allKnown(1, 0, 2, 0, 0))), "level 3 preview: 2 held"; got != want {
		t.Errorf("compact form %q, want %q", got, want)
	}
}

func TestWithoutAPreviewTheNoteIsUnchanged(t *testing.T) {
	spectest.Proves(t, feature, "publish-level-3-status", "without-a-preview-the-note-is-unchanged")
	golden, err := os.ReadFile(filepath.Join("testdata", "note-github.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Note(pullRequest(), "", nil); got != string(golden) || strings.Contains(got, "Level 3") {
		t.Fatalf("note\n%s\nwant\n%s", got, golden)
	}
	if got := StatusDescription(pullRequest(), nil); got != Description(pullRequest()) {
		t.Fatalf("status %q, want %q", got, Description(pullRequest()))
	}
	// The status stays success whatever the preview holds: the preview
	// informs and never blocks.
	fixture, err := os.ReadFile(filepath.Join("testdata", "github-level3.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fixture), `"state": "success"`) || strings.Contains(string(fixture), `"state": "failure"`) {
		t.Fatal("the status of a run with a preview is not success")
	}
}
