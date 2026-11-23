package app

import (
	"context"
	"testing"
	"testing/fstest"

	"go.putnami.dev/protocol/features/spectest"
)

type fakeProcs struct {
	value int
	sets  []int
}

func (p *fakeProcs) gomaxprocs(n int) int {
	previous := p.value
	if n > 0 {
		p.value = n
		p.sets = append(p.sets, n)
	}
	return previous
}

func noEnv(string) string { return "" }

func cgroupFS(group string, files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{"proc/self/cgroup": {Data: []byte(group)}}
	for name, data := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(data)}
	}
	return fsys
}

func env(key, value string) func(string) string {
	return func(name string) string {
		if name == key {
			return value
		}
		return ""
	}
}

func TestStartLowersGOMAXPROCSToACgroupLimitBelowIt(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "cpu-quota-parallelism", "start-lowers-gomaxprocs-to-a-cgroup-limit-below-it")

	cases := []struct {
		name  string
		fsys  fstest.MapFS
		procs int
		want  int
	}{
		{
			name:  "one CPU in a private cgroup namespace",
			fsys:  cgroupFS("0::/\n", map[string]string{"sys/fs/cgroup/cpu.max": "100000 100000\n"}),
			procs: 2,
			want:  1,
		},
		{
			name:  "a fraction of a CPU rounds up to one",
			fsys:  cgroupFS("0::/\n", map[string]string{"sys/fs/cgroup/cpu.max": "50000 100000\n"}),
			procs: 10,
			want:  1,
		},
		{
			name: "the smallest limit of the cgroup and its ancestors wins",
			fsys: cgroupFS("0::/kubepods/pod1/ctr\n", map[string]string{
				"sys/fs/cgroup/kubepods/pod1/ctr/cpu.max": "max 100000\n",
				"sys/fs/cgroup/kubepods/pod1/cpu.max":     "250000 100000\n",
				"sys/fs/cgroup/kubepods/cpu.max":          "800000 100000\n",
			}),
			procs: 10,
			want:  3,
		},
	}
	malformed := []string{"0", "-1", "abc"}
	for _, value := range malformed {
		t.Run("an invalid GOMAXPROCS "+value+" is ignored", func(t *testing.T) {
			procs := &fakeProcs{value: 2}
			fsys := cgroupFS("0::/\n", map[string]string{"sys/fs/cgroup/cpu.max": "100000 100000\n"})
			if _, to, changed := applyCPUQuota(fsys, env("GOMAXPROCS", value), procs.gomaxprocs); !changed || to != 1 {
				t.Fatalf("applyCPUQuota with GOMAXPROCS=%q = (%d, %v), want (1, true)", value, to, changed)
			}
		})
	}

	t.Run("Start runs the step", func(t *testing.T) {
		calls := 0
		previous := limitProcs
		limitProcs = func() (int, int, bool) { calls++; return 2, 1, true }
		t.Cleanup(func() { limitProcs = previous })
		a := New("cpu-quota-start").Run(func(context.Context) error { return nil })
		if err := a.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = a.Stop(context.Background()) })
		if calls != 1 {
			t.Fatalf("Start called the CPU quota step %d times, want 1", calls)
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			procs := &fakeProcs{value: tc.procs}
			from, to, changed := applyCPUQuota(tc.fsys, noEnv, procs.gomaxprocs)
			if !changed || from != tc.procs || to != tc.want || procs.value != tc.want {
				t.Fatalf("applyCPUQuota = (%d, %d, %v), GOMAXPROCS %d; want (%d, %d, true), GOMAXPROCS %d",
					from, to, changed, procs.value, tc.procs, tc.want, tc.want)
			}
		})
	}
}

func TestStartKeepsGOMAXPROCSWithoutALimitOrWhenSet(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "cpu-quota-parallelism", "start-keeps-gomaxprocs-without-a-limit-or-when-set")

	oneCPU := cgroupFS("0::/\n", map[string]string{"sys/fs/cgroup/cpu.max": "100000 100000\n"})
	cases := []struct {
		name   string
		fsys   fstest.MapFS
		getenv func(string) string
		procs  int
	}{
		{name: "no cgroup file, as on macOS", fsys: fstest.MapFS{}, getenv: noEnv, procs: 8},
		{name: "cgroup v1 only", fsys: cgroupFS("4:cpu,cpuacct:/docker/abc\n", nil), getenv: noEnv, procs: 8},
		{name: "no limit", fsys: cgroupFS("0::/\n", map[string]string{"sys/fs/cgroup/cpu.max": "max 100000\n"}), getenv: noEnv, procs: 8},
		{name: "a malformed cpu.max", fsys: cgroupFS("0::/\n", map[string]string{"sys/fs/cgroup/cpu.max": "lots\n"}), getenv: noEnv, procs: 8},
		{name: "a cgroup above the namespace root", fsys: cgroupFS("0::/../other\n", map[string]string{"sys/fs/cgroup/other/cpu.max": "100000 100000\n"}), getenv: noEnv, procs: 8},
		{name: "a limit at or above GOMAXPROCS", fsys: cgroupFS("0::/\n", map[string]string{"sys/fs/cgroup/cpu.max": "200000 100000\n"}), getenv: noEnv, procs: 2},
		{name: "GOMAXPROCS set in the environment", fsys: oneCPU, procs: 2, getenv: env("GOMAXPROCS", "2")},
		{name: "GODEBUG turns off container limits", fsys: oneCPU, procs: 2, getenv: env("GODEBUG", "http2debug=1,containermaxprocs=0")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			procs := &fakeProcs{value: tc.procs}
			if _, _, changed := applyCPUQuota(tc.fsys, tc.getenv, procs.gomaxprocs); changed || len(procs.sets) != 0 {
				t.Fatalf("applyCPUQuota changed GOMAXPROCS: sets %v", procs.sets)
			}
		})
	}
}
