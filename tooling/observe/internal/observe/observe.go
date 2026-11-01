// Package observe composes one observation of a CI job: the CI and the
// repository it ran for, the base of the change, the changed files and the
// reports, with every finding as a diagnostic.
package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/tooling/observe/internal/ci"
	"go.putnami.dev/tooling/observe/internal/gitbase"
	"go.putnami.dev/tooling/observe/internal/reports"
)

// Producer names the program that made an observation.
type Producer struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Observation is what one run of observe found.
type Observation struct {
	Subject  ci.Subject `json:"subject"`
	CI       ci.Run     `json:"ci"`
	Producer Producer   `json:"producer"`
	// Base is the base candidate the CI or a flag named, before Git resolved
	// it; Subject.BaseSHA is the merge base it resolved to.
	Base *ci.Base `json:"base,omitempty"`
	// DeepenedCommits counts the commits a shallow clone was deepened by.
	DeepenedCommits int                   `json:"deepenedCommits,omitempty"`
	ChangedFiles    []gitbase.ChangedFile `json:"changedFiles"`
	Reports         []reports.Report      `json:"reports"`
	Diagnostics     []diag.Diagnostic     `json:"diagnostics"`
}

// Options are the inputs of one run.
type Options struct {
	// Dir is the directory the command runs in.
	Dir       string
	Env       ci.Env
	ReadFile  ci.ReadFile
	Overrides ci.Overrides
	JUnit     []string
	Coverage  []string
	Fetch     bool
	// Resolve turns a named report path into an absolute one.
	Resolve  func(string) string
	Producer Producer
}

// Run observes the job.
func Run(ctx context.Context, opts Options) Observation {
	detection := ci.Detect(opts.Env, opts.ReadFile, opts.Overrides)
	resolved := gitbase.Resolve(ctx, gitbase.Request{Dir: opts.Dir, Head: detection.Head, Base: detection.Base, Fetch: opts.Fetch})
	o := Observation{
		Subject:         detection.Subject,
		CI:              detection.Run,
		Producer:        opts.Producer,
		Base:            detection.Base,
		DeepenedCommits: resolved.Deepened,
		ChangedFiles:    resolved.ChangedFiles,
	}
	if o.ChangedFiles == nil {
		o.ChangedFiles = []gitbase.ChangedFile{}
	}
	if o.Subject.SHA == "" {
		o.Subject.SHA = resolved.HeadSHA
	}
	o.Subject.BaseSHA = resolved.BaseSHA
	root := resolved.Root
	if root == "" {
		root = opts.Dir
	}
	if o.Subject.Repository == "" {
		o.Subject.Repository = repositoryName(ctx, root)
	}
	found, reportDiags := reports.Collect(reports.Request{Root: root, JUnit: opts.JUnit, Coverage: opts.Coverage, Resolve: opts.Resolve})
	o.Reports = found
	if o.Reports == nil {
		o.Reports = []reports.Report{}
	}
	o.Diagnostics = append(append(append([]diag.Diagnostic{}, detection.Diagnostics...), resolved.Diagnostics...), reportDiags...)
	return o
}

// repositoryName names a repository no CI names: the path of its origin
// remote without credentials or .git suffix, else its directory name.
func repositoryName(ctx context.Context, root string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "remote", "get-url", "origin").Output()
	if err == nil {
		if name := remotePath(strings.TrimSpace(string(out))); name != "" {
			return name
		}
	}
	return filepath.Base(root)
}

// remotePath returns the repository path of a remote URL: the URL path, or
// the part after the colon of an scp-like address.
func remotePath(remote string) string {
	var p string
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil {
			return ""
		}
		p = parsed.Path
	} else if host, after, ok := strings.Cut(remote, ":"); ok && !strings.Contains(host, "/") && len(host) > 1 {
		p = after
	} else {
		return ""
	}
	return strings.TrimSuffix(strings.Trim(p, "/"), ".git")
}

// WriteJSON writes the observation as one JSON document.
func WriteJSON(w io.Writer, o Observation) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(o)
}

// maxListedFiles caps the changed files the text summary lists.
const maxListedFiles = 50

// WriteText writes the observation as a summary for a CI log.
func WriteText(w io.Writer, o Observation) error {
	var b strings.Builder
	fmt.Fprintf(&b, "putnami observe: %s, %s\n", systemLabel(o.CI.System), repositoryLabel(o.Subject))
	fmt.Fprintf(&b, "  commit   %s%s\n", short(o.Subject.SHA), commitDetail(o.Subject))
	fmt.Fprintf(&b, "  base     %s\n", baseLabel(o))
	fmt.Fprintf(&b, "  changed  %s\n", count(len(o.ChangedFiles), "file"))
	for i, file := range o.ChangedFiles {
		if i == maxListedFiles {
			fmt.Fprintf(&b, "    ... and %d more\n", len(o.ChangedFiles)-maxListedFiles)
			break
		}
		name := file.Path
		if file.PreviousPath != "" {
			name = file.PreviousPath + " -> " + file.Path
		}
		fmt.Fprintf(&b, "    %-9s %s\n", file.Status, name)
	}
	fmt.Fprintf(&b, "  reports  %s\n", count(len(o.Reports), "report"))
	for _, r := range o.Reports {
		fmt.Fprintf(&b, "    %-9s %s\n", r.Format, r.Path)
	}
	for _, d := range o.Diagnostics {
		fmt.Fprintf(&b, "  %s %s: %s\n", d.Severity, d.Code, d.Message)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func systemLabel(system string) string {
	switch system {
	case ci.SystemGitHubActions:
		return "GitHub Actions"
	case ci.SystemGitLabCI:
		return "GitLab CI"
	}
	return "generic CI"
}

func repositoryLabel(s ci.Subject) string {
	label := s.Repository
	if s.Host != "" {
		label = s.Host + "/" + label
	}
	if s.ProjectID != "" {
		label += " (project " + s.ProjectID + ")"
	}
	return label
}

func commitDetail(s ci.Subject) string {
	var parts []string
	if s.Branch != "" {
		parts = append(parts, "on "+s.Branch)
	}
	if s.ChangeRequest != nil {
		kind := "pull request"
		if s.Provider == ci.ProviderGitLab {
			kind = "merge request"
		}
		parts = append(parts, fmt.Sprintf("%s #%d", kind, s.ChangeRequest.Number))
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, ", ")
}

func baseLabel(o Observation) string {
	if o.Base == nil {
		return "none"
	}
	if o.Subject.BaseSHA == "" {
		return "unresolved (" + baseCandidate(o.Base) + ")"
	}
	label := short(o.Subject.BaseSHA) + ", merge base with " + baseCandidate(o.Base)
	if o.DeepenedCommits > 0 {
		label += fmt.Sprintf("; the shallow clone was deepened by up to %d commits", o.DeepenedCommits)
	}
	return label
}

func baseCandidate(b *ci.Base) string {
	name := b.Ref
	if name == "" {
		name = short(b.SHA)
	}
	return name + " from the " + b.Source
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "unknown"
	}
	return sha
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
