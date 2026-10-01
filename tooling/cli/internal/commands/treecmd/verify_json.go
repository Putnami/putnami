package treecmd

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The verifier reads evidence the way a JavaScript runtime reads it, because
// its verdicts and its output bytes are a contract its callers already parse:
// a parsed JSON value is one of
//
//	nil        undefined: a member that is absent
//	jsNull     null
//	bool       true or false
//	float64    every number, as an IEEE 754 double
//	string     UTF-8, where an escaped lone surrogate is kept as its three
//	           generalized UTF-8 bytes so it survives a round trip
//	[]any      an array
//	*jsObject  an object, with its members in insertion order
//
// and the helpers below answer property reads, iteration, equality and
// serialization with the same results JavaScript gives for these values.

// maxEvidenceBytes bounds one JSON document and one subprocess output.
const maxEvidenceBytes = 32 * 1024 * 1024

// maxJSONDepth bounds nesting, so a hostile document fails instead of
// exhausting the stack.
const maxJSONDepth = 10000

const invalidUTF8 = "The encoded data was not valid for encoding utf-8"

var errJSONEOF = errors.New("JSON Parse error: Unexpected EOF")

// jsError is an error in the words a JavaScript runtime uses, which may start
// with a capital letter.
type jsError string

func (e jsError) Error() string { return string(e) }

// jsNull is the JSON null.
type jsNull struct{}

// jsObject is a JSON object with its members in insertion order.
type jsObject struct {
	keys   []string
	values []any
	index  map[string]int
}

func newObject() *jsObject { return &jsObject{index: map[string]int{}} }

// obj builds an object whose members are keys and values, in that order.
func obj(keys []string, values ...any) *jsObject {
	o := newObject()
	for i, key := range keys {
		o.set(key, values[i])
	}
	return o
}

// set assigns a member. A new key goes last; an existing one keeps its place.
func (o *jsObject) set(key string, value any) {
	if position, ok := o.index[key]; ok {
		o.values[position] = value
		return
	}
	o.index[key] = len(o.keys)
	o.keys = append(o.keys, key)
	o.values = append(o.values, value)
}

// get returns an own member, or nil when it is absent.
func (o *jsObject) get(key string) any {
	if position, ok := o.index[key]; ok {
		return o.values[position]
	}
	return nil
}

func (o *jsObject) has(key string) bool {
	_, ok := o.index[key]
	return ok
}

// orderedKeys is the order JavaScript enumerates own keys in: array indexes
// ascending, then every other key in insertion order.
func (o *jsObject) orderedKeys() []string {
	var indexes, names []string
	for _, key := range o.keys {
		if isArrayIndex(key) {
			indexes = append(indexes, key)
		} else {
			names = append(names, key)
		}
	}
	sort.Slice(indexes, func(i, j int) bool {
		left, _ := strconv.ParseUint(indexes[i], 10, 64)
		right, _ := strconv.ParseUint(indexes[j], 10, 64)
		return left < right
	})
	return append(indexes, names...)
}

// isArrayIndex reports whether key is the canonical decimal form of an
// integer from 0 to 2^32-2.
func isArrayIndex(key string) bool {
	if key == "" || len(key) > 10 || (key[0] == '0' && key != "0") {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < '0' || key[i] > '9' {
			return false
		}
	}
	value, err := strconv.ParseUint(key, 10, 64)
	return err == nil && value <= math.MaxUint32-1
}

// parseJSON decodes raw as fatal UTF-8, drops a leading byte order mark, and
// parses the rest as one JSON value. A document that repeats a member name in
// one object is refused after it parsed.
func parseJSON(raw []byte) (any, error) {
	if len(raw) > maxEvidenceBytes {
		return nil, errors.New("record exceeds 32 MiB")
	}
	source, err := decodeUTF8(raw)
	if err != nil {
		return nil, err
	}
	reader := &jsonReader{src: source}
	value, err := reader.value()
	if err != nil {
		return nil, err
	}
	reader.skipSpace()
	if reader.pos != len(reader.src) {
		return nil, reader.syntaxError()
	}
	if reader.duplicate {
		return nil, errors.New("duplicate JSON member")
	}
	return value, nil
}

// decodeUTF8 is a fatal UTF-8 decoder that drops a leading byte order mark.
func decodeUTF8(raw []byte) (string, error) {
	if !utf8.Valid(raw) {
		return "", jsError(invalidUTF8)
	}
	return strings.TrimPrefix(string(raw), "\uFEFF"), nil
}

type jsonReader struct {
	src       string
	pos       int
	depth     int
	duplicate bool
}

func (r *jsonReader) syntaxError() error {
	if r.pos >= len(r.src) {
		return errJSONEOF
	}
	return fmt.Errorf("JSON Parse error: Unexpected character at byte %d", r.pos)
}

func (r *jsonReader) skipSpace() {
	for r.pos < len(r.src) {
		switch r.src[r.pos] {
		case ' ', '\t', '\n', '\r':
			r.pos++
		default:
			return
		}
	}
}

func (r *jsonReader) value() (any, error) {
	r.skipSpace()
	if r.pos >= len(r.src) {
		return nil, errJSONEOF
	}
	switch c := r.src[r.pos]; {
	case c == '{':
		return r.object()
	case c == '[':
		return r.array()
	case c == '"':
		return r.string()
	case c == 't':
		return r.literal("true", true)
	case c == 'f':
		return r.literal("false", false)
	case c == 'n':
		return r.literal("null", jsNull{})
	case c == '-' || (c >= '0' && c <= '9'):
		return r.number()
	}
	return nil, r.syntaxError()
}

func (r *jsonReader) literal(word string, value any) (any, error) {
	if !strings.HasPrefix(r.src[r.pos:], word) {
		return nil, r.syntaxError()
	}
	r.pos += len(word)
	return value, nil
}

func (r *jsonReader) digits() int {
	start := r.pos
	for r.pos < len(r.src) && r.src[r.pos] >= '0' && r.src[r.pos] <= '9' {
		r.pos++
	}
	return r.pos - start
}

// number reads -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)? as a double. A
// magnitude beyond the double range is an infinity, as in JavaScript.
func (r *jsonReader) number() (any, error) {
	start := r.pos
	if r.src[r.pos] == '-' {
		r.pos++
	}
	if r.pos < len(r.src) && r.src[r.pos] == '0' {
		r.pos++
	} else if r.digits() == 0 {
		return nil, r.syntaxError()
	}
	if r.pos < len(r.src) && r.src[r.pos] == '.' {
		r.pos++
		if r.digits() == 0 {
			return nil, r.syntaxError()
		}
	}
	if r.pos < len(r.src) && (r.src[r.pos] == 'e' || r.src[r.pos] == 'E') {
		r.pos++
		if r.pos < len(r.src) && (r.src[r.pos] == '+' || r.src[r.pos] == '-') {
			r.pos++
		}
		if r.digits() == 0 {
			return nil, r.syntaxError()
		}
	}
	value, err := strconv.ParseFloat(r.src[start:r.pos], 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return nil, r.syntaxError()
	}
	return value, nil
}

func (r *jsonReader) enter() error {
	r.depth++
	if r.depth > maxJSONDepth {
		return fmt.Errorf("JSON Parse error: nesting deeper than %d levels", maxJSONDepth)
	}
	return nil
}

func (r *jsonReader) object() (any, error) {
	r.pos++
	if err := r.enter(); err != nil {
		return nil, err
	}
	defer func() { r.depth-- }()
	o := newObject()
	r.skipSpace()
	if r.pos < len(r.src) && r.src[r.pos] == '}' {
		r.pos++
		return o, nil
	}
	for {
		r.skipSpace()
		if r.pos >= len(r.src) || r.src[r.pos] != '"' {
			return nil, r.syntaxError()
		}
		key, err := r.string()
		if err != nil {
			return nil, err
		}
		r.skipSpace()
		if r.pos >= len(r.src) || r.src[r.pos] != ':' {
			return nil, r.syntaxError()
		}
		r.pos++
		member, err := r.value()
		if err != nil {
			return nil, err
		}
		if o.has(key) {
			r.duplicate = true
		}
		o.set(key, member)
		r.skipSpace()
		if r.pos < len(r.src) && r.src[r.pos] == ',' {
			r.pos++
			continue
		}
		if r.pos < len(r.src) && r.src[r.pos] == '}' {
			r.pos++
			return o, nil
		}
		return nil, r.syntaxError()
	}
}

func (r *jsonReader) array() (any, error) {
	r.pos++
	if err := r.enter(); err != nil {
		return nil, err
	}
	defer func() { r.depth-- }()
	items := []any{}
	r.skipSpace()
	if r.pos < len(r.src) && r.src[r.pos] == ']' {
		r.pos++
		return items, nil
	}
	for {
		item, err := r.value()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		r.skipSpace()
		if r.pos < len(r.src) && r.src[r.pos] == ',' {
			r.pos++
			continue
		}
		if r.pos < len(r.src) && r.src[r.pos] == ']' {
			r.pos++
			return items, nil
		}
		return nil, r.syntaxError()
	}
}

func (r *jsonReader) string() (string, error) {
	r.pos++
	var b strings.Builder
	start := r.pos
	for {
		if r.pos >= len(r.src) {
			return "", errJSONEOF
		}
		switch c := r.src[r.pos]; {
		case c == '"':
			b.WriteString(r.src[start:r.pos])
			r.pos++
			return b.String(), nil
		case c == '\\':
			b.WriteString(r.src[start:r.pos])
			if err := r.escape(&b); err != nil {
				return "", err
			}
			start = r.pos
		case c < 0x20:
			return "", r.syntaxError()
		default:
			r.pos++
		}
	}
}

var shortEscapes = map[byte]byte{'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}

// escape reads one escape sequence at the backslash. A \u escape of a lone
// surrogate is kept as generalized UTF-8; an escaped surrogate pair is one
// code point.
func (r *jsonReader) escape(b *strings.Builder) error {
	r.pos++
	if r.pos >= len(r.src) {
		return errJSONEOF
	}
	if replacement, ok := shortEscapes[r.src[r.pos]]; ok {
		b.WriteByte(replacement)
		r.pos++
		return nil
	}
	if r.src[r.pos] != 'u' {
		return r.syntaxError()
	}
	unit, ok := hex4(r.src, r.pos+1)
	if !ok {
		return r.syntaxError()
	}
	r.pos += 5
	if unit >= 0xD800 && unit <= 0xDBFF && strings.HasPrefix(r.src[r.pos:], `\u`) {
		if low, ok := hex4(r.src, r.pos+2); ok && low >= 0xDC00 && low <= 0xDFFF {
			b.WriteRune(0x10000 + (unit-0xD800)<<10 + (low - 0xDC00))
			r.pos += 6
			return nil
		}
	}
	writeUnit(b, unit)
	return nil
}

func hex4(s string, at int) (rune, bool) {
	if at+4 > len(s) {
		return 0, false
	}
	value, err := strconv.ParseUint(s[at:at+4], 16, 32)
	if err != nil {
		return 0, false
	}
	return rune(value), true
}

// writeUnit writes one UTF-16 code unit, a lone surrogate as the three bytes
// generalized UTF-8 gives it.
func writeUnit(b *strings.Builder, unit rune) {
	if unit < 0xD800 || unit > 0xDFFF {
		b.WriteRune(unit)
		return
	}
	b.WriteByte(byte(0xE0 | unit>>12))
	b.WriteByte(byte(0x80 | (unit>>6)&0x3F))
	b.WriteByte(byte(0x80 | unit&0x3F))
}

// nextUnit returns the code point at s[i:] and its byte length: a rune, a lone
// surrogate kept as generalized UTF-8, or utf8.RuneError for any other
// invalid byte.
func nextUnit(s string, i int) (rune, int) {
	r, size := utf8.DecodeRuneInString(s[i:])
	if r != utf8.RuneError || size != 1 {
		return r, size
	}
	if i+2 < len(s) && s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
		return 0xD000 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F), 3
	}
	return utf8.RuneError, 1
}

// codePoints splits s the way a for...of loop iterates a string.
func codePoints(s string) []any {
	var out []any
	for i := 0; i < len(s); {
		_, size := nextUnit(s, i)
		out = append(out, s[i:i+size])
		i += size
	}
	return out
}

// isJSSpace is the white space and line terminator set String.prototype.trim
// removes.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// jsTrim is String.prototype.trim.
func jsTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }

// text reports whether value is a string with a character that is not white
// space.
func text(value any) bool {
	s, ok := value.(string)
	return ok && jsTrim(s) != ""
}

func nullish(value any) bool {
	if value == nil {
		return true
	}
	_, isNull := value.(jsNull)
	return isNull
}

// truthy is JavaScript's Boolean(value).
func truthy(value any) bool {
	switch value := value.(type) {
	case nil, jsNull:
		return false
	case bool:
		return value
	case float64:
		return value != 0 && !math.IsNaN(value)
	case string:
		return value != ""
	}
	return true
}

// strictEqual is ===. Two arrays or objects are equal only when they are one
// value, which never happens between two parsed members.
func strictEqual(a, b any) bool {
	switch a := a.(type) {
	case nil:
		return b == nil
	case jsNull:
		_, ok := b.(jsNull)
		return ok
	case bool:
		other, ok := b.(bool)
		return ok && a == other
	case float64:
		other, ok := b.(float64)
		return ok && a == other
	case string:
		other, ok := b.(string)
		return ok && a == other
	case *jsObject:
		other, ok := b.(*jsObject)
		return ok && a == other
	}
	return false
}

// isSafeInteger is Number.isSafeInteger.
func isSafeInteger(value any) bool {
	number, ok := value.(float64)
	return ok && !math.IsInf(number, 0) && math.Trunc(number) == number && math.Abs(number) <= 1<<53-1
}

// typeError reads a member of undefined or null the way JavaScript reports it.
func typeError(value any, key string) error {
	kind := "undefined"
	if value != nil {
		kind = "null"
	}
	return jsError(fmt.Sprintf("Cannot read properties of %s (reading '%s')", kind, key))
}

// get is value[key] for a key that names no array index: a member of an
// object, the length of an array or a string, undefined on any other value,
// and a TypeError on undefined or null.
func get(value any, key string) (any, error) {
	switch value := value.(type) {
	case nil, jsNull:
		return nil, typeError(value, key)
	case *jsObject:
		return value.get(key), nil
	case []any:
		if key == "length" {
			return float64(len(value)), nil
		}
	case string:
		if key == "length" {
			return float64(utf16Length(value)), nil
		}
	}
	return nil, nil
}

// optional is value?.[key].
func optional(value any, key string) any {
	if nullish(value) {
		return nil
	}
	member, _ := get(value, key)
	return member
}

// hasOwn is Object.hasOwn(value, key) for the keys the verifier asks about,
// none of which is an array index or length.
func hasOwn(value any, key string) (bool, error) {
	if nullish(value) {
		return false, jsError("Cannot convert undefined or null to object")
	}
	object, ok := value.(*jsObject)
	return ok && object.has(key), nil
}

// iterate is the sequence a for...of loop visits: the elements of an array,
// the code points of a string, and a TypeError for anything else.
func iterate(value any) ([]any, error) {
	switch value := value.(type) {
	case []any:
		return value, nil
	case string:
		return codePoints(value), nil
	}
	return nil, fmt.Errorf("%s is not iterable", jsTypeName(value))
}

// lengthPositive is value.length > 0 for a value the verifier iterates next:
// an array or a string. Any other value either has no length, or has one and
// makes the iteration that follows throw, so it answers false.
func lengthPositive(value any) (bool, error) {
	switch value := value.(type) {
	case nil, jsNull:
		return false, typeError(value, "length")
	case []any:
		return len(value) > 0, nil
	case string:
		return value != "", nil
	}
	return false, nil
}

func jsTypeName(value any) string {
	switch value.(type) {
	case nil:
		return "undefined"
	case jsNull:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	}
	return "object"
}

func utf16Length(s string) int {
	length := 0
	for i := 0; i < len(s); {
		r, size := nextUnit(s, i)
		length++
		if r > 0xFFFF {
			length++
		}
		i += size
	}
	return length
}

// jsToString is String(value), which a regular expression test applies to its
// argument and a child process applies to each of its arguments.
func jsToString(value any) string {
	switch value := value.(type) {
	case nil:
		return "undefined"
	case jsNull:
		return "null"
	case bool:
		return strconv.FormatBool(value)
	case float64:
		return jsNumber(value)
	case string:
		return value
	case []any:
		parts := make([]string, len(value))
		for i, item := range value {
			if !nullish(item) {
				parts[i] = jsToString(item)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// jsNumber is Number.prototype.toString(): the shortest decimal digits that
// read back as the same double, in fixed notation from 1e-7 to below 1e21 and
// in exponent notation outside.
func jsNumber(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	case value == 0:
		return "0"
	}
	sign := ""
	if value < 0 {
		sign, value = "-", -value
	}
	mantissa, exponent, _ := strings.Cut(strconv.FormatFloat(value, 'e', -1, 64), "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	power, _ := strconv.Atoi(exponent)
	k, n := len(digits), power+1
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	exponentSign := "+"
	if n-1 < 0 {
		exponentSign = "-"
	}
	written := strconv.Itoa(int(math.Abs(float64(n - 1))))
	if k == 1 {
		return sign + digits + "e" + exponentSign + written
	}
	return sign + digits[:1] + "." + digits[1:] + "e" + exponentSign + written
}

// stringify is JSON.stringify(value, null, gap) for a value that is not
// undefined.
func stringify(value any, gap string) string {
	var b strings.Builder
	writeJSON(&b, value, gap, "")
	return b.String()
}

func writeJSON(b *strings.Builder, value any, gap, indent string) {
	switch value := value.(type) {
	case bool:
		b.WriteString(strconv.FormatBool(value))
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			b.WriteString("null")
		} else {
			b.WriteString(jsNumber(value))
		}
	case string:
		writeQuoted(b, value)
	case []any:
		writeArray(b, value, gap, indent)
	case *jsObject:
		writeObject(b, value, gap, indent)
	default:
		b.WriteString("null")
	}
}

func writeArray(b *strings.Builder, items []any, gap, indent string) {
	if len(items) == 0 {
		b.WriteString("[]")
		return
	}
	inner := indent + gap
	b.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		if gap != "" {
			b.WriteString("\n" + inner)
		}
		writeJSON(b, item, gap, inner)
	}
	if gap != "" {
		b.WriteString("\n" + indent)
	}
	b.WriteByte(']')
}

func writeObject(b *strings.Builder, o *jsObject, gap, indent string) {
	var keys []string
	for _, key := range o.orderedKeys() {
		if o.get(key) != nil {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		b.WriteString("{}")
		return
	}
	inner := indent + gap
	b.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		if gap != "" {
			b.WriteString("\n" + inner)
		}
		writeQuoted(b, key)
		b.WriteByte(':')
		if gap != "" {
			b.WriteByte(' ')
		}
		writeJSON(b, o.get(key), gap, inner)
	}
	if gap != "" {
		b.WriteString("\n" + indent)
	}
	b.WriteByte('}')
}

// writeQuoted quotes s the way JSON.stringify does: it escapes the quote, the
// backslash, the control characters and lone surrogates, and nothing else.
func writeQuoted(b *strings.Builder, s string) {
	const hexDigits = "0123456789abcdef"
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := nextUnit(s, i)
		i += size
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || (r >= 0xD800 && r <= 0xDFFF):
			b.WriteString(`\u`)
			for shift := 12; shift >= 0; shift -= 4 {
				b.WriteByte(hexDigits[(r>>shift)&0xF])
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// equalStrings is equal(value, want) for a want made of strings: value must be
// an array of the same strings in the same order.
func equalStrings(value any, want []string) bool {
	items, ok := value.([]any)
	if !ok || len(items) != len(want) {
		return false
	}
	for i, item := range items {
		if s, ok := item.(string); !ok || s != want[i] {
			return false
		}
	}
	return true
}

// sortedStrings sorts values by code point. It reports false when a value is
// not a string: such a list can never equal a list of strings.
func sortedStrings(values []any) ([]string, bool) {
	out := make([]string, 0, len(values))
	for _, value := range values {
		s, ok := value.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out, true
}

// uniqueSorted deduplicates and sorts strings by code point. Byte order on
// UTF-8 is code point order.
func uniqueSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

// hasDuplicate reports whether two values of list are one value under
// SameValueZero, the equality a Set keys on. Arrays and objects are distinct.
func hasDuplicate(list []any) bool {
	seen := make(map[string]bool, len(list))
	for _, value := range list {
		var key string
		switch value := value.(type) {
		case nil:
			key = "u"
		case jsNull:
			key = "n"
		case bool:
			key = "b" + strconv.FormatBool(value)
		case float64:
			if value == 0 {
				value = 0
			}
			key = "f" + strconv.FormatFloat(value, 'g', -1, 64)
		case string:
			key = "s" + value
		default:
			continue
		}
		if seen[key] {
			return true
		}
		seen[key] = true
	}
	return false
}

// includesString is list.includes(value) for a string value.
func includesString(list []any, value string) bool {
	for _, item := range list {
		if s, ok := item.(string); ok && s == value {
			return true
		}
	}
	return false
}
