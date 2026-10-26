package agentsession

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	collab "go.putnami.dev/protocol/collaboration"
)

const (
	// missionIssueMax bounds the issue number `agent attach` takes.
	missionIssueMax = 1_000_000_000
	// declaredDir holds the mission declarations, under the session store.
	declaredDir = "declared"
	// workspaceManifest marks a workspace root.
	workspaceManifest = "putnami.workspace.json"
)

// missionToken reports a value missions takes as a mission key or a
// repository: printable characters without whitespace, at most
// collab.MaxTokenLength.
func missionToken(value string) bool {
	return value != "" && len([]rune(value)) <= collab.MaxTokenLength &&
		!strings.ContainsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) })
}

// missionDeclaration records the issue a checkout declared for one branch.
// Every session of that checkout and branch reports to its mission, so a new
// session continues the mission of the one it replaces.
type missionDeclaration struct {
	Root       string    `json:"root"`
	Branch     string    `json:"branch"`
	Issue      int       `json:"issue"`
	DeclaredAt time.Time `json:"declared_at"`
}

func missionDeclarationPath(dir, root, branch string) string {
	sum := sha256.Sum256([]byte(root + "\x00" + branch))
	return filepath.Join(dir, declaredDir, hex.EncodeToString(sum[:])+".json")
}

// declareMissionIssue makes issue-<issue> the mission of this checkout and
// branch.
func declareMissionIssue(dir, root, branch string, issue int, now time.Time) error {
	if issue <= 0 || issue > missionIssueMax {
		return errors.New("the issue must be a positive number")
	}
	data, err := json.Marshal(missionDeclaration{Root: root, Branch: branch, Issue: issue, DeclaredAt: now.UTC()})
	if err != nil {
		return err
	}
	return writeFileAtomic(missionDeclarationPath(dir, root, branch), data)
}

func declaredIssue(dir, root, branch string) int {
	data, err := os.ReadFile(missionDeclarationPath(dir, root, branch))
	if err != nil {
		return 0
	}
	var declaration missionDeclaration
	if json.Unmarshal(data, &declaration) != nil || declaration.Root != root || declaration.Branch != branch ||
		declaration.Issue <= 0 || declaration.Issue > missionIssueMax {
		return 0
	}
	return declaration.Issue
}

// sessionMissionKey is issue-<n> once the checkout declared an issue for its
// branch, else branch:<branch>. It answers "" when neither is known.
func sessionMissionKey(dir, root, branch string) string {
	if issue := declaredIssue(dir, root, branch); issue > 0 {
		return "issue-" + strconv.Itoa(issue)
	}
	if branch == "" {
		return ""
	}
	key := "branch:" + branch
	if !missionToken(key) {
		return ""
	}
	return key
}

// missionTitle is the title a new mission starts with.
func missionTitle(key string) string {
	if issue, ok := strings.CutPrefix(key, "issue-"); ok {
		return "Issue #" + issue
	}
	return strings.TrimPrefix(key, "branch:")
}

// checkoutBranch reads the branch of the checkout, or "" on a detached HEAD.
func (e *Env) checkoutBranch(ctx context.Context, root string) string {
	branch, err := e.git(ctx, root, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(branch)
}

// originRepository is the lowercase owner/name of the checkout's origin
// remote, or "" when origin names no such repository, such as a local path.
func (e *Env) originRepository(ctx context.Context, root string) string {
	remote, err := e.git(ctx, root, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return repositoryFromRemote(remote)
}

// repositoryFromRemote reads owner/name from a remote URL: scp-like
// (git@host:owner/name.git), ssh:// or https://. It answers the last two
// path segments in lowercase, without .git, and "" for a local path, a file:
// URL or a URL with fewer segments. Only the repository is kept: no host and
// no credential.
func repositoryFromRemote(remote string) string {
	r := strings.TrimSpace(remote)
	lower := strings.ToLower(r)
	if r == "" || strings.HasPrefix(r, "/") || strings.HasPrefix(r, ".") || strings.HasPrefix(r, "~") || strings.HasPrefix(lower, "file:") {
		return ""
	}
	if i := strings.Index(r, "://"); i >= 0 {
		r = r[i+3:]
	} else if colon := strings.IndexByte(r, ':'); colon < 0 || strings.Contains(r[:colon], "/") {
		// Without a scheme, only the scp-like form host:path names a remote.
		return ""
	}
	head := r
	if slash := strings.IndexByte(r, '/'); slash >= 0 {
		head = r[:slash]
	}
	if at := strings.LastIndex(head, "@"); at >= 0 {
		r = r[at+1:]
	}
	if colon := strings.IndexByte(r, ':'); colon >= 0 {
		if slash := strings.IndexByte(r, '/'); slash < 0 || colon < slash {
			r = r[:colon] + "/" + r[colon+1:]
		}
	}
	if cut := strings.IndexAny(r, "?#"); cut >= 0 {
		r = r[:cut]
	}
	path := strings.TrimSuffix(strings.Trim(r, "/"), ".git")
	segments := strings.Split(path, "/")
	if len(segments) < 3 {
		return ""
	}
	owner, name := segments[len(segments)-2], segments[len(segments)-1]
	if owner == "" || name == "" || name == "." || name == ".." || owner == "." || owner == ".." {
		return ""
	}
	repository := strings.ToLower(owner + "/" + name)
	if !missionToken(repository) {
		return ""
	}
	return repository
}

// sessionWorkspaceRoot is the nearest directory above cwd that holds a
// workspace manifest, else fallback.
func sessionWorkspaceRoot(cwd, fallback string) string {
	if strings.TrimSpace(cwd) == "" || !filepath.IsAbs(cwd) {
		return fallback
	}
	for dir := filepath.Clean(cwd); ; {
		if _, err := os.Stat(filepath.Join(dir, workspaceManifest)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return fallback
		}
		dir = parent
	}
}

// canonicalRoot resolves symbolic links, so the hook and `agent attach` name
// a checkout the same way.
func canonicalRoot(root string) string {
	if strings.TrimSpace(root) == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		return resolved
	}
	return filepath.Clean(root)
}
