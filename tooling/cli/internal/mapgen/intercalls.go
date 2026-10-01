package mapgen

import (
	"sort"
	"strings"
	"unicode"
)

// Intercall derivation: the map's RUNTIME topology.
//
// `dependsOn` is a BUILD edge — it says a project compiles against another. It
// cannot answer "who calls the auth server at runtime", because a service
// addresses another service through CONFIGURATION, not through a module graph.
// That address is already in the map: a URL-shaped config key (`…Url`,
// `…Endpoint`, `…jwks`) names the callee in its own key path. This file turns
// those keys into edges.
//
// The derivation is a HEURISTIC, and it is built to fail closed. Every step is
// allowed to give up, and giving up emits nothing:
//
//   - a candidate callee must already look like a runtime unit in the map
//     (an endpoint, a config key, or type "application"), so a library that
//     merely shares a word with a key is never a target;
//   - an alias two projects both claim at the same tier is AMBIGUOUS and is
//     dropped rather than resolved to whichever one was seen first;
//   - a hint that resolves to the project itself is skipped, because a config
//     root named for its own project is the common shape (`auth.baseUrl` in
//     auth-server is not a call);
//   - a key whose every hint fails to resolve contributes no edge, silently.
//
// Absence in this section therefore means "not derivable from config", never
// "no call". That asymmetry is deliberate: an invented edge would be read as a
// fact, while a missing one costs a reader a grep they were going to do anyway.
//
// Everything here is a pure function of the reduced entries, so it inherits the
// package's determinism contract: sorted evidence, sorted targets, no map
// iteration order in any output.

// projectTypeApplication is the declared type that, on its own, makes a project
// an addressable intercall target. It is one disjunct of three because most
// projects in a real workspace leave the type unset — using it alone would index
// almost nothing.
const projectTypeApplication = "application"

// roleWords are the trailing words a project name uses to say WHAT KIND of unit
// it is rather than to name it. Stripping one yields the stem operators
// actually type into a config key: `authServerUrl`, yes, but far more often just
// `auth.…`.
var roleWords = map[string]bool{
	"server":  true,
	"service": true,
	"gateway": true,
	"api":     true,
	"worker":  true,
	"daemon":  true,
}

// urlWords are the trailing leaf words that make a config key URL-SHAPED — the
// only keys that participate at all. A key that does not carry an address
// cannot name a callee.
var urlWords = map[string]bool{
	"url":      true,
	"uri":      true,
	"endpoint": true,
	"address":  true,
	"addr":     true,
	"origin":   true,
	"host":     true,
}

// leafNoiseWords are stripped from the END of a URL-shaped leaf to leave the
// part that might name a callee: `configServerUrl` → `configserver`,
// `ingestBaseUrl` → `ingest`, `jwksUrl` → nothing at all (which is correct — the
// callee of a JWKS URL is named by an ancestor segment, not by the leaf).
var leafNoiseWords = map[string]bool{
	"url":      true,
	"uri":      true,
	"endpoint": true,
	"address":  true,
	"addr":     true,
	"origin":   true,
	"host":     true,
	"jwks":     true,
	"base":     true,
}

// linkIntercalls derives every project's outbound intercalls from its config
// keys and fills the reverse CalledBy edge, in place.
//
// It runs over the REDUCED entries because both halves are properties of the
// whole live set: resolution needs the other projects' identities, and the
// reverse edge needs the forward ones. A fragment cannot carry either, which is
// why this needs no fragment format change.
func linkIntercalls(entries []ProjectEntry) {
	index := buildAliasIndex(entries)
	for i := range entries {
		entries[i].Intercalls = deriveIntercalls(index, entries[i])
	}

	byID := make(map[string]int, len(entries))
	for i := range entries {
		byID[entries[i].ID] = i
	}
	for i := range entries {
		for _, call := range entries[i].Intercalls {
			if j, ok := byID[call.To]; ok {
				entries[j].CalledBy = append(entries[j].CalledBy, entries[i].ID)
			}
		}
	}
	for i := range entries {
		entries[i].CalledBy = sortedSet(entries[i].CalledBy)
	}
}

// deriveIntercalls resolves one project's URL-shaped config keys into outbound
// edges: one entry per distinct target, carrying every key that implied it.
func deriveIntercalls(index *aliasIndex, entry ProjectEntry) []Intercall {
	evidence := make(map[string][]string)
	for _, key := range entry.ConfigKeys {
		target := resolveIntercall(index, entry.ID, key.Key)
		if target == "" {
			continue
		}
		evidence[target] = append(evidence[target], key.Key)
	}
	if len(evidence) == 0 {
		return nil
	}
	calls := make([]Intercall, 0, len(evidence))
	for to, keys := range evidence {
		calls = append(calls, Intercall{To: to, ConfigKeys: sortedSet(keys)})
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].To < calls[j].To })
	return calls
}

// resolveIntercall walks a config key's hints from the LEAF backwards to the
// root — most specific first — and returns the first project a hint names
// uniquely. A self-match is SKIPPED and the walk continues, so a service whose
// config root is named after itself still contributes its nested calls.
func resolveIntercall(index *aliasIndex, selfID, key string) string {
	for _, hint := range intercallHints(key) {
		id := index.resolve(hint)
		if id == "" || id == selfID {
			continue
		}
		return id
	}
	return ""
}

// intercallHints returns a URL-shaped key's resolution candidates, most specific
// first: the leaf stripped of its address words, then each ancestor segment from
// the innermost outwards. A key that is not URL-shaped yields none.
func intercallHints(key string) []string {
	segments := strings.Split(key, ".")
	leaf := segments[len(segments)-1]
	if !urlShaped(leaf) {
		return nil
	}
	hints := make([]string, 0, len(segments))
	if hint := strings.Join(stripTrailing(splitWords(leaf), leafNoiseWords), ""); hint != "" {
		hints = append(hints, hint)
	}
	for i := len(segments) - 2; i >= 0; i-- {
		if hint := norm(segments[i]); hint != "" {
			hints = append(hints, hint)
		}
	}
	return hints
}

// urlShaped reports whether a key's leaf carries an address: its last word is an
// address word, or it mentions a JWKS document (`putnamiJwks`, `oidc.jwks_url`).
func urlShaped(leaf string) bool {
	words := splitWords(leaf)
	if len(words) == 0 {
		return false
	}
	return urlWords[words[len(words)-1]] || strings.Contains(norm(leaf), "jwks")
}

// aliasIndex maps the names a config key might use onto project ids, in two
// tiers so that exactness wins: a FULL alias is the project's own basename or
// package name, a STEM alias is that name with a trailing role word removed.
//
// Only ADDRESSABLE projects are indexed. Ambiguity is resolved by refusing to
// resolve: an alias claimed at its most exact tier by more than one project
// names nothing, because guessing here would invent a runtime edge — the exact
// failure that made a hardcoded target list repo-specific in the first place.
type aliasIndex struct {
	full map[string][]string
	stem map[string][]string
}

// buildAliasIndex indexes every addressable live project under its aliases.
func buildAliasIndex(entries []ProjectEntry) *aliasIndex {
	index := &aliasIndex{full: map[string][]string{}, stem: map[string][]string{}}
	for _, entry := range entries {
		if !addressable(entry) {
			continue
		}
		for _, name := range []string{lastSegment(entry.Path), lastSegment(entry.Name)} {
			if alias := norm(name); alias != "" {
				index.full[alias] = append(index.full[alias], entry.ID)
			}
			if alias := stemAlias(name); alias != "" {
				index.stem[alias] = append(index.stem[alias], entry.ID)
			}
		}
	}
	for alias, ids := range index.full {
		index.full[alias] = sortedSet(ids)
	}
	for alias, ids := range index.stem {
		index.stem[alias] = sortedSet(ids)
	}
	return index
}

// resolve returns the project an alias names, or "" when it names none or more
// than one. The full tier is consulted first and its verdict is final: an alias
// that two projects claim exactly is ambiguous, and falling back to the weaker
// stem tier would answer a question the exact tier already refused.
func (ix *aliasIndex) resolve(alias string) string {
	for _, tier := range []map[string][]string{ix.full, ix.stem} {
		switch ids := tier[alias]; len(ids) {
		case 0:
			continue
		case 1:
			return ids[0]
		default:
			return ""
		}
	}
	return ""
}

// addressable reports whether the map already shows a project as a runtime unit,
// which is the guard that keeps a library from becoming a false callee: a key
// like `…delivery.ci.ingestBaseUrl` must not resolve to a `libs/ci` library just
// because a segment matches its name. The declared type is only ONE of the three
// tests because most projects leave it unset.
func addressable(entry ProjectEntry) bool {
	return len(entry.Endpoints) > 0 || len(entry.ConfigKeys) > 0 || entry.Type == projectTypeApplication
}

// stemAlias returns a name's role-stripped stem ("auth-server" → "auth"), or ""
// when the name has no trailing role word or is nothing BUT one ("api" stays a
// full alias rather than becoming an empty stem).
func stemAlias(name string) string {
	words := splitWords(name)
	if len(words) < 2 || !roleWords[words[len(words)-1]] {
		return ""
	}
	return strings.Join(words[:len(words)-1], "")
}

// stripTrailing drops trailing words that are in the given set.
func stripTrailing(words []string, drop map[string]bool) []string {
	for len(words) > 0 && drop[words[len(words)-1]] {
		words = words[:len(words)-1]
	}
	return words
}

// lastSegment returns the final '/'-delimited segment of a path or a package
// name, so "@putnami/cache-server" and "delivery/workloads/cache-server" both
// reduce to "cache-server".
func lastSegment(s string) string {
	s = strings.Trim(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// norm folds a name to its comparable form: lowercase, alphanumerics only. It is
// what makes `db-gateway`, `dbGateway`, and `db_gateway` the same word.
func norm(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// splitWords splits an identifier into lowercase words on kebab, snake, and
// camel boundaries, treating a run of capitals as one acronym: "configServerUrl"
// → [config server url], "jwks_url" → [jwks url], "tokenURL" → [token url],
// "controlPlaneAPI" → [control plane api].
func splitWords(s string) []string {
	var words []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			words = append(words, strings.ToLower(string(current)))
			current = nil
		}
	}
	runes := []rune(s)
	for i, r := range runes {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r):
			if len(current) > 0 {
				prev := current[len(current)-1]
				// A capital starts a new word after a lowercase/digit run, and also
				// at the tail of an acronym that runs into a word ("APIServer").
				if !unicode.IsUpper(prev) || (i+1 < len(runes) && unicode.IsLower(runes[i+1])) {
					flush()
				}
			}
			current = append(current, r)
		default:
			current = append(current, r)
		}
	}
	flush()
	return words
}
