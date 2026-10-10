package identitycli

import (
	"context"
	"net/http"
	"strings"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// WhoAmI backs `putnami cloud whoami`: who this machine is signed in as, and
// which workspace this repository is linked to, as one status. A
// machine that is not signed in, or a repository that is not linked, is a
// failing check with the command that fixes it, and the command exits 1.
func WhoAmI(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	return clicore.WriteStatus(params, ioctx, WhoamiStatusNode(params, workspaceRoot, env, ioctx))
}

// WhoamiSession is what the whoami status knows about the session on this
// machine. Err is the error reading the session answered; it is nil when a
// session or PUTNAMI_CLOUD_TOKEN answered.
type WhoamiSession struct {
	// User is the signed-in user, from the userinfo endpoint, then the token
	// claims.
	User User
	// CIToken reports that no session is stored and PUTNAMI_CLOUD_TOKEN stands
	// in for one on the workspace calls.
	CIToken bool
	// ExpiresAt is when the session ends, zero when it is not known.
	ExpiresAt time.Time
	Err       error
}

// WhoamiLink is the workspace link of the repository: the id and name
// .putnami/cloud-link.json records. Err is the error reading it.
type WhoamiLink struct {
	WorkspaceID   string
	WorkspaceName string
	Err           error
}

// WhoamiStatusNode is the whoami status: the session on this machine and the
// workspace link of the repository at workspaceRoot. It reads the stored
// session (refreshed when it expired), asks the issuer's userinfo endpoint
// who the session belongs to, and reads the link file. Every request ends
// when ioctx.Context ends.
func WhoamiStatusNode(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) clicore.StatusNode {
	return WhoamiStatusNodeFrom(readWhoamiSession(params, env, ioctx), readWhoamiLink(workspaceRoot), now(ioctx))
}

// WhoamiStatusNodeFrom folds the session and the link into the whoami node.
// The root state is the worst of the two checks.
func WhoamiStatusNodeFrom(session WhoamiSession, link WhoamiLink, at time.Time) clicore.StatusNode {
	sessionNode := whoamiSessionNode(session)
	linkNode := whoamiLinkNode(link)
	node := clicore.StatusNode{
		ID:       "whoami",
		Title:    "whoami",
		State:    clicore.WorstStatus(sessionNode.State, linkNode.State),
		Children: []clicore.StatusNode{sessionNode, linkNode},
	}
	var parts []string
	switch {
	case sessionNode.State == clicore.StatusOK && session.CIToken:
		parts = append(parts, "PUTNAMI_CLOUD_TOKEN")
	case sessionNode.State == clicore.StatusOK:
		parts = append(parts, userLabel(session.User))
	default:
		parts = append(parts, sessionNode.Detail)
	}
	if linkNode.State == clicore.StatusOK {
		parts = append(parts, "workspace "+shortWorkspace(link))
	} else {
		parts = append(parts, linkNode.Detail)
	}
	node.Detail = strings.Join(parts, ", ")
	for _, child := range node.Children {
		if child.State != clicore.StatusOK && child.Fix != "" {
			node.Fix = child.Fix
			break
		}
	}
	if !session.ExpiresAt.IsZero() && session.Err == nil && !session.CIToken {
		remaining := max(session.ExpiresAt.Sub(at).Round(time.Second), 0)
		node.Metrics = append(node.Metrics, clicore.StatusMetric{
			ID: "session_expires_in", Title: "session expires in", Value: remaining.Seconds(),
			Unit: clicore.UnitSeconds, Kind: clicore.MetricHealth,
		})
	}
	return node
}

// whoamiSessionNode is the session check. A refused or missing session
// fails; a session that could not be read for another reason (the sign-in
// service did not answer a refresh) is unknown.
func whoamiSessionNode(session WhoamiSession) clicore.StatusNode {
	node := clicore.StatusNode{ID: "whoami.session", Title: "session"}
	switch {
	case session.Err == nil && session.CIToken:
		node.State = clicore.StatusOK
		node.Detail = "PUTNAMI_CLOUD_TOKEN is set; no session on this machine"
	case session.Err == nil:
		node.State = clicore.StatusOK
		node.Detail = SignedInAs(userLabel(session.User))
	case clicore.ExitCode(session.Err) == clicore.ExitAuth:
		node.State = clicore.StatusFailing
		node.Detail = "not signed in"
		node.Fix = "putnami cloud login"
	default:
		return clicore.UnknownStatus(node.ID, node.Title, session.Err, "putnami cloud login")
	}
	return node
}

// whoamiLinkNode is the workspace link check.
func whoamiLinkNode(link WhoamiLink) clicore.StatusNode {
	node := clicore.StatusNode{ID: "whoami.workspace", Title: "workspace"}
	if link.Err != nil || link.WorkspaceID == "" {
		node.State = clicore.StatusFailing
		node.Detail = "this repository is not linked to a workspace"
		node.Fix = "putnami cloud setup"
		return node
	}
	node.State = clicore.StatusOK
	node.Detail = "linked to workspace " + link.WorkspaceID
	if link.WorkspaceName != "" && link.WorkspaceName != link.WorkspaceID {
		node.Detail = "linked to workspace " + link.WorkspaceName + " (" + link.WorkspaceID + ")"
	}
	return node
}

// SignedInAs is the one wording every surface uses for a working session:
// `login`, `whoami` and the `putnami cloud status` synthesis.
func SignedInAs(identity string) string {
	if strings.TrimSpace(identity) == "" {
		return "signed in"
	}
	return "signed in as " + identity
}

// SignedOutMessage is what `putnami cloud logout` prints.
const SignedOutMessage = "Signed out of Putnami Cloud."

// readWhoamiSession reads the stored session, refreshing it when it expired,
// and asks the userinfo endpoint who it belongs to. A userinfo endpoint that
// refuses the token (401) ends the session; one that does not answer falls
// back to the token claims, since the session still works.
func readWhoamiSession(params map[string]any, env map[string]string, ioctx clicore.IO) WhoamiSession {
	auth, err := clicore.ActiveAuth(params, env, ioctx)
	if err != nil {
		if clicore.EnvGet(env, "PUTNAMI_CLOUD_TOKEN") != "" {
			return WhoamiSession{CIToken: true}
		}
		return WhoamiSession{Err: err}
	}
	claims := clicore.DecodeJWT(auth.AccessToken)
	session := WhoamiSession{User: userIdentity(nil, claims), ExpiresAt: sessionExpiry(auth)}
	client := contextClient(ioctx)
	endpoints := clicore.AuthEndpoints(params, env, client, auth.Issuer)
	userinfo, err := fetchUserInfo(client, endpoints, auth.AccessToken)
	switch {
	case err == nil:
		session.User = userIdentity(userinfo, claims)
	case clicore.ExitCode(err) == clicore.ExitAuth:
		return WhoamiSession{Err: err}
	}
	return session
}

// sessionExpiry is when the session ends: the refresh token's expiry when it
// carries one, else the access token's when there is no refresh token. A
// session that refreshes with an opaque refresh token has no known end.
func sessionExpiry(auth *clicore.StoredToken) time.Time {
	if auth.RefreshToken != "" {
		return claimTime(clicore.DecodeJWT(auth.RefreshToken)["exp"])
	}
	if expires, err := time.Parse(time.RFC3339Nano, auth.ExpiresAt); err == nil {
		return expires
	}
	return time.Time{}
}

func claimTime(value any) time.Time {
	if seconds, ok := value.(float64); ok && seconds > 0 {
		return time.Unix(int64(seconds), 0).UTC()
	}
	return time.Time{}
}

func readWhoamiLink(workspaceRoot string) WhoamiLink {
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return WhoamiLink{Err: err}
	}
	return WhoamiLink{
		WorkspaceID:   strings.TrimSpace(clicore.StringValue(link["workspace_id"])),
		WorkspaceName: strings.TrimSpace(clicore.StringValue(link["workspace_name"])),
	}
}

// userLabel names the user the way people read it: email, then name, then id.
func userLabel(user User) string {
	return clicore.FirstString(user.Email, user.Name, user.ID, "an unnamed user")
}

// shortWorkspace is the workspace name, or the first eight characters of its
// id, the way a short commit sha is read.
func shortWorkspace(link WhoamiLink) string {
	if link.WorkspaceName != "" {
		return link.WorkspaceName
	}
	if len(link.WorkspaceID) > 8 {
		return link.WorkspaceID[:8]
	}
	return link.WorkspaceID
}

func now(ioctx clicore.IO) time.Time {
	if ioctx.Now != nil {
		return ioctx.Now()
	}
	return time.Now()
}

// contextClient is ioctx's HTTP client with every request bound to
// ioctx.Context, so `putnami cloud status` can cancel the userinfo read.
func contextClient(ioctx clicore.IO) *http.Client {
	base := ioctx.Client
	if base == nil {
		base = http.DefaultClient
	}
	if ioctx.Context == nil || ioctx.Context.Done() == nil {
		return base
	}
	bound := *base
	transport := bound.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	bound.Transport = contextTransport{ctx: ioctx.Context, base: transport}
	return &bound
}

// contextTransport sends each request under ctx.
type contextTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t contextTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := t.ctx.Err(); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(request.WithContext(t.ctx))
}
