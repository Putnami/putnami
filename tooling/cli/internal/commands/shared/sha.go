package shared

// ShortSHA truncates a git commit SHA to its short (12-character) form for
// human-readable output. A sha already at or under that length is returned
// unchanged.
func ShortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
