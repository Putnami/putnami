package jobs

import (
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// The scheduler reserves memory for a job only when it knows the machine's
// physical memory, so every platform the CLI ships for reads it: Linux from
// /proc/meminfo, darwin through sysctl, Windows through GlobalMemoryStatusEx.
// A fixture root describes a machine of its own and reads nothing from the
// host.
func TestTotalMemoryBytesReadsTheHostMemory(t *testing.T) {
	t.Parallel()
	if got := totalMemoryBytes(); got == 0 {
		t.Fatalf("totalMemoryBytes() = 0 on %s: the scheduler would admit jobs on CPU alone", runtime.GOOS)
	}
	if got := totalMemoryBytesAt(t.TempDir(), mapReader(nil)); got != 0 {
		t.Fatalf("a fixture root without meminfo read %d bytes: the host's memory leaked into it", got)
	}
}

func TestMemoryEnvironmentV2ExactGaugesAndWindowedCounters(t *testing.T) {
	t.Parallel()
	const root = "/runner"
	const dir = "/runner/sys/fs/cgroup/workload"
	opening := map[string]string{
		"/runner/proc/self/cgroup":     "0::/workload\n",
		dir + "/memory.current":        "0\n",
		dir + "/memory.max":            "2147483648\n",
		dir + "/memory.peak":           "1073741824\n",
		dir + "/memory.stat":           "anon 0\nfile 0\nshmem 0\nkernel 12\n",
		dir + "/memory.events":         "low 1\nhigh 2\nmax 3\noom 4\noom_kill 5\n",
		dir + "/memory.pressure":       "some avg10=0 total=10\nfull avg10=0 total=2\n",
		"/runner/proc/pressure/memory": "some total=1000\nfull total=100\n",
	}
	closing := map[string]string{
		"/runner/proc/self/cgroup": "0::/workload\n",
		dir + "/memory.current":    "536870912\n",
		dir + "/memory.max":        "2147483648\n",
		dir + "/memory.peak":       "805306368\n",
		dir + "/memory.stat":       "anon 402653184\nfile 134217728\nshmem 0\n",
		dir + "/memory.events":     "low 1\nhigh 5\nmax 4\noom 4\noom_kill 6\n",
		dir + "/memory.pressure":   "some avg10=0 total=40\nfull avg10=0 total=7\n",
	}

	begin := environmentSample{memoryCapacity: memoryCapacityReading{physicalBytes: 8 << 30, hasPhysical: true}}
	end := environmentSample{}
	readCgroupMemoryFrom(root, &begin, mapReader(opening))
	readCgroupMemoryFrom(root, &end, mapReader(closing))

	capacity := resolveMemoryCapacity(begin.memoryCapacity)
	if capacity == nil || capacity.EffectiveBytes != 2<<30 ||
		capacity.EffectiveSource != MemoryCapacityCgroupLimit || capacity.PhysicalBytes != 8<<30 ||
		capacity.CgroupLimit == nil || capacity.CgroupLimit.Source != CgroupMemoryV2 {
		t.Fatalf("capacity = %+v, want stable 2 GiB v2 bound with independent physical RAM", capacity)
	}
	memory := resolveCgroupMemory(begin.memory, end.memory)
	if memory == nil || memory.Source != CgroupMemoryV2 || memory.Closing.CurrentBytes != 512<<20 {
		t.Fatalf("memory = %+v, want closing v2 gauges", memory)
	}
	wantComposition := CgroupMemoryComposition{AnonBytes: 384 << 20, FileBytes: 128 << 20}
	if memory.Closing.Composition == nil || *memory.Closing.Composition != wantComposition {
		t.Errorf("composition = %+v, want %+v", memory.Closing.Composition, wantComposition)
	}
	if memory.Closing.LifetimePeak == nil || memory.Closing.LifetimePeak.Bytes != 768<<20 {
		t.Errorf("lifetime peak = %+v, want 768 MiB", memory.Closing.LifetimePeak)
	}
	wantEvents := CgroupMemoryEvents{High: 3, Max: 1, OOMKill: 1}
	if memory.Events == nil || *memory.Events != wantEvents {
		t.Errorf("events = %+v, want complete delta %+v (including measured zeros)", memory.Events, wantEvents)
	}
	pressure := resolveMemoryPressure(begin.memoryPressure, end.memoryPressure, memory)
	if pressure == nil || pressure.Scope != MemoryPressureCgroup ||
		pressure.SomeStalledUs != 30 || pressure.FullStalledUs != 5 {
		t.Errorf("pressure = %+v, want cgroup-scoped 30/5 delta", pressure)
	}
}

func TestMemoryEnvironmentV2KeepsLeafGaugesWithInheritedRootCapacity(t *testing.T) {
	t.Parallel()
	const root = "/runner"
	const dir = "/runner/sys/fs/cgroup/workload"
	for _, test := range []struct {
		name      string
		leafLimit string
		wantLimit bool
	}{
		{name: "unlimited", leafLimit: "max\n", wantLimit: true},
		{name: "absent", wantLimit: true},
		{name: "malformed", leafLimit: "not-a-limit\n", wantLimit: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := map[string]string{
				"/runner/proc/self/cgroup":         "0::/workload\n",
				dir + "/memory.current":            "536870912\n",
				dir + "/memory.max":                test.leafLimit,
				"/runner/sys/fs/cgroup/memory.max": "2147483648\n",
			}
			sample := environmentSample{}
			readCgroupMemoryFrom(root, &sample, mapReader(files))

			if !sample.memory.present || fixturePath(sample.memory.identity) != dir || sample.memory.current != 512<<20 {
				t.Fatalf("leaf gauge = %+v, want the workload's 512 MiB gauge", sample.memory)
			}
			if sample.memoryCapacity.hasCgroup != test.wantLimit {
				t.Fatalf("capacity = %+v, want inherited root limit present=%t", sample.memoryCapacity, test.wantLimit)
			}
			if test.wantLimit && (sample.memoryCapacity.cgroupBytes != 2<<30 ||
				sample.memoryCapacity.cgroupSource != CgroupMemoryV2) {
				t.Errorf("capacity = %+v, want stable 2 GiB v2 root bound", sample.memoryCapacity)
			}
		})
	}
}

func TestMemoryEnvironmentDropsImpossibleMemorySubfacts(t *testing.T) {
	t.Parallel()
	const root = "/runner"
	const dir = "/runner/sys/fs/cgroup/workload"
	files := map[string]string{
		"/runner/proc/self/cgroup": "0::/workload\n",
		dir + "/memory.current":    "1\n",
		dir + "/memory.max":        "1024\n",
		dir + "/memory.stat":       "anon 1\nfile 2\nshmem 3\n",
		dir + "/memory.pressure":   "some total=3\nfull total=4\n",
	}
	sample := environmentSample{}
	readCgroupMemoryFrom(root, &sample, mapReader(files))
	if !sample.memory.present {
		t.Fatal("valid closing gauge was dropped with invalid optional facts")
	}
	if sample.memory.composition != nil {
		t.Errorf("composition with shmem > file survived: %+v", sample.memory.composition)
	}
	if sample.memoryPressure.present {
		t.Errorf("PSI sample with full > some survived: %+v", sample.memoryPressure)
	}

	beginSome, beginFull, beginOK := parseMemoryPressure("some total=100\nfull total=90\n")
	endSome, endFull, endOK := parseMemoryPressure("some total=110\nfull total=105\n")
	if !beginOK || !endOK {
		t.Fatal("individually consistent PSI endpoints were rejected")
	}
	begin := memoryPressureSample{
		present: true, scope: MemoryPressureCgroup, identity: dir, some: beginSome, full: beginFull,
	}
	end := memoryPressureSample{
		present: true, scope: MemoryPressureCgroup, identity: dir, some: endSome, full: endFull,
	}
	if pressure := resolveMemoryPressure(begin, end, &CgroupMemory{Source: CgroupMemoryV2}); pressure != nil {
		t.Errorf("PSI delta with full > some survived: %+v", pressure)
	}
}

func TestMemoryEnvironmentDropsPartialResetUnsafeAndMismatchedFacts(t *testing.T) {
	t.Parallel()
	const root = "/runner"
	const dir = "/runner/sys/fs/cgroup/a"
	files := map[string]string{
		"/runner/proc/self/cgroup":     "0::/a\n",
		dir + "/memory.current":        "1\n",
		dir + "/memory.max":            "max\n",
		dir + "/memory.peak":           "9007199254740992\n",
		dir + "/memory.stat":           "anon 1\nfile malformed\nshmem 0\n",
		dir + "/memory.events":         "low 1\nhigh 2\nmax 3\noom 4\n",
		dir + "/memory.pressure":       "some total=3\n",
		"/runner/proc/pressure/memory": "some total=10\nfull total=5\n",
	}
	sample := environmentSample{}
	readMemoryEnvironmentFrom(root, &sample, mapReader(files), 0)
	if !sample.memory.present {
		t.Fatal("safe current gauge was dropped with independent malformed optional files")
	}
	if sample.memory.composition != nil || sample.memory.peak != nil || sample.memory.events != nil {
		t.Errorf("partial/unsafe nested facts survived: %+v", sample.memory)
	}
	if !sample.memoryPressure.present || sample.memoryPressure.scope != MemoryPressureHost {
		t.Errorf("partial cgroup PSI did not fall back honestly to host PSI: %+v", sample.memoryPressure)
	}

	begin := memorySample{
		present: true, source: CgroupMemoryV2, identity: dir, current: 1,
		events: &CgroupMemoryEvents{Low: 5, High: 5, Max: 5, OOM: 5, OOMKill: 5},
	}
	end := begin
	end.current = 2
	end.events = &CgroupMemoryEvents{Low: 4, High: 6, Max: 6, OOM: 6, OOMKill: 6}
	if memory := resolveCgroupMemory(begin, end); memory == nil || memory.Events != nil {
		t.Errorf("one reset counter must omit the whole events block, got %+v", memory)
	}
	end = begin
	end.identity = "/runner/sys/fs/cgroup/b"
	if memory := resolveCgroupMemory(begin, end); memory != nil {
		t.Errorf("cgroup identity mismatch published %+v", memory)
	}
	end = begin
	end.source = CgroupMemoryV1
	if memory := resolveCgroupMemory(begin, end); memory != nil {
		t.Errorf("cgroup source mismatch published %+v", memory)
	}

	overflow := environmentSample{}
	overflowFiles := map[string]string{
		"/runner/proc/self/cgroup": "0::/a\n",
		dir + "/memory.current":    "9007199254740992\n",
		dir + "/memory.max":        "18446744073709551616\n",
	}
	readCgroupMemoryFrom(root, &overflow, mapReader(overflowFiles))
	if overflow.memory.present || overflow.memoryCapacity.hasCgroup {
		t.Errorf("overflow/unsafe values were retained: %+v", overflow)
	}
}

func TestMemoryEnvironmentV1KeepsOnlyExactFiniteFacts(t *testing.T) {
	t.Parallel()
	const root = "/runner"
	const dir = "/runner/sys/fs/cgroup/memory/container"
	files := map[string]string{
		"/runner/proc/self/cgroup":         "2:memory:/container\n",
		dir + "/memory.usage_in_bytes":     "0\n",
		dir + "/memory.max_usage_in_bytes": "1024\n",
		dir + "/memory.limit_in_bytes":     "2147483648\n",
	}
	sample := environmentSample{}
	readCgroupMemoryFrom(root, &sample, mapReader(files))
	if !sample.memory.present || sample.memory.source != CgroupMemoryV1 || sample.memory.current != 0 ||
		sample.memory.peak == nil || sample.memory.peak.Bytes != 1024 || sample.memory.events != nil ||
		sample.memory.composition != nil {
		t.Fatalf("v1 exact facts = %+v", sample.memory)
	}
	if !sample.memoryCapacity.hasCgroup || sample.memoryCapacity.cgroupBytes != 2<<30 ||
		sample.memoryCapacity.cgroupSource != CgroupMemoryV1 {
		t.Errorf("v1 finite limit = %+v", sample.memoryCapacity)
	}

	files[dir+"/memory.limit_in_bytes"] = "9223372036854771712\n"
	unlimited := environmentSample{}
	readCgroupMemoryFrom(root, &unlimited, mapReader(files))
	if unlimited.memoryCapacity.hasCgroup {
		t.Errorf("v1 unlimited sentinel published as finite: %+v", unlimited.memoryCapacity)
	}
}

func TestReadCgroupMemoryUsesOnlyBoundedExactPaths(t *testing.T) {
	t.Parallel()
	const root = "/runner"
	const dir = "/runner/sys/fs/cgroup/workload"
	files := map[string]string{
		"/runner/proc/self/cgroup": "0::/workload\n",
		dir + "/memory.current":    "0\n",
		dir + "/memory.max":        "max\n",
	}
	var reads []string
	read := func(path string) string {
		reads = append(reads, fixturePath(path))
		return files[fixturePath(path)]
	}
	sample := environmentSample{}
	readCgroupMemoryFrom(root, &sample, read)
	want := []string{
		"/runner/proc/self/cgroup",
		dir + "/memory.current", dir + "/memory.max", dir + "/memory.peak",
		dir + "/memory.stat", dir + "/memory.events", "/runner/sys/fs/cgroup/memory.max",
		dir + "/memory.pressure",
	}
	if !reflect.DeepEqual(reads, want) {
		t.Errorf("reads = %v, want bounded direct reads %v", reads, want)
	}
}

func mapReader(files map[string]string) func(string) string {
	return func(path string) string { return files[fixturePath(path)] }
}

// fixturePath is the key a fixture map holds for a path the reader joined: the
// fixtures spell the cgroup tree in slash form, and filepath.Join gives
// backslashes on Windows.
func fixturePath(path string) string {
	return filepath.ToSlash(filepath.Clean(path))
}

func readMemoryEnvironmentFrom(
	root string,
	sample *environmentSample,
	read func(string) string,
	physical uint64,
) {
	if physical > 0 {
		sample.memoryCapacity = memoryCapacityReading{physicalBytes: physical, hasPhysical: true}
	}
	readCgroupMemoryFrom(root, sample, read)
	if !sample.memoryPressure.present {
		readHostMemoryPressureFrom(root, sample, read)
	}
}
