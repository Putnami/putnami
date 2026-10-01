//go:build windows

package proctree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The cost Tree.Start adds to exec.Cmd.Start: a suspended start, a job, and a
// resume. Only the start is timed. Run on a Windows host with
//
//	go test -run '^$' -bench 'BenchmarkStart' -count 3 ./proctree
//
// and compare the two results: the gap must stay flat as the host runs more
// threads.
func benchCommand() *exec.Cmd {
	return exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), "/c", "exit 0")
}

func BenchmarkStartExecCmd(b *testing.B) {
	b.StopTimer()
	for range b.N {
		cmd := benchCommand()
		b.StartTimer()
		if err := cmd.Start(); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		_ = cmd.Wait()
	}
}

func BenchmarkStartTree(b *testing.B) {
	b.StopTimer()
	for range b.N {
		cmd := benchCommand()
		tree := New(cmd)
		b.StartTimer()
		if err := tree.Start(); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		_ = cmd.Wait()
		_ = tree.Close()
	}
}
