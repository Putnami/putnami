package upload

import (
	"strings"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/publish"
)

// TokenEnv is the Putnami Cloud token a CI job uploads with.
const TokenEnv = "PUTNAMI_CLOUD_TOKEN"

// Step is what a run did with its evidence document.
type Step struct {
	// Getenv reads the environment.
	Getenv func(string) string
	// Upload is what the hand-off to the reporter did.
	Upload publish.Upload
	// ReportURL is the report URL the reporter answered, "" for none.
	ReportURL string
}

// Next returns the line that ends the text summary: the report URL an upload
// answered, else the command that keeps the full report. Outside a CI that
// is putnami observe --open; in a CI job whose reporter is Putnami Cloud and
// that holds no token, the token setting and the command that creates it;
// else --upload.
func Next(s Step) string {
	switch {
	case s.ReportURL != "":
		return "Full report: " + s.ReportURL
	case s.Upload == publish.Uploaded:
		return "Next: the evidence reporter stored the evidence and answered no report URL"
	case s.Upload == publish.UploadFailed:
		return "Next: the upload failed, as the observe.upload_failed warning says; fix it and run again"
	}
	if _, inCI := evidence.DetectCISystem(s.Getenv); !inCI {
		return "Next: run `putnami observe --open` to sign in and open the full report"
	}
	if name, _, err := Reporter(s.Getenv); err == nil && name == DefaultReporter && strings.TrimSpace(s.Getenv(TokenEnv)) == "" {
		return "Next: set " + TokenEnv + " in this CI to a token from `putnami cloud token create --name ci --scopes delivery.ingest`, then add --upload to keep the full report"
	}
	return "Next: add --upload to keep the full report"
}
