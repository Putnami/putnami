package jobs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/workspace"
)

// Runner-environment capture.
//
// The whole point of these tests is that the interesting cases cannot be
// produced on the machine running them: a developer's macOS laptop has no
// cgroup, no PSI and no /proc/stat, and a CI container has one quota rather
// than the several this file needs to distinguish. So the capture reads under a
// ROOT, and every test below hands it a fixture tree describing exactly one
// runner — including the empty tree, which is the degradation path and the one
// most likely to regress silently.

// fixtureRunner writes one runner's procfs/sysfs surface under a temp root.
func fixtureRunner(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return root
}

// quotaLimitedRunner is a container pinned to 8 of the host's cores, in a
// delegated cgroup, on a host with visible steal — the CI shape the epic is
// about. The two maps are the same files at the start and the end of a session.
func quotaLimitedRunner(t *testing.T) (root string, closing map[string]string) {
	t.Helper()
	const cgroupDir = "sys/fs/cgroup/kubepods/burstable/podabc"
	opening := map[string]string{
		"proc/self/cgroup":      "0::/kubepods/burstable/podabc\n",
		cgroupDir + "/cpu.max":  "800000 100000\n",
		cgroupDir + "/cpu.stat": "usage_usec 1000000\nuser_usec 800000\nsystem_usec 200000\nnr_periods 100\nnr_throttled 5\nthrottled_usec 20000\n",
		"proc/pressure/cpu":     "some avg10=0.31 avg60=0.12 avg300=0.04 total=1000\n",
		"proc/stat":             "cpu  100 20 30 400 50 6 7 80 900 1000\ncpu0 100 20 30 400 50 6 7 80 900 1000\n",
		"proc/cpuinfo":          "processor\t: 0\nmodel name\t: AMD EPYC 7B13 64-Core Processor\n",
	}
	closing = map[string]string{
		cgroupDir + "/cpu.stat": "usage_usec 3500000\nuser_usec 2800000\nsystem_usec 700000\nnr_periods 135\nnr_throttled 9\nthrottled_usec 148000\n",
		"proc/pressure/cpu":     "some avg10=4.02 avg60=1.55 avg300=0.44 total=4000\n",
		"proc/stat":             "cpu  200 20 30 400 60 6 7 100 900 1000\ncpu0 200 20 30 400 60 6 7 100 900 1000\n",
	}
	return fixtureRunner(t, opening), closing
}

// sessionOver takes the opening sample, applies the closing file contents, and
// takes the closing sample — the exact sequence beginRun/finalizeRun perform,
// with the samples' clocks pinned so the window is assertable.
func sessionOver(t *testing.T, root string, closing map[string]string, window time.Duration) RunnerEnvironment {
	t.Helper()
	begin := captureEnvironmentSample(root)
	for path, content := range closing {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	end := captureEnvironmentSample(root)
	begin.at = time.Unix(0, 0)
	end.at = begin.at.Add(window)
	return resolveRunnerEnvironment(root, begin, end)
}

// TestRunnerEnvironment_WindowsEveryCumulativeCounter is criterion 4: the
// kernel's counters are cumulative since boot, so only the DELTA over the
// session says anything about this run. A capture that published the raw
// readings would report a runner's entire uptime as this build's throttling.
func TestRunnerEnvironment_WindowsEveryCumulativeCounter(t *testing.T) {
	t.Parallel()
	root, closing := quotaLimitedRunner(t)
	environment := sessionOver(t, root, closing, 2*time.Second)

	if environment.Window != 2*time.Second {
		t.Errorf("window = %v, want 2s", environment.Window)
	}
	if environment.CPUModel != "AMD EPYC 7B13 64-Core Processor" {
		t.Errorf("cpu model = %q", environment.CPUModel)
	}
	if environment.OS != runtime.GOOS || environment.Arch != runtime.GOARCH {
		t.Errorf("host facts = %s/%s, want the running platform", environment.OS, environment.Arch)
	}

	cgroup := environment.Cgroup
	if cgroup == nil {
		t.Fatal("no cgroup block for a runner whose cpu.max exists")
	}
	if cgroup.PeriodUs != 100000 || cgroup.QuotaUs != 800000 {
		t.Errorf("cpu.max = %d/%d, want 800000/100000", cgroup.QuotaUs, cgroup.PeriodUs)
	}
	// 3500000-1000000: the cgroup's own CPU over the window, not since boot.
	if !cgroup.HasUsage || cgroup.UsageUs != 2500000 {
		t.Errorf("usage = %dus (has=%v), want the 2500000us delta", cgroup.UsageUs, cgroup.HasUsage)
	}
	if cgroup.Throttle == nil {
		t.Fatal("no throttle block for a cpu.stat carrying the bandwidth counters")
	}
	want := CgroupThrottle{Periods: 35, ThrottledPeriods: 4, ThrottledUs: 128000}
	if *cgroup.Throttle != want {
		t.Errorf("throttle = %+v, want %+v", *cgroup.Throttle, want)
	}

	if environment.CPUPressure == nil || environment.CPUPressure.SomeStalledUs != 3000 {
		t.Errorf("psi = %+v, want the 3000us stall delta", environment.CPUPressure)
	}

	// 100+20+30+400+50+6+7+80 = 693 opening, 823 closing.
	host := environment.HostCPU
	if host == nil {
		t.Fatal("no host CPU block for a runner with /proc/stat")
	}
	if host.StealTicks != 20 || host.IOWaitTicks != 10 || host.TotalTicks != 130 {
		t.Errorf("host cpu = %+v, want steal 20 / iowait 10 / total 130", *host)
	}
}

// TestRunnerEnvironment_AbsentFilesStayAbsent is criterion 3, and the case a
// developer machine actually hits. A tree with none of the files must yield
// three ABSENT blocks — not zeroed ones, which would read as "measured: never
// throttled, no pressure, no steal" for a machine that cannot measure any of it.
func TestRunnerEnvironment_AbsentFilesStayAbsent(t *testing.T) {
	t.Parallel()
	root := fixtureRunner(t, nil)
	environment := sessionOver(t, root, nil, time.Second)

	if environment.Cgroup != nil {
		t.Errorf("cgroup block invented on a machine with no cgroups: %+v", environment.Cgroup)
	}
	if environment.CPUPressure != nil {
		t.Errorf("PSI block invented on a kernel with no PSI: %+v", environment.CPUPressure)
	}
	if environment.HostCPU != nil {
		t.Errorf("/proc/stat block invented where the file does not exist: %+v", environment.HostCPU)
	}
	if environment.CPUModel != "" {
		t.Errorf("cpu model = %q, want empty rather than a fabricated string", environment.CPUModel)
	}
	// What the platform DOES have is still recorded — the degradation is
	// per-member, not all-or-nothing.
	if environment.OS != runtime.GOOS || environment.Arch != runtime.GOARCH || environment.LogicalCPUs != runtime.NumCPU() {
		t.Errorf("host facts lost: %+v", environment)
	}
}

// TestRunnerEnvironment_UnlimitedQuotaIsAbsentNotZero: cpu.max "max" means no
// ceiling. Reporting a quota of 0 would make an unbounded runner look
// completely starved in every allocation figure derived from it.
func TestRunnerEnvironment_UnlimitedQuotaIsAbsentNotZero(t *testing.T) {
	t.Parallel()
	root := fixtureRunner(t, map[string]string{
		"proc/self/cgroup":       "0::/\n",
		"sys/fs/cgroup/cpu.max":  "max 100000\n",
		"sys/fs/cgroup/cpu.stat": "usage_usec 500000\n",
	})
	environment := sessionOver(t, root, map[string]string{
		"sys/fs/cgroup/cpu.stat": "usage_usec 900000\n",
	}, time.Second)

	if environment.Cgroup == nil {
		t.Fatal("no cgroup block for an unlimited but readable cpu.max")
	}
	if environment.Cgroup.QuotaUs != 0 || environment.Cgroup.PeriodUs != 100000 {
		t.Errorf("cpu.max = %+v, want quota 0 (unlimited) and period 100000", *environment.Cgroup)
	}
	// An unlimited cgroup exposes no bandwidth counters, so no throttle block.
	if environment.Cgroup.Throttle != nil {
		t.Errorf("throttle block invented for a cgroup with no bandwidth counters: %+v", *environment.Cgroup.Throttle)
	}
	millicores, fromQuota := environment.AllocatedMillicores()
	if fromQuota {
		t.Error("an unlimited cgroup reported its allocation as a quota")
	}
	if millicores != runtime.NumCPU()*1000 {
		t.Errorf("allocation = %d millicores, want the visible core count", millicores)
	}
}

// TestRunnerEnvironment_QuotaBoundsTheAllocation is the other half: a quota IS
// the ceiling, and it is what the run's actual-vs-allocated ratio divides by.
func TestRunnerEnvironment_QuotaBoundsTheAllocation(t *testing.T) {
	t.Parallel()
	root, closing := quotaLimitedRunner(t)
	environment := sessionOver(t, root, closing, time.Second)

	millicores, fromQuota := environment.AllocatedMillicores()
	if !fromQuota || millicores != 8000 {
		t.Errorf("allocation = %d millicores (quota=%v), want 8000 from the cgroup quota", millicores, fromQuota)
	}
	if environment.LogicalCPUs == 8 {
		t.Skip("this machine has exactly 8 visible CPUs, so the quota and the core count cannot be told apart here")
	}
	if millicores == environment.LogicalCPUs*1000 {
		t.Errorf("allocation fell back to the core count despite a quota of 800000/100000")
	}
}

// TestRunnerEnvironment_ResetCounterIsDroppedNotClamped: a counter that went
// backwards means the two samples do not describe one window. Clamping the
// difference to zero would publish a broken measurement as the most reassuring
// possible reading — "no throttling at all" — which is the failure mode a cost
// baseline can least afford.
func TestRunnerEnvironment_ResetCounterIsDroppedNotClamped(t *testing.T) {
	t.Parallel()
	root, _ := quotaLimitedRunner(t)
	const cgroupDir = "sys/fs/cgroup/kubepods/burstable/podabc"
	environment := sessionOver(t, root, map[string]string{
		// Every counter smaller than it was at the start.
		cgroupDir + "/cpu.stat": "usage_usec 10\nnr_periods 1\nnr_throttled 0\nthrottled_usec 0\n",
		"proc/pressure/cpu":     "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\n",
		"proc/stat":             "cpu  1 1 1 1 1 1 1 1 1 1\n",
	}, time.Second)

	if environment.Cgroup == nil {
		t.Fatal("the cgroup's configuration is not a counter and must survive")
	}
	if environment.Cgroup.HasUsage {
		t.Errorf("reset usage counter published as %dus", environment.Cgroup.UsageUs)
	}
	if environment.Cgroup.Throttle != nil {
		t.Errorf("reset throttle counters published as %+v", *environment.Cgroup.Throttle)
	}
	if environment.CPUPressure != nil {
		t.Errorf("reset PSI counter published as %+v", *environment.CPUPressure)
	}
	if environment.HostCPU != nil {
		t.Errorf("reset /proc/stat counters published as %+v", *environment.HostCPU)
	}
}

// TestRunnerEnvironment_CgroupNamespaceRoot covers the layout a container with
// its own cgroup namespace shows: /proc/self/cgroup reads "0::/" and the mount
// root IS the container's cgroup, so the delegated-path lookup resolves to the
// mount and finds cpu.max there.
func TestRunnerEnvironment_CgroupNamespaceRoot(t *testing.T) {
	t.Parallel()
	root := fixtureRunner(t, map[string]string{
		"proc/self/cgroup":       "0::/\n",
		"sys/fs/cgroup/cpu.max":  "400000 100000\n",
		"sys/fs/cgroup/cpu.stat": "usage_usec 10\nnr_periods 2\nnr_throttled 0\nthrottled_usec 0\n",
	})
	environment := sessionOver(t, root, map[string]string{
		"sys/fs/cgroup/cpu.stat": "usage_usec 400010\nnr_periods 6\nnr_throttled 0\nthrottled_usec 0\n",
	}, time.Second)

	if environment.Cgroup == nil || environment.Cgroup.QuotaUs != 400000 {
		t.Fatalf("cgroup = %+v, want the mount-root quota of 400000", environment.Cgroup)
	}
	// The zeros here are MEASUREMENTS: four periods elapsed and none of them was
	// throttled, which is exactly the finding that rules out quota starvation.
	if environment.Cgroup.Throttle == nil {
		t.Fatal("throttle block dropped, losing the measured 'never throttled'")
	}
	want := CgroupThrottle{Periods: 4}
	if *environment.Cgroup.Throttle != want {
		t.Errorf("throttle = %+v, want %+v", *environment.Cgroup.Throttle, want)
	}
}

// TestRunnerEnvironment_CgroupWithoutBandwidthCounters: an older kernel's
// cpu.stat carries usage but no nr_periods. Usage is published, the throttle
// block is not — a partial file contributes what it has and claims nothing else.
func TestRunnerEnvironment_CgroupWithoutBandwidthCounters(t *testing.T) {
	t.Parallel()
	root := fixtureRunner(t, map[string]string{
		"proc/self/cgroup":       "0::/\n",
		"sys/fs/cgroup/cpu.max":  "200000 100000\n",
		"sys/fs/cgroup/cpu.stat": "usage_usec 1000\n",
	})
	environment := sessionOver(t, root, map[string]string{
		"sys/fs/cgroup/cpu.stat": "usage_usec 6000\n",
	}, time.Second)

	if environment.Cgroup == nil || !environment.Cgroup.HasUsage || environment.Cgroup.UsageUs != 5000 {
		t.Fatalf("cgroup = %+v, want a 5000us usage delta", environment.Cgroup)
	}
	if environment.Cgroup.Throttle != nil {
		t.Errorf("throttle block invented from a cpu.stat that has none: %+v", *environment.Cgroup.Throttle)
	}
}

// TestRunnerEnvironment_CgroupV1GVisorFallback covers the production runner:
// gVisor exposes v1's exact cpu and cpuacct files, no v2 unified entry, no PSI,
// and an all-zero /proc/stat that cannot measure host steal.
func TestRunnerEnvironment_CgroupV1GVisorFallback(t *testing.T) {
	t.Parallel()
	root := fixtureRunner(t, map[string]string{
		"proc/self/cgroup":                    "4:cpu:/\n3:cpuacct:/\n1:name=systemd:/\n",
		"sys/fs/cgroup/cpu/cpu.cfs_quota_us":  "800000\n",
		"sys/fs/cgroup/cpu/cpu.cfs_period_us": "100000\n",
		"sys/fs/cgroup/cpuacct/cpuacct.usage": "1851660000000\n",
		"proc/stat":                           "cpu  0 0 0 0 0 0 0 0 0 0\n",
	})
	environment := sessionOver(t, root, map[string]string{
		"sys/fs/cgroup/cpuacct/cpuacct.usage": "1854160000000\n",
	}, time.Second)
	if environment.Cgroup == nil {
		t.Fatal("no cgroup block for a v1-only runner with an exact quota")
	}
	if environment.Cgroup.PeriodUs != 100000 || environment.Cgroup.QuotaUs != 800000 {
		t.Errorf("v1 quota = %d/%d, want 800000/100000", environment.Cgroup.QuotaUs, environment.Cgroup.PeriodUs)
	}
	if !environment.Cgroup.HasUsage || environment.Cgroup.UsageUs != 2500000 {
		t.Errorf("v1 usage = %dus (has=%v), want the 2500000us delta", environment.Cgroup.UsageUs, environment.Cgroup.HasUsage)
	}
	if environment.Cgroup.Throttle != nil {
		t.Errorf("v1 runner invented unavailable throttle counters: %+v", *environment.Cgroup.Throttle)
	}
	if environment.CPUPressure != nil || environment.HostCPU != nil {
		t.Errorf("gVisor invented unavailable PSI/host CPU facts: pressure=%+v host=%+v", environment.CPUPressure, environment.HostCPU)
	}
	millicores, fromQuota := environment.AllocatedMillicores()
	if !fromQuota || millicores != 8000 {
		t.Errorf("allocation = %d (quota=%v), want 8000 millicores from the v1 quota", millicores, fromQuota)
	}
}

// TestRunnerEnvironment_CgroupV1MembershipPathAbsent is the production runner
// as actually probed: gVisor advertises a /container0 membership
// in every v1 hierarchy but never creates those directories, so the values only
// exist at the controller root.
//
// It asserts the PROVENANCE, not just the number. Cloud Run makes
// runtime.NumCPU() equal the quota's 8 cores, so a missed quota still yields
// 8000 millicores from logical-cpus and the reading looks right while being
// sourced from the wrong fact — on any runner where the two differ, every
// utilization ratio built on that denominator would be silently wrong.
func TestRunnerEnvironment_CgroupV1MembershipPathAbsent(t *testing.T) {
	t.Parallel()
	root := fixtureRunner(t, map[string]string{
		"proc/self/cgroup":                    "4:cpuset:/container0\n3:cpuacct:/container0\n2:memory:/container0\n1:cpu:/container0\n",
		"sys/fs/cgroup/cpu/cpu.cfs_quota_us":  "800000\n",
		"sys/fs/cgroup/cpu/cpu.cfs_period_us": "100000\n",
		"sys/fs/cgroup/cpuacct/cpuacct.usage": "432970000000\n",
		"proc/stat":                           "cpu  0 0 0 0 0 0 0 0 0 0\n",
	})
	environment := sessionOver(t, root, map[string]string{
		"sys/fs/cgroup/cpuacct/cpuacct.usage": "435470000000\n",
	}, time.Second)

	if environment.Cgroup == nil {
		t.Fatal("no cgroup block: the membership join stat'd a path gVisor never created")
	}
	if environment.Cgroup.PeriodUs != 100000 || environment.Cgroup.QuotaUs != 800000 {
		t.Errorf("v1 quota = %d/%d, want 800000/100000 from the controller root",
			environment.Cgroup.QuotaUs, environment.Cgroup.PeriodUs)
	}
	if !environment.Cgroup.HasUsage || environment.Cgroup.UsageUs != 2500000 {
		t.Errorf("v1 usage = %dus (has=%v), want the 2500000us delta", environment.Cgroup.UsageUs, environment.Cgroup.HasUsage)
	}
	if environment.CPUPressure != nil || environment.HostCPU != nil {
		t.Errorf("gVisor invented unavailable PSI/host CPU facts: pressure=%+v host=%+v",
			environment.CPUPressure, environment.HostCPU)
	}
	millicores, fromQuota := environment.AllocatedMillicores()
	if !fromQuota || millicores != 8000 {
		t.Errorf("allocation = %d (quota=%v), want 8000 millicores sourced from the quota, not the core count",
			millicores, fromQuota)
	}
}

// TestReadCgroupCPU_ExactPathFallbacks is deliberately backed only by a map of
// exact file paths. It has no directory-listing operation at all, reproducing
// the gVisor shape where ReadDir("/sys/fs/cgroup") is empty while direct reads
// below cpu and cpuacct succeed.
func TestReadCgroupCPU_ExactPathFallbacks(t *testing.T) {
	t.Parallel()
	const root = "/fake-runner"
	for _, tc := range []struct {
		name       string
		files      map[string]string
		hasCgroup  bool
		periodUs   int64
		quotaUs    int64
		hasUsage   bool
		usageUs    int64
		grantCores int
	}{
		{
			name: "v2 present",
			files: map[string]string{
				"proc/self/cgroup":                    "0::/\n4:cpu:/\n3:cpuacct:/\n",
				"sys/fs/cgroup/cpu.max":               "200000 100000\n",
				"sys/fs/cgroup/cpu.stat":              "usage_usec 42\n",
				"sys/fs/cgroup/cpu/cpu.cfs_quota_us":  "800000\n",
				"sys/fs/cgroup/cpu/cpu.cfs_period_us": "100000\n",
				"sys/fs/cgroup/cpuacct/cpuacct.usage": "1851660000000\n",
			},
			hasCgroup: true, periodUs: 100000, quotaUs: 200000,
			hasUsage: true, usageUs: 42, grantCores: 2,
		},
		{
			name: "v1 only with empty root listing",
			files: map[string]string{
				"proc/self/cgroup":                    "4:cpu:/\n3:cpuacct:/\n",
				"sys/fs/cgroup/cpu/cpu.cfs_quota_us":  "800000\n",
				"sys/fs/cgroup/cpu/cpu.cfs_period_us": "100000\n",
				"sys/fs/cgroup/cpuacct/cpuacct.usage": "1851660000000\n",
			},
			hasCgroup: true, periodUs: 100000, quotaUs: 800000,
			hasUsage: true, usageUs: 1851660000, grantCores: 8,
		},
		{
			// The production runner, probed on a live serverless deployment: gVisor names
			// /container0 in every hierarchy and creates none of them, so the
			// only readable quota is the controller ROOT. Joining the
			// membership onto the mount stats an absent path, and before the
			// root fallback the whole block came back missing — which the CI
			// substrate hid, because its 8-core quota happens to equal
			// runtime.NumCPU() and logical-cpus reported the same number.
			name: "v1 membership directory the runtime never created",
			files: map[string]string{
				"proc/self/cgroup":                    "4:cpuset:/container0\n3:cpuacct:/container0\n2:memory:/container0\n1:cpu:/container0\n",
				"sys/fs/cgroup/cpu/cpu.cfs_quota_us":  "800000\n",
				"sys/fs/cgroup/cpu/cpu.cfs_period_us": "100000\n",
				"sys/fs/cgroup/cpuacct/cpuacct.usage": "432970000000\n",
			},
			hasCgroup: true, periodUs: 100000, quotaUs: 800000,
			hasUsage: true, usageUs: 432970000, grantCores: 8,
		},
		{
			// The root fallback must not outrank a membership directory that
			// does exist, on ANY candidate mount — a root carries the PARENT's
			// quota. Here the first mount tried has readable root files and the
			// second holds the real membership, so a per-mount ordering would
			// read 1 core instead of 6.
			name: "v1 membership on a later mount outranks an earlier root",
			files: map[string]string{
				"proc/self/cgroup":                            "5:cpu,cpuacct:/pod\n",
				"sys/fs/cgroup/cpu,cpuacct/cpu.cfs_quota_us":  "100000\n",
				"sys/fs/cgroup/cpu,cpuacct/cpu.cfs_period_us": "100000\n",
				"sys/fs/cgroup/cpu,cpuacct/cpuacct.usage":     "999999000\n",
				"sys/fs/cgroup/cpu/pod/cpu.cfs_quota_us":      "600000\n",
				"sys/fs/cgroup/cpu/pod/cpu.cfs_period_us":     "100000\n",
				"sys/fs/cgroup/cpuacct/pod/cpuacct.usage":     "7000000\n",
			},
			hasCgroup: true, periodUs: 100000, quotaUs: 600000,
			hasUsage: true, usageUs: 7000, grantCores: 6,
		},
		{
			name: "v1 combined mount with non-root membership",
			files: map[string]string{
				"proc/self/cgroup":                                          "5:cpu,cpuacct:/docker/runner\n",
				"sys/fs/cgroup/cpu,cpuacct/cpu.cfs_quota_us":                "100000\n",
				"sys/fs/cgroup/cpu,cpuacct/cpu.cfs_period_us":               "100000\n",
				"sys/fs/cgroup/cpu,cpuacct/cpuacct.usage":                   "999999000\n",
				"sys/fs/cgroup/cpu,cpuacct/docker/runner/cpu.cfs_quota_us":  "600000\n",
				"sys/fs/cgroup/cpu,cpuacct/docker/runner/cpu.cfs_period_us": "100000\n",
				"sys/fs/cgroup/cpu,cpuacct/docker/runner/cpuacct.usage":     "1234000\n",
			},
			hasCgroup: true, periodUs: 100000, quotaUs: 600000,
			hasUsage: true, usageUs: 1234, grantCores: 6,
		},
		{
			name: "v1 separate mounts with distinct memberships",
			files: map[string]string{
				"proc/self/cgroup":                               "4:cpu:/quota/team\n3:cpuacct:/usage/team\n",
				"sys/fs/cgroup/cpu/cpu.cfs_quota_us":             "100000\n",
				"sys/fs/cgroup/cpu/cpu.cfs_period_us":            "100000\n",
				"sys/fs/cgroup/cpuacct/cpuacct.usage":            "999999000\n",
				"sys/fs/cgroup/cpu/quota/team/cpu.cfs_quota_us":  "350000\n",
				"sys/fs/cgroup/cpu/quota/team/cpu.cfs_period_us": "100000\n",
				"sys/fs/cgroup/cpuacct/usage/team/cpuacct.usage": "9000000\n",
			},
			hasCgroup: true, periodUs: 100000, quotaUs: 350000,
			hasUsage: true, usageUs: 9000, grantCores: 3,
		},
		{
			name: "v1 mountinfo resolves a nonstandard combined mount",
			files: map[string]string{
				"proc/self/cgroup":    "5:cpu,cpuacct:/tenant/run\n",
				"proc/self/mountinfo": "36 25 0:32 / /custom/cgroups/compute rw,nosuid,nodev,noexec,relatime - cgroup cgroup rw,cpu,cpuacct\n",
				"custom/cgroups/compute/tenant/run/cpu.cfs_quota_us":  "250000\n",
				"custom/cgroups/compute/tenant/run/cpu.cfs_period_us": "100000\n",
				"custom/cgroups/compute/tenant/run/cpuacct.usage":     "42000\n",
			},
			hasCgroup: true, periodUs: 100000, quotaUs: 250000,
			hasUsage: true, usageUs: 42, grantCores: 2,
		},
		{
			// mountinfo's mount ROOT, which /proc/self/cgroup knows nothing
			// about: the cgroupfs is attached at the container's own cgroup, so
			// the membership must be rebased onto it before it can be joined.
			// The naive join looks for .../cpu,cpuacct/docker/abc/worker under a
			// mount that already IS /docker/abc, and the root fallback would
			// then read the CONTAINER's 8-core quota for a worker pinned to 2.
			name: "v1 mount attached at an ancestor cgroup rebases the membership",
			files: map[string]string{
				"proc/self/cgroup":                                   "5:cpu,cpuacct:/docker/abc/worker\n",
				"proc/self/mountinfo":                                "36 25 0:32 /docker/abc /sys/fs/cgroup/cpu,cpuacct rw,nosuid,nodev,noexec,relatime - cgroup cgroup rw,cpu,cpuacct\n",
				"sys/fs/cgroup/cpu,cpuacct/cpu.cfs_quota_us":         "800000\n",
				"sys/fs/cgroup/cpu,cpuacct/cpu.cfs_period_us":        "100000\n",
				"sys/fs/cgroup/cpu,cpuacct/cpuacct.usage":            "999999000\n",
				"sys/fs/cgroup/cpu,cpuacct/worker/cpu.cfs_quota_us":  "200000\n",
				"sys/fs/cgroup/cpu,cpuacct/worker/cpu.cfs_period_us": "100000\n",
				"sys/fs/cgroup/cpu,cpuacct/worker/cpuacct.usage":     "5000000\n",
			},
			hasCgroup: true, periodUs: 100000, quotaUs: 200000,
			hasUsage: true, usageUs: 5000, grantCores: 2,
		},
		{
			// The same layout with the rebased directory missing. The mount
			// point is reachable and readable, but it is a known ANCESTOR — so
			// the block must come back ABSENT rather than claim the container's
			// quota as this process's. The gVisor root fallback is deliberately
			// not available here: it is only honest for a mount attached at the
			// hierarchy root, where nothing is known to sit in between.
			name: "v1 ancestor mount never substitutes its own quota",
			files: map[string]string{
				"proc/self/cgroup":                            "5:cpu,cpuacct:/docker/abc/worker\n",
				"proc/self/mountinfo":                         "36 25 0:32 /docker/abc /sys/fs/cgroup/cpu,cpuacct rw,nosuid,nodev,noexec,relatime - cgroup cgroup rw,cpu,cpuacct\n",
				"sys/fs/cgroup/cpu,cpuacct/cpu.cfs_quota_us":  "800000\n",
				"sys/fs/cgroup/cpu,cpuacct/cpu.cfs_period_us": "100000\n",
				"sys/fs/cgroup/cpu,cpuacct/cpuacct.usage":     "999999000\n",
			},
		},
		{
			// A mount exposing a SIBLING's cgroup cannot name this process's at
			// any depth, so it is dropped entirely rather than rebased onto a
			// path that would silently resolve somewhere else.
			name: "v1 sibling mount is not a usable hierarchy",
			files: map[string]string{
				"proc/self/cgroup":                            "5:cpu,cpuacct:/docker/abc\n",
				"proc/self/mountinfo":                         "36 25 0:32 /docker/xyz /sys/fs/cgroup/cpu,cpuacct rw,relatime - cgroup cgroup rw,cpu,cpuacct\n",
				"sys/fs/cgroup/cpu,cpuacct/cpu.cfs_quota_us":  "800000\n",
				"sys/fs/cgroup/cpu,cpuacct/cpu.cfs_period_us": "100000\n",
			},
		},
		{
			name: "neither",
			files: map[string]string{
				"proc/self/cgroup":                    "4:cpu:/\n3:cpuacct:/\n",
				"sys/fs/cgroup/cpu/cpu.cfs_period_us": "100000\n",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := func(path string) string {
				relative, err := filepath.Rel(root, path)
				if err != nil {
					return ""
				}
				return tc.files[filepath.ToSlash(relative)]
			}
			var sample environmentSample
			readCgroupCPUFrom(root, &sample, read)
			if sample.hasCgroup != tc.hasCgroup || sample.periodUs != tc.periodUs || sample.quotaUs != tc.quotaUs {
				t.Errorf("quota = %d/%d (has=%v), want %d/%d (has=%v)",
					sample.quotaUs, sample.periodUs, sample.hasCgroup, tc.quotaUs, tc.periodUs, tc.hasCgroup)
			}
			if sample.hasUsage != tc.hasUsage || sample.usageUs != tc.usageUs {
				t.Errorf("usage = %dus (has=%v), want %dus (has=%v)",
					sample.usageUs, sample.hasUsage, tc.usageUs, tc.hasUsage)
			}
			if sample.hasThrottle {
				t.Errorf("fixture without bandwidth counters invented throttle data: %+v", sample)
			}
			if grant := sample.cpuBandwidthCores(); grant != tc.grantCores {
				t.Errorf("allocator grant = %d cores, want %d", grant, tc.grantCores)
			}
		})
	}
}

// TestProcStatTotalExcludesGuestColumns pins the one arithmetic subtlety in the
// /proc/stat line: `guest` and `guest_nice` are ALREADY counted inside `user`
// and `nice`, so folding them into the total would inflate the denominator
// every steal ratio is read against.
func TestProcStatTotalExcludesGuestColumns(t *testing.T) {
	t.Parallel()
	var sample environmentSample
	root := fixtureRunner(t, map[string]string{
		"proc/stat": "cpu  1 2 3 4 5 6 7 8 1000 2000\n",
	})
	readHostCPUTime(root, &sample)
	if !sample.hasHostCPU {
		t.Fatal("aggregate cpu line not read")
	}
	if sample.totalTicks != 36 {
		t.Errorf("total = %d ticks, want 36 (the first eight columns only)", sample.totalTicks)
	}
	if sample.ioWaitTicks != 5 || sample.stealTicks != 8 {
		t.Errorf("columns = iowait %d / steal %d, want 5 / 8", sample.ioWaitTicks, sample.stealTicks)
	}
}

// TestProcStatShortLineIsNotRead: a kernel whose aggregate line stops before
// the steal column cannot answer the question this block exists for, so it
// contributes nothing rather than a total with a silently missing member.
func TestProcStatShortLineIsNotRead(t *testing.T) {
	t.Parallel()
	var sample environmentSample
	root := fixtureRunner(t, map[string]string{"proc/stat": "cpu  1 2 3 4\n"})
	readHostCPUTime(root, &sample)
	if sample.hasHostCPU {
		t.Errorf("a /proc/stat line without a steal column was read as %+v", sample)
	}
}

func TestParseCPUMax(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
		period  int64
		quota   int64
		ok      bool
	}{
		{name: "quota", content: "800000 100000\n", period: 100000, quota: 800000, ok: true},
		{name: "unlimited", content: "max 100000\n", period: 100000, ok: true},
		{name: "empty", content: ""},
		{name: "one field", content: "800000\n"},
		{name: "unparseable period", content: "800000 wat\n"},
		// A quota the kernel would never write; the period is still usable, and
		// the quota degrades to unlimited rather than to a nonsense ceiling.
		{name: "zero quota", content: "0 100000\n", period: 100000, ok: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			period, quota, ok := parseCPUMax(testCase.content)
			if ok != testCase.ok || period != testCase.period || quota != testCase.quota {
				t.Errorf("parseCPUMax(%q) = %d, %d, %v; want %d, %d, %v",
					testCase.content, period, quota, ok, testCase.period, testCase.quota, testCase.ok)
			}
		})
	}
}

func TestParseCgroupV1CPUQuota(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		quota  string
		period string
		wantP  int64
		wantQ  int64
		wantOK bool
	}{
		{name: "quota", quota: "800000\n", period: "100000\n", wantP: 100000, wantQ: 800000, wantOK: true},
		{name: "unlimited", quota: "-1\n", period: "100000\n", wantP: 100000, wantOK: true},
		{name: "missing quota", period: "100000\n"},
		{name: "invalid period", quota: "800000\n", period: "0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			period, quota, ok := parseCgroupV1CPUQuota(tc.quota, tc.period)
			if period != tc.wantP || quota != tc.wantQ || ok != tc.wantOK {
				t.Errorf("parseCgroupV1CPUQuota(%q, %q) = %d, %d, %v; want %d, %d, %v",
					tc.quota, tc.period, period, quota, ok, tc.wantP, tc.wantQ, tc.wantOK)
			}
		})
	}
}

// TestReadCPUPressureTakesOnlyTheSomeTotal: the avg columns are decaying
// averages over windows that are not this session's, so the capture reads the
// cumulative total and nothing else — and ignores the `full` line, which is not
// the CPU-pressure signal.
func TestReadCPUPressureTakesOnlyTheSomeTotal(t *testing.T) {
	t.Parallel()
	var sample environmentSample
	root := fixtureRunner(t, map[string]string{
		"proc/pressure/cpu": "some avg10=1.00 avg60=2.00 avg300=3.00 total=98765\nfull avg10=0.00 total=11111\n",
	})
	readCPUPressure(root, &sample)
	if !sample.hasPressure || sample.someStalledUs != 98765 {
		t.Errorf("psi = %d (has=%v), want the some-line total 98765", sample.someStalledUs, sample.hasPressure)
	}
}

// TestAllocatedMillicoresOnNoEnvironment: a nil environment states no
// allocation at all, which is what makes the report layer omit the CPU balance
// rather than divide by a guess.
func TestAllocatedMillicoresOnNoEnvironment(t *testing.T) {
	t.Parallel()
	var environment *RunnerEnvironment
	if millicores, fromQuota := environment.AllocatedMillicores(); millicores != 0 || fromQuota {
		t.Errorf("nil environment reported %d millicores (quota=%v)", millicores, fromQuota)
	}
}

// TestCPUBandwidthCores: the scheduler sizes its CPU grants from this, so an
// unbounded host must report 0 (keep the core count) and a quota must round
// down to whole CPUs without ever reaching 0 — half a CPU still runs jobs.
func TestCPUBandwidthCores(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		sample environmentSample
		want   int
	}{
		{"no cgroup", environmentSample{}, 0},
		{"cpu.max is max", environmentSample{hasCgroup: true, periodUs: 100_000, quotaUs: 0}, 0},
		{"two cpus", environmentSample{hasCgroup: true, periodUs: 100_000, quotaUs: 200_000}, 2},
		{"two and a half cpus", environmentSample{hasCgroup: true, periodUs: 100_000, quotaUs: 250_000}, 2},
		{"half a cpu floors at one", environmentSample{hasCgroup: true, periodUs: 100_000, quotaUs: 50_000}, 1},
		{"zero period is unusable", environmentSample{hasCgroup: true, periodUs: 0, quotaUs: 200_000}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sample.cpuBandwidthCores(); got != tc.want {
				t.Errorf("cpuBandwidthCores() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPrepareScheduling_ClampsAllocatorToCapturedCgroupQuota(t *testing.T) {
	t.Parallel()
	scheduler := &Scheduler{ws: &workspace.Workspace{Root: t.TempDir()}}
	scheduler.environmentBegin = environmentSample{
		hasCgroup: true,
		periodUs:  100_000,
		quotaUs:   250_000,
		memoryCapacity: memoryCapacityReading{
			physicalBytes: 8 << 30,
			hasPhysical:   true,
			cgroupBytes:   2 << 30,
			hasCgroup:     true,
			cgroupSource:  CgroupMemoryV2,
		},
	}
	memoryCapacity, _, ok := scheduler.environmentBegin.memoryCapacity.effectiveBytes()
	if !ok {
		t.Fatal("opening memory capacity did not resolve")
	}
	decision := resolveParallelDecisionWithHardware(SchedulerConfig{}, nil, 12, memoryCapacity)
	scheduler.prepareScheduling(decision)

	if got := scheduler.cpuAlloc.totalCPU; got != 2 {
		t.Fatalf("allocator capacity = %d, want captured quota floor 2 (not logical CPUs or GOMAXPROCS)", got)
	}
	if got := scheduler.resources.cpuCapacity; got != 2 {
		t.Fatalf("admission capacity = %d, want the same captured quota floor 2", got)
	}
	if decision.MemoryTotalMiB != 2048 || decision.MemoryUsableMiB != 1536 {
		t.Fatalf("memory decision = total %d / usable %d MiB, want 2048 / 1536",
			decision.MemoryTotalMiB, decision.MemoryUsableMiB)
	}
	if got := scheduler.resources.memoryCapacity; got != 1536*1024*1024 {
		t.Fatalf("memory admission = %d, want the unchanged 75%% of opening 2 GiB", got)
	}
}

// TestCaptureEnvironmentOnTheRealRoot is the smoke test criterion 3 needs on
// every platform: pointed at the machine actually running the suite, capture
// must not panic and must produce the host facts, whatever the kernel exposes.
func TestCaptureEnvironmentOnTheRealRoot(t *testing.T) {
	t.Parallel()
	begin := captureEnvironmentSample(runnerEnvironmentRoot)
	environment := resolveRunnerEnvironment(runnerEnvironmentRoot, begin, captureEnvironmentSample(runnerEnvironmentRoot))
	if environment.OS != runtime.GOOS || environment.Arch != runtime.GOARCH {
		t.Errorf("host facts = %s/%s, want the running platform", environment.OS, environment.Arch)
	}
	if environment.LogicalCPUs < 1 {
		t.Errorf("logical CPUs = %d", environment.LogicalCPUs)
	}
	if environment.Window < 0 {
		t.Errorf("window = %v", environment.Window)
	}
	if runtime.GOOS == "darwin" && (environment.Cgroup != nil || environment.CPUPressure != nil || environment.HostCPU != nil) {
		t.Errorf("darwin reported a Linux-only block: %+v", environment)
	}
}
