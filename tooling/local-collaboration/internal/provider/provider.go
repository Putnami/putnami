// Package provider implements the tasks and proposals contracts of
// go.putnami.dev/protocol/collaboration over the local file store.
//
// It is the reference provider: every operation reads or writes one store
// directory inside the workspace, nothing leaves the machine, and what it
// cannot do it says. It records proposals and reviews but runs no hosted
// checks (proposals.status always reports checks unsupported) and offers no
// merge; tasks.assign, tasks.link and tasks.claim are not offered either.
package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/tooling/local-collaboration/internal/store"
)

// DefaultRoot is the store directory, relative to the workspace root, when
// the binding's settings name none.
const DefaultRoot = ".putnami/collaboration/local"

// DefaultRepository is the repository a proposal belongs to when neither the
// request nor the settings name one.
const DefaultRepository = "local"

// Settings is the binding's settings object this provider reads.
type Settings struct {
	// Root is the store directory: absolute, or relative to the workspace root.
	Root string `json:"root,omitempty"`
	// Repository is the repository identity proposals default to.
	Repository string `json:"repository,omitempty"`
}

// Provider answers operations. Now is the clock every timestamp reads.
type Provider struct {
	Now func() time.Time
}

// New returns a provider on the wall clock.
func New() *Provider {
	return &Provider{Now: time.Now}
}

func (p *Provider) timestamp() string {
	return p.Now().UTC().Format(time.RFC3339)
}

// Handlers is the dispatch table collab.Serve routes on.
func (p *Provider) Handlers() map[collab.OperationKey]collab.Handler {
	key := func(contract, operation string) collab.OperationKey {
		return collab.OperationKey{Contract: contract, Version: 1, Operation: operation}
	}
	return map[collab.OperationKey]collab.Handler{
		key(collab.ContractTasks, collab.OperationFind):       p.findTasks,
		key(collab.ContractTasks, collab.OperationGet):        p.getTask,
		key(collab.ContractTasks, collab.OperationCreate):     p.createTask,
		key(collab.ContractTasks, collab.OperationUpdate):     p.updateTask,
		key(collab.ContractTasks, collab.OperationTransition): p.transitionTask,
		key(collab.ContractProposals, collab.OperationFind):   p.findProposals,
		key(collab.ContractProposals, collab.OperationUpsert): p.upsertProposal,
		key(collab.ContractProposals, collab.OperationStatus): p.proposalStatus,
		key(collab.ContractProposals, collab.OperationReview): p.reviewProposal,
	}
}

// inputOf returns a call's validated request document. collab.Serve decodes
// it into the operation's input type, so any other type is a dispatch defect.
func inputOf[T any](call collab.Call) (T, *collab.Failure) {
	input, ok := call.Input.(T)
	if !ok {
		return input, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid,
			"the %s request is a %T", call.OperationKey, call.Input)
	}
	return input, nil
}

// settingsOf decodes the binding's settings strictly: an unknown setting is a
// configuration error, never ignored.
func settingsOf(call collab.Call) (Settings, *collab.Failure) {
	var settings Settings
	if len(bytes.TrimSpace(call.Settings)) == 0 {
		return settings, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(call.Settings))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return settings, collab.Fail(collab.OutcomeInvalid, "settings.invalid",
			"the local provider's settings are root and repository: %v", err)
	}
	return settings, nil
}

// storeOf opens the store a call's settings name.
func storeOf(call collab.Call) (*store.Store, Settings, *collab.Failure) {
	settings, failure := settingsOf(call)
	if failure != nil {
		return nil, settings, failure
	}
	root := settings.Root
	if root == "" {
		root = DefaultRoot
	}
	if !filepath.IsAbs(root) {
		if call.Request.WorkspaceRoot == "" {
			return nil, settings, collab.Fail(collab.OutcomeInvalid, "settings.invalid",
				"a relative store root needs the workspace root, and the request carries none")
		}
		root = filepath.Join(call.Request.WorkspaceRoot, root)
	}
	return store.Open(root), settings, nil
}

// readState reads a store for a read operation.
func readState(s *store.Store) (*store.State, *collab.Failure) {
	state, err := s.Read()
	if err != nil {
		return nil, collab.Fail(collab.OutcomeUnavailable, "store.unreadable", "%v", err)
	}
	return state, nil
}

// update runs a mutation. A failure that left nothing on disk is
// unavailable, and retryable when another writer held the lock too long; one
// whose rename may have landed is unresolved; a refusal the change itself
// returned keeps its outcome.
func update(s *store.Store, change func(*store.State) error) *collab.Failure {
	_, err := s.Update(change)
	if err == nil || errors.Is(err, errNothingToWrite) {
		return nil
	}
	var refusal *refused
	if errors.As(err, &refusal) {
		return refusal.failure
	}
	if errors.Is(err, store.ErrBusy) {
		failure := collab.Fail(collab.OutcomeUnavailable, "store.busy", "%v", err)
		failure.Error.Retryable = true
		return failure
	}
	var write *store.WriteError
	if errors.As(err, &write) && write.Landed {
		return collab.Fail(collab.OutcomeUnresolved, "store.unsynced", "%v", err)
	}
	return collab.Fail(collab.OutcomeUnavailable, "store.unwritable", "%v", err)
}

// errNothingToWrite ends a store change that found its answer already
// written — a replayed idempotency key — so the store writes nothing.
var errNothingToWrite = errors.New("nothing to write")

// refused carries a contract failure out of a store change, which then
// writes nothing.
type refused struct {
	failure *collab.Failure
}

func (r *refused) Error() string { return r.failure.Error.Message }

func refuse(failure *collab.Failure) error { return &refused{failure: failure} }

// foreign reports whether a reference was issued by another store.
func foreign(state *store.State, ref collab.Ref) bool {
	return ref.Source != state.Source()
}

func notFound(what string, ref collab.Ref) *collab.Failure {
	return collab.Fail(collab.OutcomeNotFound, what+".missing", "no %s %s in source %s", what, ref.ID, ref.Source)
}

func conflictOn(current string, format string, args ...any) *collab.Failure {
	failure := collab.Fail(collab.OutcomeConflict, collab.ReasonRevisionConflict, format, args...)
	failure.Error.Current = current
	return failure
}

// nextRevision advances an item revision "r<n>".
func nextRevision(revision string) string {
	n, err := strconv.Atoi(strings.TrimPrefix(revision, "r"))
	if err != nil {
		n = 0
	}
	return "r" + strconv.Itoa(n+1)
}

// ordinal is the number inside an identifier "<letter>-<n>".
func ordinal(id string) int {
	_, number, found := strings.Cut(id, "-")
	if !found {
		return 0
	}
	n, err := strconv.Atoi(number)
	if err != nil {
		return 0
	}
	return n
}

// keyDigest is the store key of an idempotency key used by one operation: a
// key names one write of that operation, so tasks.create and proposals.review
// never share one.
func keyDigest(operation, key string) string {
	sum := sha256.Sum256([]byte(operation + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

// requestDigest identifies what an idempotent request asked for, so a key
// reused with other content is recognized.
func requestDigest(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// page cuts one bounded page after cursor from items already in canonical
// order. The cursor is the ordinal of the last item of the previous page.
func page[T any](items []T, id func(T) string, request *collab.PageRequest) ([]T, collab.Page, *collab.Failure) {
	size := collab.DefaultPageSize
	after := 0
	if request != nil {
		if request.Size > 0 {
			size = request.Size
		}
		if request.Cursor != "" {
			n, err := strconv.Atoi(request.Cursor)
			if err != nil || n < 0 {
				return nil, collab.Page{}, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid,
					"cursor %q was not issued by this provider", request.Cursor)
			}
			after = n
		}
	}
	out := make([]T, 0, size)
	var next collab.Page
	for _, item := range items {
		if ordinal(id(item)) <= after {
			continue
		}
		if len(out) == size {
			next.Next = strconv.Itoa(ordinal(id(out[len(out)-1])))
			break
		}
		out = append(out, item)
	}
	return out, next, nil
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
