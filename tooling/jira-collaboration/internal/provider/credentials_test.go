package provider

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/jira-collaboration/internal/jira"
)

func TestACredentialNeverReachesAnAnswer(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "a-credential-never-reaches-an-answer")
	h := newHarness(t)
	basic := base64.StdEncoding.EncodeToString([]byte(fakeEmail + ":" + fakeToken))
	echo := "token " + fakeToken + " basic " + basic + " stranger ATATT3xSTRANGERtokenSTRANGERtoken99"
	h.fake.addFault("POST", `^/rest/api/2/issue$`, statusFault(http.StatusBadRequest, echo, true))
	text := h.serve("create", `{"title":"Echo","body":"`+fakeToken+`","idempotencyKey":"echo"}`)
	if strings.Contains(text, fakeToken) || strings.Contains(text, basic) || strings.Contains(text, "ATATT") {
		t.Fatalf("an answer carries a credential: %s", text)
	}
	mustContain(t, text, jira.RedactedMarker)
}

func TestAMissingCredentialIsDenied(t *testing.T) {
	spectest.Proves(t, feature, "credential-safety", "a-missing-credential-is-denied")
	h := newHarness(t)
	delete(h.env, jira.TokenVariable)
	failure := h.fails("find", `{}`, collab.OutcomeDenied)
	if failure.Reason != reasonUnauthenticated || h.fake.count("") != 0 {
		t.Fatalf("a missing credential answered %+v after %d requests", failure, h.fake.count(""))
	}
	mustContain(t, failure.Message, "JIRA_API_TOKEN")
}
