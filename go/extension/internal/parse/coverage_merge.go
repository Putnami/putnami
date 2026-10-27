package parse

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MergeCoverageBlocks returns the lines of a Go coverage profile, without its
// mode line, with each block listed once, in the order the blocks first
// appear.
//
// `go test -coverpkg` writes the blocks of every package it instruments once
// for each test binary, so a module with many test packages lists each block
// once per test package. The lines of one block are merged the way
// `go tool cover` merges them: in set mode the counts are ORed, in count and
// atomic mode they add. The coverage a profile reports, and the HTML report
// `go tool cover` renders from it, therefore do not change.
//
// A block listed once keeps its line byte for byte. A line that is not a block
// line is kept as it is, in its place. Two lines that name one location with
// different statement counts are not the same block, and both stay, so
// `go tool cover` still reports the inconsistency.
func MergeCoverageBlocks(mode string, lines []string) []string {
	type entry struct {
		line   string
		block  string
		count  int64
		merged bool
	}
	entries := make([]*entry, 0, len(lines))
	byBlock := make(map[string]*entry, len(lines))
	for _, line := range lines {
		location, statements, count, ok := coverageBlockFields(line)
		if !ok {
			entries = append(entries, &entry{line: line})
			continue
		}
		block := location + " " + statements
		first, seen := byBlock[block]
		if !seen {
			first = &entry{line: line, block: block, count: count}
			byBlock[block] = first
			entries = append(entries, first)
			continue
		}
		if mode == "set" {
			first.count |= count
		} else {
			first.count += count
		}
		first.merged = true
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		if !e.merged {
			out[i] = e.line
			continue
		}
		out[i] = e.block + " " + strconv.FormatInt(e.count, 10)
	}
	return out
}

// MergeCoverageProfile rewrites the Go coverage profile at path so that it
// lists each block once (MergeCoverageBlocks). The file is replaced by a
// rename, so a failure leaves the profile as `go test` wrote it. A profile
// without a repeated block is left untouched.
func MergeCoverageProfile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var modeLine string
	var lines []string
	scanner := LineScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "mode:") {
			if modeLine == "" {
				modeLine = line
			}
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if modeLine == "" {
		// Not a profile this function knows how to write back.
		return nil
	}
	merged := MergeCoverageBlocks(CoverageMode(modeLine), lines)
	if len(merged) == len(lines) {
		return nil
	}
	var out strings.Builder
	out.WriteString(modeLine)
	out.WriteByte('\n')
	for _, line := range merged {
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return replaceFile(path, []byte(out.String()))
}

// CoverageMode returns the mode a profile's mode line names: set, count or
// atomic.
func CoverageMode(modeLine string) string {
	return strings.TrimSpace(strings.TrimPrefix(modeLine, "mode:"))
}

// coverageBlockFields splits a block line, "file.go:line.col,line.col
// statements count", into its location, its statement count as written, and
// its count. ok is false for any other line.
func coverageBlockFields(line string) (location, statements string, count int64, ok bool) {
	countAt := strings.LastIndexByte(line, ' ')
	if countAt <= 0 {
		return "", "", 0, false
	}
	statementsAt := strings.LastIndexByte(line[:countAt], ' ')
	if statementsAt <= 0 {
		return "", "", 0, false
	}
	location, statements = line[:statementsAt], line[statementsAt+1:countAt]
	if !strings.Contains(location, ":") {
		return "", "", 0, false
	}
	if _, err := strconv.Atoi(statements); err != nil {
		return "", "", 0, false
	}
	count, err := strconv.ParseInt(line[countAt+1:], 10, 64)
	if err != nil || count < 0 {
		return "", "", 0, false
	}
	return location, statements, count, true
}

// replaceFile writes data to a temporary file next to path and renames it over
// path.
func replaceFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
