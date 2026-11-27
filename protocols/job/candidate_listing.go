package job

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	diag "go.putnami.dev/protocol/diagnostic"
)

// EncodeCandidateFiles is the content of a candidate listing file (Tree's
// WorkspaceCandidates) and its digest, the lowercase hex SHA-256 of that
// content: the JSON array of files, in their order. The orchestrator writes
// it, and a job reads it back with ReadCandidateListing.
func EncodeCandidateFiles(files []CandidateFile) ([]byte, string, error) {
	if files == nil {
		files = []CandidateFile{}
	}
	data, err := json.Marshal(files)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

// ReadCandidateListing reads the candidate cut the listing names, the
// workspace's: it refuses a file whose bytes do not match the digest, and a
// cut that is not sorted, unique and made of clean paths and known modes.
func ReadCandidateListing(listing *CandidateListing) ([]CandidateFile, error) {
	if listing == nil {
		return nil, fmt.Errorf("no candidate listing")
	}
	data, err := os.ReadFile(listing.File)
	if err != nil {
		return nil, fmt.Errorf("read the candidate listing: %w", err)
	}
	sum := sha256.Sum256(data)
	if digest := hex.EncodeToString(sum[:]); digest != listing.Digest {
		return nil, fmt.Errorf("the candidate listing %s has digest %s, not %s", listing.File, digest, listing.Digest)
	}
	files, ok := decodeCanonicalCandidateFiles(data)
	if !ok {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&files); err != nil {
			return nil, fmt.Errorf("decode the candidate listing %s: %w", listing.File, err)
		}
	}
	if files == nil {
		return nil, fmt.Errorf("the candidate listing %s holds no array", listing.File)
	}
	if diags := ValidateCandidateFiles("workspaceCandidates", []string{"."}, files); diag.HasErrors(diags) {
		return nil, fmt.Errorf("the candidate listing %s: %s", listing.File, diags[0].Message)
	}
	return files, nil
}

// decodeCanonicalCandidateFiles decodes the exact bytes EncodeCandidateFiles
// writes: [{"path":"…","mode":"…"},…], whose strings hold no escape but the
// ones json.Marshal writes for <, >, &, U+2028 and U+2029. It answers false
// for anything else (whitespace, another member order, another escape, a
// control character, invalid UTF-8, trailing bytes), and the caller then
// decodes with encoding/json, so both paths give the same files.
func decodeCanonicalCandidateFiles(data []byte) ([]CandidateFile, bool) {
	const (
		open      = `{"path":"`
		separator = `","mode":"`
	)
	if len(data) < 2 || data[0] != '[' {
		return nil, false
	}
	files := make([]CandidateFile, 0, bytes.Count(data, []byte(separator)))
	rest := data[1:]
	if len(rest) == 1 && rest[0] == ']' {
		return files, true
	}
	for {
		if !bytes.HasPrefix(rest, []byte(open)) {
			return nil, false
		}
		rest = rest[len(open):]
		filePath, after, ok := canonicalString(rest)
		if !ok || !bytes.HasPrefix(after, []byte(separator[1:])) {
			return nil, false
		}
		rest = after[len(separator)-1:]
		mode, after, ok := canonicalString(rest)
		if !ok || len(after) < 2 || after[0] != '}' {
			return nil, false
		}
		files = append(files, CandidateFile{Path: filePath, Mode: mode})
		switch after[1] {
		case ',':
			rest = after[2:]
		case ']':
			if len(after) != 2 {
				return nil, false
			}
			return files, true
		default:
			return nil, false
		}
	}
}

// canonicalString reads a JSON string body up to its closing quote when the
// body is valid UTF-8 and holds no control character, no escape but the
// HTML-safe ones json.Marshal writes (htmlEscapes), and none of the runes
// json.Marshal escapes written raw. It returns the bytes after the quote.
func canonicalString(data []byte) (string, []byte, bool) {
	end := bytes.IndexByte(data, '"')
	if end < 0 {
		return "", nil, false
	}
	body := data[:end]
	escaped := false
	for _, b := range body {
		switch {
		case b < 0x20, b == '<', b == '>', b == '&':
			return "", nil, false
		case b == '\\':
			escaped = true
		}
	}
	if !utf8.Valid(body) || bytes.Contains(body, lineSeparator) || bytes.Contains(body, paragraphSeparator) {
		return "", nil, false
	}
	if !escaped {
		return string(body), data[end+1:], true
	}
	var text strings.Builder
	text.Grow(len(body))
	for len(body) > 0 {
		at := bytes.IndexByte(body, '\\')
		if at < 0 {
			text.Write(body)
			break
		}
		text.Write(body[:at])
		body = body[at:]
		if len(body) < len(`\u0026`) {
			return "", nil, false
		}
		r, ok := htmlEscapes[string(body[:len(`\u0026`)])]
		if !ok {
			return "", nil, false
		}
		text.WriteRune(r)
		body = body[len(`\u0026`):]
	}
	return text.String(), data[end+1:], true
}

// htmlEscapes are the escapes json.Marshal writes inside a string, as it
// writes them, and the rune each stands for.
var htmlEscapes = map[string]rune{
	`\u003c`: '<',
	`\u003e`: '>',
	`\u0026`: '&',
	`\u2028`: '\u2028',
	`\u2029`: '\u2029',
}

var (
	lineSeparator      = []byte("\u2028")
	paragraphSeparator = []byte("\u2029")
)
