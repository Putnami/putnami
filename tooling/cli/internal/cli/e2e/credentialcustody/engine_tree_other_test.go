//go:build !linux

package credentialcustody

import "fmt"

// engineTree is the process tree rooted at pid. Outside Linux there is no
// /proc to read it from: it is pid alone, with nothing known of its state.
func engineTree(pid int) []treeProcess { return []treeProcess{{pid: pid}} }

func (p treeProcess) String() string { return fmt.Sprintf("pid=%d", p.pid) }

// dumpTree asks nothing of tree outside Linux: the test kills it.
func dumpTree([]treeProcess) {}
