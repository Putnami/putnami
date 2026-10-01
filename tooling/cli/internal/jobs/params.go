package jobs

// paramStrings returns the value for key in params as []string. JSON
// deserialization yields []any of strings, so the entries are coerced.
// Returns nil if missing or not a slice; non-string entries are skipped.
func paramStrings(params map[string]any, key string) []string {
	v, ok := params[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			result = append(result, s)
		}
	}
	return result
}
