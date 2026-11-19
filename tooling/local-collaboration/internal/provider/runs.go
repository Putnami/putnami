package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strconv"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/tooling/local-collaboration/internal/store"
)

// The missions v2 runs (ADR 0010 of the collaboration protocol). The store
// keeps every run in the missions document. Its one caller is the person who
// works in the workspace, so every run is the caller's and no report is
// refused as run.denied.

// startContent is what an idempotency key binds a start to: the mission's
// repository and key, the kind, the input and the session, never the title.
type startContent struct {
	Repository string             `json:"repository"`
	Key        string             `json:"key"`
	Kind       collab.RunKind     `json:"kind"`
	Input      collab.RunInput    `json:"input"`
	Session    *collab.SessionRef `json:"session,omitempty"`
}

func (p *Provider) startRun(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in, failure := inputOf[*collab.MissionsStartInput](call)
	if failure != nil {
		return nil, failure
	}
	s, _, failure := storeOf(call)
	if failure != nil {
		return nil, failure
	}
	digest := requestDigest(startContent{
		Repository: in.Mission.Repository, Key: in.Mission.Key, Kind: in.Kind, Input: in.Input, Session: in.Session,
	})
	var result *collab.MissionsRunResult
	failure = p.updateMissions(call, s, func(m *store.Missions) error {
		if index := slices.IndexFunc(m.Runs, func(run store.Run) bool { return run.IdempotencyKey == in.IdempotencyKey }); index >= 0 {
			if m.Runs[index].Digest != digest {
				return refuse(collab.Fail(collab.OutcomeConflict, collab.ReasonIdempotencyMismatch,
					"idempotency key %q was already used to start another run", in.IdempotencyKey))
			}
			result = &collab.MissionsRunResult{Run: m.Runs[index].Run, Replayed: true}
			return nil
		}
		m.Next.Run++
		run := store.Run{
			Run: collab.Run{
				ID: "R-" + strconv.Itoa(m.Next.Run), Mission: collab.MissionName{Repository: in.Mission.Repository, Key: in.Mission.Key},
				Kind: in.Kind, State: collab.RunStateRunning, Input: in.Input, Session: in.Session, StartedAt: p.timestamp(),
			},
			IdempotencyKey: in.IdempotencyKey, Digest: digest,
		}
		m.Runs = append(m.Runs, run)
		result = &collab.MissionsRunResult{Run: run.Run}
		return nil
	})
	if failure != nil {
		return nil, failure
	}
	return result, nil
}

func (p *Provider) reportRun(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in, failure := inputOf[*collab.MissionsReportInput](call)
	if failure != nil {
		return nil, failure
	}
	s, _, failure := storeOf(call)
	if failure != nil {
		return nil, failure
	}
	digest := requestDigest(in)
	var result *collab.MissionsRunResult
	failure = p.updateMissions(call, s, func(m *store.Missions) error {
		index := slices.IndexFunc(m.Runs, func(run store.Run) bool { return run.ID == in.Run })
		if index < 0 {
			return refuse(collab.Fail(collab.OutcomeNotFound, collab.ReasonRunNotFound, "no run %s", in.Run))
		}
		run := &m.Runs[index]
		if run.State != collab.RunStateRunning {
			if run.ReportDigest == digest {
				result = &collab.MissionsRunResult{Run: run.Run, Replayed: true}
				return nil
			}
			return refuse(collab.Fail(collab.OutcomeConflict, collab.ReasonRunClosed, "run %s already ended %s", in.Run, run.State))
		}
		if in.Result != nil && in.Result.Kind() != run.Kind {
			return refuse(collab.Fail(collab.OutcomeInvalid, collab.ReasonRunKindMismatch,
				"run %s is a %s run; the result is a %s result", in.Run, run.Kind, in.Result.Kind()))
		}
		ended := p.timestamp()
		if ended < run.StartedAt {
			ended = run.StartedAt
		}
		run.State, run.Result, run.Failure, run.EndedAt, run.ReportDigest = in.State, in.Result, in.Failure, ended, digest
		result = &collab.MissionsRunResult{Run: run.Run}
		return nil
	})
	if failure != nil {
		return nil, failure
	}
	return result, nil
}

func (p *Provider) listRuns(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in, failure := inputOf[*collab.MissionsRunsInput](call)
	if failure != nil {
		return nil, failure
	}
	after, failure := runCursor(in.Page)
	if failure != nil {
		return nil, failure
	}
	s, _, failure := storeOf(call)
	if failure != nil {
		return nil, failure
	}
	m, err := s.ReadMissions()
	if err != nil {
		return nil, collab.Fail(collab.OutcomeUnavailable, "store.unreadable", "%v", err)
	}
	var held []runPosition
	runs := map[string]collab.Run{}
	for index, run := range m.Runs {
		position := positionOfRun(run.Run, index)
		if run.Mission.Repository == in.Mission.Repository && run.Mission.Key == in.Mission.Key &&
			(in.Kind == "" || run.Kind == in.Kind) && (after == nil || after.compare(position) < 0) {
			held = append(held, position)
			runs[run.ID] = run.Run
		}
	}
	slices.SortFunc(held, runPosition.compare)
	result := &collab.MissionsRunsResult{Items: make([]collab.Run, 0, len(held))}
	if size := pageSize(in.Page); len(held) > size {
		held = held[:size]
		result.Page.Next = held[size-1].cursor()
	}
	for _, position := range held {
		result.Items = append(result.Items, runs[position.ID])
	}
	return result, nil
}

// runPosition is a run's place in the runs order: the latest start first
// and, of runs started in the same second of the provider's clock, the last
// started first. Seq is the run's place in the store's start order, which
// only grows: runs are appended and never removed. A cursor names the
// position of a page's last run, so a run started between two pages is
// neither repeated nor makes one skipped.
type runPosition struct {
	StartedAt time.Time `json:"startedAt"`
	Seq       int       `json:"seq"`
	ID        string    `json:"id"`
}

func positionOfRun(run collab.Run, index int) runPosition {
	started, _ := time.Parse(time.RFC3339Nano, run.StartedAt)
	return runPosition{StartedAt: started, Seq: index + 1, ID: run.ID}
}

func (p runPosition) compare(q runPosition) int {
	if c := q.StartedAt.Compare(p.StartedAt); c != 0 {
		return c
	}
	return q.Seq - p.Seq
}

func (p runPosition) cursor() string {
	encoded, _ := json.Marshal(p)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func runCursor(page *collab.PageRequest) (*runPosition, *collab.Failure) {
	if page == nil || page.Cursor == "" {
		return nil, nil
	}
	var position runPosition
	encoded, err := base64.RawURLEncoding.DecodeString(page.Cursor)
	if err == nil {
		err = json.Unmarshal(encoded, &position)
	}
	if err != nil || position.ID == "" || position.Seq < 1 || position.StartedAt.IsZero() {
		return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "cursor %q was not issued by this provider", page.Cursor)
	}
	return &position, nil
}
