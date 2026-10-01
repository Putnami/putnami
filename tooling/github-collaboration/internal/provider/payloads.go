package provider

import "encoding/json"

// The GitHub REST documents this provider reads. Only the members it uses are
// declared; GitHub's other members are ignored.

type ghUser struct {
	Login string `json:"login"`
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghIssue struct {
	ID            int64           `json:"id"`
	Number        int             `json:"number"`
	Title         string          `json:"title"`
	Body          *string         `json:"body"`
	State         string          `json:"state"`
	StateReason   *string         `json:"state_reason"`
	Labels        []ghLabel       `json:"labels"`
	Assignees     []ghUser        `json:"assignees"`
	User          *ghUser         `json:"user"`
	HTMLURL       string          `json:"html_url"`
	CreatedAt     string          `json:"created_at"`
	UpdatedAt     string          `json:"updated_at"`
	RepositoryURL string          `json:"repository_url"`
	PullRequest   json.RawMessage `json:"pull_request"`
}

// isPullRequest reports whether an issues endpoint answered a pull request.
func (i *ghIssue) isPullRequest() bool {
	return len(i.PullRequest) > 0 && string(i.PullRequest) != "null"
}

func (i *ghIssue) labelNames() []string { return labelNamesOf(i.Labels) }

func (i *ghIssue) assigneeLogins() []string { return loginsOf(i.Assignees) }

func (p *ghPull) labelNames() []string { return labelNamesOf(p.Labels) }

func (p *ghPull) assigneeLogins() []string { return loginsOf(p.Assignees) }

func labelNamesOf(labels []ghLabel) []string {
	names := make([]string, 0, len(labels))
	for _, label := range labels {
		names = append(names, label.Name)
	}
	return names
}

func loginsOf(users []ghUser) []string {
	names := make([]string, 0, len(users))
	for _, user := range users {
		names = append(names, user.Login)
	}
	return names
}

type ghSearch struct {
	TotalCount int       `json:"total_count"`
	Items      []ghIssue `json:"items"`
}

type ghRepo struct {
	FullName string `json:"full_name"`
}

type ghBranch struct {
	Ref   string  `json:"ref"`
	Label string  `json:"label"`
	SHA   string  `json:"sha"`
	Repo  *ghRepo `json:"repo"`
}

type ghPull struct {
	Number    int       `json:"number"`
	NodeID    string    `json:"node_id"`
	Title     string    `json:"title"`
	Body      *string   `json:"body"`
	State     string    `json:"state"`
	Draft     bool      `json:"draft"`
	MergedAt  *string   `json:"merged_at"`
	HTMLURL   string    `json:"html_url"`
	UpdatedAt string    `json:"updated_at"`
	User      *ghUser   `json:"user"`
	Labels    []ghLabel `json:"labels"`
	Assignees []ghUser  `json:"assignees"`
	Base      ghBranch  `json:"base"`
	Head      ghBranch  `json:"head"`
}

type ghReview struct {
	ID          int64   `json:"id"`
	User        *ghUser `json:"user"`
	Body        string  `json:"body"`
	State       string  `json:"state"`
	CommitID    string  `json:"commit_id"`
	HTMLURL     string  `json:"html_url"`
	SubmittedAt string  `json:"submitted_at"`
}

type ghRef struct {
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

type ghCheckRuns struct {
	TotalCount int `json:"total_count"`
	CheckRuns  []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
	} `json:"check_runs"`
}

type ghCombinedStatus struct {
	TotalCount int `json:"total_count"`
	Statuses   []struct {
		Context   string `json:"context"`
		State     string `json:"state"`
		TargetURL string `json:"target_url"`
	} `json:"statuses"`
}

type ghMerge struct {
	Merged bool   `json:"merged"`
	SHA    string `json:"sha"`
}
