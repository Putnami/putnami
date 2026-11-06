// Package codehost owns the code-host vocabulary every Putnami surface shares:
// the slugs that name a code host, the words each host uses for its change
// request and its diff, the public defaults of the hosted slugs, and the
// marker of the note Putnami writes on a change request.
//
// It holds words and defaults only. Talking to a host, its tokens and the
// body of a note belong to the extension or service that publishes.
//
// vocabulary.json is the document every reader is held to: the table below
// and the TypeScript binding @putnami/codehost-protocol are compared with it
// and with fixtures/expectations.json by their tests.
package codehost

import (
	"strconv"
	"strings"
)

// ProtocolVersion is the version of vocabulary.json.
const ProtocolVersion = 1

// Code-host slugs.
const (
	GitHub  = "github"
	GitLab  = "gitlab"
	Generic = "generic"
)

// Slugs returns the closed slug set, in its canonical order.
func Slugs() []string { return []string{GitHub, GitLab, Generic} }

// Valid reports whether slug is one of the code-host slugs, compared
// case-sensitively.
func Valid(slug string) bool {
	switch slug {
	case GitHub, GitLab, Generic:
		return true
	}
	return false
}

// NoteMarker is the first line of the Putnami note on a change request: the
// publisher writes it, and every reader finds Putnami's note by it. Notes
// already published carry this value, so it never changes.
const NoteMarker = "<!-- putnami-observe:summary -->"

// Vocabulary is what one code host calls its things, and where it lives by
// default.
type Vocabulary struct {
	// Slug is the code host's slug.
	Slug string `json:"slug"`
	// Name is the code host's display name.
	Name string `json:"name"`
	// ChangeRequest is the noun of a change request: "pull request" on GitHub.
	ChangeRequest string `json:"changeRequest"`
	// ReferencePrefix comes before a change request's number: "#" on GitHub.
	ReferencePrefix string `json:"referencePrefix"`
	// DiffTab is the label of the tab that shows a change request's diff.
	DiffTab string `json:"diffTab"`
	// DefaultHost is the public host name, "" when the slug has none.
	DefaultHost string `json:"defaultHost,omitempty"`
	// DefaultAPIBase is the public REST API base URL, "" when the slug has
	// none. A self-managed or Enterprise host names its own.
	DefaultAPIBase string `json:"defaultApiBase,omitempty"`
}

var vocabularies = []Vocabulary{
	{
		Slug:            GitHub,
		Name:            "GitHub",
		ChangeRequest:   "pull request",
		ReferencePrefix: "#",
		DiffTab:         "Files changed",
		DefaultHost:     "github.com",
		DefaultAPIBase:  "https://api.github.com",
	},
	{
		Slug:            GitLab,
		Name:            "GitLab",
		ChangeRequest:   "merge request",
		ReferencePrefix: "!",
		DiffTab:         "Changes",
		DefaultHost:     "gitlab.com",
		DefaultAPIBase:  "https://gitlab.com/api/v4",
	},
	{
		Slug:            Generic,
		Name:            "Git host",
		ChangeRequest:   "change request",
		ReferencePrefix: "#",
		DiffTab:         "Changes",
	},
}

// Vocabularies returns the vocabulary of every slug, in the slug order.
func Vocabularies() []Vocabulary {
	return append([]Vocabulary(nil), vocabularies...)
}

// Lookup returns the vocabulary of slug, and false when slug is not a
// code-host slug.
func Lookup(slug string) (Vocabulary, bool) {
	for _, v := range vocabularies {
		if v.Slug == slug {
			return v, true
		}
	}
	return Vocabulary{}, false
}

// For returns the vocabulary to display for slug: its own, or the generic one
// when slug is not a code-host slug.
func For(slug string) Vocabulary {
	if v, ok := Lookup(slug); ok {
		return v
	}
	v, _ := Lookup(Generic)
	return v
}

// ChangeRequestLabel names a change request on the code host slug: the noun,
// a space, the prefix and the number, as in "merge request !12". A number that
// is not positive gives the noun alone.
func ChangeRequestLabel(slug string, number int) string {
	v := For(slug)
	if number <= 0 {
		return v.ChangeRequest
	}
	return v.ChangeRequest + " " + v.ReferencePrefix + strconv.Itoa(number)
}

// IsPutnamiNote reports whether a note body is the Putnami note: after leading
// spaces, tabs, carriage returns and line feeds, it starts with NoteMarker.
func IsPutnamiNote(body string) bool {
	return strings.HasPrefix(strings.TrimLeft(body, " \t\r\n"), NoteMarker)
}
