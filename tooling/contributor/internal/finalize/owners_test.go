package finalize

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/tooling/contributor/internal/host"
)

// ownersHost serves workspace.owners on a Unix socket from a fixed table of
// owners, or refuses every call. It records the paths each call asked about.
type ownersHost struct {
	socket, token string
	owners        map[string][]wire.OwningProject
	refuse        *wire.Error

	mu    sync.Mutex
	asked [][]string
}

func newOwnersHost(t *testing.T, owners map[string][]wire.OwningProject, refuse *wire.Error) *ownersHost {
	t.Helper()
	// A Unix socket path is limited to about 100 bytes, shorter than a
	// test's temporary directory on macOS.
	dir, err := os.MkdirTemp("/tmp", "po")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	h := &ownersHost{socket: filepath.Join(dir, "host.sock"), token: "launch-token", owners: owners, refuse: refuse}
	listener, err := net.Listen("unix", h.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go h.serve(conn)
		}
	}()
	return h
}

func (h *ownersHost) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		frame, err := wire.ReadFrame(reader, wire.MaxFrameBytes)
		if err != nil {
			return
		}
		var request wire.Request
		var params wire.WorkspaceOwnersParams
		if json.Unmarshal(frame, &request) != nil {
			return
		}
		response := wire.Response{JSONRPC: wire.JSONRPCVersion, ID: request.ID}
		switch {
		case request.Token != h.token || request.Method != wire.MethodWorkspaceOwners || json.Unmarshal(request.Params, &params) != nil:
			response.Error = &wire.Error{Code: wire.CodeUnauthorized, Message: "refused"}
		case h.refuse != nil:
			h.record(params.Paths)
			response.Error = h.refuse
		default:
			h.record(params.Paths)
			result := wire.WorkspaceOwnersResult{}
			for _, path := range params.Paths {
				projects := h.owners[path]
				if projects == nil {
					projects = []wire.OwningProject{}
				}
				result.Owners = append(result.Owners, wire.PathOwners{Path: path, Projects: projects})
			}
			response.Result, _ = json.Marshal(result)
		}
		reply, err := wire.EncodeFrame(response)
		if err != nil {
			return
		}
		if _, err := conn.Write(reply); err != nil {
			return
		}
	}
}

func (h *ownersHost) record(paths []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked = append(h.asked, slices.Clone(paths))
}

func (h *ownersHost) calls() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.asked)
}

// channel returns the host.Channel a launch on h takes from its environment.
func (h *ownersHost) channel(t *testing.T) *host.Channel {
	t.Helper()
	t.Setenv(wire.SocketEnv, h.socket)
	t.Setenv(wire.TokenEnv, h.token)
	channel := host.Take("run `putnami change finalize`")
	t.Cleanup(func() { _ = channel.Close() })
	return channel
}

// ownersRepo is a Git repository whose workspace is the directory ws below
// its top level. Its base commit holds four featureless projects (lib,
// changed, manifest and mistyped), one feature owner (owner) and one
// featureless project with a feature manifest (former). Its head commit makes
// changed featureless only on the branch, adds a feature manifest to
// manifest, removes the one of former, gives the putnami.json of mistyped a
// type error outside featureAuthority, and adds the new featureless project
// fresh.
type ownersRepo struct {
	top, root, base string
}

const (
	featurelessConfig = `{"name":"%s","featureAuthority":{"none":"Internal plumbing with no user-facing feature."}}`
	ownerConfig       = `{"name":"%s","featureAuthority":{"owner":"cli/contributor-workflows"}}`
)

func newOwnersRepo(t *testing.T) ownersRepo {
	t.Helper()
	top := t.TempDir()
	repo := ownersRepo{top: top, root: filepath.Join(top, "ws")}
	write := func(file, content string) {
		t.Helper()
		path := filepath.Join(repo.root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(file string) {
		t.Helper()
		if err := os.Remove(filepath.Join(repo.root, filepath.FromSlash(file))); err != nil {
			t.Fatal(err)
		}
	}
	project := func(name, config string) {
		write(name+"/putnami.json", strings.ReplaceAll(config, "%s", name))
		write(name+"/source.go", "package "+name+"\n")
	}
	project("lib", featurelessConfig)
	project("changed", ownerConfig)
	project("manifest", featurelessConfig)
	project("owner", ownerConfig)
	project("former", featurelessConfig)
	project("mistyped", featurelessConfig)
	write("former/putnami.features.json", "{}\n")
	write("../outside.txt", "outside the workspace\n")
	repo.git(t, "init", "-q", top)
	repo.git(t, "add", "-A")
	repo.git(t, "commit", "-q", "-m", "chore: base")
	repo.base = strings.TrimSpace(repo.git(t, "rev-parse", "HEAD"))
	project("changed", featurelessConfig)
	write("manifest/putnami.features.json", "{}\n")
	remove("former/putnami.features.json")
	project("fresh", featurelessConfig)
	write("mistyped/putnami.json", `{"name":"mistyped","tags":"x","featureAuthority":{"none":"Internal plumbing with no user-facing feature."}}`)
	repo.git(t, "add", "-A")
	repo.git(t, "commit", "-q", "-m", "feat: head")
	return repo
}

// git runs git in the workspace with a fixed identity and no signing.
func (r ownersRepo) git(t *testing.T, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	command.Dir = r.root
	command.Env = append(gitEnvironment(os.Environ()), "GIT_CONFIG_GLOBAL=/dev/null")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

// projectOwners is the table the fake host answers from: each source file
// belongs to its project.
func projectOwners() map[string][]wire.OwningProject {
	owners := map[string][]wire.OwningProject{}
	for _, name := range []string{"lib", "changed", "manifest", "owner", "former", "mistyped", "fresh"} {
		owners[name+"/source.go"] = []wire.OwningProject{{ID: "/" + name, Path: name}}
		owners[name+"/putnami.json"] = []wire.OwningProject{{ID: "/" + name, Path: name}}
	}
	return owners
}

// TestOwnersComeFromTheHostAndFeaturelessnessFromBothRevisions pins the
// ownership the hosted partial delivery reads: the host names the owners,
// and a project is exempt only when it is featureless on the branch and at
// base, or new on the branch.
func TestOwnersComeFromTheHostAndFeaturelessnessFromBothRevisions(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "change-finalize-parity", "ownership-comes-from-the-host-channel")
	spectest.Proves(t, "tooling/contributor-executable", "squash-ready-publication", "a-featureless-partial-needs-no-workflow")
	repo := newOwnersRepo(t)
	for _, tc := range []struct {
		name     string
		paths    []string
		projects []string
		unowned  []string
	}{
		{name: "featureless at head and base", paths: []string{"ws/lib/source.go", "ws/lib/putnami.json"}},
		{name: "featureless at head only", paths: []string{"ws/changed/putnami.json"}, projects: []string{"/changed"}},
		{name: "new on the branch", paths: []string{"ws/fresh/source.go"}},
		{name: "a feature manifest at head", paths: []string{"ws/manifest/source.go"}, projects: []string{"/manifest"}},
		{name: "a feature manifest at base", paths: []string{"ws/former/source.go"}, projects: []string{"/former"}},
		{name: "a feature owner", paths: []string{"ws/owner/source.go"}, projects: []string{"/owner"}},
		// The workspace loader decodes no config from this putnami.json, so the
		// core reads the project as one that holds requirements.
		{name: "a type error outside featureAuthority at head", paths: []string{"ws/mistyped/source.go"}, projects: []string{"/mistyped"}},
		{name: "a path no project owns", paths: []string{"ws/README.md"}, unowned: []string{"ws/README.md"}},
		{name: "every kind at once", paths: []string{"ws/README.md", "ws/owner/source.go", "ws/lib/source.go", "ws/manifest/source.go"},
			projects: []string{"/manifest", "/owner"}, unowned: []string{"ws/README.md"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newOwnersHost(t, projectOwners(), nil)
			resolve := HostOwners(repo.root, fake.channel(t).Owners)
			got, err := resolve(context.Background(), repo.base, tc.paths)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.Projects, tc.projects) || !slices.Equal(got.Unowned, tc.unowned) {
				t.Errorf("ownership %+v, want projects %v and unowned %v", got, tc.projects, tc.unowned)
			}
			asked := make([]string, 0, len(tc.paths))
			for _, path := range tc.paths {
				asked = append(asked, strings.TrimPrefix(path, "ws/"))
			}
			if calls := fake.calls(); len(calls) != 1 || !slices.Equal(calls[0], asked) {
				t.Errorf("the host was asked %v, want one call for %v", calls, asked)
			}
		})
	}
}

// TestAPathOutsideTheWorkspaceIsUnownedWithoutAskingTheHost pins that a
// changed path outside the workspace prefix has no owner, and that the host
// is not asked about it.
func TestAPathOutsideTheWorkspaceIsUnownedWithoutAskingTheHost(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "change-finalize-parity", "ownership-comes-from-the-host-channel")
	repo := newOwnersRepo(t)
	fake := newOwnersHost(t, projectOwners(), nil)
	resolve := HostOwners(repo.root, fake.channel(t).Owners)
	got, err := resolve(context.Background(), repo.base, []string{"outside.txt"})
	if err != nil || len(got.Projects) != 0 || !slices.Equal(got.Unowned, []string{"outside.txt"}) {
		t.Fatalf("ownership %+v, %v, want outside.txt unowned", got, err)
	}
	got, err = resolve(context.Background(), repo.base, []string{"outside.txt", "ws/owner/source.go"})
	if err != nil || !slices.Equal(got.Projects, []string{"/owner"}) || !slices.Equal(got.Unowned, []string{"outside.txt"}) {
		t.Fatalf("ownership %+v, %v, want /owner and outside.txt unowned", got, err)
	}
	if calls := fake.calls(); len(calls) != 1 || !slices.Equal(calls[0], []string{"owner/source.go"}) {
		t.Errorf("the host was asked %v, want one call for owner/source.go", calls)
	}
}

// TestAHostRefusalFailsTheOwnership pins that a refusal by the host is an
// error naming the call, and never an empty ownership.
func TestAHostRefusalFailsTheOwnership(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "change-finalize-parity", "ownership-comes-from-the-host-channel")
	repo := newOwnersRepo(t)
	fake := newOwnersHost(t, nil, &wire.Error{Code: wire.CodeUnavailable, Message: "this command runs outside any workspace, so no project owns a path"})
	resolve := HostOwners(repo.root, fake.channel(t).Owners)
	_, err := resolve(context.Background(), repo.base, []string{"ws/lib/source.go"})
	var refusal *wire.Error
	if !errors.As(err, &refusal) || refusal.Code != wire.CodeUnavailable ||
		!strings.HasPrefix(err.Error(), "resolve changed project ownership: workspace.owners through the putnami host: ") {
		t.Fatalf("error %v, want the host's refusal naming the call", err)
	}
}

// TestAPathTheChannelCannotCarryFailsClosed pins that a changed path that is
// not valid UTF-8 fails the ownership with an error naming it, before the
// host is asked anything.
func TestAPathTheChannelCannotCarryFailsClosed(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "change-finalize-parity", "ownership-comes-from-the-host-channel")
	repo := newOwnersRepo(t)
	fake := newOwnersHost(t, projectOwners(), nil)
	resolve := HostOwners(repo.root, fake.channel(t).Owners)
	_, err := resolve(context.Background(), repo.base, []string{"ws/lib/source.go", "ws/lib/caf\xe9.go"})
	if err == nil || !strings.Contains(err.Error(), `"ws/lib/caf\xe9.go"`) || !strings.Contains(err.Error(), "not a valid UTF-8 workspace path") {
		t.Fatalf("error %v, want a refusal naming the path", err)
	}
	if calls := fake.calls(); len(calls) != 0 {
		t.Errorf("the host was asked %v, want nothing", calls)
	}
}

// TestOwnershipWithoutAChannelNamesTheLaunch pins that a run with no host
// channel cannot resolve ownership, and says which launch opens one.
func TestOwnershipWithoutAChannelNamesTheLaunch(t *testing.T) {
	repo := newOwnersRepo(t)
	t.Setenv(wire.SocketEnv, "")
	t.Setenv(wire.TokenEnv, "")
	channel := host.Take("run `putnami change finalize`")
	_, err := HostOwners(repo.root, channel.Owners)(context.Background(), repo.base, []string{"ws/lib/source.go"})
	if err == nil || !strings.Contains(err.Error(), "run `putnami change finalize` (a hosted run opens no channel; "+wire.SocketEnv+" and "+wire.TokenEnv+" not set)") {
		t.Fatalf("error %v, want the launch remedy", err)
	}
}

// TestFileAtReadsTheBaseRevision pins the base reads: a file at base, a file
// that base lacks, and a revision that could read as a flag.
func TestFileAtReadsTheBaseRevision(t *testing.T) {
	t.Parallel()
	repo := newOwnersRepo(t)
	content, found, err := fileAt(repo.root, repo.base, "changed/putnami.json")
	if err != nil || !found || !strings.Contains(string(content), `"owner"`) {
		t.Fatalf("fileAt = %q, %t, %v, want the base config", content, found, err)
	}
	if _, found, err := fileAt(repo.root, repo.base, "fresh/putnami.json"); err != nil || found {
		t.Fatalf("fileAt of a file base lacks = %t, %v", found, err)
	}
	for _, revision := range []string{"", "-p", " HEAD", "HEAD\n", strings.Repeat("a", 1025)} {
		if _, _, err := fileAt(repo.root, revision, "lib/putnami.json"); err == nil || !strings.HasPrefix(err.Error(), "invalid revision") {
			t.Errorf("fileAt(%q) error %v, want an invalid revision", revision, err)
		}
	}
	if prefix, err := workspacePrefix(repo.root); err != nil || prefix != "ws/" {
		t.Errorf("workspacePrefix = %q, %v, want ws/", prefix, err)
	}
}
