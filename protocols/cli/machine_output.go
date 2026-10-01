package cli

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

const machineOutputRedacted = "[REDACTED]"

var machineOutputSensitiveMembers = map[string]bool{
	"authorization":      true,
	"proxyauthorization": true,
	"cookie":             true,
	"setcookie":          true,
	"token":              true,
	"accesstoken":        true,
	"refreshtoken":       true,
	"password":           true,
	"secret":             true,
	"apikey":             true,
	"clientsecret":       true,
	"privatekey":         true,
	"secretkey":          true,
	"credential":         true,
	"credentials":        true,
}

var (
	machineOutputPrivateKeyHeader = regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----`)
	machineOutputTokenPatterns    = []*regexp.Regexp{
		regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`),
		regexp.MustCompile(`gh[pousr]_[0-9A-Za-z]{36,255}`),
		regexp.MustCompile(`github_pat_[0-9A-Za-z_]{20,255}`),
		regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),
		regexp.MustCompile(`xox[baprs]-[0-9A-Za-z-]{10,}`),
		regexp.MustCompile(`sk_(?:live|test)_[0-9A-Za-z]{16,}`),
	}
)

// SanitizeMachineOutputValue returns a recursively sanitized copy of a decoded
// JSON value. Member names are retained; they are compared as lowercase ASCII
// after removing '-', '_' and '.' only to detect sensitive members, whose
// values become "[REDACTED]". Every other string value is made valid UTF-8,
// ECMA-48 terminal escape sequences are removed, and remaining C0/C1 controls
// except HT, LF and CR become U+FFFD.
func SanitizeMachineOutputValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for name, child := range typed {
			if machineOutputSensitiveMembers[normalizeSensitiveMember(name)] {
				out[name] = machineOutputRedacted
				continue
			}
			out[name] = SanitizeMachineOutputValue(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = SanitizeMachineOutputValue(child)
		}
		return out
	case string:
		return SanitizeMachineOutputString(typed)
	default:
		return value
	}
}

// SanitizeMachineOutputString applies the string portion of sanitization v1.
func SanitizeMachineOutputString(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	var out strings.Builder
	for i := 0; i < len(value); {
		if value[i] == 0x1b {
			i = skipTerminalEscape(value, i)
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == 0x9b { // Eight-bit CSI.
			i = skipTerminalCSI(value, i+size)
			continue
		}
		if isTerminalStringIntroducer(r) { // Eight-bit DCS/SOS/OSC/PM/APC.
			i = skipTerminalString(value, i+size)
			continue
		}
		if isUnsafeMachineControl(r) {
			out.WriteRune('\uFFFD')
		} else {
			out.WriteRune(r)
		}
		i += size
	}
	sanitized := out.String()
	if machineOutputPrivateKeyHeader.MatchString(sanitized) {
		return machineOutputRedacted
	}
	for _, pattern := range machineOutputTokenPatterns {
		sanitized = pattern.ReplaceAllString(sanitized, machineOutputRedacted)
	}
	return sanitized
}

func skipTerminalEscape(value string, start int) int {
	if start+1 >= len(value) {
		return len(value)
	}
	next := value[start+1]
	switch next {
	case '[': // CSI: parameters/intermediates followed by one final byte.
		return skipTerminalCSI(value, start+2)
	case ']', 'P', 'X', '^', '_': // OSC/DCS/SOS/PM/APC through BEL or ST.
		return skipTerminalString(value, start+2)
	default:
		// ECMA-48 two-byte Fe escape; an unknown following byte is preserved
		// while ESC itself is removed.
		if next >= 0x40 && next <= 0x5f {
			return start + 2
		}
		return start + 1
	}
}

func skipTerminalCSI(value string, start int) int {
	for i := start; i < len(value); i++ {
		if value[i] >= 0x40 && value[i] <= 0x7e {
			return i + 1
		}
	}
	return len(value)
}

func skipTerminalString(value string, start int) int {
	for i := start; i < len(value); {
		if value[i] == 0x07 {
			return i + 1
		}
		if value[i] == 0x1b && i+1 < len(value) && value[i+1] == '\\' {
			return i + 2
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == 0x9c { // Eight-bit ST.
			return i + size
		}
		i += size
	}
	return len(value)
}

func isTerminalStringIntroducer(r rune) bool {
	return r == 0x90 || r == 0x98 || r == 0x9d || r == 0x9e || r == 0x9f
}

func isUnsafeMachineControl(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' {
		return false
	}
	return r < 0x20 || (r >= 0x7f && r <= 0x9f)
}

func normalizeSensitiveMember(name string) string {
	var out strings.Builder
	for _, r := range name {
		switch r {
		case '-', '_', '.':
			continue
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		out.WriteRune(r)
	}
	return out.String()
}

func checkMachineOutputSanitized(v *validator, path string, value any) {
	switch typed := value.(type) {
	case map[string]any:
		for _, name := range sortedKeys(typed) {
			childPath := join(path, name)
			if machineOutputSensitiveMembers[normalizeSensitiveMember(name)] {
				if text, ok := typed[name].(string); !ok || text != machineOutputRedacted {
					v.add(ViolationUnsanitized, childPath)
				}
				continue
			}
			checkMachineOutputSanitized(v, childPath, typed[name])
		}
	case []any:
		for i, child := range typed {
			checkMachineOutputSanitized(v, indexPath(path, i), child)
		}
	case string:
		if SanitizeMachineOutputString(typed) != typed {
			v.add(ViolationUnsanitized, path)
		}
	}
}

// IsMachineOutputFailurePriority reports whether a record consumes the
// dedicated protected reserve. It principally retains failure evidence; the
// successful managed-publication handoff also uses it because that terminal
// control record arrives only after ordinary capacity may be exhausted.
// Within each partition records keep arrival order. A test:case record is
// never failure priority: the failed task's task:end already is.
func IsMachineOutputFailurePriority(record SessionStreamRecord) bool {
	if record.Record == RecordTaskEnd && record.Task != nil {
		return record.Task.Status == TaskStatusFailed || record.Task.Status == TaskStatusCanceled
	}
	return record.Record == RecordTaskEvent && failurePriorityEvent(record.Event)
}

// IsMachineOutputDebugDetail reports whether a record is debug-level task
// detail: a task:event whose level is debug, or any test:case. Normal live streams elide this class before spending their ordinary
// partition; verbose streams admit it under their larger fixed budget. Failure
// priority always takes precedence when a malformed or future event satisfies
// both classifications.
func IsMachineOutputDebugDetail(record SessionStreamRecord) bool {
	if record.Record == RecordTestCase {
		return true
	}
	return record.Record == RecordTaskEvent && debugDetailEvent(record.Event)
}

func failurePriorityRecord(root map[string]any) bool {
	record, _ := childString(root, "record")
	if record == RecordTaskEnd {
		status, _ := childString(childObject(root, "task"), "status")
		return status == TaskStatusFailed || status == TaskStatusCanceled
	}
	return record == RecordTaskEvent && failurePriorityEvent(childObject(root, "event"))
}

// debugDetailRecord is IsMachineOutputDebugDetail over a decoded record. The
// two must agree, since whole-stream validation recomputes the producer's
// selection with this one.
func debugDetailRecord(root map[string]any) bool {
	record, _ := childString(root, "record")
	if record == RecordTestCase {
		return true
	}
	return record == RecordTaskEvent && debugDetailEvent(childObject(root, "event"))
}

func debugDetailEvent(event map[string]any) bool {
	level, _ := childString(event, "level")
	return level == "debug"
}

func failurePriorityEvent(event map[string]any) bool {
	if event == nil {
		return false
	}
	if level, _ := childString(event, "level"); level == "error" {
		return true
	}
	typeName, _ := childString(event, "type")
	switch typeName {
	case "diagnostic":
		severity, _ := childString(event, "severity")
		return severity == "error"
	case "phase":
		status, _ := childString(event, "status")
		return status == "failed"
	case "result":
		result := childObject(event, "data")
		status, present := childString(result, "status")
		if !present {
			// The runtime wire nests the result under data. The CLI's documented
			// task:event projection flattens that object, so whole-stream
			// validation must classify both equivalent representations alike.
			status, _ = childString(event, "status")
		}
		if status == "FAILED" {
			return true
		}
		// A successful managed publication's immutable release-set reference is
		// deployment-control evidence. It is produced only at Finish, after the
		// ordinary partition may already be exhausted, so it consumes the same
		// bounded protected reserve as terminal failure evidence. Runtime owns
		// the nested result wire; this protocol only classifies the opaque key.
		releaseSet := childObject(childObject(result, "data"), "releaseSet")
		return status == "OK" && releaseSet != nil
	default:
		return false
	}
}

// checkFinalRecordReserve enforces the dedicated 16 KiB terminal reserve on a
// standalone session:end document using compact JSON plus one LF.
func checkFinalRecordReserve(v *validator, root map[string]any) {
	if record, _ := childString(root, "record"); record != RecordSessionEnd {
		return
	}
	if _, bounded := childValue(root, "machineOutput"); !bounded {
		return
	}
	line, err := compactMachineOutputLine(root)
	if err != nil || len(line) > MachineOutputFinalReserveBytes {
		v.add(ViolationBudgetExceeded, "machineOutput")
	}
}

func compactMachineOutputLine(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type machineOutputLine struct {
	raw  []byte
	root map[string]any
}

// ValidateSessionStream validates a complete bounded live JSONL sequence
// against the complete sanitized events.jsonl session artifact. This is the
// whole-sequence check a per-line JSON schema cannot express: total bytes and
// records, mandatory final record, deterministic partition selection and exact
// split elision accounting.
func ValidateSessionStream(live, artifact []byte) []Violation {
	v := &validator{}
	liveLines := parseMachineOutputSequence(v, "live", live)
	artifactLines := parseMachineOutputSequence(v, "artifact", artifact)
	liveFinal := terminalMachineOutputLine(v, "live", liveLines)
	artifactFinal := terminalMachineOutputLine(v, "artifact", artifactLines)
	if liveFinal == nil || artifactFinal == nil {
		return v.sorted()
	}
	if !bytes.Equal(liveFinal.raw, artifactFinal.raw) {
		v.add(ViolationElisionMismatch, "stream.final")
	}

	summary := childObject(liveFinal.root, "machineOutput")
	mode, hasMode := childString(summary, "mode")
	budget, known := MachineOutputBudgetFor(mode)
	if !hasMode || !known {
		return v.sorted()
	}
	if int64(len(live)) > budget.MaxBytes {
		v.add(ViolationBudgetExceeded, "stream.bytes")
	}
	if len(liveLines) > budget.MaxRecords {
		v.add(ViolationBudgetExceeded, "stream.records")
	}
	if len(liveFinal.raw)+1 > int(budget.FinalReserveBytes) {
		v.add(ViolationBudgetExceeded, "stream.final.bytes")
	}

	wantLive, ordinaryElided, failureElided := selectMachineOutput(artifactLines[:len(artifactLines)-1], mode, budget)
	wantLive = append(wantLive, *artifactFinal)
	if !sameMachineOutputLines(liveLines, wantLive) {
		v.add(ViolationElisionMismatch, "stream.selection")
	}
	checkElision(v, summary, "ordinary", ordinaryElided)
	checkElision(v, summary, "failure", failureElided)
	return v.sorted()
}

func parseMachineOutputSequence(v *validator, prefix string, data []byte) []machineOutputLine {
	if len(data) == 0 {
		v.add(ViolationMissingField, prefix+".session:end")
		return nil
	}
	if data[len(data)-1] != '\n' {
		v.add(ViolationInvalidValue, prefix+".framing")
	}
	trimmed := bytes.TrimSuffix(data, []byte{'\n'})
	rawLines := bytes.Split(trimmed, []byte{'\n'})
	lines := make([]machineOutputLine, 0, len(rawLines))
	for i, raw := range rawLines {
		path := prefix + "[" + itoa(i) + "]"
		if len(raw) == 0 {
			v.add(ViolationInvalidJSON, path)
			continue
		}
		if !utf8.Valid(raw) {
			v.add(ViolationUnsanitized, path)
			continue
		}
		hasUnpairedSurrogate := hasUnpairedJSONSurrogate(raw)
		if hasUnpairedSurrogate {
			// encoding/json replaces an isolated UTF-16 surrogate while
			// JSON.parse preserves it. Reject the raw spelling before either
			// decoder can give the two bindings different sanitized values.
			v.add(ViolationUnsanitized, path)
		}
		if hasJSONWhitespaceOutsideStrings(raw) {
			v.add(ViolationInvalidValue, path)
		}
		local := &validator{}
		root, ok := decodeDocument(local, raw)
		if ok {
			validateSessionStreamRecord(local, root)
			if !hasUnpairedSurrogate {
				checkMachineOutputSanitized(local, "", root)
			}
		}
		for _, violation := range local.sorted() {
			childPath := path
			if violation.Path != "" {
				childPath += "." + violation.Path
			}
			v.add(violation.Code, childPath)
		}
		if ok {
			lines = append(lines, machineOutputLine{raw: append([]byte(nil), raw...), root: root})
		}
	}
	return lines
}

func hasUnpairedJSONSurrogate(line []byte) bool {
	inString := false
	for i := 0; i < len(line); {
		if !inString {
			if line[i] == '"' {
				inString = true
			}
			i++
			continue
		}
		switch line[i] {
		case '"':
			inString = false
			i++
		case '\\':
			if i+1 >= len(line) {
				return false
			}
			if line[i+1] != 'u' || i+6 > len(line) {
				i += 2
				continue
			}
			code, ok := jsonHexCodeUnit(line[i+2 : i+6])
			if !ok {
				i += 2
				continue
			}
			if code >= 0xdc00 && code <= 0xdfff {
				return true
			}
			if code < 0xd800 || code > 0xdbff {
				i += 6
				continue
			}
			if i+12 > len(line) || line[i+6] != '\\' || line[i+7] != 'u' {
				return true
			}
			low, ok := jsonHexCodeUnit(line[i+8 : i+12])
			if !ok || low < 0xdc00 || low > 0xdfff {
				return true
			}
			i += 12
		default:
			i++
		}
	}
	return false
}

func jsonHexCodeUnit(raw []byte) (uint16, bool) {
	var value uint16
	for _, b := range raw {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value |= uint16(b - '0')
		case b >= 'a' && b <= 'f':
			value |= uint16(b-'a') + 10
		case b >= 'A' && b <= 'F':
			value |= uint16(b-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func terminalMachineOutputLine(v *validator, prefix string, lines []machineOutputLine) *machineOutputLine {
	terminal := -1
	for i := range lines {
		if record, _ := childString(lines[i].root, "record"); record == RecordSessionEnd {
			if terminal >= 0 {
				v.add(ViolationInvalidValue, prefix+"["+itoa(i)+"].record")
			}
			terminal = i
		}
	}
	if terminal < 0 {
		v.add(ViolationMissingField, prefix+".session:end")
		return nil
	}
	if terminal != len(lines)-1 {
		v.add(ViolationInvalidValue, prefix+"["+itoa(terminal)+"].record")
		return nil
	}
	if _, bounded := childValue(lines[terminal].root, "machineOutput"); !bounded {
		v.add(ViolationMissingField, prefix+"["+itoa(terminal)+"].machineOutput")
		return nil
	}
	return &lines[terminal]
}

func selectMachineOutput(candidates []machineOutputLine, mode string, budget MachineOutputBudget) ([]machineOutputLine, MachineOutputElision, MachineOutputElision) {
	ordinaryBytes := budget.MaxBytes - budget.FailureReserveBytes - budget.FinalReserveBytes
	ordinaryRecords := budget.MaxRecords - budget.FailureReserveRecords - budget.FinalReserveRecords
	failureBytes := budget.FailureReserveBytes
	failureRecords := budget.FailureReserveRecords
	selected := make([]machineOutputLine, 0, len(candidates))
	var ordinaryElided, failureElided MachineOutputElision
	for _, candidate := range candidates {
		size := int64(len(candidate.raw) + 1)
		if failurePriorityRecord(candidate.root) {
			if failureRecords > 0 && size <= failureBytes {
				selected = append(selected, candidate)
				failureRecords--
				failureBytes -= size
			} else {
				failureElided.Records++
				failureElided.Bytes += size
			}
			continue
		}
		if mode == MachineOutputModeNormal && debugDetailRecord(candidate.root) {
			ordinaryElided.Records++
			ordinaryElided.Bytes += size
			continue
		}
		if ordinaryRecords > 0 && size <= ordinaryBytes {
			selected = append(selected, candidate)
			ordinaryRecords--
			ordinaryBytes -= size
		} else {
			ordinaryElided.Records++
			ordinaryElided.Bytes += size
		}
	}
	return selected, ordinaryElided, failureElided
}

func sameMachineOutputLines(got, want []machineOutputLine) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !bytes.Equal(got[i].raw, want[i].raw) {
			return false
		}
	}
	return true
}

func checkElision(v *validator, summary map[string]any, class string, want MachineOutputElision) {
	elision := childObject(childObject(summary, "elided"), class)
	if got, ok := childInt(elision, "records"); ok && got != int64(want.Records) {
		v.add(ViolationElisionMismatch, "stream.final.machineOutput.elided."+class+".records")
	}
	if got, ok := childInt(elision, "bytes"); ok && got != want.Bytes {
		v.add(ViolationElisionMismatch, "stream.final.machineOutput.elided."+class+".bytes")
	}
}

func hasJSONWhitespaceOutsideStrings(line []byte) bool {
	inString := false
	escaped := false
	for _, b := range line {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if b == '\\' {
				escaped = true
			} else if b == '"' {
				inString = false
			}
			continue
		}
		if b == '"' {
			inString = true
			continue
		}
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			return true
		}
	}
	return false
}
