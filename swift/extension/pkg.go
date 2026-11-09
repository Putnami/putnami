package swift

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
)

// maxPkgEntry bounds a file read out of a macOS package: swiftly is 26 MB.
const maxPkgEntry = 256 << 20

// extractPkgFile writes the file name of a flat macOS package's payload to
// dest, executable, with no installer: the package is a xar archive whose
// Payload is a gzip-compressed odc cpio archive. A package that nests
// components reads the first Payload. The package was checked against its
// pinned SHA-256 before.
func extractPkgFile(pkgPath, name, dest string) error {
	f, err := os.Open(pkgPath) //nolint:gosec // a package the extension downloaded and checked
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	payload, err := xarFile(f, "Payload")
	if err != nil {
		return err
	}
	content, err := cpioFile(payload, name)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, content, 0o755) //nolint:gosec // an executable
}

// xarHeader is the fixed header of a xar archive.
type xarHeader struct {
	Magic             [4]byte
	Size              uint16
	Version           uint16
	TOCCompressed     uint64
	TOCUncompressed   uint64
	ChecksumAlgorithm uint32
}

type xarEntry struct {
	Name  string     `xml:"name"`
	Data  *xarData   `xml:"data"`
	Files []xarEntry `xml:"file"`
}

type xarData struct {
	Offset   int64 `xml:"offset"`
	Length   int64 `xml:"length"`
	Encoding struct {
		Style string `xml:"style,attr"`
	} `xml:"encoding"`
}

// xarFile returns a reader of the first file named name in a xar archive,
// its data decoded.
func xarFile(f io.ReaderAt, name string) (io.Reader, error) {
	var h xarHeader
	if err := binary.Read(io.NewSectionReader(f, 0, 28), binary.BigEndian, &h); err != nil {
		return nil, fmt.Errorf("read the xar header: %w", err)
	}
	if string(h.Magic[:]) != "xar!" || h.TOCCompressed > maxPkgEntry {
		return nil, errors.New("not a xar archive")
	}
	tocReader, err := zlib.NewReader(io.NewSectionReader(f, int64(h.Size), int64(h.TOCCompressed)))
	if err != nil {
		return nil, fmt.Errorf("read the xar table of contents: %w", err)
	}
	var toc struct {
		Files []xarEntry `xml:"toc>file"`
	}
	if err := xml.NewDecoder(io.LimitReader(tocReader, maxPkgEntry)).Decode(&toc); err != nil {
		return nil, fmt.Errorf("read the xar table of contents: %w", err)
	}
	entry, ok := findXarEntry(toc.Files, name)
	if !ok || entry.Data == nil || entry.Data.Length > maxPkgEntry {
		return nil, fmt.Errorf("the package holds no %s", name)
	}
	heap := int64(h.Size) + int64(h.TOCCompressed)
	data := io.Reader(io.NewSectionReader(f, heap+entry.Data.Offset, entry.Data.Length))
	switch style := entry.Data.Encoding.Style; style {
	case "", "application/octet-stream":
	case "application/x-gzip":
		// xar's gzip encoding is a zlib stream.
		if data, err = zlib.NewReader(data); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("the package's %s is encoded as %s", name, style)
	}
	return data, nil
}

func findXarEntry(entries []xarEntry, name string) (xarEntry, bool) {
	for _, e := range entries {
		if e.Name == name && e.Data != nil {
			return e, true
		}
		if found, ok := findXarEntry(e.Files, name); ok {
			return found, true
		}
	}
	return xarEntry{}, false
}

// cpioFile returns the content of the file name, relative to the archive's
// root, in a payload: an odc cpio archive, gzip-compressed or not.
func cpioFile(payload io.Reader, name string) ([]byte, error) {
	r := bufio.NewReader(payload)
	if magic, err := r.Peek(2); err == nil && bytes.Equal(magic, []byte{0x1f, 0x8b}) {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		r = bufio.NewReader(gz)
	}
	want := path.Clean(name)
	header := make([]byte, 76)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			return nil, fmt.Errorf("read the package payload: %w", err)
		}
		if string(header[:6]) != "070707" {
			return nil, errors.New("the package payload is no odc cpio archive")
		}
		nameSize, err1 := strconv.ParseInt(string(header[59:65]), 8, 64)
		fileSize, err2 := strconv.ParseInt(string(header[65:76]), 8, 64)
		if err1 != nil || err2 != nil || nameSize < 1 || nameSize > 4096 || fileSize < 0 || fileSize > maxPkgEntry {
			return nil, errors.New("the package payload holds a malformed entry")
		}
		rawName := make([]byte, nameSize)
		if _, err := io.ReadFull(r, rawName); err != nil {
			return nil, err
		}
		entry := strings.TrimSuffix(string(rawName), "\x00")
		if entry == "TRAILER!!!" {
			return nil, fmt.Errorf("the package holds no %s", name)
		}
		if path.Clean(strings.TrimPrefix(entry, "./")) == want {
			content := make([]byte, fileSize)
			if _, err := io.ReadFull(r, content); err != nil {
				return nil, err
			}
			return content, nil
		}
		if _, err := io.CopyN(io.Discard, r, fileSize); err != nil {
			return nil, err
		}
	}
}
