package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The fake's merge requests keep the GitLab rules the proposals provider
// relies on: per-project iids counted apart from issues, one open merge
// request per source and target branch, a draft set by a title prefix, a head
// commit that follows its branch some reads after a push, notes with system
// notes and diff positions, approvals refused to the author and checked
// against sha, commit statuses per commit, and a merge that checks sha and
// mergeability. Quick actions in descriptions and notes run and are removed.

type fakeMR struct {
	id            int64
	iid           int
	title         string
	description   string
	state         string
	source        string
	target        string
	sourceProject int64
	sha           string
	// pending is the commit the head follows after lag more reads.
	pending      string
	lag          int
	labels       []string
	assignees    []int64
	author       int64
	created      time.Time
	updated      time.Time
	approvedBy   []int64
	notes        []*fakeNote
	squashed     bool
	removeSource bool
	// squash is the merge request's own squash flag.
	squash     bool
	mergeError string
}

// squashOnMerge is whether GitLab squashes the merge request: the target
// branch's option replaces the project's where a branch rule sets one, and
// always and never decide over the merge request's own flag.
func (f *fakeGitLab) squashOnMerge(mr *fakeMR) bool {
	option := f.squashOption
	if f.branchSquash != "" {
		option = f.branchSquash
	}
	switch option {
	case "always":
		return true
	case "never":
		return false
	}
	return mr.squash
}

type fakeNote struct {
	id       int64
	body     string
	author   int64
	created  time.Time
	system   bool
	position map[string]any
}

// fakeDraft is the title prefix GitLab reads as a draft.
var fakeDraft = regexp.MustCompile(`(?i)^\s*(?:\[draft\]|\(draft\)|draft:)`)

func (m *fakeMR) draft() bool { return fakeDraft.MatchString(m.title) }

// fakeSHA is a full commit id made from n.
func fakeSHA(n int) string { return fmt.Sprintf("%040x", n) }

var (
	mrsRoute       = regexp.MustCompile(`^/api/v4/projects/([^/]+)/merge_requests$`)
	mrRoute        = regexp.MustCompile(`^/api/v4/projects/([^/]+)/merge_requests/([0-9]+)(?:/(notes|approvals|approve|unapprove|merge))?$`)
	branchRoute    = regexp.MustCompile(`^/api/v4/projects/([^/]+)/repository/branches/([^/]+)$`)
	statusesRoute  = regexp.MustCompile(`^/api/v4/projects/([^/]+)/repository/commits/([0-9a-f]+)/statuses$`)
	projectRoute   = regexp.MustCompile(`^/api/v4/projects/([^/]+)$`)
	foreignIssueRx = regexp.MustCompile(`^/api/v4/projects/([^/]+)/issues/([0-9]+)$`)
)

// routeProposals answers the merge request routes, and reports whether it
// answered.
func (f *fakeGitLab) routeProposals(w http.ResponseWriter, r *http.Request, path string) bool {
	if match := projectRoute.FindStringSubmatch(path); match != nil && r.Method == http.MethodGet {
		if f.project(w, match[1]) {
			writeJSON(w, http.StatusOK, map[string]any{"id": f.projectID, "path_with_namespace": f.path, "merge_method": f.mergeMethod, "squash_option": f.squashOption,
				"description": "a fake project"})
		}
		return true
	}
	if match := foreignIssueRx.FindStringSubmatch(path); match != nil && r.Method == http.MethodGet && match[1] != url.PathEscape(f.path) {
		other, _ := url.PathUnescape(match[1])
		iid, _ := strconv.Atoi(match[2])
		labels, known := f.foreign[strings.ToLower(other)][iid]
		if !known {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not found"})
			return true
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": 90000 + iid, "iid": iid, "title": "foreign", "state": "opened", "labels": labels,
			"assignees": []any{}, "author": f.user(otherID), "created_at": f.clock.Format(time.RFC3339), "_links": map[string]any{}})
		return true
	}
	if match := branchRoute.FindStringSubmatch(path); match != nil && r.Method == http.MethodGet {
		if !f.project(w, match[1]) {
			return true
		}
		branch, _ := url.PathUnescape(match[2])
		sha, known := f.branches[branch]
		if !known {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Branch Not Found"})
			return true
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": branch, "commit": map[string]any{"id": sha}})
		return true
	}
	if match := statusesRoute.FindStringSubmatch(path); match != nil && r.Method == http.MethodGet {
		if f.project(w, match[1]) {
			statuses := slices.Clone(f.statuses[match[2]])
			if r.URL.Query().Get("sort") == "desc" {
				slices.Reverse(statuses)
			}
			f.page(w, r.URL.Query(), statuses)
		}
		return true
	}
	if match := mrsRoute.FindStringSubmatch(path); match != nil {
		if !f.project(w, match[1]) {
			return true
		}
		switch r.Method {
		case http.MethodGet:
			f.listMRs(w, r.URL.Query())
		case http.MethodPost:
			f.createMR(w, r)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "405 Method Not Allowed"})
		}
		return true
	}
	match := mrRoute.FindStringSubmatch(path)
	if match == nil {
		return false
	}
	if !f.project(w, match[1]) {
		return true
	}
	iid, _ := strconv.Atoi(match[2])
	mr := f.mrs[iid]
	if mr == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not found"})
		return true
	}
	switch action := match[3]; {
	case action == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, f.renderMR(mr))
	case action == "" && r.Method == http.MethodPut:
		f.editMR(w, r, mr)
	case action == "notes" && r.Method == http.MethodGet:
		var rendered []any
		for _, note := range mr.notes {
			rendered = append(rendered, f.renderNote(note))
		}
		f.page(w, r.URL.Query(), rendered)
	case action == "notes" && r.Method == http.MethodPost:
		edit, ok := f.decodeMR(w, r)
		if !ok {
			return true
		}
		body := f.mrQuickActions(mr, deref(edit.Body))
		if strings.TrimSpace(body) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"message": map[string][]string{"note": {"can't be blank"}}})
			return true
		}
		note := f.addNote(mr, body, f.me)
		writeJSON(w, http.StatusCreated, f.renderNote(note))
	case action == "approvals" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, f.renderApprovals(mr))
	case action == "approve" && r.Method == http.MethodPost:
		edit, ok := f.decodeMR(w, r)
		if !ok {
			return true
		}
		if edit.SHA != nil && *edit.SHA != f.head(mr) {
			writeJSON(w, http.StatusConflict, map[string]any{"message": "SHA does not match HEAD of source branch: " + f.head(mr)})
			return true
		}
		if (mr.author == f.me && !f.authorMayApprove) || slices.Contains(mr.approvedBy, f.me) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "401 Unauthorized"})
			return true
		}
		mr.approvedBy = append(mr.approvedBy, f.me)
		writeJSON(w, http.StatusCreated, f.renderApprovals(mr))
	case action == "unapprove" && r.Method == http.MethodPost:
		if !slices.Contains(mr.approvedBy, f.me) {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not found"})
			return true
		}
		mr.approvedBy = slices.DeleteFunc(mr.approvedBy, func(id int64) bool { return id == f.me })
		writeJSON(w, http.StatusCreated, f.renderApprovals(mr))
	case action == "merge" && r.Method == http.MethodPut:
		f.mergeMR(w, r, mr)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "405 Method Not Allowed"})
	}
	return true
}

// head is the merge request's head commit after one more read: it follows
// a push once its lag is used up.
func (f *fakeGitLab) head(mr *fakeMR) string {
	if mr.pending != "" {
		if mr.lag <= 0 {
			mr.sha, mr.pending = mr.pending, ""
		} else {
			mr.lag--
		}
	}
	return mr.sha
}

func (f *fakeGitLab) renderMR(mr *fakeMR) map[string]any {
	assignees := []any{}
	for _, id := range mr.assignees {
		assignees = append(assignees, f.user(id))
	}
	description := any(mr.description)
	if mr.description == "" {
		description = nil
	}
	sha := any(f.head(mr))
	if mr.sha == "" {
		sha = nil
	}
	return map[string]any{
		"id": mr.id, "iid": mr.iid, "project_id": f.projectID, "source_project_id": mr.sourceProject, "target_project_id": f.projectID,
		"title": mr.title, "description": description, "state": mr.state, "draft": mr.draft(), "work_in_progress": mr.draft(),
		"source_branch": mr.source, "target_branch": mr.target, "sha": sha, "squash": mr.squash, "squash_on_merge": f.squashOnMerge(mr),
		"merge_error": nilIfEmpty(mr.mergeError), "labels": append([]string{}, mr.labels...),
		"assignees": assignees, "author": f.user(mr.author),
		"web_url":    fmt.Sprintf("https://%s/%s/-/merge_requests/%d", fakeHost, f.path, mr.iid),
		"created_at": mr.created.UTC().Format("2006-01-02T15:04:05.000Z07:00"), "updated_at": mr.updated.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	}
}

func (f *fakeGitLab) renderNote(note *fakeNote) map[string]any {
	rendered := map[string]any{"id": note.id, "body": note.body, "author": f.user(note.author), "system": note.system,
		"created_at": note.created.UTC().Format("2006-01-02T15:04:05.000Z07:00"), "type": nil, "notable_type": "MergeRequest"}
	if note.position != nil {
		rendered["type"] = "DiffNote"
		rendered["position"] = note.position
	}
	return rendered
}

func (f *fakeGitLab) renderApprovals(mr *fakeMR) map[string]any {
	approved := []any{}
	for _, id := range mr.approvedBy {
		approved = append(approved, map[string]any{"user": f.user(id)})
	}
	return map[string]any{"approved": len(approved) > 0, "approved_by": approved}
}

func (f *fakeGitLab) addNote(mr *fakeMR, body string, author int64) *fakeNote {
	note := &fakeNote{id: f.nextID, body: body, author: author, created: f.clock}
	f.nextID++
	mr.notes = append(mr.notes, note)
	return note
}

// page answers one page of a list with X-Next-Page.
func (f *fakeGitLab) page(w http.ResponseWriter, query url.Values, items []any) {
	perPage, _ := strconv.Atoi(query.Get("per_page"))
	if perPage <= 0 {
		perPage = 20
	}
	page, _ := strconv.Atoi(query.Get("page"))
	if page <= 0 {
		page = 1
	}
	start := min((page-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	if end < len(items) {
		w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
	} else {
		w.Header().Set("X-Next-Page", "")
	}
	writeJSON(w, http.StatusOK, append([]any{}, items[start:end]...))
}

func (f *fakeGitLab) listMRs(w http.ResponseWriter, query url.Values) {
	var matched []*fakeMR
	for _, mr := range f.mrs {
		if source := query.Get("source_branch"); source != "" && source != mr.source {
			continue
		}
		if target := query.Get("target_branch"); target != "" && target != mr.target {
			continue
		}
		if state := query.Get("state"); state != "" && state != "all" && state != mr.state {
			continue
		}
		matched = append(matched, mr)
	}
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].created.Equal(matched[j].created) {
			return matched[i].created.Before(matched[j].created)
		}
		return matched[i].id > matched[j].id
	})
	rendered := []any{}
	for _, mr := range matched {
		rendered = append(rendered, f.renderMR(mr))
	}
	f.page(w, query, rendered)
}

type fakeMREdit struct {
	Title              *string  `json:"title"`
	Description        *string  `json:"description"`
	SourceBranch       string   `json:"source_branch"`
	TargetBranch       string   `json:"target_branch"`
	AssigneeIDs        *[]int64 `json:"assignee_ids"`
	AddLabels          *string  `json:"add_labels"`
	StateEvent         string   `json:"state_event"`
	Body               *string  `json:"body"`
	SHA                *string  `json:"sha"`
	Squash             *bool    `json:"squash"`
	RemoveSourceBranch *bool    `json:"should_remove_source_branch"`
}

func (f *fakeGitLab) decodeMR(w http.ResponseWriter, r *http.Request) (fakeMREdit, bool) {
	var edit fakeMREdit
	if r.ContentLength == 0 {
		return edit, true
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&edit); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the request body is invalid: " + err.Error()})
		return edit, false
	}
	return edit, true
}

// mrQuickActions runs /close, /approve and /merge from a description or a
// note, as GitLab does, and returns the text without them.
func (f *fakeGitLab) mrQuickActions(mr *fakeMR, text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		switch strings.TrimSpace(line) {
		case "/close":
			mr.state = "closed"
		case "/approve":
			if !slices.Contains(mr.approvedBy, f.me) {
				mr.approvedBy = append(mr.approvedBy, f.me)
			}
		case "/merge":
			mr.state = "merged"
		default:
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func (f *fakeGitLab) createMR(w http.ResponseWriter, r *http.Request) {
	edit, ok := f.decodeMR(w, r)
	if !ok {
		return
	}
	if edit.Title == nil || strings.TrimSpace(*edit.Title) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": map[string][]string{"title": {"can't be blank"}}})
		return
	}
	sha, known := f.branches[edit.SourceBranch]
	if !known {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": []string{"Source branch \"" + edit.SourceBranch + "\" does not exist"}})
		return
	}
	for _, other := range f.mrs {
		if other.state == "opened" && other.source == edit.SourceBranch && other.target == edit.TargetBranch && other.sourceProject == f.projectID {
			writeJSON(w, http.StatusConflict, map[string]any{"message": []string{fmt.Sprintf("Another open merge request already exists for this source branch: !%d", other.iid)}})
			return
		}
	}
	mr := f.newMR(edit.SourceBranch, edit.TargetBranch, *edit.Title, f.me, f.projectID)
	mr.sha = sha
	// A create without squash takes the project's default.
	mr.squash = edit.Squash != nil && *edit.Squash || edit.Squash == nil && (f.squashOption == "always" || f.squashOption == "default_on")
	mr.description = f.mrQuickActions(mr, deref(edit.Description))
	writeJSON(w, http.StatusCreated, f.renderMR(mr))
}

func (f *fakeGitLab) newMR(source, target, title string, author, sourceProject int64) *fakeMR {
	mr := &fakeMR{id: f.nextID, iid: f.nextMRIID, title: title, state: "opened", source: source, target: target, sourceProject: sourceProject,
		labels: []string{}, author: author, created: f.clock, updated: f.clock}
	f.nextID++
	f.nextMRIID++
	f.mrs[mr.iid] = mr
	return mr
}

func (f *fakeGitLab) editMR(w http.ResponseWriter, r *http.Request, mr *fakeMR) {
	edit, ok := f.decodeMR(w, r)
	if !ok {
		return
	}
	if edit.Title != nil {
		if strings.TrimSpace(*edit.Title) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"message": map[string][]string{"title": {"can't be blank"}}})
			return
		}
		mr.title = *edit.Title
	}
	if edit.Description != nil {
		mr.description = f.mrQuickActions(mr, *edit.Description)
	}
	if edit.AddLabels != nil {
		for _, label := range strings.Split(*edit.AddLabels, ",") {
			if at := strings.LastIndex(label, "::"); f.scopedLabels && at > 0 {
				mr.labels = slices.DeleteFunc(mr.labels, func(have string) bool {
					return strings.HasPrefix(strings.ToLower(have), strings.ToLower(label[:at+2])) && !strings.Contains(have[at+2:], "::")
				})
			}
			mr.labels = splitLabels(strings.Join(mr.labels, ",") + "," + label)
		}
	}
	if edit.Squash != nil {
		mr.squash = *edit.Squash
	}
	if edit.AssigneeIDs != nil {
		mr.assignees = nil
		for _, id := range *edit.AssigneeIDs {
			if _, known := f.users[id]; known && !slices.Contains(mr.assignees, id) {
				mr.assignees = append(mr.assignees, id)
			}
		}
		if f.singleAssignee && len(mr.assignees) > 1 {
			mr.assignees = mr.assignees[:1]
		}
	}
	mr.updated = f.clock
	writeJSON(w, http.StatusOK, f.renderMR(mr))
}

func (f *fakeGitLab) mergeMR(w http.ResponseWriter, r *http.Request, mr *fakeMR) {
	edit, ok := f.decodeMR(w, r)
	if !ok {
		return
	}
	f.mergeRequests = append(f.mergeRequests, edit)
	if mr.state != "opened" || mr.draft() || f.unmergeable {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "405 Method Not Allowed"})
		return
	}
	if edit.SHA != nil && *edit.SHA != f.head(mr) {
		writeJSON(w, http.StatusConflict, map[string]any{"message": "SHA does not match HEAD of source branch: " + mr.sha})
		return
	}
	if f.mergeError != "" {
		mr.mergeError = f.mergeError
		writeJSON(w, http.StatusOK, f.renderMR(mr))
		return
	}
	// The merge call sets the flag only when it asks to squash.
	if edit.Squash != nil && *edit.Squash {
		mr.squash = true
	}
	mr.state = "merged"
	mr.squashed = f.squashOnMerge(mr)
	mr.removeSource = edit.RemoveSourceBranch != nil && *edit.RemoveSourceBranch
	if mr.removeSource {
		delete(f.branches, mr.source)
	}
	writeJSON(w, http.StatusOK, f.renderMR(mr))
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
