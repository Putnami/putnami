package deliverycli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"go.putnami.dev/cloud/extension/internal/clicore"
)

const queueEntriesPath = "/v1/workspaces/ws-acme/ci/queue/entries"

// The fixture's pull requests. Constants keep tracker-shaped literals out of
// the public tree.
const (
	prLanded = iota + 1
	prDropped
	prFirst
	prSecond
	prWaiting
)

func pausedSettingStub() ciStubResponse {
	return ciStubResponse{status: http.StatusOK, body: map[string]any{
		"repository": "acme/app", "branch": "main", "configured": true, "enabled": true, "depth": 1, "paused": true,
		"pause": map[string]any{"at": "2026-11-15T10:00:00Z", "by": "user-1", "byLogin": "octo", "reason": "release freeze"},
	}}
}

func queueEntriesStub() ciStubResponse {
	return ciStubResponse{status: http.StatusOK, body: map[string]any{
		"repository": "acme/app", "branch": "main",
		"open": []map[string]any{
			{"position": 1, "pullRequest": prFirst, "title": "first", "author": "ann", "state": "green", "headSha": "a1",
				"builtOn": "0000000000000000aaaa", "queueCommit": "1111111111111111bbbb", "runId": "run-a", "attempt": 1,
				"requestedAt": "2026-11-15T09:00:00Z", "updatedAt": "2026-11-15T09:05:00Z"},
			{"position": 2, "pullRequest": prSecond, "title": "second", "author": "bob", "state": "running", "headSha": "b1",
				"builtOn": "1111111111111111bbbb", "queueCommit": "2222222222222222cccc", "runId": "run-b", "attempt": 2,
				"requestedAt": "2026-11-15T09:01:00Z", "updatedAt": "2026-11-15T09:06:00Z"},
			{"pullRequest": prWaiting, "title": "third", "author": "cy", "state": "requested", "headSha": "c1",
				"waitingFor": "conflict", "waitingDetail": "#4 in go.mod", "attempt": 0,
				"requestedAt": "2026-11-15T09:02:00Z", "updatedAt": "2026-11-15T09:07:00Z"},
		},
		"recent": []map[string]any{
			{"pullRequest": prLanded, "title": "landed one", "author": "ann", "state": "landed", "headSha": "d1",
				"landedSha": "3333333333333333dddd", "attempt": 1, "endedAt": "2026-11-15T08:00:00Z"},
			{"pullRequest": prDropped, "title": "dropped one", "author": "bob", "state": "dropped", "headSha": "e1",
				"dropReason": "red", "dropDetail": "lint failed", "attempt": 1, "endedAt": "2026-11-15T08:30:00Z"},
		},
		"rebaseOn": map[string]any{"ref": "putnami-queue/3", "sha": "1111111111111111bbbb"},
	}}
}

func TestQueueList(t *testing.T) {
	control := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET " + queuePath:        queueSettingStub(true),
		"GET " + queueEntriesPath: queueEntriesStub(),
	}}
	run := runQueue(t, control, newGitHubFake(t), "", "list")
	if run.err != nil {
		t.Fatalf("list: %v", run.err)
	}
	want := []string{
		"Merge queue of acme/app, branch main: on, depth 1.",
		"In line: 2.",
		`  1. #3 green "first" by ann: built on 000000000000, queue commit 111111111111, run run-a`,
		`  2. #4 running "second" by bob: built on 111111111111, queue commit 222222222222, run run-b, attempt 2`,
		"Waiting to enter: 1.",
		`  #5 "third" by cy: waits for conflict (#4 in go.mod)`,
		"Landed or dropped in the last 24 hours:",
		`  #1 landed "landed one" by ann: 333333333333, 2026-11-15 08:00 UTC`,
		`  #2 dropped "dropped one" by bob: red (lint failed), 2026-11-15 08:30 UTC`,
		"Rebase on putnami-queue/3 (111111111111):",
		"  git fetch origin putnami-queue/3 && git rebase FETCH_HEAD",
	}
	if got := run.lines; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("output:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(controlWrites(control)) != 0 {
		t.Fatal("list wrote")
	}
	for _, request := range control.requests {
		if request.Query != "branch=main" {
			t.Fatalf("%s %s: query %q, want branch=main", request.Method, request.Path, request.Query)
		}
	}

	run = runQueue(t, control, newGitHubFake(t), "", "list", "--output=jsonl")
	if run.err != nil || len(run.lines) != 1 {
		t.Fatalf("json list: %v, %d lines", run.err, len(run.lines))
	}
	var envelope struct {
		Data CIQueueLine `json:"data"`
	}
	if err := json.Unmarshal([]byte(run.lines[0]), &envelope); err != nil {
		t.Fatalf("json = %s: %v", run.lines[0], err)
	}
	entries := envelope.Data.Entries
	if len(entries.Open) != 3 || entries.Open[2].WaitingFor != "conflict" || len(entries.Recent) != 2 ||
		entries.RebaseOn.Ref != "putnami-queue/3" || !envelope.Data.Setting.Enabled {
		t.Fatalf("json = %s", run.lines[0])
	}
}

func TestQueueListOfAnEmptyPausedQueue(t *testing.T) {
	control := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET " + queuePath: pausedSettingStub(),
		"GET " + queueEntriesPath: {status: http.StatusOK, body: map[string]any{
			"repository": "acme/app", "branch": "main", "open": []any{}, "recent": []any{},
			"rebaseOn": map[string]any{"ref": "main", "sha": "4444444444444444eeee"},
		}},
	}}
	run := runQueue(t, control, newGitHubFake(t), "", "list")
	if run.err != nil {
		t.Fatalf("list: %v", run.err)
	}
	out := run.output()
	for _, want := range []string{
		`: paused, depth 1, paused by octo on 2026-11-15 10:00 UTC: "release freeze".`,
		"In line: none.",
		"git fetch origin main && git rebase FETCH_HEAD",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output omits %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Waiting to enter") || strings.Contains(out, "Landed or dropped") {
		t.Fatalf("output names empty sections:\n%s", out)
	}
}

func TestQueueListRefused(t *testing.T) {
	control := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET " + queuePath:        queueSettingStub(true),
		"GET " + queueEntriesPath: {status: http.StatusForbidden, body: map[string]any{"error": "forbidden", "reason": "needs platform.workspace.read"}},
	}}
	run := runQueue(t, control, newGitHubFake(t), "", "list")
	if run.err == nil || !strings.Contains(run.err.Error(), "cloud ci queue list") || clicore.ExitCode(run.err) != clicore.ExitAuth {
		t.Fatalf("err = %v (exit %d), want an auth error naming queue list", run.err, clicore.ExitCode(run.err))
	}
}

func TestQueuePause(t *testing.T) {
	control := queueControl(true, pausedSettingStub())
	run := runQueue(t, control, newGitHubFake(t), "y", "pause", "--reason", "release freeze")
	if run.err != nil {
		t.Fatalf("pause: %v", run.err)
	}
	writes := controlWrites(control)
	if len(writes) != 1 || writes[0].Body["paused"] != true || writes[0].Body["reason"] != "release freeze" {
		t.Fatalf("writes = %+v, want one PUT paused with the reason", writes)
	}
	if _, named := writes[0].Body["enabled"]; named {
		t.Fatalf("pause body names enabled: %s", writes[0].Raw)
	}
	if len(run.confirm) != 1 || !strings.Contains(run.confirm[0], "keeps its place") {
		t.Fatalf("confirm = %v", run.confirm)
	}
	if out := run.output(); !strings.Contains(out, ": paused, depth 1, paused by octo") || !strings.Contains(out, "queue resume`") {
		t.Fatalf("output:\n%s", out)
	}

	control = queueControl(true, pausedSettingStub())
	run = runQueue(t, control, newGitHubFake(t), "n", "pause", "--reason", "freeze")
	if run.err == nil || len(controlWrites(control)) != 0 {
		t.Fatalf("declined pause: err %v, writes %v", run.err, controlWrites(control))
	}
}

func TestQueuePauseChangesNothingWhenPausedOrOff(t *testing.T) {
	control := &ciFakeServer{responses: map[string]ciStubResponse{"GET " + queuePath: pausedSettingStub()}}
	run := runQueue(t, control, newGitHubFake(t), "y", "pause", "--reason", "again")
	if run.err != nil || len(controlWrites(control)) != 0 || len(run.confirm) != 0 {
		t.Fatalf("pause when paused: err %v, writes %v, confirm %v", run.err, controlWrites(control), run.confirm)
	}

	control = queueControl(false, ciStubResponse{})
	run = runQueue(t, control, newGitHubFake(t), "y", "pause", "--reason", "freeze")
	if run.err == nil || !strings.Contains(run.err.Error(), "Only a queue that is on pauses") || len(controlWrites(control)) != 0 {
		t.Fatalf("pause when off: err %v, writes %v", run.err, controlWrites(control))
	}
}

func TestQueueResume(t *testing.T) {
	control := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET " + queuePath: pausedSettingStub(),
		"PUT " + queuePath: queueSettingStub(true),
	}}
	run := runQueue(t, control, newGitHubFake(t), "", "resume")
	if run.err != nil {
		t.Fatalf("resume: %v", run.err)
	}
	writes := controlWrites(control)
	if len(writes) != 1 || writes[0].Body["paused"] != false {
		t.Fatalf("writes = %+v, want one PUT paused false", writes)
	}
	for _, field := range []string{"enabled", "reason"} {
		if _, named := writes[0].Body[field]; named {
			t.Fatalf("resume body names %s: %s", field, writes[0].Raw)
		}
	}
	if out := run.output(); !strings.Contains(out, ": on, depth 1.") || !strings.Contains(out, "moves again from its first entry") {
		t.Fatalf("output:\n%s", out)
	}

	control = queueControl(true, ciStubResponse{})
	run = runQueue(t, control, newGitHubFake(t), "", "resume")
	if run.err != nil || len(controlWrites(control)) != 0 {
		t.Fatalf("resume when not paused: err %v, writes %v", run.err, controlWrites(control))
	}
}

func TestQueueStatusOfAPausedQueue(t *testing.T) {
	control := &ciFakeServer{responses: map[string]ciStubResponse{"GET " + queuePath: pausedSettingStub()}}
	run := runQueue(t, control, newGitHubFake(t, mainRuleset(t)), "", "status")
	if run.err != nil {
		t.Fatalf("status: %v", run.err)
	}
	if out := run.output(); !strings.Contains(out, "Nothing is built and nothing lands") || !strings.Contains(out, `"release freeze"`) {
		t.Fatalf("output:\n%s", out)
	}
}

func TestQueueLineUsage(t *testing.T) {
	for args, want := range map[string]string{
		"pause": "requires --reason",
		"pause --reason " + strings.Repeat("x", 501): "--reason holds at most 500 bytes",
	} {
		control := queueControl(true, ciStubResponse{})
		run := runQueue(t, control, newGitHubFake(t), "y", strings.Fields(args)...)
		if run.err == nil || !strings.Contains(run.err.Error(), want) || clicore.ExitCode(run.err) != clicore.ExitUsage {
			t.Fatalf("%s: err = %v, want a usage error saying %q", args, run.err, want)
		}
		if len(control.requests) != 0 {
			t.Fatalf("%s: called delivery-api", args)
		}
	}
}
