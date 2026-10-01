package sessionreporter

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
)

type readAfterProcessExit struct {
	io.Reader
	done <-chan struct{}
	ctx  context.Context
}

func (r readAfterProcessExit) Read(data []byte) (int, error) {
	select {
	case <-r.done:
		return r.Reader.Read(data)
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}
}

func TestReportingProcessFinalAckBeforeExit(t *testing.T) {
	if os.Getenv("REPORTER_ACK_EXIT_HELPER") == "1" {
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			os.Exit(21)
		}
		chunk, err := protocolcli.ParseSessionReportingChunk(scanner.Bytes())
		if err != nil || !chunk.Final || chunk.Artifact != "events.jsonl" {
			os.Exit(22)
		}
		if err := json.NewEncoder(os.Stdout).Encode(chunk.Ack()); err != nil {
			os.Exit(23)
		}
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err := spawn(LaunchSpec{Command: executable, Args: []string{"-test.run=^TestReportingProcessFinalAckBeforeExit$"}, Env: append(os.Environ(), "REPORTER_ACK_EXIT_HELPER=1")})
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Force Wait to finish before the first ACK read. StdoutPipe lets Wait
	// close this descriptor even though the child already wrote a valid ACK.
	p.scanner = bufio.NewScanner(readAfterProcessExit{Reader: p.stdout, done: p.done, ctx: ctx})
	chunk := protocolcli.NewSessionReportingChunk("session", "events.jsonl", 0, 0, nil, true)
	ack, err := p.call(ctx, chunk)
	if err != nil || ack == nil || !ack.OK || !ack.Matches(chunk) {
		t.Fatalf("final ACK lost after provider exit: ack=%+v err=%v", ack, err)
	}
}
