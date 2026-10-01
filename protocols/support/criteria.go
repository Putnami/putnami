package support

import "regexp"

// Promotion and demotion criteria are product policy, not wire. ADR 0002 states
// the full checklist per subject kind and status; this file implements only the
// share a machine can decide from the subject's own documentation, so a
// classification and the document that justifies it cannot drift apart.
//
// It is unexported on purpose: it is the module gate's reader, not a second
// public contract on top of a package whose whole point is that policy stays off
// the wire. Export it when the package half of ADR 0002 §4 needs it from
// tooling/cli, and not before.
//
// Nothing here reads the filesystem: callers supply the document text, and the
// gate that walks the workspace lives in the test that owns the workspace.

var (
	// fencedBlock matches a fenced code block, opening and closing fence
	// included. Fences are stripped before scanning so an example that documents
	// this very convention is not mistaken for the document's own declaration.
	fencedBlock = regexp.MustCompile("(?ms)^[ \t]*(```|~~~).*?^[ \t]*(```|~~~)[ \t]*$")
	// documentHeading matches an ATX heading of level two or deeper. Level one
	// is the document title, never a section.
	documentHeading = regexp.MustCompile(`(?m)^(#{2,})[ \t]+(.*)$`)
	// supportHeadingWord matches the whole word "support" in a heading title, so
	// "Supported backends" is not mistaken for a support-status section.
	supportHeadingWord = regexp.MustCompile(`(?i)\bsupport\b`)
	// statusToken matches a support status written as inline code. The
	// convention across owning modules is to write the reviewed value in
	// backticks; prose that merely names a status is not a declaration.
	statusToken = regexp.MustCompile("`(stable|preview|experimental)`")
	// catalogLink matches a Markdown link whose target is the reviewed catalog.
	// Naming the file in prose is not citing it: the point is that a reader can
	// click through to the authority.
	catalogLink = regexp.MustCompile(`\]\([^)\s]*` + regexp.QuoteMeta(CatalogFilename) + `[^)\s]*\)`)
)

// statusDeclaration is what a subject's own documentation says about its
// reviewed support status. It is derived, never authored: the catalog stays the
// only authority, and this is how the gate checks that the subject repeats it.
type statusDeclaration struct {
	// section reports whether the document has a support section at all.
	section bool
	// status is the first status the support section writes as inline code,
	// which the convention places on the section's own status line. Later
	// mentions — "why not `stable`", "evidence still missing for `stable`" — are
	// deliberately ignored.
	status Status
	// linksCatalog reports whether the support section links the reviewed
	// catalog, so a reader lands on the authority rather than on this restatement.
	linksCatalog bool
}

// readStatusDeclaration extracts the support status a document declares for its
// own subject. The support section is the first heading of level two or deeper
// whose title contains the word "support", running to the next heading at that
// level or above. Fenced code blocks are removed first, so a documented example
// never becomes the declaration.
func readStatusDeclaration(documentation string) statusDeclaration {
	section, ok := supportSection(fencedBlock.ReplaceAllString(documentation, ""))
	if !ok {
		return statusDeclaration{}
	}
	declaration := statusDeclaration{
		section:      true,
		linksCatalog: catalogLink.MatchString(section),
	}
	if match := statusToken.FindStringSubmatch(section); match != nil {
		declaration.status = Status(match[1])
	}
	return declaration
}

// explain returns why the document does not agree with the reviewed status, or
// an empty string when it does. The wording is written for a gate failure: it
// names what to add, not merely what is wrong.
func (declaration statusDeclaration) explain(reviewed Status) string {
	switch {
	case !declaration.section:
		return "has no support section; add a heading naming support that states the reviewed status"
	case declaration.status == "":
		return "has a support section that writes no status as inline code; state `" + string(reviewed) + "`"
	case declaration.status != reviewed:
		return "declares `" + string(declaration.status) + "` but the reviewed catalog says `" + string(reviewed) + "`"
	case !declaration.linksCatalog:
		return "states its status without linking to " + CatalogFilename + ", so a reader cannot reach the authority"
	default:
		return ""
	}
}

// supportSection returns the body of the document's support section.
func supportSection(documentation string) (string, bool) {
	headings := documentHeading.FindAllStringSubmatchIndex(documentation, -1)
	for index, heading := range headings {
		title := documentation[heading[4]:heading[5]]
		if !supportHeadingWord.MatchString(title) {
			continue
		}
		level := heading[3] - heading[2]
		end := len(documentation)
		for _, next := range headings[index+1:] {
			if next[3]-next[2] <= level {
				end = next[0]
				break
			}
		}
		return documentation[heading[0]:end], true
	}
	return "", false
}
