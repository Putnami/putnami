package providertest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
)

// memoryStore is the smallest provider that honors the memory contract: one
// lock around a record list, so a checkpoint compares and writes in one step.
// The memory scenarios must pass against it.
type memoryStore struct {
	mu      sync.Mutex
	records []collab.MemoryRecord
	// writes holds, per record id, the idempotency key and request digest of
	// the write that produced its current revision.
	writes map[string][2]string
}

const memoryStoreTime = "2026-09-24T08:00:00Z"

func newMemoryStore() *memoryStore { return &memoryStore{writes: map[string][2]string{}} }

// handlers is one provider instance over the store. Every call builds a new
// table, as a new provider process would.
func (m *memoryStore) handlers() map[collab.OperationKey]collab.Handler {
	key := func(operation string) collab.OperationKey {
		return collab.OperationKey{Contract: collab.ContractMemory, Version: 1, Operation: operation}
	}
	return map[collab.OperationKey]collab.Handler{
		key(collab.OperationContext):    m.context,
		key(collab.OperationMission):    m.mission,
		key(collab.OperationCheckpoint): m.checkpoint,
	}
}

// missionID names a mission record by a digest of its identity, so the
// reference stays a token however long the identity's members are.
func missionID(identity collab.MemoryIdentity) string {
	sum := sha256.Sum256([]byte(identity.Workspace + "\x00" + identity.Repository + "\x00" + identity.Mission))
	return hex.EncodeToString(sum[:16])
}

func (m *memoryStore) find(id string) int {
	for i, record := range m.records {
		if record.Ref.ID == id {
			return i
		}
	}
	return -1
}

func (m *memoryStore) context(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.MemoryContextInput)
	m.mu.Lock()
	defer m.mu.Unlock()
	var matches []collab.MemoryRecord
	for _, record := range m.records {
		have, want := record.Identity, in.Identity
		if want.Workspace != "" && have.Workspace != want.Workspace || want.Mission != "" && have.Mission != want.Mission {
			continue
		}
		matches = append(matches, record)
	}
	items, next := pageOf(matches, in.Page)
	return &collab.MemoryListResult{Items: items, Page: next}, nil
}

func (m *memoryStore) mission(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.MemoryMissionInput)
	identity := in.Identity
	identity.Mission = in.Mission
	m.mu.Lock()
	defer m.mu.Unlock()
	index := m.find(missionID(identity))
	if index < 0 {
		return nil, collab.Fail(collab.OutcomeNotFound, "mission.missing", "no mission %s", in.Mission)
	}
	return &collab.MemoryRecordResult{Record: m.records[index]}, nil
}

func (m *memoryStore) checkpoint(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.MemoryCheckpointInput)
	identity := in.Identity
	identity.Mission = in.Mission
	id := missionID(identity)
	asked, _ := json.Marshal([]any{in.Title, in.Content, in.Sources, in.Evidence})
	m.mu.Lock()
	defer m.mu.Unlock()
	index := m.find(id)
	if index >= 0 {
		current := m.records[index]
		if write := m.writes[id]; write[0] == in.IdempotencyKey {
			if write[1] != string(asked) {
				return nil, refusal(collab.ReasonIdempotencyMismatch, current.Revision, "the key wrote other content")
			}
			return &collab.MemoryCheckpointResult{Record: current, Replayed: true}, nil
		}
		if in.Precondition.MustNotExist {
			return nil, refusal(collab.ReasonAlreadyExists, current.Revision, "mission %s exists", in.Mission)
		}
		if in.Precondition.ExpectedRevision != current.Revision {
			return nil, refusal(collab.ReasonRevisionConflict, current.Revision, "mission %s is at %s", in.Mission, current.Revision)
		}
	} else if in.Precondition.ExpectedRevision != "" {
		return nil, collab.Fail(collab.OutcomeNotFound, "mission.missing", "no mission %s", in.Mission)
	}
	record := collab.MemoryRecord{
		Ref: collab.Ref{Source: memorySource, ID: id}, Revision: "r1", Kind: collab.MemoryKindMission, Identity: identity,
		Title: in.Title, Content: in.Content, Sources: in.Sources, Evidence: in.Evidence,
		Provenance: collab.Provenance{RecordedAt: memoryStoreTime},
		Freshness:  collab.Freshness{UpdatedAt: memoryStoreTime, RetrievedAt: memoryStoreTime},
	}
	if index >= 0 {
		record.Revision = bump(m.records[index].Revision)
		m.records[index] = record
	} else {
		m.records = append(m.records, record)
	}
	m.writes[id] = [2]string{in.IdempotencyKey, string(asked)}
	return &collab.MemoryCheckpointResult{Record: record}, nil
}

func refusal(reason, current, format string, args ...any) *collab.Failure {
	failure := collab.Fail(collab.OutcomeConflict, reason, format, args...)
	failure.Error.Current = current
	return failure
}

func TestTheMemoryScenariosPassAgainstACorrectProvider(t *testing.T) {
	RunMemory(t, func(t *testing.T) Target {
		target := memoryTarget(t, !strings.Contains(t.Name(), "unknown_mission"))
		store := newMemoryStore()
		target.Handlers = store.handlers()
		target.Workspace = "providertest"
		target.Reopen = func(*testing.T) map[collab.OperationKey]collab.Handler { return store.handlers() }
		return target
	})
}

func TestNormalizeNamesTheServedWorkspace(t *testing.T) {
	got := string(normalize(collab.ContractMemory, collab.OperationCheckpoint,
		`{"mission":"m","idempotencyKey":"k","content":"c","precondition":{"mustNotExist":true}}`, "served"))
	if !strings.Contains(got, `"identity":{"workspace":"served"}`) {
		t.Errorf("a checkpoint normalized to %s", got)
	}
	got = string(normalize(collab.ContractMemory, collab.OperationContext, `{"identity":{"workspace":"named"}}`, "served"))
	if !strings.Contains(got, `"workspace":"named"`) || !strings.Contains(got, `"size":`+strconv.Itoa(collab.DefaultPageSize)) {
		t.Errorf("a context request that names its workspace normalized to %s", got)
	}
}
