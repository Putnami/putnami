package features

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"math"
	"reflect"
	"sync"

	diag "go.putnami.dev/protocol/diagnostic"
)

// manifestMemoCapacity bounds the manifest findings memo. A workspace holds
// one entry per distinct manifest content, far below it; a process that sees
// more distinct contents drops every entry and starts over, so the memo never
// holds more than this many answers.
const manifestMemoCapacity = 4096

// maxContentKeyDepth bounds the walk of a value's content. The documents this
// package validates nest a few levels deep and hold no cycle; a deeper value
// is not memoized.
const maxContentKeyDepth = 32

// manifestFindings memoizes ValidateManifest for this process. It is keyed on
// the manifest's content, every field included, and never on where the
// manifest came from, so a hit answers what a fresh validation of the same
// content answers. Nothing persists past the process.
var manifestFindings = newFindingsMemo(manifestMemoCapacity)

// findingsMemo holds validation findings by content key. It is safe for
// concurrent use: a caller always gets its own copy, so it may change the
// findings it holds, as callers that prefix a field do, without changing
// what the memo or another caller holds.
type findingsMemo struct {
	mu       sync.Mutex
	capacity int
	entries  map[[sha256.Size]byte][]diag.Diagnostic
}

func newFindingsMemo(capacity int) *findingsMemo {
	return &findingsMemo{capacity: capacity, entries: map[[sha256.Size]byte][]diag.Diagnostic{}}
}

// findings returns the memoized answer for key, or validate's answer, which it
// keeps. Two callers that miss the same key both validate; their answers are
// equal, since validate is a function of the content the key covers.
func (memo *findingsMemo) findings(key [sha256.Size]byte, validate func() []diag.Diagnostic) []diag.Diagnostic {
	memo.mu.Lock()
	held, ok := memo.entries[key]
	memo.mu.Unlock()
	if ok {
		return copyFindings(held)
	}
	fresh := validate()
	memo.mu.Lock()
	if len(memo.entries) >= memo.capacity {
		memo.entries = map[[sha256.Size]byte][]diag.Diagnostic{}
	}
	memo.entries[key] = copyFindings(fresh)
	memo.mu.Unlock()
	return fresh
}

// copyFindings copies findings, keeping a nil answer nil. A diagnostic holds
// no reference, so the copy shares nothing with findings.
func copyFindings(findings []diag.Diagnostic) []diag.Diagnostic {
	if findings == nil {
		return nil
	}
	out := make([]diag.Diagnostic, len(findings))
	copy(out, findings)
	return out
}

// contentKey is the SHA-256 of an encoding of value that holds every field,
// exported or not, a nil apart from an empty list, and every string's exact
// bytes. The encoding is injective for one type, and the key names the type,
// so two values share a key only when a validator reads the same content from
// both. ok is false for a value whose content the walk does not encode: a map,
// an interface, a function, a channel, or one nested beyond
// maxContentKeyDepth. That value is validated fresh.
func contentKey(domain string, value any) (key [sha256.Size]byte, ok bool) {
	v := reflect.ValueOf(value)
	encoder := contentEncoder{hash: sha256.New(), buffer: make([]byte, 0, contentEncoderBuffer)}
	encoder.text(domain)
	encoder.text(v.Type().String())
	if !encoder.value(v, 0) {
		return key, false
	}
	encoder.flush()
	encoder.hash.Sum(key[:0])
	return key, true
}

// contentEncoderBuffer is how many encoded bytes the encoder gathers before it
// hashes them, so a large document is hashed in chunks instead of encoded
// whole first.
const contentEncoderBuffer = 4096

type contentEncoder struct {
	hash   hash.Hash
	buffer []byte
}

func (encoder *contentEncoder) flush() {
	encoder.hash.Write(encoder.buffer)
	encoder.buffer = encoder.buffer[:0]
}

func (encoder *contentEncoder) text(value string) {
	encoder.buffer = binary.AppendUvarint(encoder.buffer, uint64(len(value)))
	if len(encoder.buffer)+len(value) > contentEncoderBuffer {
		encoder.flush()
		encoder.hash.Write([]byte(value))
		return
	}
	encoder.buffer = append(encoder.buffer, value...)
}

func (encoder *contentEncoder) value(v reflect.Value, depth int) bool {
	if depth > maxContentKeyDepth {
		return false
	}
	if len(encoder.buffer) > contentEncoderBuffer-2*binary.MaxVarintLen64 {
		encoder.flush()
	}
	switch v.Kind() {
	case reflect.Bool:
		if v.Bool() {
			encoder.buffer = append(encoder.buffer, 'T')
		} else {
			encoder.buffer = append(encoder.buffer, 'F')
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		encoder.buffer = binary.AppendVarint(append(encoder.buffer, 'i'), v.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		encoder.buffer = binary.AppendUvarint(append(encoder.buffer, 'u'), v.Uint())
	case reflect.Float32, reflect.Float64:
		encoder.buffer = binary.BigEndian.AppendUint64(append(encoder.buffer, 'f'), math.Float64bits(v.Float()))
	case reflect.String:
		encoder.buffer = append(encoder.buffer, 's')
		encoder.text(v.String())
	case reflect.Pointer:
		if v.IsNil() {
			encoder.buffer = append(encoder.buffer, 'n')
			return true
		}
		encoder.buffer = append(encoder.buffer, 'p')
		return encoder.value(v.Elem(), depth+1)
	case reflect.Slice:
		if v.IsNil() {
			encoder.buffer = append(encoder.buffer, 'n')
			return true
		}
		encoder.buffer = binary.AppendUvarint(append(encoder.buffer, 'l'), uint64(v.Len()))
		for i := 0; i < v.Len(); i++ {
			if !encoder.value(v.Index(i), depth+1) {
				return false
			}
		}
	case reflect.Array:
		encoder.buffer = binary.AppendUvarint(append(encoder.buffer, 'a'), uint64(v.Len()))
		for i := 0; i < v.Len(); i++ {
			if !encoder.value(v.Index(i), depth+1) {
				return false
			}
		}
	case reflect.Struct:
		encoder.buffer = binary.AppendUvarint(append(encoder.buffer, 'r'), uint64(v.NumField()))
		for i := 0; i < v.NumField(); i++ {
			if !encoder.value(v.Field(i), depth+1) {
				return false
			}
		}
	default:
		return false
	}
	return true
}
