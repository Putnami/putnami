package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	collab "go.putnami.dev/protocol/collaboration"
)

// maxMessageBytes bounds every message an envelope carries.
const maxMessageBytes = 4 << 10

// minSecretLength is the shortest environment value treated as a secret. A
// shorter value matches too much ordinary text to be redacted safely.
const minSecretLength = 8

// redactedMarker replaces every redacted byte run.
const redactedMarker = "[redacted]"

var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s"]+@`)

// redactor removes credentials from what the orchestrator emits: the values
// of the credential-named environment variables the provider process
// inherits, and the userinfo of any URL. Provider standard error never reaches
// an envelope at all; this covers what does — messages and results a provider
// wrote.
type redactor struct {
	needles []string
}

// newRedactor collects the secrets of an environment (os.Environ form).
func newRedactor(environ []string) redactor {
	seen := map[string]bool{}
	var needles []string
	for _, entry := range environ {
		name, value, found := strings.Cut(entry, "=")
		if !found || len(value) < minSecretLength || !collab.IsCredentialName(name) || seen[value] {
			continue
		}
		seen[value] = true
		needles = append(needles, value)
	}
	// Longest first, so a secret that contains another is redacted whole.
	sort.Slice(needles, func(i, j int) bool { return len(needles[i]) > len(needles[j]) })
	return redactor{needles: needles}
}

// Text redacts and bounds one message.
func (r redactor) redactText(text string) string {
	for _, needle := range r.needles {
		text = strings.ReplaceAll(text, needle, redactedMarker)
	}
	text = urlUserinfo.ReplaceAllString(text, "${1}"+redactedMarker+"@")
	if len(text) > maxMessageBytes {
		cut := maxMessageBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "…"
	}
	return text
}

// Envelope redacts every message of an envelope, and every secret and URL
// userinfo inside its result. A result that carries a secret it cannot remove
// safely is withheld: a read then answers unavailable, and a mutation
// unresolved, because the write completed but its answer cannot be delivered.
func (r redactor) redactEnvelope(envelope collab.Envelope, mutating bool) collab.Envelope {
	if envelope.Error != nil {
		redacted := *envelope.Error
		redacted.Message = r.redactText(redacted.Message)
		redacted.Reason = r.redactText(redacted.Reason)
		redacted.Reconcile = r.redactText(redacted.Reconcile)
		envelope.Error = &redacted
	}
	if len(envelope.Result) == 0 {
		return envelope
	}
	result, changed, safe := r.redactResult(envelope.Result)
	if !safe {
		envelope.Result = nil
		envelope.Outcome = collab.OutcomeUnavailable
		envelope.Error = &collab.Error{
			Message: "the provider's result carried a credential and could not be redacted safely; it was withheld",
			Reason:  collab.ReasonProviderInvalidResponse,
		}
		if mutating {
			envelope.Outcome = collab.OutcomeUnresolved
			envelope.Error.Reconcile = "the provider reported the write as done; " + ReconcileHint(envelope.Contract, envelope.Operation)
		}
		return envelope
	}
	if changed {
		envelope.Result = result
	}
	return envelope
}

// errUnredactable reports a secret redaction cannot remove without changing
// the result document's shape.
var errUnredactable = errors.New("a secret cannot be redacted in place")

// redactResult redacts a result document by value. It decodes the document,
// redacts every string — member names included — and re-encodes the document
// only when something was redacted, keeping member order. Matching decoded
// strings finds a secret however the provider's JSON encoder spelled it: raw,
// \u-escaped or \/-escaped. safe is false when the document carries a secret
// that cannot be removed in place — inside a number, or in a document that
// does not decode.
func (r redactor) redactResult(document json.RawMessage) (redacted json.RawMessage, changed, safe bool) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var out bytes.Buffer
	changed, err := r.redactJSONValue(decoder, &out)
	if err == nil {
		if _, trailing := decoder.Token(); !errors.Is(trailing, io.EOF) {
			err = errors.New("trailing data after the result document")
		}
	}
	switch {
	case errors.Is(err, errUnredactable):
		return nil, false, false
	case err != nil:
		// A document that does not decode is scanned as text; it is withheld
		// when any secret or URL userinfo appears in it.
		return nil, false, !r.textCarriesSecret(string(document))
	case !changed:
		return document, false, true
	}
	return json.RawMessage(out.Bytes()), true, true
}

// redactJSONValue copies one JSON value from decoder to out with every string
// redacted, and reports whether anything was.
func (r redactor) redactJSONValue(decoder *json.Decoder, out *bytes.Buffer) (bool, error) {
	token, err := decoder.Token()
	if err != nil {
		return false, err
	}
	switch value := token.(type) {
	case json.Delim:
		if value != '{' && value != '[' {
			return false, fmt.Errorf("unexpected %q", value)
		}
		object := value == '{'
		out.WriteByte(byte(value))
		changed := false
		for first := true; decoder.More(); first = false {
			if !first {
				out.WriteByte(',')
			}
			if object {
				key, err := decoder.Token()
				if err != nil {
					return false, err
				}
				name, _ := key.(string)
				redacted, keyChanged := r.redactString(name)
				writeJSONString(out, redacted)
				out.WriteByte(':')
				changed = changed || keyChanged
			}
			memberChanged, err := r.redactJSONValue(decoder, out)
			if err != nil {
				return false, err
			}
			changed = changed || memberChanged
		}
		if _, err := decoder.Token(); err != nil {
			return false, err
		}
		if object {
			out.WriteByte('}')
		} else {
			out.WriteByte(']')
		}
		return changed, nil
	case string:
		redacted, changed := r.redactString(value)
		writeJSONString(out, redacted)
		return changed, nil
	case json.Number:
		if r.textCarriesSecret(value.String()) {
			return false, errUnredactable
		}
		out.WriteString(value.String())
	case bool:
		out.WriteString(strconv.FormatBool(value))
	case nil:
		out.WriteString("null")
	}
	return false, nil
}

// redactString removes every secret and URL userinfo from one decoded string.
func (r redactor) redactString(value string) (string, bool) {
	redacted := value
	for _, needle := range r.needles {
		redacted = strings.ReplaceAll(redacted, needle, redactedMarker)
	}
	redacted = urlUserinfo.ReplaceAllString(redacted, "${1}"+redactedMarker+"@")
	return redacted, redacted != value
}

// textCarriesSecret reports whether text holds a secret, in its raw or its
// JSON-escaped spelling, or a URL userinfo.
func (r redactor) textCarriesSecret(text string) bool {
	if urlUserinfo.MatchString(text) {
		return true
	}
	for _, needle := range r.needles {
		if strings.Contains(text, needle) {
			return true
		}
		if encoded, err := json.Marshal(needle); err == nil && strings.Contains(text, string(encoded[1:len(encoded)-1])) {
			return true
		}
	}
	return false
}

// writeJSONString appends value as a JSON string literal, leaving <, > and &
// as they are.
func writeJSONString(out *bytes.Buffer, value string) {
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	out.Truncate(out.Len() - 1) // Encode ends the literal with a newline.
}
