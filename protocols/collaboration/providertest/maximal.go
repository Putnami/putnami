package providertest

import (
	"strconv"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
)

// The characters a maximal document is filled with, the ones a JSON encoder
// expands most: a control character becomes a six-byte escape in a text
// member; a token holds four-byte printable runes, which are never escaped;
// a reference's source takes '"', which becomes two bytes.
const (
	maximalEscaped = "\x01"
	maximalWide    = "\U0001D52A"
	maximalQuote   = `"`
)

// maximalFill is n characters of unit ending in suffix, so the members of
// one list differ.
func maximalFill(unit string, n int, suffix string) string {
	return strings.Repeat(unit, n-len([]rune(suffix))) + suffix
}

// MaximalToken is a token of the largest length the contract accepts: prefix,
// then the widest printable rune up to MaxTokenLength characters. A prefix
// unique to a run keeps the token unique on a shared backend.
func MaximalToken(prefix string) string {
	return prefix + strings.Repeat(maximalWide, collab.MaxTokenLength-len([]rune(prefix)))
}

// MaximalCheckpoint is a memory.checkpoint request that creates mission in
// identity with every other member at its contract bound, filled with the
// characters that encode largest: a title and content of control
// characters, the most sources and evidence entries, each with its longest
// locator or identifiers. The contract accepts it, so a memory provider that
// writes it must read it back unchanged. key is its idempotency key.
func MaximalCheckpoint(mission, key string, identity collab.MemoryIdentity) map[string]any {
	sources := make([]any, 0, collab.MaxListMembers)
	evidence := make([]any, 0, collab.MaxListMembers)
	for i := range collab.MaxListMembers {
		suffix := strconv.Itoa(i)
		sources = append(sources, map[string]any{
			"source": "x:" + maximalFill(maximalQuote, collab.MaxTokenLength-2, suffix),
			"id":     maximalFill(maximalWide, collab.MaxTokenLength, suffix),
		})
		evidence = append(evidence, map[string]any{
			"kind": "gate", "locator": maximalFill(maximalEscaped, collab.MaxLocatorLength, suffix),
			"digest": "sha256:" + strings.Repeat("b", 64),
		})
	}
	members := map[string]any{}
	for name, value := range map[string]string{"workspace": identity.Workspace, "repository": identity.Repository, "scope": identity.Scope} {
		if value != "" {
			members[name] = value
		}
	}
	return map[string]any{
		"mission":        mission,
		"identity":       members,
		"precondition":   map[string]any{"mustNotExist": true},
		"idempotencyKey": key,
		"title":          strings.Repeat(maximalEscaped, collab.MaxTitleLength),
		"content":        strings.Repeat(maximalEscaped, collab.MaxContentBytes),
		"sources":        sources,
		"evidence":       evidence,
	}
}
