package estimate

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// kind is the JSON type a member holds.
type kind int

const (
	kindInteger kind = iota
	kindString
	kindStringList
	kindObject
	kindObjectList
)

// rule is the contract of one object member: its type, whether it must be
// present, and its bounds. For an integer, min and max bound the value; for a
// string, its length in code points; for a list, its item count, with
// itemMin and itemMax bounding the length of each string item. pattern is an
// ECMAScript pattern, as the JSON Schema states it, that a string or each
// string item matches. fields are the members of an object or of each object
// item; an object refuses every other member.
type rule struct {
	name     string
	kind     kind
	required bool
	min, max int
	itemMin  int
	itemMax  int
	pattern  string
	unique   bool
	fields   []rule
}

// compiled holds the RE2 form of every rule pattern. init fills it, and it is
// only read afterwards.
var compiled = map[string]*regexp.Regexp{}

var unicodeEscape = regexp.MustCompile(`\\u([0-9A-Fa-f]{4})`)

// re2 returns the RE2 form of an ECMAScript pattern, whose \uXXXX escapes
// RE2 writes \x{XXXX}.
func re2(pattern string) *regexp.Regexp {
	if re, ok := compiled[pattern]; ok {
		return re
	}
	return regexp.MustCompile(unicodeEscape.ReplaceAllString(pattern, `\x{$1}`))
}

func compileRules(rules []rule) {
	for _, r := range rules {
		if r.pattern != "" {
			compiled[r.pattern] = re2(r.pattern)
		}
		compileRules(r.fields)
	}
}

// checkObject checks that value is an object that holds only the members
// rules name, each within its rule.
func checkObject(path string, value any, rules []rule) error {
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: must be an object", path)
	}
	known := map[string]bool{}
	for _, r := range rules {
		known[r.name] = true
	}
	var unknown []string
	for name := range object {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("%s: unknown member %q", path, unknown[0])
	}
	for _, r := range rules {
		member := member(path, r.name)
		v, present := object[r.name]
		if !present {
			if r.required {
				return fmt.Errorf("%s: required", member)
			}
			continue
		}
		if err := checkValue(member, v, r); err != nil {
			return err
		}
	}
	return nil
}

func checkValue(path string, value any, r rule) error {
	switch r.kind {
	case kindInteger:
		_, err := integer(path, value, r.min, r.max)
		return err
	case kindString:
		return checkString(path, value, r.min, r.max, r.pattern)
	case kindObject:
		return checkObject(path, value, r.fields)
	case kindStringList, kindObjectList:
		list, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s: must be an array", path)
		}
		if len(list) < r.min || len(list) > r.max {
			return fmt.Errorf("%s: must hold from %d to %d items", path, r.min, r.max)
		}
		seen := map[string]int{}
		for i, item := range list {
			at := index(path, i)
			if r.kind == kindObjectList {
				if err := checkObject(at, item, r.fields); err != nil {
					return err
				}
				continue
			}
			if err := checkString(at, item, r.itemMin, r.itemMax, r.pattern); err != nil {
				return err
			}
			text, _ := item.(string)
			if first, repeated := seen[text]; r.unique && repeated {
				return fmt.Errorf("%s: repeats %s", at, index(path, first))
			}
			seen[text] = i
		}
	}
	return nil
}

// integer reads a JSON number written without a fraction or an exponent and
// within [min, max].
func integer(path string, value any, min, max int) (int, error) {
	number, ok := value.(json.Number)
	text := string(number)
	if ok && strings.ContainsAny(text, ".eE") {
		ok = false
	}
	n, err := strconv.Atoi(text)
	if !ok || err != nil || n < min || n > max {
		if min == max {
			return 0, fmt.Errorf("%s: must be %d", path, min)
		}
		return 0, fmt.Errorf("%s: must be an integer from %d to %d", path, min, max)
	}
	return n, nil
}

func checkString(path string, value any, min, max int, pattern string) error {
	text, ok := value.(string)
	if !ok {
		return fmt.Errorf("%s: must be a string", path)
	}
	if n := utf8.RuneCountInString(text); n < min || n > max {
		return fmt.Errorf("%s: must be from %d to %d characters", path, min, max)
	}
	if pattern != "" && !re2(pattern).MatchString(text) {
		return fmt.Errorf("%s: %q does not match %s", path, text, pattern)
	}
	return nil
}
