package sessionreporter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/sdk/extension/proctree"
)

type process struct {
	cmd     *exec.Cmd
	tree    *proctree.Tree
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	scanner *bufio.Scanner
	done    chan struct{}
}

func spawn(spec LaunchSpec) (*process, error) {
	cmd := exec.Command(spec.Command, spec.Args...)
	cmd.Dir, cmd.Env, cmd.Stderr = spec.Dir, spec.Env, io.Discard
	tree := proctree.New(cmd)
	cmd.WaitDelay = 200 * time.Millisecond
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Own the read descriptor independently of cmd.Wait: a provider may exit
	// immediately after writing its final ACK, before our reader consumes it.
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	cmd.Stdout = stdoutWriter
	if err := tree.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		return nil, err
	}
	_ = stdoutWriter.Close()
	p := &process{cmd: cmd, tree: tree, stdin: stdin, stdout: stdout, scanner: bufio.NewScanner(stdout), done: make(chan struct{})}
	p.scanner.Buffer(make([]byte, 4096), protocolcli.SessionReportingLineBytes)
	go func() { _ = cmd.Wait(); close(p.done) }()
	return p, nil
}

func (p *process) call(ctx context.Context, chunk protocolcli.SessionReportingChunk) (*protocolcli.SessionReportingAck, error) {
	line, err := json.Marshal(chunk)
	if err != nil {
		return nil, fmt.Errorf("encode reporter frame")
	}
	type response struct {
		ack *protocolcli.SessionReportingAck
		err error
	}
	result := make(chan response, 1)
	go func() {
		if _, err := p.stdin.Write(append(line, '\n')); err != nil {
			result <- response{err: fmt.Errorf("reporter pipe write failed")}
			return
		}
		if !p.scanner.Scan() {
			result <- response{err: fmt.Errorf("reporter pipe closed or exceeded limit")}
			return
		}
		ack, err := protocolcli.ParseSessionReportingAck(p.scanner.Bytes())
		result <- response{ack, err}
	}()
	select {
	case r := <-result:
		return r.ack, r.err
	case <-ctx.Done():
		// Closing both pipes also interrupts a provider that never reads stdin;
		// a timeout must bound writes as well as acknowledgement reads.
		_ = p.stdin.Close()
		_ = p.stdout.Close()
		<-result
		return nil, fmt.Errorf("reporter operation timed out")
	}
}

func (p *process) close() {
	_ = p.stdin.Close()
	_ = p.tree.Terminate()
	select {
	case <-p.done:
	case <-time.After(200 * time.Millisecond):
	}
	// Kill descendants even when the root of the tree exited first.
	_ = p.tree.Kill()
	_ = p.stdout.Close()
	<-p.done
	_ = p.tree.Close()
}
