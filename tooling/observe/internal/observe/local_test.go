package observe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/ci"
)

// laptopClone is a clone of acme/widgets on github.com whose default branch is
// main, checked out on a feature branch one commit ahead. It returns the
// clone and the commit of main.
func laptopClone(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init", "--quiet", "--initial-branch=main")
	writeDemoFile(t, root, "README.md", "widgets\n")
	git(t, root, "add", "-A")
	git(t, root, "commit", "--quiet", "-m", "base")
	main := git(t, root, "rev-parse", "HEAD")
	git(t, root, "remote", "add", "origin", "git@github.com:acme/widgets.git")
	git(t, root, "update-ref", "refs/remotes/origin/main", main)
	git(t, root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	git(t, root, "checkout", "--quiet", "-b", "feature")
	writeDemoFile(t, root, "widget.go", "package widgets\n")
	git(t, root, "add", "-A")
	git(t, root, "commit", "--quiet", "-m", "head")
	return root, main
}

func laptopOptions(root string, projectID func(context.Context, string, string, string) (string, error)) Options {
	return Options{
		Dir:       root,
		Env:       func(string) string { return "" },
		ReadFile:  func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Resolve:   func(p string) string { return filepath.Join(root, p) },
		Producer:  Producer{Name: "@putnami/observe", Version: "1.0.0"},
		ProjectID: projectID,
	}
}

func TestALaptopRunKeysItsOriginByTheNumericProjectID(t *testing.T) {
	spectest.Proves(t, feature, "laptop-repository-key", "a-laptop-run-keys-its-origin-by-the-numeric-project-id")
	root, _ := laptopClone(t)
	var asked []string
	o := Run(context.Background(), laptopOptions(root, func(_ context.Context, provider, host, path string) (string, error) {
		asked = append(asked, provider+" "+host+" "+path)
		return "4242", nil
	}))
	if strings.Join(asked, ";") != "github github.com acme/widgets" {
		t.Fatalf("asked %v", asked)
	}
	s := o.Subject
	if s.Provider != ci.ProviderGitHub || s.Host != "github.com" || s.ProjectID != "4242" || s.Repository != "acme/widgets" || s.Branch != "feature" {
		t.Fatalf("subject %+v", s)
	}
	for _, d := range o.Diagnostics {
		if d.Code == ci.CodeProjectIDMissing {
			t.Errorf("diagnostic %s: %s", d.Code, d.Message)
		}
	}
	validEvidence(t, o)
}

func TestAnOriginWithoutAnIDIsADiagnosticNamingTheToken(t *testing.T) {
	spectest.Proves(t, feature, "laptop-repository-key", "an-origin-without-an-id-is-a-diagnostic-naming-the-token")
	root, _ := laptopClone(t)
	o := Run(context.Background(), laptopOptions(root, func(context.Context, string, string, string) (string, error) {
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
}

func TestALaptopRunMeasuresFromTheRemoteDefaultBranch(t *testing.T) {
	spectest.Proves(t, feature, "laptop-base", "a-laptop-run-measures-from-the-remote-default-branch")
	root, main := laptopClone(t)
	o := Run(context.Background(), laptopOptions(root, nil))
	if o.Base == nil || o.Base.Ref != "origin/main" || o.Base.Source != ci.BaseFromDefaultBranch {
		t.Fatalf("base %+v", o.Base)
	}
	if o.Subject.BaseSHA != main {
		t.Errorf("base sha %s, want %s", o.Subject.BaseSHA, main)
	}
	if len(o.ChangedFiles) != 1 || o.ChangedFiles[0].Path != "widget.go" {
		t.Errorf("changed files %+v", o.ChangedFiles)
	}
	for _, d := range o.Diagnostics {
		if d.Code == ci.CodeBaseMissing {
			t.Errorf("diagnostic %s: %s", d.Code, d.Message)
		}
	}

	git(t, root, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	o = Run(context.Background(), laptopOptions(root, nil))
	missing := false
	for _, d := range o.Diagnostics {
		missing = missing || d.Code == ci.CodeBaseMissing
	}
	if o.Base != nil || !missing {
		t.Errorf("a clone without origin/HEAD: base %+v, diagnostics %+v", o.Base, o.Diagnostics)
	}
}
