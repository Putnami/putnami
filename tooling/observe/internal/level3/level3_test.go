package level3

import (
	"encoding/json"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/observe-upload"

func signal(t *testing.T, p *Preview, name string) Signal {
	t.Helper()
	for _, s := range p.Signals {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no signal %s in %+v", name, p)
	return Signal{}
}

func TestAnUnreadableSignalIsUnknownNotZero(t *testing.T) {
	spectest.Proves(t, feature, "upload-answers-the-level-3-preview", "an-unreadable-signal-is-unknown-not-zero")
	p := Parse(json.RawMessage(`{
		"level": 3,
		"baseBranch": " main\n",
		"brokenRules": {"state": "known", "count": 0},
		"coverageLost": {"state": "unknown", "reason": "no coverage\nof main"},
		"deletedDecisions": {"state": "known", "count": -1},
		"untestedSpecs": {"state": "known"},
		"vanishedTests": "measured"
	}`))
	if p == nil || p.Level != 3 || p.BaseBranch != "main" || len(p.Signals) != len(Names) {
		t.Fatalf("preview %+v", p)
	}
	if s := signal(t, p, "brokenRules"); !s.Known || s.Count != 0 || s.NotCompared != 0 {
		t.Errorf("a known zero is %+v, want known 0", s)
	}
	partly := Parse(json.RawMessage(`{"vanishedTests": {"state": "known", "count": 0, "notCompared": ["apps/web", 7]}}`))
	if s := signal(t, partly, "vanishedTests"); !s.Known || s.NotCompared != 2 || len(partly.Partial()) != 1 {
		t.Errorf("a partly compared signal is %+v", s)
	}
	if s := signal(t, Parse(json.RawMessage(`{"vanishedTests": {"state": "known", "count": 0, "notCompared": "apps/web"}}`)), "vanishedTests"); s.Known {
		t.Errorf("a notCompared that is not a list is %+v, want unknown", s)
	}
	for name, reason := range map[string]string{
		"coverageLost":     "no coverage of main",
		"deletedDecisions": "the reporter answered it known without a count",
		"untestedSpecs":    "the reporter answered it known without a count",
		"vanishedTests":    "the reporter answered it in a form observe does not read",
	} {
		if s := signal(t, p, name); s.Known || s.Count != 0 || s.Reason != reason {
			t.Errorf("%s = %+v, want unknown because %q", name, s, reason)
		}
	}
	if len(p.Held()) != 0 || len(p.Unknown()) != 4 {
		t.Errorf("held %v, unknown %v", p.Held(), p.Unknown())
	}

	// A signal the preview leaves out, or a state other than known, is
	// unknown with a reason, never a zero.
	p = Parse(json.RawMessage(`{"brokenRules": {"state": "pending", "count": 4}}`))
	if s := signal(t, p, "brokenRules"); s.Known || s.Count != 0 || s.Reason != noReason {
		t.Errorf("a pending signal is %+v", s)
	}
	if s := signal(t, p, "vanishedTests"); s.Known || s.Reason != "the reporter answered no state for it" {
		t.Errorf("a missing signal is %+v", s)
	}
	if p.Level != 3 {
		t.Errorf("a preview without a level is of level %d, want 3", p.Level)
	}
	if long := Parse(json.RawMessage(`{"brokenRules": {"state": "unknown", "reason": "` + strings.Repeat("é", 300) + `"}}`)); len([]rune(signal(t, long, "brokenRules").Reason)) != maxReason+3 {
		t.Errorf("a long reason is not bounded")
	}
}

func TestAMalformedPreviewIsDropped(t *testing.T) {
	spectest.Proves(t, feature, "upload-answers-the-level-3-preview", "a-malformed-preview-is-dropped")
	for _, raw := range []string{"", "null", "[]", `"known"`, "3", "{"} {
		if p := Parse(json.RawMessage(raw)); p != nil {
			t.Errorf("Parse(%q) = %+v, want nil", raw, p)
		}
	}
}

func TestThePhrasesNameEachSignal(t *testing.T) {
	cases := map[string][2]string{
		"brokenRules":      {"1 broken rule", "broken rules"},
		"coverageLost":     {"2 components that lost coverage", "lost coverage"},
		"deletedDecisions": {"1 deleted decision", "deleted decisions"},
		"untestedSpecs":    {"2 untested specs", "untested specs"},
		"vanishedTests":    {"1 vanished test", "vanished tests"},
	}
	for i, name := range Names {
		s := Signal{Name: name, Known: true, Count: int64(i%2 + 1)}
		if got := [2]string{s.Phrase(), s.Label()}; got != cases[name] {
			t.Errorf("%s: %v, want %v", name, got, cases[name])
		}
	}
}
