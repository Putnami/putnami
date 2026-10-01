package sharedtest

// Contains reports whether v is present in s.
func Contains(s []string, v string) bool {
	for _, item := range s {
		if item == v {
			return true
		}
	}
	return false
}
