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
// of the default branch, keeps one evidence document per commit and prints
// the summary.
func runHistory(ctx context.Context, opts observe.Options, out outputs, getenv func(string) string, stdout, stderr io.Writer) int {
	h := observe.RunHistory(ctx, observe.HistoryOptions{Options: opts, Window: out.history, Now: historyNow()})
	kept := keepHistory(ctx, h, out, getenv, stderr)
	h.Diagnostics = append(h.Diagnostics, kept.diagnostics...)
	if out.open && kept.reportURL != "" {
		if _, inCI := evidence.DetectCISystem(getenv); !inCI {
			if err := openBrowser(kept.reportURL); err != nil {
				fmt.Fprintf(stderr, "putnami observe: the browser did not open (%v); open %s\n", err, kept.reportURL)
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
	err := observe.WriteHistoryText(stdout, h, func(sha string) string { return kept.lines[sha] })
	if err == nil {
		_, err = fmt.Fprintln(stdout, upload.Next(upload.Step{Getenv: getenv, Upload: kept.upload, ReportURL: kept.reportURL}))
	}
	if err != nil {
		fmt.Fprintf(stderr, "putnami observe: %v\n", err)
		return 1
	}
	return 0
}

// keptHistory is what a --history run did with its evidence documents.
type keptHistory struct {
	// lines say, by commit, where its document went.
	lines map[string]string
	// upload is what the hand-offs did: uploaded when every attempted one
	// succeeded, failed when one failed.
	upload publish.Upload
	// reportURL is the report URL of the newest uploaded commit.
	reportURL   string
	diagnostics []diag.Diagnostic
}

// keepHistory writes each commit's evidence document to the --evidence
// directory, or to a temporary one for the upload alone, and hands each to
// the evidence reporter on --upload or --open. The first failed hand-off
// stops the uploads: the ones left would fail the same way.
func keepHistory(ctx context.Context, h observe.History, out outputs, getenv func(string) string, stderr io.Writer) keptHistory {
	kept := keptHistory{lines: map[string]string{}, upload: publish.NotUploaded}
	dir := out.evidence
	if dir == "" && !out.upload {
		return kept
	}
	if len(h.Commits) == 0 {
		return kept
	}
	if dir == "" {
		tmp, err := os.MkdirTemp("", "putnami-evidence-")
		if err != nil {
			kept.upload, kept.diagnostics = publish.UploadFailed, upload.Skipped()
			return kept
		}
		defer os.RemoveAll(tmp)
		dir = tmp
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		kept.diagnostics = []diag.Diagnostic{diag.Warningf(observe.CodeEvidenceUnwritable, "evidence", "the evidence directory %s cannot be created: %v", dir, err)}
		return kept
	}
	_, inCI := evidence.DetectCISystem(getenv)
	signIn := out.open && !inCI
	now := time.Now()
	for i, c := range h.Commits {
		path := filepath.Join(dir, c.Commit.SHA+".json")
		if diags := observe.WriteEvidence(path, c.Observation, now); len(diags) > 0 {
			kept.diagnostics = append(kept.diagnostics, diags...)
			continue
		}
		kept.lines[c.Commit.SHA] = path
		if out.evidence == "" {
			kept.lines[c.Commit.SHA] = ""
		}
		if !out.upload || kept.upload == publish.UploadFailed {
			continue
		}
		reportURL, diags := upload.Run(ctx, upload.Request{Path: path, Getenv: getenv, Output: stderr, SignIn: signIn})
		if len(diags) > 0 {
			kept.upload = publish.UploadFailed
			kept.diagnostics = append(kept.diagnostics, diags...)
			if left := len(h.Commits) - i - 1; left > 0 {
				noun := "commits were"
				if left == 1 {
					noun = "commit was"
				}
				kept.diagnostics = append(kept.diagnostics, diag.Warningf(upload.CodeUploadFailed, "upload",
					"the uploads stopped at the first failure: %d more %s not uploaded", left, noun))
			}
			continue
		}
		// One sign-in is enough: the reporter keeps the credential.
		signIn = false
		kept.upload = publish.Uploaded
		kept.lines[c.Commit.SHA] = "uploaded"
		if reportURL != "" {
			kept.reportURL = reportURL
		}
	}
	return kept
}
