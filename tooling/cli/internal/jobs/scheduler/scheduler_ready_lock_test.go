//go:build unix

package scheduler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// readyEventLine is a typed v2 readiness claim for a server, as a serve
// wrapper's forwarder writes it on the step's stdout.
const readyEventLine = `{"v":2,"type":"ready","time":"2026-11-19T16:00:00.000Z","data":{"target":"server","name":"app","endpoints":[{"scheme":"http","host":"localhost","port":3000}]}}`

// TestScheduler_ARunNeverSeesAnotherSessionsGenerateEmptyWhatItReads
// reproduces a run beside a generate loop. One session generates proj's .gen
// over and over, emptying it before it writes it again. The other session
// generates .gen, then runs a one-shot step that reads .gen throughout its
// run. The run step holds the project reader lock from its preparation to its
// capture, so it never finds .gen empty, and the generate loop goes on once
// the run ended.
func TestScheduler_ARunNeverSeesAnotherSessionsGenerateEmptyWhatItReads(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-read-exclusion",
		"a-run-step-never-sees-another-sessions-generate-empty-what-it-reads")
	f := newOutputRaceFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	data := shellQuote(f.genData())
	generateScript := filepath.Join(f.root, "generate.sh")
	writeExecutable(t, generateScript, "#!/bin/sh\n"+
		"mkdir -p "+shellQuote(filepath.Dir(f.genData()))+"\n"+
		"rm -f "+data+"\n"+
		"sleep 0.02\n"+
		"printf 'gen\\n' > "+data+"\n")
	runScript := filepath.Join(f.root, "run.sh")
	writeExecutable(t, runScript, "#!/bin/sh\n"+
		"printf 'started\\n' > "+shellQuote(f.started)+"\n"+
		"i=0\n"+
		"while [ $i -lt 40 ]; do\n"+
		"  if [ ! -f "+data+" ]; then printf 'missing\\n' > "+shellQuote(f.seen)+"; exit 1; fi\n"+
		"  sleep 0.01\n"+
		"  i=$((i+1))\n"+
		"done\n")

	// The generate loop: each iteration is a session of its own, under a
	// session id of its own, that empties and rewrites .gen.
	stop := make(chan struct{})
	loopDone := make(chan error, 1)
	var generated atomic.Int32
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				loopDone <- nil
				return
			default:
			}
			generate := f.resourceJob("build~generate", generateScript, genDataOutputs(), nil)
			result := f.plan(fmt.Sprintf("20261119-170000-%06d", i), nil, generate).Run(ctx)
			if job := result.Results[generate.Key()]; job == nil || job.Status != "success" {
				loopDone <- fmt.Errorf("generate iteration %d: %+v", i, job)
				return
			}
			generated.Add(1)
		}
	}()

	generate := f.resourceJob("run~generate", generateScript, genDataOutputs(), nil)
	run := f.resourceJob("run~run", runScript, nil, genReads())
	run.DependsOn = []string{generate.Key()}
	running := runAsync(ctx, f.plan(outputRaceSessionA, nil, generate, run))
	awaitStarted(t, f.started, running, "the run step")
	before := generated.Load()

	var result *SchedulerResult
	select {
	case result = <-running:
	case <-time.After(answerWaitBudget):
		t.Fatal("the run session never ended")
	}
	if job := result.Results[run.Key()]; job == nil || job.Status != "success" {
		t.Fatalf("run step result = %+v (seen %q): it found .gen empty", job, readOptional(f.seen))
	}
	deadline := time.Now().Add(answerWaitBudget)
	for generated.Load() <= before {
		if time.Now().After(deadline) {
			t.Fatal("the generate loop never ran again once the run ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	if err := <-loopDone; err != nil {
		t.Fatal(err)
	}
}

// TestScheduler_AWriterRunsBesideAReadyServeStep proves at the executor that a
// serve-workload step's readiness claim on its stdout releases its reader
// lock: another session's generate of the project runs to completion while
// the serve step still serves.
func TestScheduler_AWriterRunsBesideAReadyServeStep(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-serve-step-releases-its-reader-locks-once-ready",
		"a-writer-runs-beside-a-ready-serve-step")
	f := newOutputRaceFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writeFileAt(t, f.genData(), "initial\n")

	serveScript := filepath.Join(f.root, "serve.sh")
	writeExecutable(t, serveScript, "#!/bin/sh\n"+
		"printf '%s\\n' '"+readyEventLine+"'\n"+
		"printf 'started\\n' > "+shellQuote(f.started)+"\n"+
		releaseWaitScript(f.release, os.Getpid()))
	writeScript := filepath.Join(f.root, "write.sh")
	writeExecutable(t, writeScript, "#!/bin/sh\nprintf 'a\\n' > "+shellQuote(f.genData())+"\n")

	serve := f.resourceJob("serve~serve", serveScript, nil, genReads())
	serving := runAsync(ctx, f.servePlan(outputRaceSessionA, nil, []*ScheduledJob{serve}, serve))
	f.awaitFirstStarted(t, serving)

	writer := f.resourceJob("build~generate", writeScript, genDataOutputs(), nil)
	writing := runAsync(ctx, f.plan("20261119-170001-000001", nil, writer))
	select {
	case result := <-writing:
		requireSuccess(t, "writing", result, writer.Key())
	case result := <-serving:
		t.Fatalf("the serve session ended before the writer ran: %+v", result.Results)
	case <-time.After(answerWaitBudget):
		t.Fatal("the writer never ran beside the ready serve step")
	}
	if got := readFileAt(t, f.genData()); got != "a\n" {
		t.Fatalf(".gen/data.txt = %q, want the writer's", got)
	}
	if err := os.WriteFile(f.release, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}
	requireSuccess(t, "serving", <-serving, serve.Key())
}

// readOptional is the content of path, or "" when it cannot be read.
func readOptional(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
