// Package level3 reads the level 3 preview an evidence reporter answers
// beside its report URL: what level 3 would have held on the change, signal
// by signal. A signal is known, with its count, or unknown, with its reason;
// an unknown signal never reads as zero. The preview informs and never
// blocks.
package level3

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

const (
	// Known is the state of a signal the evidence measured.
	Known = "known"
	// maxReason bounds the reason observe keeps for one signal.
	maxReason = 200
	// noReason is the reason of an unknown signal that gave none.
	noReason = "the reporter gave no reason"
	// MaxNotCompared is the most components Delivery lists as not compared
	// for one signal: a list that long may have been cut.
	MaxNotCompared = 50
)

// Signal is one signal of the preview.
type Signal struct {
	// Name is the signal's member in the preview, such as brokenRules.
	Name string
	// Known says whether the evidence measured it.
	Known bool
	// Count is what level 3 would have held on a known signal; 0 when the
	// signal is unknown.
	Count int64
	// Reason says why an unknown signal is unknown, on one line.
	Reason string
	// NotCompared counts the components a known signal could not compare,
	// so its count holds for the others only.
	NotCompared int
}

// NotComparedCut says whether the reporter may have cut the list of
// components the signal did not compare: it lists MaxNotCompared or more.
func (s Signal) NotComparedCut() bool {
	return s.NotCompared >= MaxNotCompared
}

// Preview is the level 3 preview of one change.
type Preview struct {
	// Level is the level the preview is of.
	Level int64
	// BaseBranch is the branch the change was compared with, "" when the
	// preview names none.
	BaseBranch string
	// Signals are the signals, in the order of Names, each known or unknown.
	Signals []Signal
}

// Names are the signals of a preview, in the order a note lists them.
var Names = []string{"brokenRules", "coverageLost", "deletedDecisions", "untestedSpecs", "vanishedTests"}

// Parse reads the level3Preview member of a reporter's result. It returns nil
// when raw is absent, null or not a JSON object. A signal that is missing,
// not an object, in a state other than Known, or whose count is not a
// non-negative integer is unknown, with its reason or one that says why.
func Parse(raw json.RawMessage) *Preview {
	var members map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &members) != nil || members == nil {
		return nil
	}
	p := &Preview{Level: 3}
	var level float64
	if json.Unmarshal(members["level"], &level) == nil && level >= 1 && level <= 9 && level == math.Trunc(level) {
		p.Level = int64(level)
	}
	var base string
	if json.Unmarshal(members["baseBranch"], &base) == nil {
		p.BaseBranch = oneLine(base)
	}
	for _, name := range Names {
		p.Signals = append(p.Signals, parseSignal(name, members[name]))
	}
	return p
}

func parseSignal(name string, raw json.RawMessage) Signal {
	s := Signal{Name: name}
	var member struct {
		State  *string          `json:"state"`
		Count  *json.RawMessage `json:"count"`
		Reason *string          `json:"reason"`
		// NotCompared lists the components a known signal could not
		// compare; an entry of another type still counts.
		NotCompared []json.RawMessage `json:"notCompared"`
	}
	if len(raw) == 0 || string(raw) == "null" {
		s.Reason = "the reporter answered no state for it"
		return s
	}
	if json.Unmarshal(raw, &member) != nil {
		s.Reason = "the reporter answered it in a form observe does not read"
		return s
	}
	reason := ""
	if member.Reason != nil {
		reason = oneLine(*member.Reason)
	}
	state := ""
	if member.State != nil {
		state = strings.TrimSpace(*member.State)
	}
	if state != Known {
		s.Reason = reason
		if s.Reason == "" {
			s.Reason = noReason
		}
		return s
	}
	var count float64
	if member.Count == nil || json.Unmarshal(*member.Count, &count) != nil || count < 0 || count != math.Trunc(count) || count > math.MaxInt32 {
		s.Reason = "the reporter answered it known without a count"
		return s
	}
	s.Known, s.Count, s.NotCompared = true, int64(count), len(member.NotCompared)
	return s
}

// Held returns the known signals that count above zero.
func (p Preview) Held() []Signal {
	var held []Signal
	for _, s := range p.Signals {
		if s.Known && s.Count > 0 {
			held = append(held, s)
		}
	}
	return held
}

// Unknown returns the signals the evidence did not measure.
func (p Preview) Unknown() []Signal {
	var unknown []Signal
	for _, s := range p.Signals {
		if !s.Known {
			unknown = append(unknown, s)
		}
	}
	return unknown
}

// Partial returns the known signals that could not compare every
// component: their count holds for the compared ones only.
func (p Preview) Partial() []Signal {
	var partial []Signal
	for _, s := range p.Signals {
		if s.Known && s.NotCompared > 0 {
			partial = append(partial, s)
		}
	}
	return partial
}

// Phrase names what a known signal would have held: 1 broken rule,
// 2 components that lost coverage.
func (s Signal) Phrase() string {
	singular, plural := nouns(s.Name)
	if s.Count == 1 {
		return "1 " + singular
	}
	return strconv.FormatInt(s.Count, 10) + " " + plural
}

// Label names the signal itself: broken rules, lost coverage.
func (s Signal) Label() string {
	switch s.Name {
	case "brokenRules":
		return "broken rules"
	case "coverageLost":
		return "lost coverage"
	case "deletedDecisions":
		return "deleted decisions"
	case "untestedSpecs":
		return "untested specs"
	case "vanishedTests":
		return "vanished tests"
	}
	return s.Name
}

func nouns(name string) (singular, plural string) {
	switch name {
	case "brokenRules":
		return "broken rule", "broken rules"
	case "coverageLost":
		return "component that lost coverage", "components that lost coverage"
	case "deletedDecisions":
		return "deleted decision", "deleted decisions"
	case "untestedSpecs":
		return "untested spec", "untested specs"
	case "vanishedTests":
		return "vanished test", "vanished tests"
	}
	return name, name
}

// oneLine keeps text on one line, at most maxReason runes.
func oneLine(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > maxReason {
		text = string(runes[:maxReason]) + "..."
	}
	return text
}
