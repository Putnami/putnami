package clicore

import (
	"fmt"
	"strings"
)

// RepositoryKeyFromRemote derives (provider, owner/name) from a git remote
// URL, accepting https, ssh, git, and scp-like forms. The provider is currently
// github because Source's first supported provider is GitHub.
func RepositoryKeyFromRemote(remote string) (provider, repoKey string, err error) {
	r := strings.TrimSpace(remote)
	if r == "" {
		return "", "", fmt.Errorf("empty remote URL")
	}
	if i := strings.Index(r, "://"); i >= 0 {
		r = r[i+3:]
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
	path := strings.TrimSuffix(strings.Trim(r, "/"), ".git")
	segs := strings.Split(path, "/")
	if len(segs) < 3 || strings.TrimSpace(segs[len(segs)-2]) == "" || strings.TrimSpace(segs[len(segs)-1]) == "" {
		return "", "", fmt.Errorf("remote %q does not resolve to a host/owner/name repository", remote)
	}
	return "github", segs[len(segs)-2] + "/" + segs[len(segs)-1], nil
}
