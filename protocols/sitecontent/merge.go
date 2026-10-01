package sitecontent

import (
	"fmt"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// PrefixOwnerKind identifies who owns a site URL prefix during merge planning.
type PrefixOwnerKind string

// Prefix owner kinds used by ValidatePrefixOwners and ValidateMergePlan.
const (
	PrefixOwnerSiteLocal PrefixOwnerKind = "site-local"
	PrefixOwnerBundle    PrefixOwnerKind = "bundle"
)

// PrefixOwner declares one owner of a site URL prefix. Overlap between any two
// owners is a hard merge error.
type PrefixOwner struct {
	Kind      PrefixOwnerKind
	Name      string
	URLPrefix string
}

// ValidURLPrefix reports whether p is a normalized absolute site URL prefix.
// Unlike ValidMountPrefix, it permits "/" so a caller can model a local site
// tree that intentionally owns the whole URL space.
func ValidURLPrefix(p string) bool {
	return p == "/" || ValidMountPrefix(p)
}

// URLInPrefix reports whether urlPath is equal to prefix or is below prefix on
// a path-segment boundary. "/docs/cloud" is inside "/docs"; "/docs-cloud" is
// not.
func URLInPrefix(urlPath, prefix string) bool {
	if prefix == "" || urlPath == "" {
		return false
	}
	if prefix == "/" {
		return strings.HasPrefix(urlPath, "/")
	}
	return urlPath == prefix || strings.HasPrefix(urlPath, prefix+"/")
}

// PrefixesConflict reports whether two URL prefixes overlap. Equal prefixes and
// parent/child prefixes conflict; siblings do not.
func PrefixesConflict(a, b string) bool {
	return URLInPrefix(a, b) || URLInPrefix(b, a)
}

// MountForPath returns the declared mount that owns a payload-relative file
// path. The payload path is served at URLPath(path).
func MountForPath(mounts []Mount, path string) (Mount, bool) {
	if !ValidRelPath(path) {
		return Mount{}, false
	}
	urlPath := URLPath(path)
	for _, mount := range mounts {
		if URLInPrefix(urlPath, mount.URLPrefix) {
			return mount, true
		}
	}
	return Mount{}, false
}

// ValidatePrefixOwners validates URL prefix ownership and reports any overlap.
// It is intentionally independent of bundle transport so producers and site
// consumers can run the same collision rules.
func ValidatePrefixOwners(owners []PrefixOwner) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for i, owner := range owners {
		if owner.Kind == "" {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidMount, fmt.Sprintf("owners[%d].kind", i),
				"prefix owner kind is required"))
		}
		if !ValidURLPrefix(owner.URLPrefix) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidMount, fmt.Sprintf("owners[%d].urlPrefix", i),
				"urlPrefix %q must be an absolute, normalized site path", owner.URLPrefix))
		}
	}

	// Detect prefix overlaps in O(n log n) instead of the naive O(n^2) pairwise
	// scan. Two prefixes conflict only when one is an equal-or-ancestor of the
	// other on a segment boundary (PrefixesConflict == URLInPrefix either way).
	//
	// Sort by path SEGMENTS (not raw bytes) so that an ancestor always
	// immediately precedes its descendants with no unrelated prefix interleaved,
	// then keep a stack of the still-open ancestors of the current path:
	// everything left on the stack conflicts with the current owner. A raw byte
	// sort is wrong here because a non-conflicting sibling can sort between an
	// ancestor and its descendant whenever it diverges at a byte below '/'
	// (0x2f) — e.g. "/docs-cloud" (the '-' is 0x2d) sorts between "/docs" and
	// "/docs/cloud" and would evict "/docs" from the stack, hiding the
	// /docs↔/docs/cloud overlap. Segment order gives "/docs" < "/docs/cloud" <
	// "/docs-cloud", keeping every prefix's descendants contiguous. This reports
	// the same set of conflicts as the pairwise scan.
	segs := make([][]string, len(owners))
	for i := range owners {
		segs[i] = urlSegments(owners[i].URLPrefix)
	}
	order := make([]int, len(owners))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return lessSegments(segs[order[a]], segs[order[b]])
	})

	var stack []int // original indices of ancestors still open for the current path
	for _, idx := range order {
		cur := owners[idx]
		// Drop entries that are not ancestors of cur; whatever remains are all
		// ancestors of cur, hence conflicts.
		for len(stack) > 0 && !URLInPrefix(cur.URLPrefix, owners[stack[len(stack)-1]].URLPrefix) {
			stack = stack[:len(stack)-1]
		}
		for _, anc := range stack {
			diags = append(diags, diag.Errorf(ErrorCodeMountCollision, fmt.Sprintf("owners[%d].urlPrefix", idx),
				"prefix %q owned by %s overlaps prefix %q owned by %s",
				cur.URLPrefix, cur.label(), owners[anc].URLPrefix, owners[anc].label()))
		}
		stack = append(stack, idx)
	}
	return diags
}

// urlSegments splits a URL prefix into its path segments for ancestor-aware
// ordering. Root ("/"), the empty string, and any all-slash prefix yield no
// segments, so they sort first and rank as an ancestor of everything.
func urlSegments(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// lessSegments orders two segment lists lexicographically, comparing whole
// segments as atomic units. A shorter list that is a prefix of the other (an
// ancestor) sorts first. Comparing whole segments rather than raw bytes is what
// guarantees a prefix's descendants stay contiguous in sort order.
func lessSegments(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// ValidateMergePlan checks the URL-space merge between the site's local tree
// and one or more bundles. Site-local prefixes are modeled separately from
// bundle mounts so a site can reject a bundle before overlaying files.
func ValidateMergePlan(siteLocalPrefixes []string, bundles []Manifest) []diag.Diagnostic {
	owners := make([]PrefixOwner, 0, len(siteLocalPrefixes)+len(bundles))
	for _, prefix := range siteLocalPrefixes {
		owners = append(owners, PrefixOwner{
			Kind:      PrefixOwnerSiteLocal,
			Name:      "site",
			URLPrefix: prefix,
		})
	}
	for _, bundle := range bundles {
		for _, mount := range bundle.Mounts {
			owners = append(owners, PrefixOwner{
				Kind:      PrefixOwnerBundle,
				Name:      bundle.Name,
				URLPrefix: mount.URLPrefix,
			})
		}
	}
	return ValidatePrefixOwners(owners)
}

func (o PrefixOwner) label() string {
	if o.Name == "" {
		return string(o.Kind)
	}
	return fmt.Sprintf("%s %q", o.Kind, o.Name)
}
