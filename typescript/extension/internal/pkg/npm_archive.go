package pkg

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.putnami.dev/typescript/extension/internal/project"
)

// NormalizeNPMArchive gives the npm archive at archive, packed from the staged
// package at packageDir on a host running goos, the entry modes a Unix host
// gives the same package: 0755 for a directory, 0755 for an executable file
// (see npmExecutables) and 0644 for any other file.
//
// On a Unix host the archive is left as it is: the packer reads the modes
// normalizeNPMPackageModes gave the staged files. A Windows filesystem keeps no
// execute bit, and bun writes 0666 for a file and 0777 for a bin entry there,
// so the modes are decided again from the package and written into the
// archive's headers. Only the mode and checksum fields change, so the
// uncompressed archive is byte-identical to the Unix one. The archive is
// compressed again, and Go's gzip encoder does not reproduce bun's, so the
// archive digest still differs from a Unix host's.
func NormalizeNPMArchive(goos, workspaceRoot, projectPath, packageDir, archive string) error {
	if goos != "windows" {
		return nil
	}
	buildOutputRoot := findBuildOutput(workspaceRoot, projectPath)
	if buildOutputRoot == "" {
		return fmt.Errorf("normalize npm archive modes: no build output for %s", projectPath)
	}
	pkg := project.ReadPackageJSONSafe(filepath.Join(packageDir, "package.json"))
	if pkg == nil {
		return fmt.Errorf("normalize npm archive modes: no package.json in %s", packageDir)
	}
	executables, err := npmExecutables(filepath.Join(buildOutputRoot, "lib"), pkg)
	if err != nil {
		return fmt.Errorf("normalize npm archive modes: %w", err)
	}
	if err := normalizeNPMArchiveModes(archive, executables); err != nil {
		return fmt.Errorf("normalize npm archive modes of %s: %w", archive, err)
	}
	return nil
}

// normalizeNPMArchiveModes rewrites the gzip-compressed tar archive at archive
// with the modes npmArchiveMode gives its entries. An archive whose modes are
// already those is left byte for byte as it is.
func normalizeNPMArchiveModes(archive string, executables map[string]struct{}) error {
	compressed, err := os.ReadFile(archive)
	if err != nil {
		return err
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return err
	}
	tarball, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if err := reader.Close(); err != nil {
		return err
	}
	changed, err := normalizeTarModes(tarball, executables)
	if err != nil || !changed {
		return err
	}
	var out bytes.Buffer
	writer, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return err
	}
	if _, err := writer.Write(tarball); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return os.WriteFile(archive, out.Bytes(), 0o644)
}

// tarBlockSize is the size of a tar header and the unit of its data.
const tarBlockSize = 512

// Offsets and lengths of the ustar header fields this file reads or writes.
const (
	tarNameEnd       = 100
	tarModeOffset    = 100
	tarModeLength    = 8
	tarSizeOffset    = 124
	tarSizeLength    = 12
	tarSumOffset     = 148
	tarSumLength     = 8
	tarTypeOffset    = 156
	tarMagicOffset   = 257
	tarPrefixOffset  = 345
	tarPrefixEnd     = 500
	tarUstarMagicLen = 6
)

// normalizeTarModes gives every entry of the uncompressed tar archive in
// tarball the mode npmArchiveMode decides, in place, and reports whether a
// header changed. A pax extended header takes the mode of the entry it
// describes, as libarchive writes it. The width and terminators of each
// rewritten field are kept, so nothing but the digits moves. An entry that is
// not a file, a directory or a pax extended header is refused: the staged
// package holds nothing else, and a guess would give a mode no Unix host gives.
func normalizeTarModes(tarball []byte, executables map[string]struct{}) (bool, error) {
	changed := false
	var pending []int // offsets of the pax headers of the next entry
	paxPath, paxSize := "", int64(-1)
	for offset := 0; offset+tarBlockSize <= len(tarball); {
		header := tarball[offset : offset+tarBlockSize]
		if isZeroBlock(header) {
			if len(pending) > 0 {
				return false, fmt.Errorf("pax header at offset %d describes no entry", pending[0])
			}
			return changed, nil
		}
		if err := checkTarChecksum(header); err != nil {
			return false, fmt.Errorf("tar header at offset %d: %w", offset, err)
		}
		size, err := tarHeaderSize(header)
		if err != nil {
			return false, fmt.Errorf("tar header at offset %d: %w", offset, err)
		}
		typeflag := header[tarTypeOffset]
		if typeflag == 'x' {
			records, err := tarData(tarball, offset, size)
			if err != nil {
				return false, err
			}
			if paxPath, paxSize, err = readPaxRecords(records, paxPath, paxSize); err != nil {
				return false, fmt.Errorf("pax header at offset %d: %w", offset, err)
			}
			pending = append(pending, offset)
			offset += tarBlockSize + tarPadded(size)
			continue
		}
		if paxSize >= 0 {
			size = paxSize
		}
		name := paxPath
		if name == "" {
			name = ustarName(header)
		}
		mode, err := npmArchiveMode(typeflag, name, executables)
		if err != nil {
			return false, fmt.Errorf("tar entry %q: %w", name, err)
		}
		for _, headerOffset := range append(pending, offset) {
			modified, err := setTarMode(tarball[headerOffset:headerOffset+tarBlockSize], mode)
			if err != nil {
				return false, fmt.Errorf("tar header at offset %d: %w", headerOffset, err)
			}
			changed = changed || modified
		}
		pending, paxPath, paxSize = nil, "", -1
		if _, err := tarData(tarball, offset, size); err != nil {
			return false, err
		}
		offset += tarBlockSize + tarPadded(size)
	}
	return false, errors.New("tar archive ends without its end-of-archive block")
}

// npmArchiveMode is the mode a Unix host gives an npm archive entry of type
// typeflag named name: the archive root directory, "package/" for bun and
// npm, is not part of a package path.
func npmArchiveMode(typeflag byte, name string, executables map[string]struct{}) (int64, error) {
	switch typeflag {
	case '5':
		return 0o755, nil
	case '0', 0, '7':
		rel := strings.TrimPrefix(name, "./")
		if _, inside, ok := strings.Cut(rel, "/"); ok {
			rel = inside
		}
		if _, executable := executables[rel]; executable {
			return 0o755, nil
		}
		return 0o644, nil
	default:
		return 0, fmt.Errorf("unsupported entry type %q", typeflag)
	}
}

// setTarMode writes mode into header's mode field and the checksum that
// follows from it, and reports whether the header changed.
func setTarMode(header []byte, mode int64) (bool, error) {
	before := bytes.Clone(header)
	if err := setOctalField(header[tarModeOffset:tarModeOffset+tarModeLength], mode); err != nil {
		return false, fmt.Errorf("mode field: %w", err)
	}
	if err := setOctalField(header[tarSumOffset:tarSumOffset+tarSumLength], tarChecksum(header)); err != nil {
		return false, fmt.Errorf("checksum field: %w", err)
	}
	return !bytes.Equal(before, header), nil
}

// setOctalField writes value over the octal digits of field, zero-padded to
// their width, and keeps the spaces and NULs around them.
func setOctalField(field []byte, value int64) error {
	start := 0
	for start < len(field) && field[start] == ' ' {
		start++
	}
	end := start
	for end < len(field) && field[end] >= '0' && field[end] <= '7' {
		end++
	}
	digits := strconv.FormatInt(value, 8)
	width := end - start
	if width == 0 || len(digits) > width {
		return fmt.Errorf("%q holds no room for %s", field, digits)
	}
	copy(field[start:end], strings.Repeat("0", width-len(digits))+digits)
	return nil
}

// tarChecksum is the unsigned sum of header's bytes with the checksum field
// counted as spaces.
func tarChecksum(header []byte) int64 {
	var sum int64
	for i, b := range header {
		if i >= tarSumOffset && i < tarSumOffset+tarSumLength {
			b = ' '
		}
		sum += int64(b)
	}
	return sum
}

func checkTarChecksum(header []byte) error {
	recorded, err := parseOctal(header[tarSumOffset : tarSumOffset+tarSumLength])
	if err != nil {
		return fmt.Errorf("checksum field: %w", err)
	}
	if recorded != tarChecksum(header) {
		return errors.New("checksum mismatch")
	}
	return nil
}

// tarHeaderSize reads the size field, in octal or in the base-256 form a
// large entry uses.
func tarHeaderSize(header []byte) (int64, error) {
	field := header[tarSizeOffset : tarSizeOffset+tarSizeLength]
	if field[0]&0x80 == 0 {
		return parseOctal(field)
	}
	if field[0] != 0x80 {
		return 0, errors.New("negative base-256 size")
	}
	var size int64
	for _, b := range field[1:] {
		if size > (1<<55)-1 {
			return 0, errors.New("size overflows")
		}
		size = size<<8 | int64(b)
	}
	return size, nil
}

func parseOctal(field []byte) (int64, error) {
	text := strings.Trim(string(field), " \x00")
	if text == "" {
		return 0, nil
	}
	return strconv.ParseInt(text, 8, 64)
}

// tarData returns the data of the entry whose header is at offset.
func tarData(tarball []byte, offset int, size int64) ([]byte, error) {
	start := int64(offset) + tarBlockSize
	if size < 0 || size > int64(len(tarball))-start {
		return nil, fmt.Errorf("tar entry at offset %d runs past the archive", offset)
	}
	return tarball[start : start+size], nil
}

func tarPadded(size int64) int {
	return int((size + tarBlockSize - 1) / tarBlockSize * tarBlockSize)
}

func isZeroBlock(block []byte) bool {
	for _, b := range block {
		if b != 0 {
			return false
		}
	}
	return true
}

// ustarName is the entry name of a ustar header: its prefix, when it has one,
// joined to its name.
func ustarName(header []byte) string {
	name := cString(header[:tarNameEnd])
	if string(header[tarMagicOffset:tarMagicOffset+tarUstarMagicLen]) != "ustar\x00" {
		return name
	}
	if prefix := cString(header[tarPrefixOffset:tarPrefixEnd]); prefix != "" {
		return prefix + "/" + name
	}
	return name
}

func cString(field []byte) string {
	if end := bytes.IndexByte(field, 0); end >= 0 {
		field = field[:end]
	}
	return string(field)
}

// readPaxRecords reads the "length key=value\n" records of a pax extended
// header and returns the path and size they set for the next entry, starting
// from path and size.
func readPaxRecords(records []byte, path string, size int64) (string, int64, error) {
	for len(records) > 0 {
		space := bytes.IndexByte(records, ' ')
		if space <= 0 {
			return "", 0, errors.New("malformed record")
		}
		length, err := strconv.Atoi(string(records[:space]))
		if err != nil || length <= space+1 || length > len(records) || records[length-1] != '\n' {
			return "", 0, errors.New("malformed record")
		}
		key, value, ok := strings.Cut(string(records[space+1:length-1]), "=")
		if !ok {
			return "", 0, errors.New("malformed record")
		}
		switch key {
		case "path":
			path = value
		case "size":
			if size, err = strconv.ParseInt(value, 10, 64); err != nil || size < 0 {
				return "", 0, errors.New("malformed size record")
			}
		}
		records = records[length:]
	}
	return path, size, nil
}
