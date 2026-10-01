// Package memory implements the memory contract of
// go.putnami.dev/protocol/collaboration over a store backend: a directory
// (package filestore) or a Git branch (package refstore), chosen by the
// binding's settings and never by default.
//
// Every operation behaves the same on both backends. Which records a
// context or search answer holds is decided from each record's identity,
// kind and scope alone (see matches), never from where a backend keeps it. A
// checkpoint is compared and written in one backend step, so of two
// checkpoints against one revision exactly one lands and the other is a
// conflict. Memory is contextual: a record references gate, review and
// qualification records and never states that anything passed.
package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	diag "go.putnami.dev/protocol/diagnostic"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/memory-store/internal/filestore"
	"go.putnami.dev/tooling/memory-store/internal/refstore"
	"go.putnami.dev/tooling/memory-store/internal/store"
)

// Backends a binding chooses between.
const (
	BackendFile = "file"
	BackendGit  = "git"
)

// Default store locations, relative to the workspace root.
const (
	DefaultFilePath = ".putnami/collaboration/memory"
	DefaultGitPath  = ".putnami/collaboration/memory.git"
)

// reconcileCheckpoint is what a caller does after an unresolved checkpoint.
const reconcileCheckpoint = "read the mission with memory.mission; repeat the checkpoint with the same idempotencyKey, which replays a write that landed instead of writing it again"

// Settings is the binding's settings object.
type Settings struct {
	// Backend is "file" or "git". It is required: no backend is chosen by
	// default.
	Backend string `json:"backend"`
	// Path is the store directory (file) or the local repository (git),
	// absolute or relative to the workspace root.
	Path string `json:"path,omitempty"`
	// Remote (git) is the name of a remote of Path, a URL or a path. Without
	// it the records live on Branch of Path.
	Remote string `json:"remote,omitempty"`
	// Branch (git) holds the records.
	Branch string `json:"branch,omitempty"`
	// Repository is the repository identity a request that names none
	// belongs to.
	Repository string `json:"repository,omitempty"`
}

// Provider answers memory operations. Now is the clock every timestamp reads.
type Provider struct {
	Now func() time.Time

	// lockTimeout, when set, bounds the file backend's wait for its lock.
	lockTimeout time.Duration
	// gitIntercept, when set, runs every git command of the Git backend.
	gitIntercept refstore.Intercept
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
	key := func(operation string) collab.OperationKey {
		return collab.OperationKey{Contract: collab.ContractMemory, Version: 1, Operation: operation}
	}
	return map[collab.OperationKey]collab.Handler{
		key(collab.OperationContext):    p.context,
		key(collab.OperationMission):    p.mission,
		key(collab.OperationCheckpoint): p.checkpoint,
		key(collab.OperationSearch):     p.search,
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

func (p *Provider) context(ctx context.Context, call collab.Call) (any, *collab.Failure) {
	in, failure := inputOf[*collab.MemoryContextInput](call)
	if failure != nil {
		return nil, failure
	}
	return p.list(ctx, call, query{identity: in.Identity, kinds: in.Kinds}, in.Page)
}

func (p *Provider) search(ctx context.Context, call collab.Call) (any, *collab.Failure) {
	in, failure := inputOf[*collab.MemorySearchInput](call)
	if failure != nil {
		return nil, failure
	}
	return p.list(ctx, call, query{identity: in.Identity, kinds: in.Kinds, text: in.Query}, in.Page)
}

func (p *Provider) list(ctx context.Context, call collab.Call, q query, page *collab.PageRequest) (any, *collab.Failure) {
	backend, settings, failure := p.backend(call)
	if failure != nil {
		return nil, failure
	}
	q.identity = settings.defaults(q.identity)
	q.selection = call.Request.Selection
	size, after, failure := pageOf(page)
	if failure != nil {
		return nil, failure
	}
	view, err := backend.Read(ctx)
	if err != nil {
		return nil, readFailure(err)
	}
	docs, err := view.List(ctx)
	if err != nil {
		return nil, readFailure(err)
	}
	if len(docs) > 0 && view.StoreID() == "" {
		return nil, collab.Fail(collab.OutcomeUnavailable, "store.invalid",
			"the memory store holds records but no %s identity document", store.MetaPath)
	}
	matched := make([]*store.Document, 0, len(docs))
	for _, doc := range docs {
		if failure := checkDocument(doc); failure != nil {
			return nil, failure
		}
		if q.matches(doc) && cursorKey(doc) > after {
			matched = append(matched, doc)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return cursorKey(matched[i]) < cursorKey(matched[j]) })
	result := &collab.MemoryListResult{Items: make([]collab.MemoryRecord, 0, min(size, len(matched)))}
	if len(matched) > size {
		matched = matched[:size]
		result.Page.Next = cursorKey(matched[size-1])
	}
	retrieved := p.timestamp()
	for _, doc := range matched {
		result.Items = append(result.Items, recordOf(backend.Kind(), view.StoreID(), doc, retrieved))
	}
	return result, nil
}

func (p *Provider) mission(ctx context.Context, call collab.Call) (any, *collab.Failure) {
	in, failure := inputOf[*collab.MemoryMissionInput](call)
	if failure != nil {
		return nil, failure
	}
	backend, settings, failure := p.backend(call)
	if failure != nil {
		return nil, failure
	}
	identity := settings.defaults(in.Identity)
	identity.Mission = in.Mission
	view, err := backend.Read(ctx)
	if err != nil {
		return nil, readFailure(err)
	}
	doc, err := view.Get(ctx, MissionID(identity))
	if err != nil {
		return nil, readFailure(err)
	}
	if doc == nil || !(query{identity: identity}).matches(doc) {
		return nil, collab.Fail(collab.OutcomeNotFound, "mission.missing", "no memory records mission %q here", in.Mission)
	}
	if failure := checkDocument(doc); failure != nil {
		return nil, failure
	}
	return &collab.MemoryRecordResult{Record: recordOf(backend.Kind(), view.StoreID(), doc, p.timestamp())}, nil
}

func (p *Provider) checkpoint(ctx context.Context, call collab.Call) (any, *collab.Failure) {
	in, failure := inputOf[*collab.MemoryCheckpointInput](call)
	if failure != nil {
		return nil, failure
	}
	backend, settings, failure := p.backend(call)
	if failure != nil {
		return nil, failure
	}
	identity := settings.defaults(in.Identity)
	identity.Mission = in.Mission
	id := MissionID(identity)
	write := &store.Write{Key: digest(in.IdempotencyKey), Request: digest(checkpointRequest{
		Identity: identity, Title: in.Title, Content: in.Content, Sources: in.Sources, Evidence: in.Evidence,
	})}
	now := p.timestamp()
	recordedBy := agentOf(call.Request.Agent)
	var replayed *store.Document
	var replayedStore string
	written, storeID, err := backend.Update(ctx, id, func(storeID string, current *store.Document) (*store.Document, error) {
		if current != nil && current.Write != nil && current.Write.Key == write.Key {
			if current.Write.Request != write.Request {
				return nil, refuse(conflict(collab.ReasonIdempotencyMismatch, current.Revision(),
					"idempotency key %q already produced another checkpoint of mission %q", in.IdempotencyKey, in.Mission))
			}
			replayed, replayedStore = current, storeID
			return nil, errReplayed
		}
		switch expected := in.Precondition.ExpectedRevision; {
		case in.Precondition.MustNotExist && current != nil:
			return nil, refuse(conflict(collab.ReasonAlreadyExists, current.Revision(),
				"mission %q already has memory; checkpoint it against its revision", in.Mission))
		case expected != "" && current == nil:
			return nil, refuse(collab.Fail(collab.OutcomeNotFound, "mission.missing",
				"mission %q has no memory yet; create it with mustNotExist", in.Mission))
		case expected != "" && current.Revision() != expected:
			return nil, refuse(conflict(collab.ReasonRevisionConflict, current.Revision(),
				"mission %q is at another revision than %s", in.Mission, expected))
		}
		next := &store.Document{
			Format: store.FormatVersion, ID: id, Kind: collab.MemoryKindMission, Identity: identity, Sequence: 1,
			Title: in.Title, Content: in.Content, Sources: in.Sources, Evidence: in.Evidence,
			Provenance: collab.Provenance{RecordedAt: now, RecordedBy: recordedBy}, UpdatedAt: now, Write: write,
		}
		if current != nil {
			next.Sequence = current.Sequence + 1
			if next.Title == "" {
				next.Title = current.Title
			}
			if next.Identity.Scope == "" {
				next.Identity.Scope = current.Identity.Scope
			}
		}
		return next, nil
	})
	switch {
	case errors.Is(err, errReplayed):
		return &collab.MemoryCheckpointResult{Record: recordOf(backend.Kind(), replayedStore, replayed, now), Replayed: true}, nil
	case err != nil:
		return nil, writeFailure(err)
	}
	return &collab.MemoryCheckpointResult{Record: recordOf(backend.Kind(), storeID, written, now)}, nil
}

// checkpointRequest is what a checkpoint asks for, digested so a repeat of
// its idempotency key with other content is recognized. The precondition is
// not part of it: a repeat after a lost answer names the same write whatever
// revision the caller read since.
type checkpointRequest struct {
	Identity collab.MemoryIdentity `json:"identity"`
	Title    string                `json:"title"`
	Content  string                `json:"content"`
	Sources  []collab.Ref          `json:"sources"`
	Evidence []collab.Evidence     `json:"evidence"`
}

// errReplayed ends a checkpoint whose idempotency key produced the current
// revision: nothing is written again.
var errReplayed = errors.New("the checkpoint was already written")

// refused carries a contract failure out of a backend change, which then
// writes nothing.
type refused struct{ failure *collab.Failure }

func (r *refused) Error() string { return r.failure.Error.Message }

func refuse(failure *collab.Failure) error { return &refused{failure: failure} }

func conflict(reason, current, format string, args ...any) *collab.Failure {
	failure := collab.Fail(collab.OutcomeConflict, reason, format, args...)
	failure.Error.Current = current
	return failure
}

// readFailure renders a backend failure of a read, which never answers
// unresolved: nothing was written.
func readFailure(err error) *collab.Failure {
	var backend *store.Error
	if !errors.As(err, &backend) {
		return collab.Fail(collab.OutcomeUnavailable, "store.unreadable", "%v", err)
	}
	if backend.Outcome == store.Invalid {
		return collab.Fail(collab.OutcomeInvalid, backend.Reason, "%v", backend.Err)
	}
	failure := collab.Fail(collab.OutcomeUnavailable, backend.Reason, "%v", backend.Err)
	failure.Error.Retryable = backend.Retryable
	return failure
}

// writeFailure renders the failure of a checkpoint. A write whose outcome
// the backend cannot establish is unresolved and never retryable.
func writeFailure(err error) *collab.Failure {
	var refusal *refused
	if errors.As(err, &refusal) {
		return refusal.failure
	}
	var backend *store.Error
	if !errors.As(err, &backend) {
		failure := collab.Fail(collab.OutcomeUnresolved, "store.unknown", "%v", err)
		failure.Error.Reconcile = reconcileCheckpoint
		return failure
	}
	outcome := map[store.Outcome]collab.Outcome{
		store.Unavailable: collab.OutcomeUnavailable,
		store.Unresolved:  collab.OutcomeUnresolved,
		store.Denied:      collab.OutcomeDenied,
		store.Invalid:     collab.OutcomeInvalid,
	}[backend.Outcome]
	failure := collab.Fail(outcome, backend.Reason, "%v", backend.Err)
	switch outcome {
	case collab.OutcomeUnresolved:
		failure.Error.Reconcile = reconcileCheckpoint
	case collab.OutcomeUnavailable:
		failure.Error.Retryable = backend.Retryable
	}
	return failure
}

// MissionID is the record id of a mission: derived from the workspace,
// repository and mission it belongs to, so every backend finds a mission at
// the same id and two repositories sharing a store never share a mission.
func MissionID(identity collab.MemoryIdentity) string {
	sum := sha256.Sum256([]byte(identity.Workspace + "\x00" + identity.Repository + "\x00" + identity.Mission))
	return "m-" + hex.EncodeToString(sum[:16])
}

func digest(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		encoded = []byte(fmt.Sprint(value))
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

var tokenPattern = regexp.MustCompile(`^[[:graph:]]{1,200}$`)

func agentOf(agent *extension.AgentIdentity) string {
	if agent == nil || !tokenPattern.MatchString(agent.ClientName) {
		return ""
	}
	return agent.ClientName
}

// recordOf renders a stored document as a contract record.
func recordOf(kind, storeID string, doc *store.Document, retrieved string) collab.MemoryRecord {
	return collab.MemoryRecord{
		Ref:        collab.Ref{Source: kind + ":" + storeID, ID: doc.ID},
		Revision:   doc.Revision(),
		Kind:       doc.Kind,
		Identity:   doc.Identity,
		Title:      doc.Title,
		Content:    doc.Content,
		Sources:    doc.Sources,
		Evidence:   doc.Evidence,
		Provenance: doc.Provenance,
		Freshness:  collab.Freshness{UpdatedAt: doc.UpdatedAt, RetrievedAt: retrieved},
	}
}

// checkDocument refuses a stored document the contract would refuse, or a
// mission stored under an id its identity does not derive, so one bad record
// is named rather than failing the answer that carries it.
func checkDocument(doc *store.Document) *collab.Failure {
	record := recordOf("store", "checked", doc, "1970-01-01T00:00:00Z")
	if diags := collab.ValidateResult(&collab.MemoryRecordResult{Record: record}); diag.HasErrors(diags) {
		return collab.Fail(collab.OutcomeUnavailable, "store.invalid", "memory record %s is invalid: %s", doc.ID, collab.FormatDiagnostics(diags))
	}
	if doc.Kind == collab.MemoryKindMission && doc.ID != MissionID(doc.Identity) {
		return collab.Fail(collab.OutcomeUnavailable, "store.invalid", "memory record %s holds a mission its identity does not name", doc.ID)
	}
	return nil
}

// query is what a context or search request selects.
type query struct {
	identity  collab.MemoryIdentity
	kinds     []collab.MemoryKind
	text      string
	selection *extension.ToolSelection
}

// matches is the context selection policy. A record is selected when:
//
//   - every identity member the request names is the record's, or the record
//     does not name that member (a record without a repository applies to
//     every repository, one without a mission to every mission);
//   - its scope and the requested scope overlap, when both name one;
//   - its kind is requested, when the request names kinds;
//   - for a narrowed selection (the orchestrator's resolved projects or
//     impacted), it names no scope or its scope overlaps a selected project;
//   - for a search, its title or content contains the query, ignoring case.
//
// Scopes and project ids compare as '/'-separated paths: two overlap when one
// contains the other, so a record about /tooling is selected for
// /tooling/cli, and one about /tooling/cli for /tooling.
func (q query) matches(doc *store.Document) bool {
	have, want := doc.Identity, q.identity
	for _, member := range [][2]string{
		{have.Workspace, want.Workspace}, {have.Repository, want.Repository}, {have.Mission, want.Mission},
	} {
		if member[0] != "" && member[1] != "" && member[0] != member[1] {
			return false
		}
	}
	if have.Scope != "" && want.Scope != "" && !overlap(have.Scope, want.Scope) {
		return false
	}
	if len(q.kinds) > 0 && !slices.Contains(q.kinds, doc.Kind) {
		return false
	}
	if q.selection != nil && q.selection.Scoped && have.Scope != "" &&
		!slices.ContainsFunc(q.selection.ProjectIDs, func(project string) bool { return overlap(have.Scope, project) }) {
		return false
	}
	if q.text != "" {
		needle := strings.ToLower(q.text)
		return strings.Contains(strings.ToLower(doc.Title), needle) || strings.Contains(strings.ToLower(doc.Content), needle)
	}
	return true
}

func overlap(a, b string) bool { return within(a, b) || within(b, a) }

// within reports whether inner lies in outer ("/" contains everything).
func within(outer, inner string) bool {
	outer, inner = strings.TrimRight(outer, "/"), strings.TrimRight(inner, "/")
	return outer == "" || inner == outer || strings.HasPrefix(inner, outer+"/")
}

// cursorKey is a record's place in the one order every answer uses: kind,
// then id. A cursor is the key of the last record of the previous page, so a
// traversal keeps its order whatever is written between two pages.
func cursorKey(doc *store.Document) string { return string(doc.Kind) + "/" + doc.ID }

func pageOf(page *collab.PageRequest) (int, string, *collab.Failure) {
	size := collab.DefaultPageSize
	if page == nil {
		return size, "", nil
	}
	if page.Size > 0 {
		size = page.Size
	}
	if page.Cursor == "" {
		return size, "", nil
	}
	kind, id, found := strings.Cut(page.Cursor, "/")
	if !found || !slices.Contains(collab.ValidMemoryKinds, collab.MemoryKind(kind)) || !store.ValidID(id) {
		return 0, "", collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "cursor %q was not issued by this provider", page.Cursor)
	}
	return size, page.Cursor, nil
}

// defaults applies the configured repository to an identity that names none.
func (s Settings) defaults(identity collab.MemoryIdentity) collab.MemoryIdentity {
	if identity.Repository == "" {
		identity.Repository = s.Repository
	}
	return identity
}

var branchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)

// settingsOf decodes the binding's settings strictly: an unknown setting is a
// configuration error, never ignored.
func settingsOf(call collab.Call) (Settings, *collab.Failure) {
	var settings Settings
	if len(bytes.TrimSpace(call.Settings)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(call.Settings))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&settings); err != nil {
			return settings, invalidSettings("the memory store's settings are backend, path, remote, branch and repository: %v", err)
		}
	}
	switch settings.Backend {
	case BackendFile:
		if settings.Remote != "" || settings.Branch != "" {
			return settings, invalidSettings("remote and branch are settings of the git backend")
		}
	case BackendGit:
		if failure := checkRemote(settings.Remote); failure != nil {
			return settings, failure
		}
		if settings.Branch != "" && !validBranch(settings.Branch) {
			return settings, invalidSettings("branch %q is not a branch name", settings.Branch)
		}
	case "":
		return settings, invalidSettings("settings.backend is required: file or git; no backend is chosen by default")
	default:
		return settings, invalidSettings("unknown backend %q: file or git", settings.Backend)
	}
	if settings.Repository != "" && !tokenPattern.MatchString(settings.Repository) {
		return settings, invalidSettings("repository %q is not a token", settings.Repository)
	}
	return settings, nil
}

func validBranch(branch string) bool {
	return branchPattern.MatchString(branch) && !strings.Contains(branch, "..") && !strings.Contains(branch, "//") &&
		!strings.HasSuffix(branch, "/") && !strings.HasSuffix(branch, ".") && !strings.HasSuffix(branch, ".lock")
}

// checkRemote refuses a remote that could be read as an option or that
// carries credentials: Git's credential helpers and SSH agent authenticate,
// and a URL with a password, an HTTP user, a query or a fragment would
// publish it in the binding.
func checkRemote(remote string) *collab.Failure {
	switch {
	case remote == "":
		return nil
	case strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, " \t\r\n"):
		return invalidSettings("remote %q is not a remote name, URL or path", remote)
	}
	parsed, err := url.Parse(remote)
	if err != nil || parsed.Scheme == "" {
		return nil
	}
	if strings.ContainsAny(remote, "?#") {
		return invalidSettings("the remote URL carries a query or a fragment, which can hold a credential; name the repository without one")
	}
	if parsed.User == nil {
		return nil
	}
	if _, hasPassword := parsed.User.Password(); hasPassword || parsed.Scheme == "http" || parsed.Scheme == "https" {
		return invalidSettings("the remote URL carries credentials; configure a Git credential helper or an SSH key instead")
	}
	return nil
}

func invalidSettings(format string, args ...any) *collab.Failure {
	return collab.Fail(collab.OutcomeInvalid, "settings.invalid", format, args...)
}

// backend opens the store a call's settings name.
func (p *Provider) backend(call collab.Call) (store.Backend, Settings, *collab.Failure) {
	settings, failure := settingsOf(call)
	if failure != nil {
		return nil, settings, failure
	}
	root := call.Request.WorkspaceRoot
	resolve := func(path, fallback string) (string, *collab.Failure) {
		if path == "" {
			path = fallback
		}
		if filepath.IsAbs(path) {
			return filepath.Clean(path), nil
		}
		if root == "" || !filepath.IsAbs(root) {
			return "", invalidSettings("a relative store path needs the workspace root, and the request carries none")
		}
		return filepath.Join(root, path), nil
	}
	if settings.Backend == BackendFile {
		path, failure := resolve(settings.Path, DefaultFilePath)
		if failure != nil {
			return nil, settings, failure
		}
		backend := filestore.Open(path)
		if p.lockTimeout > 0 {
			backend.LockTimeout = p.lockTimeout
		}
		return backend, settings, nil
	}
	path, failure := resolve(settings.Path, DefaultGitPath)
	if failure != nil {
		return nil, settings, failure
	}
	remote := settings.Remote
	if strings.HasPrefix(remote, "./") || strings.HasPrefix(remote, "../") {
		if remote, failure = resolve(remote, ""); failure != nil {
			return nil, settings, failure
		}
	}
	return refstore.Open(refstore.Config{
		Path: path, Remote: remote, Branch: settings.Branch, Now: p.Now, Intercept: p.gitIntercept,
	}), settings, nil
}
