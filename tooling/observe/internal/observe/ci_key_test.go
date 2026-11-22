package observe

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/ci"
)

// otherCI is the environment of a CI that names no code host: one variable
// per marker of the evidence protocol, and CI=true for a generic CI.
type otherCI struct {
	system   string
	variable string
	value    string
}

func otherCIs() []otherCI {
	var out []otherCI
	for _, marker := range evidence.CIMarkers {
		switch marker.System {
		case evidence.CISystemGitHubActions, evidence.CISystemGitLabCI:
			continue
		}
		value := marker.Value
		if value == "" {
			value = "1"
		}
		out = append(out, otherCI{marker.System, marker.Variable, value})
	}
	return append(out, otherCI{evidence.CISystemGeneric, "CI", "true"})
}

func (c otherCI) options(root string, projectID func(context.Context, string, string, string) (string, error)) Options {
	opts := laptopOptions(root, projectID)
	opts.Env = func(name string) string {
		if name == c.variable {
			return c.value
		}
		return ""
	}
	opts.Overrides = ci.Overrides{Base: "main"}
	return opts
}

func TestEveryOtherCIMarkerIsCovered(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range otherCIs() {
		seen[c.system] = true
	}
	for _, system := range evidence.CISystems {
		if system != evidence.CISystemGitHubActions && system != evidence.CISystemGitLabCI && !seen[system] {
			t.Errorf("no case covers the CI system %s", system)
		}
	}
}

func TestAnotherCIKeysItsOriginByTheNumericProjectID(t *testing.T) {
	spectest.Proves(t, feature, "ci-repository-key", "another-ci-keys-its-origin-by-the-numeric-project-id")
	root, _ := laptopClone(t)
	for _, c := range otherCIs() {
		t.Run(c.system, func(t *testing.T) {
			var asked []string
			o := Run(context.Background(), c.options(root, func(_ context.Context, provider, host, path string) (string, error) {
				asked = append(asked, provider+" "+host+" "+path)
				return "4242", nil
			}))
			if strings.Join(asked, ";") != "github github.com acme/widgets" {
				t.Fatalf("asked %v", asked)
			}
			s := o.Subject
			if s.Provider != ci.ProviderGitHub || s.Host != "github.com" || s.ProjectID != "4242" || s.Repository != "acme/widgets" {
				t.Fatalf("subject %+v", s)
			}
			if o.CI.System != c.system {
				t.Errorf("system %s, want %s", o.CI.System, c.system)
			}
			for _, d := range o.Diagnostics {
				if d.Code == ci.CodeProjectIDMissing {
					t.Errorf("diagnostic %s: %s", d.Code, d.Message)
				}
			}
			doc := validEvidence(t, o)
			if doc.Subject.Provider != ci.ProviderGitHub || doc.Subject.ProjectID != "4242" || doc.CI.System != c.system {
				t.Errorf("evidence subject %+v, ci %+v", doc.Subject, doc.CI)
			}
		})
	}
}

func TestAnotherCIWithoutAnIDIsADiagnosticNamingTheToken(t *testing.T) {
	spectest.Proves(t, feature, "ci-repository-key", "another-ci-without-an-id-is-a-diagnostic-naming-the-token")
	root, _ := laptopClone(t)
	for _, c := range otherCIs() {
		t.Run(c.system, func(t *testing.T) {
			o := Run(context.Background(), c.options(root, func(context.Context, string, string, string) (string, error) {
				return "", errors.New("the API answered 404: Not Found")
			}))
			if o.Subject.Provider != ci.ProviderGeneric || o.Subject.ProjectID != "" || o.Subject.Host != "github.com" {
				t.Fatalf("subject %+v", o.Subject)
			}
			found := false
			for _, d := range o.Diagnostics {
				if d.Code == ci.CodeProjectIDMissing && strings.Contains(d.Message, "GITHUB_TOKEN") && strings.Contains(d.Message, "404") {
					found = true
				}
			}
			if !found {
				t.Fatalf("no diagnostic names GITHUB_TOKEN: %+v", o.Diagnostics)
			}
			validEvidence(t, o)
		})
	}
}

func TestAnotherCIWithoutUploadAsksTheCodeHostNothing(t *testing.T) {
	spectest.Proves(t, feature, "ci-repository-key", "another-ci-without-upload-asks-the-code-host-nothing")
	root, _ := laptopClone(t)
	for _, c := range otherCIs() {
		t.Run(c.system, func(t *testing.T) {
			o := Run(context.Background(), c.options(root, nil))
			s := o.Subject
			if s.Provider != ci.ProviderGeneric || s.Host != "" || s.ProjectID != "" || s.Repository != "acme/widgets" {
				t.Fatalf("subject %+v", s)
			}
			for _, d := range o.Diagnostics {
				if d.Code == ci.CodeProjectIDMissing {
					t.Errorf("diagnostic %s: %s", d.Code, d.Message)
				}
			}
			validEvidence(t, o)
		})
	}
}
