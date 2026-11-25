//go:build darwin

package machineprobe

import "testing"

func TestDecodeLoadavgPadsTheTrimmedScale(t *testing.T) {
	t.Parallel()
	// ldavg[0] = 4096 at fscale 2048 is a load of 2; the last scale byte, a
	// NUL, is the one syscall.Sysctl drops.
	raw := []byte{0, 16, 0, 0, 0, 8, 0, 0, 0, 4, 0, 0, 0, 0, 0, 0, 0, 8, 0, 0, 0, 0, 0}
	if load, ok := decodeLoadavg(raw); !ok || load != 2 {
		t.Fatalf("load = %v, %v; want 2", load, ok)
	}
	if _, ok := decodeLoadavg(raw[:10]); ok {
		t.Fatal("a short answer decoded")
	}
	if _, live := OneMinuteLoad(); !live {
		t.Fatal("this host reports no load average")
	}
}
