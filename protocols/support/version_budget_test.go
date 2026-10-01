package support

// The support catalog's compatibility budget, executable.
//
// This contract has no read window and no migration: it accepts the exact
// integer token 1 and nothing else. That is deliberate, and it is the opposite
// of the lock file's bounded window. A lock is GENERATED, so a migration can
// rewrite it mechanically; a support catalog is AUTHORED — it is a reviewed
// statement about what the project publicly promises — and mechanically
// rewriting one would change that promise without review.
//
// TestStrictParserRejectsNonCanonicalJSON already covers a float and a missing
// member. What it does not cover is the rest of the token space a JSON writer
// can legally produce for "one", nor the remedy the user is told, and those are
// exactly the ways an exact-match rule erodes into a lenient one.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProtocolVersionIsAnExactIntegerToken pins the rejection across every
// spelling of the accepted value a producer might emit. Each of these decodes
// to 1 in a lenient reader; none of them is the token this contract accepts.
func TestProtocolVersionIsAnExactIntegerToken(t *testing.T) {
	catalogWith := func(token string) string {
		return fmt.Sprintf(`{"protocolVersion":%s,"entries":[{"id":"@putnami/cli","kind":"package","status":"stable"}]}`, token)
	}

	tests := []struct {
		name  string
		token string
	}{
		{"a newer version", "2"},
		{"a string", `"1"`},
		{"a float", "1.0"},
		{"exponent notation", "1e0"},
		{"a signed integer", "+1"},
		{"null", "null"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			catalog, diagnostics := ParseCatalog([]byte(catalogWith(test.token)))
			if catalog != nil {
				t.Fatalf("catalog = %#v, want nil — a rejected catalog must not be returned", catalog)
			}
			if len(diagnostics) != 1 {
				t.Fatalf("diagnostics = %#v, want exactly one", diagnostics)
			}
			// An explicit null is refused by the structural pass before the
			// version pass reaches it; both are refusals, and which one fires
			// is not part of the promise.
			code := diagnostics[0].Code
			if code != ErrorCodeInvalidProtocolVersion && code != ErrorCodeParseError {
				t.Fatalf("diagnostic code = %q, want %q or %q",
					code, ErrorCodeInvalidProtocolVersion, ErrorCodeParseError)
			}
		})
	}

	// The accepted token still parses, so the test above is a rejection test and
	// not a "nothing parses" test.
	if catalog, diagnostics := ParseCatalog([]byte(catalogWith("1"))); catalog == nil || len(diagnostics) != 0 {
		t.Fatalf("the exact token %d was rejected: %#v", ProtocolVersion, diagnostics)
	}
}

// TestProtocolVersionRejectionNamesTheAcceptedToken pins the user-visible half.
// The catalog is hand-authored, so the only fix is to edit the file — which the
// message has to say, because there is no command that does it.
func TestProtocolVersionRejectionNamesTheAcceptedToken(t *testing.T) {
	_, diagnostics := ParseCatalog([]byte(`{"protocolVersion":2,"entries":[]}`))
	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want exactly one", diagnostics)
	}
	message := diagnostics[0].Message
	for _, want := range []string{
		fmt.Sprintf("exact integer token %d", ProtocolVersion),
		"not supported",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("rejection %q does not name %q", message, want)
		}
	}
	if diagnostics[0].Field != "protocolVersion" {
		t.Errorf("diagnostic field = %q, want protocolVersion", diagnostics[0].Field)
	}
}

// TestReadmeStatesTheAcceptedVersion keeps the prose and the constant together.
// The number is stated in three places a user may read — this module's README,
// the CLI compatibility budget, and the parser — and the parser is the only one
// a test would otherwise notice moving.
func TestReadmeStatesTheAcceptedVersion(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	want := fmt.Sprintf("the exact integer `%d`", ProtocolVersion)
	if !strings.Contains(string(readme), want) {
		t.Errorf("protocols/support/README.md does not state %q", want)
	}

	// The workspace-wide budget states the same number. It lives in another
	// module, so this check is skipped when this module is tested standalone —
	// the same guard TestRootCatalogIsTheCanonicalAuthority uses.
	workspaceRoot := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(workspaceRoot, "putnami.workspace.json")); err != nil {
		if os.IsNotExist(err) {
			t.Skip("workspace root sentinel is absent; module is being tested standalone")
		}
		t.Fatalf("inspect workspace root sentinel: %v", err)
	}
	budgetPath := filepath.Join(workspaceRoot, "tooling", "cli", "doc", "21-compatibility-and-migration.md")
	budget, err := os.ReadFile(budgetPath)
	if err != nil {
		t.Fatalf("read the compatibility budget: %v", err)
	}
	claim := fmt.Sprintf("accepted only at support catalog protocol version %d", ProtocolVersion)
	if !strings.Contains(string(budget), claim) {
		t.Errorf("%s does not state %q.\nThe budget and this parser must move together.", budgetPath, claim)
	}
}
