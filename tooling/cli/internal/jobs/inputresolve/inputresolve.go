// Package inputresolve expands the input selectors a run reads: it asks the
// extension that lists a selector in runtime.inputSelectors, through the
// workspace protocol's input-resolve call, and keeps each answer for the
// length of the process while the digest of its basis holds.
//
// The core knows no selector. An extension answers which files a selector
// reads in a project and the basis it computed them from
// (wsproto.InputBasis); the resolver checks the answer against the request,
// hashes the tree as the basis selects it (wsproto.InputBasisDigest), and asks
// again for one input when that digest changes. An error answer, a failed call
// and a rejected answer fail the input (store.ErrInputSelector), and none is
// kept.
package inputresolve

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// provider answers one input-resolve request for one extension.
// workspace.ExecProbeProvider is the production provider.
type provider interface {
	ResolveInputs(wsproto.InputResolveRequest) (wsproto.InputResolveResult, error)
}

// Resolver answers input queries for a set of extensions. It is safe for
// concurrent use: answers are kept under one lock, and the calls to one
// extension run one at a time, so one extension process answers one request
// at a time and a query that waits for a call reads its answer.
type Resolver struct {
	root string
	// listed maps an extension's name to the selectors its manifest lists.
	listed map[string][]string
	// newProvider builds an extension's provider on its first call.
	newProvider func(name string) provider

	mu        sync.Mutex
	providers map[string]provider
	answers   map[string]kept
	calls     map[string]*sync.Mutex
}

// kept is one answer the resolver keeps, with the digest of its basis when it
// was received.
type kept struct {
	expansion store.InputExpansion
	digest    string
}

// New returns the resolver for the workspace ws and the extensions a run
// loaded. The first extension of a name wins, so a caller lists the
// descriptions its runtime phase prepared first. An extension's runtime is
// prepared on its first call when the run has not prepared it, as a workspace
// probe's provider is, and preparation lands in report, which may be nil. ctx
// scopes the preparation and every call.
func New(ctx context.Context, ws *workspace.Workspace, extensions []*extension.ExtensionDescription, report *scheduler.PreparationReport) *Resolver {
	byName := make(map[string]*extension.ExtensionDescription, len(extensions))
	for _, ext := range extensions {
		if ext != nil && byName[ext.Name] == nil {
			byName[ext.Name] = ext
		}
	}
	return newResolver(ws.Root, extensions, func(name string) provider {
		ext := byName[name]
		return &workspace.ExecProbeProvider{
			Extension:         ext.Name,
			Dir:               ws.Root,
			Context:           ctx,
			Resolve:           runtimeResolver(ctx, ws, ext, report),
			PreparedBeforeRun: !extension.StartsRepositoryCode(ws.Root, ext),
		}
	})
}

// newResolver is New with the provider of each extension built by
// newProvider.
func newResolver(root string, extensions []*extension.ExtensionDescription, newProvider func(string) provider) *Resolver {
	listed := make(map[string][]string, len(extensions))
	for _, ext := range extensions {
		if ext == nil {
			continue
		}
		if _, known := listed[ext.Name]; known {
			continue
		}
		listed[ext.Name] = nil
		if ext.Runtime != nil {
			listed[ext.Name] = slices.Clone(ext.Runtime.InputSelectors)
		}
	}
	return &Resolver{
		root:        root,
		listed:      listed,
		newProvider: newProvider,
		providers:   make(map[string]provider),
		answers:     make(map[string]kept),
		calls:       make(map[string]*sync.Mutex),
	}
}

// runtimeResolver prepares the runtime of ext on first use and returns its
// executable. A run that prepared it in its runtime phase reads the
// executable it recorded. A preparation that failed fails every later call
// with its error, so one broken runtime is prepared once per resolver. The
// calls to one extension run one at a time, so the closure needs no lock.
func runtimeResolver(ctx context.Context, ws *workspace.Workspace, ext *extension.ExtensionDescription, report *scheduler.PreparationReport) func() (string, error) {
	var failed error
	return func() (string, error) {
		if ext.RuntimeExecutable == "" && failed == nil {
			failed = scheduler.SynchronizeExtensionRuntimes(ctx, ws, []*extension.ExtensionDescription{ext}, report)
		}
		if failed != nil {
			return "", failed
		}
		return ext.RuntimeExecutable, nil
	}
}

// ResolveInput answers one query: the answer the resolver keeps when the
// digest of its basis holds, or the extension's answer to a request for this
// input alone. Every failure wraps store.ErrInputSelector, and a nil resolver
// fails every query.
func (r *Resolver) ResolveInput(query store.InputQuery) (store.InputExpansion, error) {
	if r == nil {
		return store.InputExpansion{}, fmt.Errorf("%w: no resolver expands selector %q", store.ErrInputSelector, query.Selector)
	}
	asked, err := r.request(query)
	if err != nil {
		return store.InputExpansion{}, err
	}
	key := answerKey(query.Extension, asked)
	if expansion, ok := r.exact(key, query.Dir); ok {
		return expansion, nil
	}
	call := r.call(query.Extension)
	call.Lock()
	defer call.Unlock()
	// A call that held the lock may have answered this input.
	if expansion, ok := r.exact(key, query.Dir); ok {
		return expansion, nil
	}
	answers, err := r.ask(query.Extension, []wsproto.InputResolveQuery{asked})
	if err != nil {
		return store.InputExpansion{}, fmt.Errorf("%w: %s %s: %w", store.ErrInputSelector, query.Extension, query.Selector, err)
	}
	answer := answers[0]
	if answer.Error != "" {
		return store.InputExpansion{}, fmt.Errorf("%w: %s answers %s in %s: %s",
			store.ErrInputSelector, query.Extension, query.Selector, asked.Project, answer.Error)
	}
	expansion, err := r.keep(key, query.Dir, answer)
	if err != nil {
		return store.InputExpansion{}, fmt.Errorf("%w: %s %s in %s: %w", store.ErrInputSelector, query.Extension, query.Selector, asked.Project, err)
	}
	return expansion, nil
}

// PrimeInputs asks each extension once, in one request, for every query it
// keeps no answer for, and keeps each answer that is not an error. A nil
// resolver asks nothing. An answer
// it keeps is checked when ResolveInput reads it. A query the resolver
// refuses, an error answer and a failed call are left to ResolveInput, which
// reports them for the input that reads them.
func (r *Resolver) PrimeInputs(queries []store.InputQuery) {
	if r == nil {
		return
	}
	byExtension := make(map[string][]pendingInput)
	seen := make(map[string]bool)
	for _, query := range queries {
		asked, err := r.request(query)
		if err != nil {
			continue
		}
		key := answerKey(query.Extension, asked)
		if seen[key] || r.holds(key) {
			continue
		}
		seen[key] = true
		byExtension[query.Extension] = append(byExtension[query.Extension], pendingInput{asked: asked, dir: query.Dir, key: key})
	}
	names := make([]string, 0, len(byExtension))
	for name := range byExtension {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		r.prime(name, byExtension[name])
	}
}

// pendingInput is one input PrimeInputs asks for: the query, its project
// directory and the key its answer is kept at.
type pendingInput struct {
	asked wsproto.InputResolveQuery
	dir   string
	key   string
}

// prime asks one extension, in one request, for each of inputs it keeps no
// answer for once its call lock is held.
func (r *Resolver) prime(name string, inputs []pendingInput) {
	call := r.call(name)
	call.Lock()
	defer call.Unlock()
	var ask []pendingInput
	for _, input := range inputs {
		if !r.holds(input.key) {
			ask = append(ask, input)
		}
	}
	if len(ask) == 0 {
		return
	}
	asked := make([]wsproto.InputResolveQuery, len(ask))
	for i, input := range ask {
		asked[i] = input.asked
	}
	answers, err := r.ask(name, asked)
	if err != nil {
		return
	}
	for i, answer := range answers {
		if answer.Error == "" {
			_, _ = r.keep(ask[i].key, ask[i].dir, answer)
		}
	}
}

// holds reports whether the resolver keeps an answer at key.
func (r *Resolver) holds(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.answers[key]
	return ok
}

// SelectsInput reports whether the expansion of a query reads rel, a slash
// path relative to the query's directory: a file it lists, or a path a set of
// its basis selects, so a path that a write removed or created is still
// answered. A query the resolver cannot answer selects every path.
func (r *Resolver) SelectsInput(query store.InputQuery, rel string) bool {
	expansion, err := r.ResolveInput(query)
	if err != nil {
		return true
	}
	if _, found := slices.BinarySearch(expansion.Files, rel); found {
		return true
	}
	return basisSelects(expansion.Basis, rel)
}

// basisSelects reports whether a set of either kind of basis selects rel.
func basisSelects(basis wsproto.InputBasis, rel string) bool {
	for _, sets := range [][][]string{basis.Content, basis.Listing} {
		for _, set := range sets {
			if wsproto.SelectsPath(rel, set) {
				return true
			}
		}
	}
	return false
}

// request turns a query into the input the resolver asks for, or refuses it:
// an extension that does not list the selector, and a directory outside the
// workspace root.
func (r *Resolver) request(query store.InputQuery) (wsproto.InputResolveQuery, error) {
	if !slices.Contains(r.listed[query.Extension], query.Selector) {
		return wsproto.InputResolveQuery{}, fmt.Errorf("%w: extension %s does not list selector %q in runtime.inputSelectors",
			store.ErrInputSelector, query.Extension, query.Selector)
	}
	rel, err := filepath.Rel(r.root, query.Dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return wsproto.InputResolveQuery{}, fmt.Errorf("%w: project directory %s is outside the workspace %s",
			store.ErrInputSelector, query.Dir, r.root)
	}
	return wsproto.InputResolveQuery{
		Project:  filepath.ToSlash(rel),
		Selector: query.Selector,
		Patterns: slices.Clone(query.Patterns),
	}, nil
}

// answerKey identifies an input by its extension, project, selector and
// pattern set, whose order and repeats carry no meaning.
func answerKey(extensionName string, asked wsproto.InputResolveQuery) string {
	patterns := slices.Clone(asked.Patterns)
	slices.Sort(patterns)
	patterns = slices.Compact(patterns)
	return extensionName + "\x00" + asked.Project + "\x00" + asked.Selector + "\x00" + strings.Join(patterns, "\x00")
}

// exact returns the answer kept at key while the digest of its basis under dir
// holds. An answer whose digest moved, or cannot be computed, is dropped.
func (r *Resolver) exact(key, dir string) (store.InputExpansion, bool) {
	r.mu.Lock()
	answer, ok := r.answers[key]
	r.mu.Unlock()
	if !ok {
		return store.InputExpansion{}, false
	}
	digest, err := wsproto.InputBasisDigest(dir, answer.expansion.Basis)
	if err == nil && digest == answer.digest {
		return answer.expansion, true
	}
	r.mu.Lock()
	if current, ok := r.answers[key]; ok && current.digest == answer.digest {
		delete(r.answers, key)
	}
	r.mu.Unlock()
	return store.InputExpansion{}, false
}

// keep records an answer that is not an error at key with the digest of its
// basis under dir, and returns it as an expansion.
func (r *Resolver) keep(key, dir string, answer wsproto.InputResolveAnswer) (store.InputExpansion, error) {
	expansion := store.InputExpansion{Files: slices.Clone(answer.Files), Basis: answer.Basis}
	digest, err := wsproto.InputBasisDigest(dir, answer.Basis)
	if err != nil {
		return store.InputExpansion{}, fmt.Errorf("digest the answer's basis: %w", err)
	}
	r.mu.Lock()
	r.answers[key] = kept{expansion: expansion, digest: digest}
	r.mu.Unlock()
	return expansion, nil
}

// call returns the lock that runs the calls to one extension one at a time.
func (r *Resolver) call(name string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	call, ok := r.calls[name]
	if !ok {
		call = &sync.Mutex{}
		r.calls[name] = call
	}
	return call
}

// ask sends one request to an extension and returns its answers in request
// order. The caller holds the extension's call lock.
func (r *Resolver) ask(name string, inputs []wsproto.InputResolveQuery) ([]wsproto.InputResolveAnswer, error) {
	r.mu.Lock()
	p, ok := r.providers[name]
	if !ok {
		p = r.newProvider(name)
		r.providers[name] = p
	}
	r.mu.Unlock()
	result, err := p.ResolveInputs(wsproto.InputResolveRequest{
		Version:   wsproto.InputResolveProtocolVersion,
		Extension: name,
		Inputs:    inputs,
	})
	if err != nil {
		return nil, err
	}
	if len(result.Inputs) != len(inputs) {
		return nil, errors.New("the answer does not answer every asked input")
	}
	return result.Inputs, nil
}

// Matcher returns the resolver as the selector matcher change impact reads
// (workspace.InputSelectorMatcher). A nil resolver's matcher selects every
// path for each selector.
func (r *Resolver) Matcher() workspace.InputSelectorMatcher { return matcher{r} }

// matcher adapts a Resolver to workspace.InputSelectorMatcher.
type matcher struct{ r *Resolver }

func (m matcher) SelectsInput(query workspace.InputSelectorQuery, rel string) bool {
	return m.r.SelectsInput(store.InputQuery(query), rel)
}

func (m matcher) PrimeInputs(queries []workspace.InputSelectorQuery) {
	converted := make([]store.InputQuery, len(queries))
	for i, query := range queries {
		converted[i] = store.InputQuery(query)
	}
	m.r.PrimeInputs(converted)
}
