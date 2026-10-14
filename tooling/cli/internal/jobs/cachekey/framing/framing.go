// Package framing frames the labeled fields a cache key or content digest
// hashes. Each field is written as its label's length, the label, the value's
// size, then the value, so no two different field lists hash the same bytes.
package framing

import (
	"bytes"
	"encoding/binary"
	"hash"
	"io"
)

// HashField writes value to h as one field named label.
func HashField(h hash.Hash, label string, value []byte) {
	_ = HashStream(h, label, int64(len(value)), bytes.NewReader(value))
}

// HashStream writes the size bytes reader yields to h as one field named
// label. Lengths and sizes are unsigned 64-bit little-endian integers.
func HashStream(h hash.Hash, label string, size int64, reader io.Reader) error {
	if err := binary.Write(h, binary.LittleEndian, uint64(len(label))); err != nil {
		return err
	}
	if _, err := io.WriteString(h, label); err != nil {
		return err
	}
	if err := binary.Write(h, binary.LittleEndian, uint64(size)); err != nil {
		return err
	}
	_, err := io.Copy(h, reader)
	return err
}
