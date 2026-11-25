package credentialcustody

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// engineTree is the process tree rooted at pid: pid and every live
// descendant, parents before children, each with its state, kernel wait
// channel and command line, read from /proc.
func engineTree(pid int) []treeProcess {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	for _, entry := range entries {
		child, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if parent, ok := parentPID(child); ok {
			children[parent] = append(children[parent], child)
		}
	}
	var tree []treeProcess
	queue := []int{pid}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		process, ok := readTreeProcess(current)
		if !ok {
			continue
		}
		tree = append(tree, process)
		next := children[current]
		slices.Sort(next)
		queue = append(queue, next...)
	}
	return tree
}

// readTreeProcess reads what engineTree reports of pid.
func readTreeProcess(pid int) (treeProcess, bool) {
	stat, err := os.ReadFile(procPath(pid, "stat"))
	if err != nil {
		return treeProcess{}, false
	}
	process := treeProcess{pid: pid}
	if close := strings.LastIndex(string(stat), ")"); close >= 0 {
		if fields := strings.Fields(string(stat)[close+1:]); len(fields) > 1 {
			process.state = fields[0]
			process.ppid, _ = strconv.Atoi(fields[1])
		}
	}
	if wchan, err := os.ReadFile(procPath(pid, "wchan")); err == nil {
		process.wchan = string(wchan)
	}
	if cmdline, err := os.ReadFile(procPath(pid, "cmdline")); err == nil {
		process.args = strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " "))
	}
	if environ, err := os.ReadFile(procPath(pid, "environ")); err == nil {
		for _, entry := range strings.Split(string(environ), "\x00") {
			if role, ok := strings.CutPrefix(entry, custodyRoleEnv+"="); ok {
				process.role = role
			}
		}
	}
	return process, true
}

func (p treeProcess) String() string {
	args := p.args
	if len(args) > 160 {
		args = args[:160] + "…"
	}
	return fmt.Sprintf("pid=%d ppid=%d state=%s wchan=%s role=%s args=%s", p.pid, p.ppid, p.state, p.wchan, p.role, args)
}

// dumpTree continues every process of tree that a stop signal left stopped,
// then asks each to dump its goroutines (SIGQUIT), children before their
// parents, so a child's dump still finds the parent that reads its output. A
// Go process writes its dump to its standard error and exits; another process
// ends or ignores the signal.
func dumpTree(tree []treeProcess) {
	for _, process := range tree {
		_ = syscall.Kill(process.pid, syscall.SIGCONT)
	}
	for index := len(tree) - 1; index >= 0; index-- {
		_ = syscall.Kill(tree[index].pid, syscall.SIGQUIT)
		if index == 1 {
			time.Sleep(engineDumpPause)
		}
	}
}
