package git

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // Git's own index checksum, not a security boundary
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// indexEntry is one entry of the index: its path, slash-separated and relative to
// the work tree's root, its mode, its object and its stage.
type indexEntry struct {
	Path   string
	Mode   uint32
	Object string
	Stage  int
	// SkipWorktree is the extended flag a sparse checkout sets.
	SkipWorktree bool
	// IntentToAdd is the extended flag `git add -N` sets.
	IntentToAdd bool
}

// The mode of a gitlink, a submodule's commit.
const modeGitlink = 0o160000

// localIndex is an index read in-process, its entries in Git's order: by path,
// then by stage.
type localIndex struct {
	Entries []indexEntry
	// split is whether the index is a split index.
	split bool
	// key is the size of the file read and its checksum (indexKey).
	key string
}

// unsupportedError says the in-process reader met a state it does not read;
// the caller then asks Git, which reads it or refuses it.
type unsupportedError struct{ reason string }

func (e *unsupportedError) Error() string { return "read by Git: " + e.reason }

func unsupported(format string, args ...any) error {
	return &unsupportedError{reason: fmt.Sprintf(format, args...)}
}

// The index flags of an on-disk entry (read-cache-ll.h).
const (
	ceNameMask     = 0x0fff
	ceStageMask    = 0x3000
	ceStageShift   = 12
	ceExtended     = 0x4000
	ceSkipWorktree = 1 << 30
	ceIntentToAdd  = 1 << 29
	ceExtendedMask = ceSkipWorktree | ceIntentToAdd
)

// readIndex reads the index file at path, whose objects take hashSize bytes,
// as Git 2.43 reads it: versions 2 to 4, the split index's link to its shared
// index in gitDir, and the optional extensions. A missing file is an empty
// index. It refuses, as unsupported, what it does not read: an unknown
// version, a required extension it does not know, a sparse index, an index
// without a checksum (index.skipHash). A checksum that does not match is
// unsupported too: Git 2.43 reads such an index without checking it, so the
// reader steps aside rather than read a file it cannot trust.
func readIndex(path string, hashSize int, gitDir string) (*localIndex, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &localIndex{}, nil
	}
	if err != nil {
		return nil, unsupported("read the index: %v", err)
	}
	index, link, err := parseIndex(data, hashSize)
	if err != nil {
		return nil, err
	}
	index.key = fmt.Sprintf("%d %x", len(data), data[len(data)-hashSize:])
	if link == nil {
		return index, nil
	}
	if link.base == "" {
		return index, nil
	}
	sharedData, err := os.ReadFile(filepath.Join(gitDir, "sharedindex."+link.base))
	if err != nil {
		return nil, unsupported("read the shared index: %v", err)
	}
	if hex.EncodeToString(sharedData[len(sharedData)-min(hashSize, len(sharedData)):]) != link.base {
		return nil, unsupported("the shared index is not the one the index names")
	}
	shared, sharedLink, err := parseIndex(sharedData, hashSize)
	if err != nil {
		return nil, err
	}
	if sharedLink != nil {
		return nil, unsupported("a shared index that links to another")
	}
	merged, err := mergeSplit(shared, index, link)
	if err != nil {
		return nil, err
	}
	merged.split, merged.key = true, index.key
	return merged, nil
}

// splitLink is the link extension of a split index: the shared index it
// names and its bitmaps.
type splitLink struct {
	base             string
	deleted, replace []int
	hasBitmaps       bool
}

// parseIndex parses one index file.
func parseIndex(data []byte, hashSize int) (*localIndex, *splitLink, error) {
	if len(data) < 12+hashSize {
		return nil, nil, unsupported("an index smaller than its header")
	}
	if !bytes.Equal(data[:4], []byte("DIRC")) {
		return nil, nil, unsupported("an index without its signature")
	}
	version := binary.BigEndian.Uint32(data[4:8])
	if version < 2 || version > 4 {
		return nil, nil, unsupported("index version %d", version)
	}
	trailer := data[len(data)-hashSize:]
	if bytes.Equal(trailer, make([]byte, hashSize)) {
		return nil, nil, unsupported("an index without a checksum (index.skipHash)")
	}
	var h hash.Hash
	if hashSize == sha256.Size {
		h = sha256.New()
	} else {
		h = sha1.New() //nolint:gosec // Git's index checksum
	}
	h.Write(data[:len(data)-hashSize])
	if !bytes.Equal(h.Sum(nil), trailer) {
		return nil, nil, unsupported("an index whose checksum does not match")
	}
	count := binary.BigEndian.Uint32(data[8:12])
	body := data[:len(data)-hashSize]
	offset := 12
	entries := make([]indexEntry, 0, count)
	var previous []byte
	for range count {
		entry, name, size, err := parseEntry(body, offset, version, hashSize, previous)
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, entry)
		previous = name
		offset += size
	}
	var link *splitLink
	for offset < len(body) {
		if offset+8 > len(body) {
			return nil, nil, unsupported("a truncated index extension")
		}
		signature := body[offset : offset+4]
		size := int(binary.BigEndian.Uint32(body[offset+4 : offset+8]))
		offset += 8
		if size < 0 || offset+size > len(body) {
			return nil, nil, unsupported("a truncated index extension")
		}
		payload := body[offset : offset+size]
		offset += size
		switch string(signature) {
		case "link":
			parsed, err := parseLink(payload, hashSize)
			if err != nil {
				return nil, nil, err
			}
			link = parsed
		case "sdir":
			return nil, nil, unsupported("a sparse index")
		default:
			if signature[0] < 'A' || signature[0] > 'Z' {
				return nil, nil, unsupported("the index extension %q", signature)
			}
		}
	}
	return &localIndex{Entries: entries}, link, nil
}

// parseEntry parses the entry at offset and returns it, its full name and
// its size on disk.
func parseEntry(body []byte, offset int, version uint32, hashSize int, previous []byte) (indexEntry, []byte, int, error) {
	fixed := 40 + hashSize + 2
	if offset+fixed > len(body) {
		return indexEntry{}, nil, 0, unsupported("a truncated index entry")
	}
	raw := body[offset:]
	mode := binary.BigEndian.Uint32(raw[24:28])
	object := hex.EncodeToString(raw[40 : 40+hashSize])
	flags := uint32(binary.BigEndian.Uint16(raw[40+hashSize : fixed]))
	nameStart := fixed
	if flags&ceExtended != 0 {
		if version < 3 || offset+fixed+2 > len(body) {
			return indexEntry{}, nil, 0, unsupported("an extended entry in index version %d", version)
		}
		extended := uint32(binary.BigEndian.Uint16(raw[fixed:fixed+2])) << 16
		if extended&^ceExtendedMask != 0 {
			return indexEntry{}, nil, 0, unsupported("unknown index entry flags 0x%08x", extended)
		}
		flags |= extended
		nameStart += 2
	}
	length := int(flags & ceNameMask)
	var name []byte
	var size int
	if version == 4 {
		strip, used, ok := decodeVarint(raw[nameStart:])
		if !ok {
			return indexEntry{}, nil, 0, unsupported("a malformed index name")
		}
		if strip > uint64(len(previous)) {
			return indexEntry{}, nil, 0, unsupported("a malformed index name")
		}
		suffixStart := nameStart + used
		end := bytes.IndexByte(raw[suffixStart:], 0)
		if end < 0 {
			return indexEntry{}, nil, 0, unsupported("an unterminated index name")
		}
		kept := previous[:len(previous)-int(strip)]
		name = make([]byte, 0, len(kept)+end)
		name = append(append(name, kept...), raw[suffixStart:suffixStart+end]...)
		if length != ceNameMask && length != len(name) {
			return indexEntry{}, nil, 0, unsupported("an index name whose length disagrees")
		}
		size = suffixStart + end + 1
	} else {
		end := bytes.IndexByte(raw[nameStart:], 0)
		if end < 0 {
			return indexEntry{}, nil, 0, unsupported("an unterminated index name")
		}
		name = raw[nameStart : nameStart+end]
		if length != ceNameMask && length != len(name) {
			return indexEntry{}, nil, 0, unsupported("an index name whose length disagrees")
		}
		size = (nameStart + len(name) + 8) &^ 7
		if offset+size > len(body) {
			return indexEntry{}, nil, 0, unsupported("a truncated index entry")
		}
	}
	return indexEntry{
		Path:         string(name),
		Mode:         mode,
		Object:       object,
		Stage:        int((flags & ceStageMask) >> ceStageShift),
		SkipWorktree: flags&ceSkipWorktree != 0,
		IntentToAdd:  flags&ceIntentToAdd != 0,
	}, name, size, nil
}

// decodeVarint decodes Git's offset varint (varint.c).
func decodeVarint(buf []byte) (uint64, int, bool) {
	if len(buf) == 0 {
		return 0, 0, false
	}
	i := 0
	c := buf[i]
	i++
	value := uint64(c & 127)
	for c&128 != 0 {
		if i >= len(buf) || value+1 == 0 || (value+1)>>57 != 0 {
			return 0, 0, false
		}
		value++
		c = buf[i]
		i++
		value = value<<7 + uint64(c&127)
	}
	return value, i, true
}

// parseLink parses the link extension of a split index.
func parseLink(payload []byte, hashSize int) (*splitLink, error) {
	if len(payload) < hashSize {
		return nil, unsupported("a corrupt link extension")
	}
	link := &splitLink{}
	if base := payload[:hashSize]; !bytes.Equal(base, make([]byte, hashSize)) {
		link.base = hex.EncodeToString(base)
	}
	rest := payload[hashSize:]
	if len(rest) == 0 {
		return link, nil
	}
	link.hasBitmaps = true
	deleted, used, err := readEWAH(rest)
	if err != nil {
		return nil, err
	}
	rest = rest[used:]
	replace, used, err := readEWAH(rest)
	if err != nil {
		return nil, err
	}
	if used != len(rest) {
		return nil, unsupported("garbage at the end of the link extension")
	}
	link.deleted, link.replace = deleted, replace
	return link, nil
}

// readEWAH reads one EWAH bitmap (ewah_io.c) and returns its set bits in
// order, and the bytes it took.
func readEWAH(data []byte) ([]int, int, error) {
	if len(data) < 8 {
		return nil, 0, unsupported("a corrupt EWAH bitmap")
	}
	words := int(binary.BigEndian.Uint32(data[4:8]))
	end := 8 + 8*words + 4
	if words < 0 || end > len(data) {
		return nil, 0, unsupported("a corrupt EWAH bitmap")
	}
	buffer := make([]uint64, words)
	for i := range buffer {
		buffer[i] = binary.BigEndian.Uint64(data[8+8*i:])
	}
	var bits []int
	pos, pointer := 0, 0
	for pointer < len(buffer) {
		word := buffer[pointer]
		running := int((word >> 1) & 0xffffffff)
		if word&1 != 0 {
			for range running * 64 {
				bits = append(bits, pos)
				pos++
			}
		} else {
			pos += running * 64
		}
		pointer++
		literals := int(word >> 33)
		if pointer+literals > len(buffer) {
			return nil, 0, unsupported("a corrupt EWAH bitmap")
		}
		for range literals {
			for c := range 64 {
				if buffer[pointer]&(1<<c) != 0 {
					bits = append(bits, pos)
				}
				pos++
			}
			pointer++
		}
	}
	return bits, end, nil
}

// mergeSplit merges a split index into its shared index, as Git's
// merge_base_index does.
func mergeSplit(shared, front *localIndex, link *splitLink) (*localIndex, error) {
	entries := slicesClone(shared.Entries)
	removed := make([]bool, len(entries))
	replacements := 0
	for _, pos := range link.replace {
		if pos >= len(entries) || replacements >= len(front.Entries) {
			return nil, unsupported("a corrupt link extension")
		}
		source := front.Entries[replacements]
		if source.Path != "" {
			return nil, unsupported("a corrupt link extension")
		}
		source.Path = entries[pos].Path
		entries[pos] = source
		replacements++
	}
	for _, pos := range link.deleted {
		if pos >= len(entries) {
			return nil, unsupported("a corrupt link extension")
		}
		removed[pos] = true
	}
	kept := entries[:0]
	for i, entry := range entries {
		if !removed[i] {
			kept = append(kept, entry)
		}
	}
	entries = kept
	for _, added := range front.Entries[replacements:] {
		if added.Path == "" {
			return nil, unsupported("a corrupt link extension")
		}
		entries = addEntry(entries, added)
	}
	return &localIndex{Entries: entries}, nil
}

// addEntry inserts entry in the sorted entries as Git's add_index_entry does
// with ADD_CACHE_OK_TO_ADD: it replaces the entry of the same path and stage,
// and a merged entry replaces the unmerged stages of its path.
func addEntry(entries []indexEntry, entry indexEntry) []indexEntry {
	pos := sort.Search(len(entries), func(i int) bool { return compareEntry(entries[i], entry) >= 0 })
	if pos < len(entries) && compareEntry(entries[pos], entry) == 0 {
		entries[pos] = entry
		return entries
	}
	if entry.Stage == 0 {
		for pos < len(entries) && entries[pos].Path == entry.Path {
			entries = append(entries[:pos], entries[pos+1:]...)
		}
	}
	entries = append(entries, indexEntry{})
	copy(entries[pos+1:], entries[pos:])
	entries[pos] = entry
	return entries
}

// compareEntry orders two entries as the index does: by path bytes, then by
// stage.
func compareEntry(a, b indexEntry) int {
	if c := bytes.Compare([]byte(a.Path), []byte(b.Path)); c != 0 {
		return c
	}
	return a.Stage - b.Stage
}

func slicesClone(entries []indexEntry) []indexEntry { return append([]indexEntry(nil), entries...) }

// ModeString is the mode as ls-files --stage prints it: six octal digits.
func (e indexEntry) ModeString() string { return fmt.Sprintf("%06o", e.Mode) }

// StageString is the stage as ls-files --stage prints it.
func (e indexEntry) StageString() string { return strconv.Itoa(e.Stage) }
