package extension

import (
	"os"
	"sort"
	"strings"
	"testing"
)

const lifecycleDocPath = "doc/07-lifecycles.md"

// TestLifecycleFailureVocabularies pins the three closed vocabularies: sorted,
// duplicate-free, and namespaced by the lifecycle that owns them. A consumer
// switching exhaustively on one of these must be able to trust that a code with
// a "runtime." prefix is a runtime failure and nothing else.
func TestLifecycleFailureVocabularies(t *testing.T) {
	vocabularies := []struct {
		name   string
		prefix string
		codes  []string
	}{
		{"runtime", "runtime.", ValidRuntimeFailureCodes},
		{"workspace", "workspace.", ValidWorkspaceFailureCodes},
		{"sensitive", "sensitive.", ValidSensitiveFailureCodes},
	}

	seen := map[string]string{}
	for _, vocabulary := range vocabularies {
		t.Run(vocabulary.name, func(t *testing.T) {
			if len(vocabulary.codes) == 0 {
				t.Fatal("a lifecycle with no failure codes cannot report anything")
			}
			for i, code := range vocabulary.codes {
				if !strings.HasPrefix(code, vocabulary.prefix) {
					t.Errorf("code %q is not namespaced %q", code, vocabulary.prefix)
				}
				if i > 0 && vocabulary.codes[i-1] >= code {
					t.Errorf("%s codes are not in canonical (sorted) order: %v", vocabulary.name, vocabulary.codes)
				}
				if owner, ok := seen[code]; ok {
					t.Errorf("code %q is claimed by both %s and %s", code, owner, vocabulary.name)
				}
				seen[code] = vocabulary.name
				if !IsLifecycleFailureCode(code) {
					t.Errorf("IsLifecycleFailureCode(%q) = false for a member of the vocabulary", code)
				}
			}
		})
	}

	if len(ValidLifecycleFailureCodes) != len(seen) {
		t.Errorf("the union has %d codes, the three vocabularies have %d",
			len(ValidLifecycleFailureCodes), len(seen))
	}
	if !sort.StringsAreSorted(ValidLifecycleFailureCodes) {
		t.Errorf("ValidLifecycleFailureCodes is not sorted: %v", ValidLifecycleFailureCodes)
	}
	for _, code := range []string{"", "runtime", "runtime.nope", "job.invalid_version", "required-field"} {
		if IsLifecycleFailureCode(code) {
			t.Errorf("IsLifecycleFailureCode(%q) = true outside the closed vocabulary", code)
		}
	}
	for _, code := range ValidLifecycleRecoveryCodes {
		if !IsLifecycleRecoveryCode(code) {
			t.Errorf("IsLifecycleRecoveryCode(%q) = false for a recovery member", code)
		}
		if IsLifecycleFailureCode(code) {
			t.Errorf("successful recovery %q is also classified as a failure", code)
		}
	}
}

// TestLifecycleFailureCodesAreNotValidationCodes keeps the two families apart.
// A validation code says a manifest is wrong; a lifecycle code says a correct
// manifest could not be executed. Sharing a spelling would make a consumer's
// switch ambiguous, so the namespaces may never intersect.
func TestLifecycleFailureCodesAreNotValidationCodes(t *testing.T) {
	validationCodes := []string{
		"required-field", "invalid-enum", "invalid-value", "invalid-output-path",
		"invalid-runtime-path", "invalid-workspace-path", "invalid-template-var",
		"marker-not-input", "unresolved-sync-task", "finalizer-binding",
		"finalizer-export", "finalizer-producer-without-invocation-output",
		"invalid-finalizer-relation", "invalid-finalizer-frontier",
		"shared-finalizer-consumer",
		"invocation-scope-required", "invocation-scope-conflict",
		"invocation-cache-conflict", "sensitive-path-leak",
	}
	for _, code := range validationCodes {
		if IsLifecycleFailureCode(code) {
			t.Errorf("validation code %q collides with the lifecycle failure vocabulary", code)
		}
		if strings.Contains(code, ".") {
			t.Errorf("validation code %q uses the dotted lifecycle spelling", code)
		}
	}
}

// TestLifecycleFailureCodesAreDocumented is the docs-agreement ratchet: every
// code must appear in the lifecycle documentation, so a code a consumer can
// receive is a code an extension author can look up. The reverse direction —
// a documented code that does not exist — is caught too, because a table entry
// nobody emits is a promise the CLI does not keep.
func TestLifecycleFailureCodesAreDocumented(t *testing.T) {
	data, err := os.ReadFile(lifecycleDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", lifecycleDocPath, err)
	}
	doc := string(data)
	for _, code := range ValidLifecycleFailureCodes {
		if !strings.Contains(doc, "`"+code+"`") {
			t.Errorf("%s does not document the failure code %q", lifecycleDocPath, code)
		}
	}
	for _, code := range ValidLifecycleRecoveryCodes {
		if !strings.Contains(doc, "`"+code+"`") {
			t.Errorf("%s does not document the recovery code %q", lifecycleDocPath, code)
		}
	}
	for _, prefix := range []string{"runtime.", "workspace.", "sensitive."} {
		for _, documented := range documentedCodes(doc, prefix) {
			if !IsLifecycleFailureCode(documented) && !IsLifecycleRecoveryCode(documented) {
				t.Errorf("%s documents %q, which is not part of the failure or recovery vocabulary", lifecycleDocPath, documented)
			}
		}
	}
}

// documentedCodes returns the `code`-quoted tokens in the doc that start with
// prefix and are spelled as failure codes.
//
// The doc quotes manifest FIELD paths under the same prefixes
// ("runtime.executable", "workspace.inputs"), so the two have to be told apart.
// The discriminator is the spelling the vocabulary uses: a failure code's
// suffix is snake_case, a field path's is camelCase.
// TestLifecycleFailureCodesAreSnakeCase keeps that discriminator honest.
func documentedCodes(doc, prefix string) []string {
	var found []string
	for _, chunk := range strings.Split(doc, "`") {
		if !strings.HasPrefix(chunk, prefix) || strings.ContainsAny(chunk, " \n\t") {
			continue
		}
		if !strings.Contains(strings.TrimPrefix(chunk, prefix), "_") {
			continue
		}
		found = append(found, chunk)
	}
	return found
}

// TestLifecycleFailureCodesAreSnakeCase pins the spelling the documentation
// scan relies on. Without it, a camelCase code would make the reverse half of
// TestLifecycleFailureCodesAreDocumented silently vacuous.
func TestLifecycleFailureCodesAreSnakeCase(t *testing.T) {
	for _, code := range ValidLifecycleFailureCodes {
		suffix := code[strings.Index(code, ".")+1:]
		if !strings.Contains(suffix, "_") {
			t.Errorf("failure code %q is not snake_case; the doc scan cannot tell it from a field path", code)
		}
		if strings.ToLower(code) != code {
			t.Errorf("failure code %q is not lower case", code)
		}
	}
}
