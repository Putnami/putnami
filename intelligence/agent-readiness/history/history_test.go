package history

import "testing"

func TestIsPullRequestSubject(t *testing.T) {
	for subject, want := range map[string]bool{
		"Merge pull request #12 from acme/feature":  true,
		"feat(api): add route (#3691)":              true,
		"Merge branch 'feature' into 'main'":        true,
		"Merged in feature/x (pull request #7)":     true,
		"Merged PR 381: add login":                  true,
		"Merge branch 'feature'":                    false,
		"fix: typo":                                 false,
		"refers to #12 in the middle (#12) of text": false,
	} {
		if got := IsPullRequestSubject(subject); got != want {
			t.Errorf("IsPullRequestSubject(%q) = %v, want %v", subject, got, want)
		}
	}
}

func TestIsPullRequestBody(t *testing.T) {
	for line, want := range map[string]bool{
		"See merge request acme/core!2076":                               true,
		"kraaft/monorepo!6200 'fix/x' merged into 'master' by 'Someone'": true,
		"Reviewed-on: https://review.example.org/c/project/+/1234":       true,
		"* fix: handle long resource name":                               false,
		"great work!12 times":                                            false,
		"see docs/setup.md for details":                                  false,
	} {
		if got := IsPullRequestBody(line); got != want {
			t.Errorf("IsPullRequestBody(%q) = %v, want %v", line, got, want)
		}
	}
}
