package deliverycli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/client"
	deliveryapiclient "go.putnami.dev/cloud/clients/delivery-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

const (
	runnerNextFailure = 1
	runnerNextEmpty   = 3
	runnerNextDenied  = 4
	runnerNextMaxEnv  = 64 << 10
)

var runnerNextID = regexp.MustCompile(`^[a-f0-9]{64}$`)

// RunnerNext is the image's hidden machine-poll mode. It runs before the
// interactive CLI/auth dispatcher: its only authority is a purpose-scoped
// runner bearer in a read-only host file. Stdin carries one bounded request, stdout
// carries only the next environment. Error bodies are never printed because a
// provider error can contain credential-bearing request or response details.
// Exit 0 means assignment, 3 means idle, 4 means authentication refused, and 1
// means a retryable transport/contract failure. The host owns the polling loop.
func RunnerNext(args []string, input io.Reader, output io.Writer) int {
	if len(args) != 3 {
		return runnerNextFailure
	}
	origin := runnerNextOrigin(args[0])
	seconds, err := strconv.Atoi(args[1])
	if err != nil || seconds < 1 || seconds > 10 || origin == "" {
		return runnerNextFailure
	}
	data, err := io.ReadAll(io.LimitReader(input, 4097))
	if err != nil || len(data) > 4096 {
		return runnerNextFailure
	}
	var request deliveryapiclient.RunnerNextRequest
	if err := json.Unmarshal(data, &request); err != nil || request.WorkspaceId == nil ||
		strings.TrimSpace(*request.WorkspaceId) == "" || request.MachineRunId == nil ||
		!runnerNextID.MatchString(*request.MachineRunId) || request.CompletedRunId == nil ||
		!runnerNextID.MatchString(*request.CompletedRunId) {
		return runnerNextFailure
	}
	header, err := os.Open(args[2]) //nolint:gosec // G304: explicit host-only credential file, never a checkout path
	if err != nil {
		return runnerNextFailure
	}
	data, err = io.ReadAll(io.LimitReader(header, 8193))
	_ = header.Close()
	if err != nil || len(data) > 8192 {
		return runnerNextFailure
	}
	bearer, ok := strings.CutPrefix(strings.TrimSuffix(string(data), "\n"), "Authorization: Bearer ")
	if !ok || bearer == "" || strings.ContainsAny(bearer, " \t\r\n\x00") {
		return runnerNextFailure
	}
	deadline := time.Duration(seconds) * time.Second
	api, err := clicore.NewServiceClient[deliveryapiclient.DeliveryClient](deliveryapiclient.RegisterDeliveryClient,
		clicore.ServiceBindingFor(origin, nil))
	if err != nil {
		return runnerNextFailure
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	response, err := api.CreateApiCiRunnerNext(client.WithForwardedUserToken(ctx, bearer), deliveryapiclient.CreateApiCiRunnerNextInput{Body: request})
	if err != nil {
		if status := clicore.ServiceStatus(err); status == http.StatusUnauthorized || status == http.StatusForbidden {
			return runnerNextDenied
		}
		return runnerNextFailure
	}
	if response == nil || response.Env == nil {
		return runnerNextFailure
	}
	if *response.Env == "" {
		return runnerNextEmpty
	}
	if len(*response.Env) > runnerNextMaxEnv || strings.ContainsAny(*response.Env, "\x00\r") {
		return runnerNextFailure
	}
	if _, err := io.WriteString(output, *response.Env); err != nil { //nolint:gosec // G705: host-only Docker env-file stdout, never HTML; escaping would change credential bytes
		return runnerNextFailure
	}
	return 0
}

func runnerNextOrigin(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		u.Path != "/api/ci/runner/next" || u.RawPath != "" || strings.ContainsAny(endpoint, " \t\r\n") {
		return ""
	}
	loopback := u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
