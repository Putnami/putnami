package architecture

import (
	"os"
	"strings"
	"testing"

	archproto "go.putnami.dev/protocol/architecture"
	diag "go.putnami.dev/protocol/diagnostic"
)

// Pin holds a committed putnami.architecture.json to the Go program that
// authors it: the builder IS the author, the JSON is its projection, and a hand
// edit the program does not make fails the owning project's test run rather than
// surviving as a difference nobody notices.
//
// It generalizes @putnami/sdd's manifest harness
// (tooling/sdd-extension/manifest_contract_test.go, TestCommittedManifestIsThe-
// AuthoredOne) to any domain. Call it from a test in a project the domain owns:
//
//	func TestCommittedManifestIsTheAuthoredOne(t *testing.T) {
//	    architecture.Pin(t, "../../putnami.architecture.json", authoredDomain)
//	}
//
// # What is compared
//
// Both documents are compared in the PROTOCOL's canonical form — the committed
// file is read through the same strict reader `architecture validate` uses, then
// re-rendered by the protocol's own canonical writer, and the authoring is
// rendered by that same writer. The comparison is therefore over the DOCUMENT:
// two-space indentation, member order inside an object, and the order sibling
// exports were written in are the encoder's business, not the manifest's.
//
// That normalization has to be the protocol's. A hand-rolled one would be a
// second opinion about canonical form, and the drift between two opinions is
// exactly what this helper exists to catch.
//
// A committed file that is not itself in canonical order still passes, and that
// is deliberate: reordering a reviewed permission list is a diff nobody asked
// for, and the protocol does not require a committed manifest to be sorted — it
// requires it to MEAN the same thing.
//
// # What it cannot do
//
// Nothing here reads a workspace or a project graph, so Pin cannot judge whether
// a bound project exists or belongs to the domain that claims it. Those are
// cross-document questions and `architecture validate` in the gate answers them
// over the whole workspace.
func Pin(t *testing.T, path string, author func() *Builder) {
	t.Helper()
	pin(t, path, author)
}

// reporter is the slice of *testing.T Pin uses. It exists so this package can
// test that Pin FAILS on a tampered file — a pin that silently passed on
// everything would look identical to a passing one, and testing.TB cannot be
// implemented outside the standard library.
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

func pin(t reporter, path string, author func() *Builder) {
	t.Helper()
	if author == nil {
		t.Fatalf("architecture.Pin needs an authoring function; without one nothing is pinned")
		return
	}
	builder := author()
	if builder == nil {
		t.Fatalf("the authoring function returned no builder; call architecture.NewDomain")
		return
	}
	authored, err := builder.CanonicalBytes()
	if err != nil {
		t.Fatalf("the authored domain manifest was rejected at authoring time: %v", err)
		return
	}

	data, err := os.ReadFile(path) //nolint:gosec // the path is a test's own literal
	if err != nil {
		t.Fatalf("read the committed architecture manifest %s: %v", path, err)
		return
	}
	committed, diagnostics := archproto.ParseAndValidateManifest(data)
	if errs := diag.Errors(diagnostics); len(errs) > 0 {
		t.Fatalf("%s is not a valid architecture manifest:\n%s", path, (&ValidationError{Diagnostics: errs}).Error())
		return
	}
	rendered, err := archproto.MarshalManifest(committed)
	if err != nil {
		t.Fatalf("re-render the committed architecture manifest %s: %v", path, err)
		return
	}
	if string(rendered) == string(authored) {
		return
	}
	t.Errorf("%s is not the document the builder authors.\n committed:\n%s\n authored:\n%s",
		path, indent(string(rendered)), indent(string(authored)))
}

// indent offsets a whole rendering so the two documents in a failure message
// stay visibly separate from the message that introduces them.
func indent(text string) string {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for index := range lines {
		lines[index] = "  " + lines[index]
	}
	return strings.Join(lines, "\n")
}
