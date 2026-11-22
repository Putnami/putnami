package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/observe"
	"go.putnami.dev/tooling/observe/internal/publish"
	"go.putnami.dev/tooling/observe/internal/upload"
)

// historyNow is when a --history run starts. Tests replace it.
var historyNow = time.Now

// runHistory runs putnami observe --history: it observes the recent commits
// of the default branch, keeps each commit's evidence document as soon as
// the commit is observed and prints the summary.
func runHistory(ctx context.Context, opts observe.Options, out outputs, getenv func(string) string, stdout, stderr io.Writer) int {
	k, diags := newHistoryKeeper(out, getenv, stderr)
	k.diagnostics = append(k.diagnostics, diags...)
	defer k.close()
	h := observe.RunHistory(ctx, observe.HistoryOptions{
		Options: opts, Window: out.history, Now: historyNow(),
		// The wait for a person to sign in is not the run's work.
		Clock: func() time.Time { return time.Now().Add(-k.signInSpent) },
		Each:  func(c observe.HistoryCommit) { k.keep(ctx, c) },
	})
	k.finish()
	h.Diagnostics = append(h.Diagnostics, k.diagnostics...)
	if out.open && k.reportURL != "" {
		if _, inCI := evidence.DetectCISystem(getenv); !inCI {
			if err := openBrowser(k.reportURL); err != nil {
				fmt.Fprintf(stderr, "putnami observe: the browser did not open (%v); open %s\n", err, k.reportURL)
			}
		}
	}
	if out.json {
		if err := observe.WriteHistoryJSON(stdout, h); err != nil {
			fmt.Fprintf(stderr, "putnami observe: %v\n", err)
			return 1
		}
		return 0
	}
	err := observe.WriteHistoryText(stdout, h, func(sha string) string { return k.lines[sha] })
	if err == nil {
		_, err = fmt.Fprintln(stdout, upload.Next(upload.Step{Getenv: getenv, Upload: k.upload, ReportURL: k.reportURL}))
	}
	if err != nil {
		fmt.Fprintf(stderr, "putnami observe: %v\n", err)
		return 1
	}
	return 0
}

// historyKeeper writes each commit's evidence document to the --evidence
// directory, or to a temporary one for the upload alone, and hands each to
// the evidence reporter on --upload or --open. The first failed hand-off
// stops the uploads: the ones left would fail the same way.
type historyKeeper struct {
	out    outputs
	getenv func(string) string
	stderr io.Writer
	// dir receives the documents, "" when the run keeps none.
	dir string
	// temporary is true when dir is removed at the end.
	temporary bool
	signIn    bool
	// signInSpent is the time the hand-off that signed the person in took.
	signInSpent time.Duration
	now         time.Time
	// lines say, by commit, where its document went.
	lines map[string]string
	// upload is what the hand-offs did: uploaded when every attempted one
	// succeeded, failed when one failed.
	upload publish.Upload
	// reportURL is the report URL of the newest uploaded commit.
	reportURL   string
	unsent      int
	diagnostics []diag.Diagnostic
}

func newHistoryKeeper(out outputs, getenv func(string) string, stderr io.Writer) (*historyKeeper, []diag.Diagnostic) {
	_, inCI := evidence.DetectCISystem(getenv)
	k := &historyKeeper{out: out, getenv: getenv, stderr: stderr, signIn: out.open && !inCI, now: time.Now(),
		lines: map[string]string{}, upload: publish.NotUploaded}
	switch {
	case out.evidence != "":
		if err := os.MkdirAll(out.evidence, 0o755); err != nil {
			return k, []diag.Diagnostic{diag.Warningf(observe.CodeEvidenceUnwritable, "evidence", "the evidence directory %s cannot be created: %v", out.evidence, err)}
		}
		k.dir = out.evidence
	case out.upload:
		tmp, err := os.MkdirTemp("", "putnami-evidence-")
		if err != nil {
			k.upload = publish.UploadFailed
			return k, upload.Skipped()
		}
		k.dir, k.temporary = tmp, true
	}
	return k, nil
}

// keep writes the commit's document and uploads it.
func (k *historyKeeper) keep(ctx context.Context, c observe.HistoryCommit) {
	if k.dir == "" {
		return
	}
	path := filepath.Join(k.dir, c.Commit.SHA+".json")
	if diags := observe.WriteEvidence(path, c.Observation, k.now); len(diags) > 0 {
		k.diagnostics = append(k.diagnostics, diags...)
		return
	}
	if !k.temporary {
		k.lines[c.Commit.SHA] = path
	}
	if !k.out.upload {
		return
	}
	if k.upload == publish.UploadFailed {
		k.unsent++
		return
	}
	started := time.Now()
	reportURL, diags := upload.Run(ctx, upload.Request{Path: path, Getenv: k.getenv, Output: k.stderr, SignIn: k.signIn})
	if k.signIn {
		// One sign-in is enough: the reporter keeps the credential.
		k.signIn = false
		k.signInSpent = time.Since(started)
	}
	if len(diags) > 0 {
		k.upload = publish.UploadFailed
		k.diagnostics = append(k.diagnostics, diags...)
		return
	}
	k.upload = publish.Uploaded
	k.lines[c.Commit.SHA] = "uploaded"
	// The newest commit is observed first: its report is the one to open.
	if reportURL != "" && k.reportURL == "" {
		k.reportURL = reportURL
	}
}

// finish counts the observed commits the first failure left unsent.
func (k *historyKeeper) finish() {
	if k.unsent == 0 {
		return
	}
	noun := "commits were"
	if k.unsent == 1 {
		noun = "commit was"
	}
	k.diagnostics = append(k.diagnostics, diag.Warningf(upload.CodeUploadFailed, "upload",
		"the uploads stopped at the first failure: %d more %s not uploaded", k.unsent, noun))
}

func (k *historyKeeper) close() {
	if k.temporary {
		os.RemoveAll(k.dir)
	}
}
