package estimate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Range is a low and a high bound, both included.
type Range struct {
	Low  int `json:"low"`
	High int `json:"high"`
}

func (r Range) add(o Range) Range { return Range{Low: r.Low + o.Low, High: r.High + o.High} }

// Bucket is the cost of a slice whose counted files fall in the bucket. UpTo
// is the most files the bucket takes; the last bucket has none and takes
// every larger count.
type Bucket struct {
	UpTo    *int  `json:"upTo,omitempty"`
	Tokens  Range `json:"tokens"`
	Minutes Range `json:"minutes"`
}

// SizeMinutes are the size thresholds on the high bound of an estimate's
// minutes: S up to S, M up to M, L above.
type SizeMinutes struct {
	S int `json:"S"`
	M int `json:"M"`
}

// Table holds the coefficients of an estimate. A slice counts its files,
// plus EndpointFiles per endpoint and ScreenFiles per screen, and takes the
// tokens and minutes of the first bucket that holds that count.
type Table struct {
	Files         []Bucket    `json:"files"`
	EndpointFiles int         `json:"endpointFiles"`
	ScreenFiles   int         `json:"screenFiles"`
	SizeMinutes   SizeMinutes `json:"sizeMinutes"`
}

func upTo(n int) *int { return &n }

// DefaultTable is fitted on 116 sessions, from 27 September to 27 October
// 2026, that each opened one pull request, merged since. A session's files are
// the pull request's changed files; its tokens are input, cache creation,
// cache read and output tokens, subagents included; its minutes are the time
// between the main session's events, less each wait for an owner message and
// with each gap capped at 30 minutes. Each bucket holds the 25th and the 75th
// percentiles of its sessions. The data counts no endpoint and no screen: the
// allowance per endpoint and per screen is fixed, not fitted.
func DefaultTable() Table {
	return Table{
		Files: []Bucket{
			{UpTo: upTo(5), Tokens: Range{5_600_000, 16_700_000}, Minutes: Range{31, 86}},
			{UpTo: upTo(15), Tokens: Range{17_600_000, 48_500_000}, Minutes: Range{44, 144}},
			{UpTo: upTo(40), Tokens: Range{42_200_000, 132_100_000}, Minutes: Range{79, 220}},
			{Tokens: Range{136_000_000, 365_800_000}, Minutes: Range{195, 388}},
		},
		EndpointFiles: 2,
		ScreenFiles:   3,
		SizeMinutes:   SizeMinutes{S: 90, M: 240},
	}
}

const (
	maxBucketTokens  = 1_000_000_000_000
	maxBucketMinutes = 100_000
)

func rangeRules(max int) []rule {
	return []rule{
		{name: "low", kind: kindInteger, required: true, min: 0, max: max},
		{name: "high", kind: kindInteger, required: true, min: 0, max: max},
	}
}

// tableRules is the estimate member of the contributor policy. Every member
// is optional: a member present replaces the default one whole.
var tableRules = []rule{
	{name: "files", kind: kindObjectList, min: 1, max: 20, fields: []rule{
		{name: "upTo", kind: kindInteger, min: 0, max: 1_000_000},
		{name: "tokens", kind: kindObject, required: true, fields: rangeRules(maxBucketTokens)},
		{name: "minutes", kind: kindObject, required: true, fields: rangeRules(maxBucketMinutes)},
	}},
	{name: "endpointFiles", kind: kindInteger, min: 0, max: 1000},
	{name: "screenFiles", kind: kindInteger, min: 0, max: 1000},
	{name: "sizeMinutes", kind: kindObject, fields: []rule{
		{name: "S", kind: kindInteger, required: true, min: 0, max: maxBucketMinutes},
		{name: "M", kind: kindInteger, required: true, min: 0, max: maxBucketMinutes},
	}},
}

// ReadTable returns the default table with the members that
// options["@putnami/contributor"].estimate in root's putnami.workspace.json
// sets. A workspace without the file or the member uses the default table.
func ReadTable(root string) (Table, error) {
	table := DefaultTable()
	data, err := os.ReadFile(filepath.Join(root, "putnami.workspace.json"))
	if errors.Is(err, os.ErrNotExist) {
		return table, nil
	}
	if err != nil {
		return Table{}, err
	}
	var document struct {
		Options map[string]json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return Table{}, fmt.Errorf("workspace policy: %w", err)
	}
	var policy struct {
		Estimate json.RawMessage `json:"estimate"`
	}
	if raw := document.Options["@putnami/contributor"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &policy); err != nil {
			return Table{}, fmt.Errorf("contributor policy: %w", err)
		}
	}
	if len(policy.Estimate) == 0 {
		return table, nil
	}
	return overrideTable(table, policy.Estimate)
}

// overrideTable replaces the members of table that raw sets, and checks the
// result.
func overrideTable(table Table, raw []byte) (Table, error) {
	const root = "contributor policy estimate"
	value, err := decodeStrict(raw, root)
	if err != nil {
		return Table{}, err
	}
	if err := checkObject(root, value, tableRules); err != nil {
		return Table{}, err
	}
	var set struct {
		Files         []Bucket     `json:"files"`
		EndpointFiles *int         `json:"endpointFiles"`
		ScreenFiles   *int         `json:"screenFiles"`
		SizeMinutes   *SizeMinutes `json:"sizeMinutes"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return Table{}, fmt.Errorf("%s: %w", root, err)
	}
	if set.Files != nil {
		table.Files = set.Files
	}
	if set.EndpointFiles != nil {
		table.EndpointFiles = *set.EndpointFiles
	}
	if set.ScreenFiles != nil {
		table.ScreenFiles = *set.ScreenFiles
	}
	if set.SizeMinutes != nil {
		table.SizeMinutes = *set.SizeMinutes
	}
	if err := table.check(); err != nil {
		return Table{}, fmt.Errorf("%s: %w", root, err)
	}
	return table, nil
}

// check refuses a table whose buckets do not cover every file count in
// ascending order, whose ranges run backwards, or whose S threshold exceeds
// its M threshold.
func (t Table) check() error {
	if len(t.Files) == 0 {
		return errors.New("files: needs at least one bucket")
	}
	previous := -1
	for i, bucket := range t.Files {
		last := i == len(t.Files)-1
		switch {
		case last && bucket.UpTo != nil:
			return fmt.Errorf("files[%d].upTo: the last bucket takes every larger count and has no upTo", i)
		case !last && bucket.UpTo == nil:
			return fmt.Errorf("files[%d].upTo: required on every bucket but the last", i)
		case !last && *bucket.UpTo <= previous:
			return fmt.Errorf("files[%d].upTo: must exceed the upTo of files[%d]", i, i-1)
		}
		if !last {
			previous = *bucket.UpTo
		}
		if bucket.Tokens.Low > bucket.Tokens.High {
			return fmt.Errorf("files[%d].tokens: low exceeds high", i)
		}
		if bucket.Minutes.Low > bucket.Minutes.High {
			return fmt.Errorf("files[%d].minutes: low exceeds high", i)
		}
	}
	if t.SizeMinutes.S > t.SizeMinutes.M {
		return errors.New("sizeMinutes: S exceeds M")
	}
	return nil
}

// bucket returns the bucket that holds a count of files.
func (t Table) bucket(files int) Bucket {
	for _, bucket := range t.Files {
		if bucket.UpTo == nil || files <= *bucket.UpTo {
			return bucket
		}
	}
	return t.Files[len(t.Files)-1]
}
