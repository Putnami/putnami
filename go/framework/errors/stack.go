package errors

import (
	"fmt"
	"runtime"
	"strings"
)

const maxStackDepth = 32

// Stack represents a captured call stack.
type Stack []uintptr

// captureStack captures the call stack, skipping frames for
// Callers, captureStack, and the immediate caller (constructor).
func captureStack() Stack {
	pcs := make([]uintptr, maxStackDepth)
	n := runtime.Callers(3, pcs) // skip: Callers + captureStack + caller
	return Stack(pcs[:n])
}

// Frames returns the stack as runtime.Frames for iteration.
func (s Stack) Frames() *runtime.Frames {
	return runtime.CallersFrames([]uintptr(s))
}

// Format returns a human-readable multiline stack trace.
func (s Stack) Format() string {
	if len(s) == 0 {
		return ""
	}
	var b strings.Builder
	frames := s.Frames()
	for {
		frame, more := frames.Next()
		fmt.Fprintf(&b, "%s\n\t%s:%d\n", frame.Function, frame.File, frame.Line)
		if !more {
			break
		}
	}
	return b.String()
}
