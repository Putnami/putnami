package readers

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// maxLineBytes caps one line of a line-based report.
const maxLineBytes = 1 << 20

// ReadLCOV reads an LCOV tracefile. Each SF record adds to its file: DA lines,
// FN and FNDA functions, and the LF, LH, FNF and FNH totals. A record without
// end_of_record, a DA line outside a record, or a value that does not parse is
// malformed. Other record types are skipped.
func ReadLCOV(data []byte) (CoverageReport, []diag.Diagnostic) {
	if diagnostics := tooLarge(data); diagnostics != nil {
		return CoverageReport{}, diagnostics
	}
	builder := newCoverageBuilder()
	var (
		file              *fileBuilder
		lf, lh, fnf, fnh  *int
		number            int
		recordStartNumber int
	)
	malformedAt := func(format string, args ...any) (CoverageReport, []diag.Diagnostic) {
		return CoverageReport{}, malformed("LCOV line %d: "+format, append([]any{number}, args...)...)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for scanner.Scan() {
		number++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		key, value, _ := strings.Cut(line, ":")
		if key == "SF" {
			if file != nil {
				return malformedAt("SF opens a record before the record of line %d ends", recordStartNumber)
			}
			if value == "" {
				return malformedAt("SF names no file")
			}
			file = builder.file(value)
			lf, lh, fnf, fnh = nil, nil, nil, nil
			recordStartNumber = number
			continue
		}
		if file == nil {
			if key == "TN" || key == "VER" {
				continue
			}
			return malformedAt("%s outside a record", key)
		}
		switch key {
		case "DA":
			fields := strings.Split(value, ",")
			if len(fields) < 2 {
				return malformedAt("DA %q is not line,count", value)
			}
			lineNumber, err1 := strconv.Atoi(fields[0])
			hits, err2 := strconv.ParseInt(fields[1], 10, 64)
			if err1 != nil || err2 != nil {
				return malformedAt("DA %q is not line,count", value)
			}
			if diagnostics := checkLine("LCOV line "+strconv.Itoa(number), lineNumber, hits); diagnostics != nil {
				return CoverageReport{}, diagnostics
			}
			if diagnostics := file.line(lineNumber, hits); diagnostics != nil {
				return CoverageReport{}, diagnostics
			}
		case "FN":
			name, ok := functionName(value)
			if !ok {
				return malformedAt("FN %q is not line,name", value)
			}
			file.function(name, false)
		case "FNDA":
			count, name, ok := strings.Cut(value, ",")
			hits, err := strconv.ParseInt(count, 10, 64)
			if !ok || err != nil || hits < 0 || name == "" {
				return malformedAt("FNDA %q is not count,name", value)
			}
			file.function(name, hits > 0)
		case "LF", "LH", "FNF", "FNH":
			total, err := strconv.Atoi(value)
			if err != nil || total < 0 {
				return malformedAt("%s %q is not a count", key, value)
			}
			switch key {
			case "LF":
				lf = &total
			case "LH":
				lh = &total
			case "FNF":
				fnf = &total
			case "FNH":
				fnh = &total
			}
		case "end_of_record":
			if lf != nil || lh != nil {
				file.stateLines(Counts{Total: deref(lf), Covered: deref(lh)})
			}
			if fnf != nil || fnh != nil {
				file.stateFunctions(Counts{Total: deref(fnf), Covered: deref(fnh)})
			}
			file = nil
		}
	}
	if err := scanner.Err(); err != nil {
		return CoverageReport{}, malformed("LCOV line %d: %v", number+1, err)
	}
	if file != nil {
		return CoverageReport{}, malformed("LCOV record of line %d has no end_of_record", recordStartNumber)
	}
	return builder.report(nil), nil
}

// functionName reads FN:<line>,<name> and FN:<line>,<end line>,<name>.
func functionName(value string) (string, bool) {
	start, rest, ok := strings.Cut(value, ",")
	if !ok {
		return "", false
	}
	if _, err := strconv.Atoi(start); err != nil {
		return "", false
	}
	if end, name, ok := strings.Cut(rest, ","); ok {
		if _, err := strconv.Atoi(end); err == nil {
			rest = name
		}
	}
	return rest, rest != ""
}

func deref(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}
