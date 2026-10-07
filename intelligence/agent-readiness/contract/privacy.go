package contract

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// maxPayloadString bounds any string in a payload. The longest legitimate
// value is an evidence command; a longer string is treated as file contents.
const maxPayloadString = 512

// maxEvidenceSample bounds the locations a marker may cite.
const maxEvidenceSample = 5

var (
	emailPattern     = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}`)
	windowsAbsPrefix = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
)

// PrivacyViolations lists every string in the encoded payload that could
// carry a source line, an email address or an absolute path. The collector
// runs it before sending and intelligence-api runs it before storing; an
// empty result is the only acceptable one. Each entry names the JSON path.
func PrivacyViolations(payload Payload) ([]string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}
	var tree any
	if err := json.Unmarshal(encoded, &tree); err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	var violations []string
	walkStrings(tree, "$", func(path, value string) {
		if reason := privacyReason(value); reason != "" {
			violations = append(violations, path+": "+reason)
		}
	})
	for i, marker := range payload.Markers {
		if len(marker.Evidence.Sample) > maxEvidenceSample {
			violations = append(violations, fmt.Sprintf("$.markers[%d].evidence.sample: more than %d locations", i, maxEvidenceSample))
		}
		for j, location := range marker.Evidence.Sample {
			if strings.ContainsAny(location, " \t") {
				violations = append(violations, fmt.Sprintf("$.markers[%d].evidence.sample[%d]: not a location", i, j))
			}
		}
	}
	sort.Strings(violations)
	return violations, nil
}

func privacyReason(value string) string {
	switch {
	case strings.ContainsAny(value, "\r\n"):
		return "multi-line text"
	case len(value) > maxPayloadString:
		return fmt.Sprintf("longer than %d bytes", maxPayloadString)
	case emailPattern.MatchString(value):
		return "email address"
	case strings.ContainsAny(value, "{};"):
		return "source-like text"
	}
	for _, token := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '=' || r == '"' || r == '\'' || r == '('
	}) {
		switch {
		case strings.HasPrefix(token, "/"), strings.HasPrefix(token, "~"), windowsAbsPrefix.MatchString(token):
			return "absolute path"
		case token == ".." || strings.HasPrefix(token, "../") || strings.Contains(token, "/../"):
			return "path outside the repository"
		}
	}
	return ""
}

func walkStrings(node any, path string, visit func(path, value string)) {
	switch typed := node.(type) {
	case string:
		visit(path, typed)
	case []any:
		for i, item := range typed {
			walkStrings(item, fmt.Sprintf("%s[%d]", path, i), visit)
		}
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			walkStrings(typed[key], path+"."+key, visit)
		}
	}
}
