package inputresolve

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// listingProvider answers each input with the .txt files of its project, as
// the helper's list mode does, in process: after runs once the files are
// listed and before the answer returns, as a writer concurrent with the
// extension does.
type listingProvider struct {
	root  string
	after func(call int)
	// before runs when a call starts.
	before func(call int)
	calls  int
}

func (p *listingProvider) ResolveInputs(request wsproto.InputResolveRequest) (wsproto.InputResolveResult, error) {
	p.calls++
	if p.before != nil {
		p.before(p.calls)
	}
	result := wsproto.InputResolveResult{Version: wsproto.InputResolveProtocolVersion, Extension: request.Extension}
	for _, input := range request.Inputs {
		result.Inputs = append(result.Inputs, wsproto.InputResolveAnswer{
			Project:  input.Project,
			Selector: input.Selector,
			Files:    listText(filepath.Join(p.root, filepath.FromSlash(input.Project))),
			Basis:    wsproto.InputBasis{Content: [][]string{{"**/*.txt"}}},
		})
	}
	if p.after != nil {
		p.after(p.calls)
	}
	return result, nil
}

// An answer that lists a file removed before the resolver digests its basis
// is not kept: its digest describes the tree without the file, so it would
// hold while the list names a file that is gone. The resolver asks again and
// keeps the answer that lists the tree as it is, primed or not.
func TestAnAnswerThatListsAVanishedFileIsAskedAgain(t *testing.T) {
	for _, primed := range []bool{false, true} {
		root, app := selectorWorkspace(t)
		temporary := filepath.Join(app, "stamp-1.tmp.txt")
		writeFile(t, temporary, "{}")
		p := &listingProvider{root: root, after: func(call int) {
			if call == 1 {
				if err := os.Remove(temporary); err != nil {
					t.Fatal(err)
				}
			}
		}}
		resolver := newResolver(root, []*extension.ExtensionDescription{selectorsExtension()}, func(string) provider { return p })
		if primed {
			resolver.PrimeInputs([]store.InputQuery{query(app)})
		}
		got, err := resolver.ResolveInput(query(app))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.Files, []string{"a.txt", "sub/b.txt"}) {
			t.Fatalf("primed=%v: answer = %v, want the files that exist", primed, got.Files)
		}
		if _, err := resolver.ResolveInput(query(app)); err != nil {
			t.Fatal(err)
		}
		if p.calls != 2 {
			t.Fatalf("primed=%v: calls = %d, want 2: the stale answer and the one kept", primed, p.calls)
		}
	}
}

// An extension that lists a file that stays missing is asked a bounded number
// of times, then its answer is kept: the key that reads it reports the file.
func TestAnAnswerThatListsAMissingFileIsKeptAfterBoundedAsks(t *testing.T) {
	root, app := selectorWorkspace(t)
	p := &missingProvider{}
	resolver := newResolver(root, []*extension.ExtensionDescription{selectorsExtension()}, func(string) provider { return p })
	for range 2 {
		got, err := resolver.ResolveInput(query(app))
		if err != nil || !slices.Equal(got.Files, []string{"missing.txt"}) {
			t.Fatalf("answer = %v, %v, want the list the extension gave", got.Files, err)
		}
	}
	if p.calls != staleAnswerAsks {
		t.Fatalf("calls = %d, want %d: bounded asks, then a kept answer", p.calls, staleAnswerAsks)
	}
}

// missingProvider answers each input with one file that does not exist.
type missingProvider struct{ calls int }

func (p *missingProvider) ResolveInputs(request wsproto.InputResolveRequest) (wsproto.InputResolveResult, error) {
	p.calls++
	result := wsproto.InputResolveResult{Version: wsproto.InputResolveProtocolVersion, Extension: request.Extension}
	for _, input := range request.Inputs {
		result.Inputs = append(result.Inputs, wsproto.InputResolveAnswer{
			Project:  input.Project,
			Selector: input.Selector,
			Files:    []string{"missing.txt"},
			Basis:    wsproto.InputBasis{Content: [][]string{{"**/*.txt"}}},
		})
	}
	return result, nil
}

// A path of the basis that is gone when the resolver digests it, here the
// project directory a writer replaces while the extension answers, makes the
// answer stale and not the input invalid: the resolver asks again.
func TestABasisPathGoneAtItsDigestIsAskedAgain(t *testing.T) {
	root, app := selectorWorkspace(t)
	moved := app + ".moved"
	p := &listingProvider{root: root}
	p.after = func(call int) {
		if call == 1 {
			if err := os.Rename(app, moved); err != nil {
				t.Fatal(err)
			}
		}
	}
	p.before = func(call int) {
		if call == 2 {
			if err := os.Rename(moved, app); err != nil {
				t.Fatal(err)
			}
		}
	}
	resolver := newResolver(root, []*extension.ExtensionDescription{selectorsExtension()}, func(string) provider { return p })
	got, err := resolver.ResolveInput(query(app))
	if err != nil {
		t.Fatalf("a basis path gone at its digest failed the input: %v", err)
	}
	if !slices.Equal(got.Files, []string{"a.txt", "sub/b.txt"}) || p.calls != 2 {
		t.Fatalf("answer = %v after %d calls, want the project's files after 2", got.Files, p.calls)
	}
}
