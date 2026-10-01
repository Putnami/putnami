package ci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ValidationError groups field-addressable contract findings.
type ValidationError struct {
	// Diagnostics contains stable field-addressable validation findings.
	Diagnostics []diag.Diagnostic `json:"diagnostics"`
}

func (e *ValidationError) Error() string {
	errors := diag.Errors(e.Diagnostics)
	if len(errors) == 0 {
		return "ci: invalid document"
	}
	parts := make([]string, 0, len(errors))
	for _, finding := range errors {
		parts = append(parts, finding.String())
	}
	return "ci: invalid document: " + strings.Join(parts, "; ")
}

// Parse strictly decodes, validates, and normalizes a version 3 document.
func Parse(data []byte) (Document, error) {
	document, diagnostics := ParseWithDiagnostics(data)
	if diag.HasErrors(diagnostics) {
		return Document{}, &ValidationError{Diagnostics: diagnostics}
	}
	return document, nil
}

// ParseWithDiagnostics retains public-literal warnings for validation UX.
func ParseWithDiagnostics(data []byte) (Document, []diag.Diagnostic) {
	if len(data) > MaxDocumentBytes {
		return Document{}, []diag.Diagnostic{diag.Errorf(
			"ci.document_too_large", "(document)", "document is %d bytes; maximum is %d", len(data), MaxDocumentBytes,
		)}
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return Document{}, []diag.Diagnostic{diag.Errorf("ci.duplicate_key", "(document)", "%v", err)}
	}
	// A missing profile means no review intent; explicit null is not an object
	// and must not silently become absence through a Go pointer.
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) == nil && bytes.Equal(bytes.TrimSpace(fields["review"]), []byte("null")) {
		return Document{}, []diag.Diagnostic{diag.Errorf("ci.invalid_review", "review", "must be an object when present")}
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		return Document{}, []diag.Diagnostic{diag.Errorf("ci.invalid_json", "(document)", "%v", err)}
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Document{}, []diag.Diagnostic{diag.Errorf("ci.trailing_json", "(document)", "%v", err)}
	}
	diagnostics := Validate(document)
	if diag.HasErrors(diagnostics) {
		return Document{}, diagnostics
	}
	return Normalize(document), diagnostics
}

// rejectDuplicateKeys walks the token stream because encoding/json otherwise
// keeps the last occurrence of an object member.
func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := walkJSONValue(decoder, "$"); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected second JSON value")
		}
		return err
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s: expected object key", path)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("%s.%s: duplicate object key", path, key)
			}
			seen[key] = struct{}{}
			if err := walkJSONValue(decoder, path+"."+key); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		index := 0
		for decoder.More() {
			if err := walkJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
			index++
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("%s: unexpected delimiter %q", path, delim)
	}
}
