package readers

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Go cover profile modes.
const (
	goCoverSet    = "set"
	goCoverCount  = "count"
	goCoverAtomic = "atomic"
)

// goCoverBlock identifies one block: its file, its span and its statements.
type goCoverBlock struct {
	path                        string
	startLine, startCol         int
	endLine, endCol, statements int
}

// ReadGoCover reads a Go cover profile: a mode line, then one
// path:startLine.startCol,endLine.endCol statements count line per block. A
// profile concatenated from several runs repeats the mode line; every mode
// line names the same mode. A block stated more than once sums its counts in
// count and atomic mode and keeps the largest in set mode. A block with
// statements marks each line from its start line to its end line, and a line
// takes the largest count of the blocks that span it.
func ReadGoCover(data []byte) (CoverageReport, []diag.Diagnostic) {
	if diagnostics := tooLarge(data); diagnostics != nil {
		return CoverageReport{}, diagnostics
	}
	var (
		mode   string
		number int
		order  []goCoverBlock
		counts = map[goCoverBlock]int64{}
	)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for scanner.Scan() {
		number++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if value, ok := strings.CutPrefix(line, "mode:"); ok {
			value = strings.TrimSpace(value)
			if value != goCoverSet && value != goCoverCount && value != goCoverAtomic {
				return CoverageReport{}, malformed("Go cover profile line %d: mode %q is not set, count or atomic", number, value)
			}
			if mode != "" && value != mode {
				return CoverageReport{}, malformed("Go cover profile line %d: mode %s follows mode %s", number, value, mode)
			}
			mode = value
			continue
		}
		if mode == "" {
			return CoverageReport{}, malformed("Go cover profile line %d: a block before the mode line", number)
		}
		block, count, ok := goCoverLine(line)
		if !ok {
			return CoverageReport{}, malformed("Go cover profile line %d: %q is not path:start.col,end.col statements count", number, line)
		}
		current, seen := counts[block]
		switch {
		case !seen:
			order = append(order, block)
			counts[block] = count
		case mode == goCoverSet:
			counts[block] = max(current, count)
		default:
			counts[block] = saturatingAdd(current, count)
		}
	}
	if err := scanner.Err(); err != nil {
		return CoverageReport{}, malformed("Go cover profile line %d: %v", number+1, err)
	}
	if mode == "" {
		return CoverageReport{}, malformed("Go cover profile has no mode line")
	}
	builder := newCoverageBuilder()
	for _, block := range order {
		file := builder.file(block.path)
		if block.statements == 0 {
			continue
		}
		for line := block.startLine; line <= block.endLine; line++ {
			file.line(line, counts[block])
		}
	}
	return builder.report(nil), nil
}

// goCoverLine parses path:startLine.startCol,endLine.endCol statements count.
func goCoverLine(line string) (goCoverBlock, int64, bool) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return goCoverBlock{}, 0, false
	}
	colon := strings.LastIndex(fields[0], ":")
	if colon <= 0 {
		return goCoverBlock{}, 0, false
	}
	path, span := fields[0][:colon], fields[0][colon+1:]
	start, end, ok := strings.Cut(span, ",")
	if !ok {
		return goCoverBlock{}, 0, false
	}
	startLine, startCol, ok1 := goCoverPosition(start)
	endLine, endCol, ok2 := goCoverPosition(end)
	statements, err1 := strconv.Atoi(fields[1])
	count, err2 := strconv.ParseInt(fields[2], 10, 64)
	if !ok1 || !ok2 || err1 != nil || err2 != nil || statements < 0 || count < 0 || endLine < startLine {
		return goCoverBlock{}, 0, false
	}
	block := goCoverBlock{path: path, startLine: startLine, startCol: startCol, endLine: endLine, endCol: endCol, statements: statements}
	return block, count, true
}

func goCoverPosition(position string) (int, int, bool) {
	line, col, ok := strings.Cut(position, ".")
	if !ok {
		return 0, 0, false
	}
	lineNumber, err1 := strconv.Atoi(line)
	column, err2 := strconv.Atoi(col)
	if err1 != nil || err2 != nil || lineNumber < 1 || column < 1 {
		return 0, 0, false
	}
	return lineNumber, column, true
}

func saturatingAdd(a, b int64) int64 {
	if a > (1<<63-1)-b {
		return 1<<63 - 1
	}
	return a + b
}
