package distributioncli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	ciproto "go.putnami.dev/protocol/ci"
	protocol "go.putnami.dev/protocol/distribution"
)

const channelsTestCI = `{
  "version": 3,
  "commands": ["lint"],
  "distribution": {"namespace": "putnami", "channels": {"canary": {}, "stable": {}}},
  "envs": {
    "prod": {"channel": "stable", "variants": {"preview": {"channel": "beta", "routing": {}}}},
    "dev": {"workloads": [{"select": "api", "channel": "nightly"}]}
  }
}`

// channelsFixture is a workspace whose putnami.ci.json names the release-set
// namespace and declares channels, with a put bearer on this machine.
func channelsFixture(t *testing.T, document string) (string, map[string]string, clicore.IO) {
	t.Helper()
	root, env, ioctx, _ := releaseSetDirectBearerFixture(t)
	if document != "" {
		if err := os.WriteFile(filepath.Join(root, ciproto.Filename), []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, env, ioctx
}

// channelServer answers the release-set provider by operation and records
// each request body.
type channelServer struct {
	mu     sync.Mutex
	bodies map[string][]json.RawMessage
	answer func(operation string, body []byte) (int, any)
}

func newChannelServer(t *testing.T, ioctx *clicore.IO, answer func(operation string, body []byte) (int, any)) *channelServer {
	t.Helper()
	server := &channelServer{bodies: map[string][]json.RawMessage{}, answer: answer}
	ioctx.Client = &http.Client{Transport: workflowRoundTrip(func(request *http.Request) (*http.Response, error) {
		operation := strings.TrimPrefix(request.URL.Path, releaseSetProviderPath)
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		server.mu.Lock()
		server.bodies[operation] = append(server.bodies[operation], body)
		server.mu.Unlock()
		status, response := server.answer(operation, body)
		return releaseSetRawResponse(status, mustJSON(t, response)), nil
	})}
	return server
}

func (s *channelServer) calls(operation string) []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[operation]
}

func channelStatusAnswer(ref protocol.ReleaseSetRef, generation uint64, observed map[string]uint64) *protocol.ChannelStatusResponse {
	return &protocol.ChannelStatusResponse{
		ProtocolVersion: protocol.ProtocolVersion,
		Desired:         &protocol.ChannelHead{Ref: ref, Generation: generation},
		Observed:        observed,
	}
}

func TestChannelsSetPromotesAgainstTheCurrentHead(t *testing.T) {
	root, env, ioctx := channelsFixture(t, channelsTestCI)
	baseSet, baseRef := releaseSetSnapshot(t, "putnami", "0.1.0-base", 'a')
	_, targetRef := releaseSetSnapshot(t, "putnami", "0.2.0-target", 'b')
	server := newChannelServer(t, &ioctx, func(operation string, _ []byte) (int, any) {
		switch operation {
		case protocol.ResolveCommand:
			return http.StatusOK, &protocol.ResolveResponse{ProtocolVersion: protocol.ProtocolVersion, Heads: map[string]*protocol.ChannelHead{
				"stable": {Ref: baseRef, Generation: 1, ReleaseSet: &baseSet},
			}}
		default:
			return http.StatusOK, &protocol.ReleaseResponse{ProtocolVersion: protocol.ProtocolVersion, Outcome: protocol.ReleaseOutcomeReleased,
				Current: map[string]*protocol.ChannelHead{"stable": {Ref: targetRef, Generation: 2}}}
		}
	})
	var stdout []string
	ioctx.Stdout = func(line string) { stdout = append(stdout, line) }

	if err := ChannelsSet(map[string]any{"from": "canary"}, []string{"stable"}, root, env, ioctx); err != nil {
		t.Fatalf("channels set: %v", err)
	}
	sets := server.calls(protocol.ChannelSetCommand)
	if len(server.calls(protocol.ResolveCommand)) != 1 || len(sets) != 1 {
		t.Fatalf("provider calls = %v, want one resolve and one channel-set", server.bodies)
	}
	var request protocol.ChannelSetRequest
	if err := json.Unmarshal(sets[0], &request); err != nil {
		t.Fatal(err)
	}
	if request.Namespace != "putnami" || request.Channel != "stable" || request.From.Channel != "canary" || request.Expected == nil || request.Expected.ID != baseRef.ID {
		t.Fatalf("channel-set request = %+v, want stable from canary expecting %s", request, baseRef.ID)
	}
	if want := "stable -> " + targetRef.ID + " (generation 2)"; !strings.Contains(strings.Join(stdout, "\n"), want) {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestChannelsSetWithAStatedExpectationSkipsTheResolve(t *testing.T) {
	root, env, ioctx := channelsFixture(t, channelsTestCI)
	_, baseRef := releaseSetSnapshot(t, "putnami", "0.1.0-base", 'a')
	_, targetRef := releaseSetSnapshot(t, "putnami", "0.2.0-target", 'b')
	server := newChannelServer(t, &ioctx, func(string, []byte) (int, any) {
		return http.StatusOK, &protocol.ReleaseResponse{ProtocolVersion: protocol.ProtocolVersion, Outcome: protocol.ReleaseOutcomeConflict,
			Current: map[string]*protocol.ChannelHead{"stable": {Ref: targetRef, Generation: 3}}}
	})
	err := ChannelsSet(map[string]any{"from": targetRef.ID, "expected": baseRef.ID}, []string{"stable"}, root, env, ioctx)
	if clicore.ExitCode(err) != clicore.ExitAPI || !strings.Contains(err.Error(), "moved while this command ran") {
		t.Fatalf("conflict error = %v, want an API error asking to run again", err)
	}
	if len(server.calls(protocol.ResolveCommand)) != 0 {
		t.Fatal("a stated --expected must not resolve the head again")
	}
	var request protocol.ChannelSetRequest
	if err := json.Unmarshal(server.calls(protocol.ChannelSetCommand)[0], &request); err != nil {
		t.Fatal(err)
	}
	if request.From.ReleaseID != targetRef.ID || request.Expected.Digest != baseRef.Digest {
		t.Fatalf("channel-set request = %+v, want the stated release set and expectation", request)
	}
}

func TestChannelsSetRefusesBadArgumentsBeforeTheProvider(t *testing.T) {
	root, env, ioctx := channelsFixture(t, channelsTestCI)
	server := newChannelServer(t, &ioctx, func(string, []byte) (int, any) { return http.StatusInternalServerError, map[string]any{} })
	for _, tc := range []struct {
		name   string
		params map[string]any
		args   []string
		want   string
	}{
		{"no channel", map[string]any{"from": "canary"}, nil, "takes one channel"},
		{"two channels", map[string]any{"from": "canary"}, []string{"stable", "beta"}, "takes one channel"},
		{"bad channel", map[string]any{"from": "canary"}, []string{"Stable!"}, "not a portable channel name"},
		{"no source", map[string]any{}, []string{"stable"}, "needs --from"},
		{"bad source", map[string]any{"from": "Canary!"}, []string{"stable"}, "not a portable channel name"},
		{"bad expectation", map[string]any{"from": "canary", "expected": "rs_short"}, []string{"stable"}, "is not a release-set id"},
	} {
		err := ChannelsSet(tc.params, tc.args, root, env, ioctx)
		if clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want a usage error containing %q", tc.name, err, tc.want)
		}
	}
	if len(server.calls(protocol.ChannelSetCommand)) != 0 {
		t.Fatal("a refused command reached the provider")
	}
}

func TestChannelsStatusWaitsUntilEveryRegistryCaughtUp(t *testing.T) {
	previous := channelPollInterval
	channelPollInterval = time.Millisecond
	t.Cleanup(func() { channelPollInterval = previous })
	root, env, ioctx := channelsFixture(t, channelsTestCI)
	_, ref := releaseSetSnapshot(t, "putnami", "0.2.0-target", 'b')
	polls := 0
	newChannelServer(t, &ioctx, func(string, []byte) (int, any) {
		polls++
		goGeneration := uint64(1)
		if polls >= 3 {
			goGeneration = 2
		}
		return http.StatusOK, channelStatusAnswer(ref, 2, map[string]uint64{"npm": 2, "go": goGeneration})
	})
	var stdout []string
	ioctx.Stdout = func(line string) { stdout = append(stdout, line) }

	if err := ChannelsStatus(map[string]any{}, []string{"canary", "--wait", "1m"}, root, env, ioctx); err != nil {
		t.Fatalf("channels status --wait: %v", err)
	}
	if polls != 3 {
		t.Fatalf("polls = %d, want 3", polls)
	}
	want := strings.Join([]string{
		"canary  ok  " + ref.ID + ", generation 2",
		"",
		"  METRIC             VALUE  WINDOW",
		"  registries behind  0      now",
		"",
		"  STATE  CHECK  DETAIL",
		"  ok     go     generation 2, up to date",
		"  ok     npm    generation 2, up to date",
	}, "\n")
	if text := strings.Join(stdout, "\n"); text != want {
		t.Fatalf("report = %q, want %q", text, want)
	}
}

// TestChannelsStatusIsDegradedWhileARegistryIsBehind pins the exit rule: a
// registry behind is degraded and exits 0, because registries converge on
// their own; with --wait, a registry still behind when the wait ends fails.
// Either way the command writes one envelope.
func TestChannelsStatusIsDegradedWhileARegistryIsBehind(t *testing.T) {
	previous := channelPollInterval
	channelPollInterval = time.Millisecond
	t.Cleanup(func() { channelPollInterval = previous })
	root, env, ioctx := channelsFixture(t, channelsTestCI)
	_, ref := releaseSetSnapshot(t, "putnami", "0.2.0-target", 'b')
	newChannelServer(t, &ioctx, func(string, []byte) (int, any) {
		return http.StatusOK, channelStatusAnswer(ref, 2, map[string]uint64{"npm": 2, "go": 1})
	})
	var stdout []string
	ioctx.Stdout = func(line string) { stdout = append(stdout, line) }
	if err := ChannelsStatus(map[string]any{"output": "json"}, []string{"canary"}, root, env, ioctx); err != nil {
		t.Fatalf("a registry behind = %v, want exit 0", err)
	}
	var envelope struct {
		Data clicore.StatusNode `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.Join(stdout, "\n")), &envelope); err != nil {
		t.Fatalf("stdout = %q, want one envelope: %v", stdout, err)
	}
	if node := envelope.Data; node.State != clicore.StatusDegraded || !strings.HasSuffix(node.Detail, "; behind on go") ||
		node.Fix != "putnami cloud channels status canary --wait 5m" || node.Children[0].Detail != "generation 1 of 2, behind" {
		t.Fatalf("node = %+v, want degraded naming go", node)
	}

	stdout = nil
	err := ChannelsStatus(map[string]any{"output": "json"}, []string{"canary", "--wait", "5ms"}, root, env, ioctx)
	if clicore.ExitCode(err) != clicore.ExitFailure || !strings.Contains(err.Error(), "behind on go; still behind after 5ms") {
		t.Fatalf("error = %v, want failing after the wait", err)
	}
	if len(stdout) != 0 {
		t.Fatalf("a failing structured status printed %q; the error carries the envelope", stdout)
	}
}

// TestChannelsStatusWaitFailsWhenTheProviderNeverAnswers: --wait gates a
// script, so a provider error is read again until the deadline, then fails.
// Without --wait the same error is unknown and exits 0.
func TestChannelsStatusWaitFailsWhenTheProviderNeverAnswers(t *testing.T) {
	previous := channelPollInterval
	channelPollInterval = time.Millisecond
	t.Cleanup(func() { channelPollInterval = previous })
	root, env, ioctx := channelsFixture(t, channelsTestCI)
	polls := 0
	newChannelServer(t, &ioctx, func(string, []byte) (int, any) {
		polls++
		return http.StatusBadGateway, map[string]any{}
	})
	ioctx.Stdout = func(string) {}
	if err := ChannelsStatus(map[string]any{}, []string{"canary"}, root, env, ioctx); err != nil {
		t.Fatalf("an unreadable channel without --wait = %v, want unknown and exit 0", err)
	}
	polls = 0
	err := ChannelsStatus(map[string]any{}, []string{"canary", "--wait", "20ms"}, root, env, ioctx)
	if clicore.ExitCode(err) != clicore.ExitFailure || !strings.Contains(err.Error(), "still unreadable after 20ms") {
		t.Fatalf("error = %v, want failing once the wait ends", err)
	}
	if polls < 2 {
		t.Fatalf("polls = %d, want the provider read again until the deadline", polls)
	}
}

func TestChannelsStatusReportsEveryDeclaredChannel(t *testing.T) {
	root, env, ioctx := channelsFixture(t, channelsTestCI)
	_, ref := releaseSetSnapshot(t, "putnami", "0.2.0-target", 'b')
	newChannelServer(t, &ioctx, func(_ string, body []byte) (int, any) {
		var request protocol.ChannelStatusRequest
		_ = json.Unmarshal(body, &request)
		switch request.Channel {
		case "canary":
			return http.StatusInternalServerError, map[string]any{"error": "unavailable"}
		case "stable":
			return http.StatusOK, channelStatusAnswer(ref, 1, map[string]uint64{})
		default:
			return http.StatusOK, &protocol.ChannelStatusResponse{ProtocolVersion: protocol.ProtocolVersion}
		}
	})
	var stdout []string
	ioctx.Stdout = func(line string) { stdout = append(stdout, line) }
	if err := ChannelsStatus(map[string]any{}, nil, root, env, ioctx); err != nil {
		t.Fatalf("an unknown channel = %v, want exit 0", err)
	}
	text := strings.Join(stdout, "\n")
	for _, want := range []string{
		"channels  unknown  4 channels in namespace putnami; canary not read, stable behind",
		"  registries behind  1      now",
		"  ok        beta     no release yet",
		"  unknown   canary   status channel canary in namespace putnami:",
		"  degraded  stable   " + ref.ID + ", generation 1; no registry reports on it",
		"                     fix: putnami cloud channels status stable --wait 5m",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("report = %q, want %q", text, want)
		}
	}
	if err := ChannelsStatus(map[string]any{"strict": true}, nil, root, env, ioctx); clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("--strict = %v, want exit %d", err, clicore.ExitFailure)
	}
}

func TestChannelsStatusKeepsTheAuthError(t *testing.T) {
	root, env, ioctx := channelsFixture(t, channelsTestCI)
	newChannelServer(t, &ioctx, func(string, []byte) (int, any) {
		return http.StatusUnauthorized, map[string]any{"error": "unauthorized"}
	})
	if err := ChannelsStatus(map[string]any{}, nil, root, env, ioctx); clicore.ExitCode(err) != clicore.ExitAuth {
		t.Fatalf("a refused bearer = %v, want exit %d", err, clicore.ExitAuth)
	}
	if node := ChannelsStatusNode(map[string]any{}, root, env, ioctx); node.State != clicore.StatusUnknown || node.Fix != "putnami cloud login" {
		t.Fatalf("node = %+v, want unknown with the login command", node)
	}
}

func TestChannelsStatusWithoutADeclaredChannel(t *testing.T) {
	root, env, ioctx := channelsFixture(t, "")
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"), []byte(`{"name":"acme"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout []string
	ioctx.Stdout = func(line string) { stdout = append(stdout, line) }
	if err := ChannelsStatus(map[string]any{}, nil, root, env, ioctx); err != nil {
		t.Fatalf("channels status: %v", err)
	}
	if text := strings.Join(stdout, "\n"); text != "channels  ok  no channel declared in putnami.ci.json" {
		t.Fatalf("stdout = %q", text)
	}
	if namespace, err := ChannelNamespace(root); err != nil || namespace != "acme" {
		t.Fatalf("namespace = %q, %v, want the workspace name", namespace, err)
	}
}

func TestChannelsStatusRefusesBadArguments(t *testing.T) {
	root, env, ioctx := channelsFixture(t, channelsTestCI)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"canary", "stable"}, "at most one channel"},
		{[]string{"--wait", "5m"}, "--wait needs one channel"},
		{[]string{"canary", "--wait"}, "--wait needs a duration"},
		{[]string{"canary", "--wait=soon"}, "takes a duration"},
		{[]string{"Canary!"}, "not a portable channel name"},
	} {
		err := ChannelsStatus(map[string]any{}, tc.args, root, env, ioctx)
		if clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("channels status %v error = %v, want a usage error containing %q", tc.args, err, tc.want)
		}
	}
}

func TestChannelWaitReadsTheDurationFromArgvOrParams(t *testing.T) {
	for _, tc := range []struct {
		params map[string]any
		args   []string
		want   time.Duration
		rest   []string
	}{
		{map[string]any{}, []string{"canary", "--wait", "30s"}, 30 * time.Second, []string{"canary"}},
		{map[string]any{}, []string{"--wait=5m", "canary"}, 5 * time.Minute, []string{"canary"}},
		{map[string]any{"wait": "2m"}, []string{"canary"}, 2 * time.Minute, []string{"canary"}},
		{map[string]any{"wait": "true"}, []string{"canary"}, 0, []string{"canary"}},
	} {
		option, err := channelWait(tc.params, tc.args)
		if err != nil || option.duration != tc.want || strings.Join(option.rest, " ") != strings.Join(tc.rest, " ") {
			t.Fatalf("channelWait(%v, %v) = %+v, %v; want %s and %v", tc.params, tc.args, option, err, tc.want, tc.rest)
		}
	}
}

func TestDeclaredChannelsReadsDistributionAndEnvironments(t *testing.T) {
	root, _, _ := channelsFixture(t, channelsTestCI)
	channels, err := DeclaredChannels(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(channels, ","); got != "beta,canary,nightly,stable" {
		t.Fatalf("declared channels = %s", got)
	}
	broken, _, _ := channelsFixture(t, `{"version": 3, "commands": ["lint"], "unknown": true}`)
	if _, err := DeclaredChannels(broken); clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("invalid putnami.ci.json error = %v, want a usage error", err)
	}
}

func TestChannelsStatusNodeFoldsEveryChannel(t *testing.T) {
	root, env, ioctx := channelsFixture(t, channelsTestCI)
	_, ref := releaseSetSnapshot(t, "putnami", "0.2.0-target", 'b')
	newChannelServer(t, &ioctx, func(_ string, body []byte) (int, any) {
		var request protocol.ChannelStatusRequest
		_ = json.Unmarshal(body, &request)
		switch request.Channel {
		case "canary":
			return http.StatusOK, channelStatusAnswer(ref, 2, map[string]uint64{"npm": 1})
		case "stable":
			return http.StatusOK, channelStatusAnswer(ref, 1, map[string]uint64{"npm": 1})
		case "beta":
			return http.StatusInternalServerError, map[string]any{"error": "unavailable"}
		default:
			return http.StatusOK, &protocol.ChannelStatusResponse{ProtocolVersion: protocol.ProtocolVersion}
		}
	})
	node := ChannelsStatusNode(map[string]any{}, root, env, ioctx)
	if node.State != clicore.StatusUnknown || len(node.Children) != 4 {
		t.Fatalf("node = %+v, want unknown with four channels", node)
	}
	byTitle := map[string]clicore.StatusNode{}
	for _, child := range node.Children {
		byTitle[child.Title] = child
	}
	canary := byTitle["canary"]
	if canary.State != clicore.StatusDegraded || canary.Fix != "putnami cloud channels status canary --wait 5m" ||
		len(canary.Children) != 1 || canary.Children[0].ID != "channels.canary.npm" || canary.Children[0].State != clicore.StatusDegraded {
		t.Fatalf("canary = %+v, want degraded with npm behind under it", canary)
	}
	if byTitle["stable"].State != clicore.StatusOK || byTitle["nightly"].Detail != "no release yet" || byTitle["beta"].State != clicore.StatusUnknown {
		t.Fatalf("children = %+v", node.Children)
	}
	if node.Detail != "4 channels in namespace putnami; beta not read, canary behind" {
		t.Fatalf("detail = %q", node.Detail)
	}
	if len(node.Metrics) != 2 || node.Metrics[0].Value != 4 || node.Metrics[1].ID != "registries_behind" || node.Metrics[1].Value != 1 {
		t.Fatalf("metrics = %+v, want 4 channels and 1 registry behind", node.Metrics)
	}

	empty, env, ioctx := channelsFixture(t, "")
	if node := ChannelsStatusNode(map[string]any{}, empty, env, ioctx); node.State != clicore.StatusOK {
		t.Fatalf("no declared channel = %+v, want ok", node)
	}
	broken, env, ioctx := channelsFixture(t, `{"version": 3, "unknown": true}`)
	if node := ChannelsStatusNode(map[string]any{}, broken, env, ioctx); node.State != clicore.StatusUnknown {
		t.Fatalf("invalid putnami.ci.json = %+v, want unknown", node)
	}
}

func TestChannelsStatusNodeFromSaysEveryRegistryIsUpToDate(t *testing.T) {
	ref := protocol.ReleaseSetRef{ID: "rs_" + strings.Repeat("a", 64)}
	node := ChannelsStatusNodeFrom("acme", []string{"canary", "stable"}, map[string]ChannelAnswer{
		"canary": {Response: channelStatusAnswer(ref, 3, map[string]uint64{"go": 3, "oci": 3})},
		"stable": {Response: channelStatusAnswer(ref, 1, map[string]uint64{"go": 1})},
	})
	if node.State != clicore.StatusOK || node.Detail != "2 channels in namespace acme, every registry up to date" {
		t.Fatalf("node = %+v", node)
	}
	if got := node.Children[0].Children[1]; got.ID != "channels.canary.oci" || got.Detail != "generation 3, up to date" {
		t.Fatalf("registry check = %+v", got)
	}
}
