package abort

import (
	"syscall"
	"testing"
)

func TestSourceEmptyUntilSignal(t *testing.T) {
	Reset()
	if got := Source(); got != "" {
		t.Fatalf("Source() = %q before any signal, want empty", got)
	}
}

func TestRecordNamesTheSignal(t *testing.T) {
	tests := []struct {
		name string
		sig  syscall.Signal
		want string
	}{
		{"interactive ctrl-c", syscall.SIGINT, SourceUser},
		{"supervisor terminate", syscall.SIGTERM, SourceSignal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Reset()
			Record(tt.sig)
			if got := Source(); got != tt.want {
				t.Errorf("Source() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The first signal is the one that canceled the run; a second only forces the
// exit. Letting it overwrite would misattribute a Ctrl-C as a supervisor kill.
func TestRecordKeepsFirstSignal(t *testing.T) {
	Reset()
	Record(syscall.SIGINT)
	Record(syscall.SIGTERM)
	if got := Source(); got != SourceUser {
		t.Errorf("Source() = %q after SIGINT then SIGTERM, want %q", got, SourceUser)
	}
}
