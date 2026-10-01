package jobs

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The RUNNER, and how much of it a session actually got.
//
// An earlier stage made physical cost countable — every subprocess
// listed once with its measured CPU. This file makes that number
// INTERPRETABLE, because a CPU total on its own answers no question worth
// asking. The baseline measured 120.9 allocated vCPU-min against 80.7 actual, an
// average of 5.34 of 8 cores, and there are three different explanations for
// that gap with three different fixes:
//
//   - the plan had nothing else to run (a SCHEDULING problem);
//   - the cgroup quota suspended the runner mid-period (a QUOTA problem, which
//     more parallelism makes worse, not better);
//   - the host was oversubscribed and gave our ticks to a neighbor (a PLACEMENT
//     problem we cannot fix by changing the build at all).
//
// Timings cannot tell them apart. `cpu.stat`'s throttle counters, PSI, and
// /proc/stat's steal column can, and nothing in this repository read any of
// them before this slice.
//
// Two rules govern everything below.
//
// ABSENT IS NOT ZERO. macOS has no cgroups and no PSI. Recording zeros there
// would claim "measured: never throttled" for a machine that cannot throttle,
// and every later comparison against a Linux runner would be reading a fiction.
// A file that does not exist yields an absent block, full stop — the probes are
// therefore NOT gated on GOOS: they attempt the read and let the filesystem
// answer, which is the same code path that must already handle a Linux kernel
// built without PSI.
//
// COUNTERS ARE WINDOWED. Every kernel counter here is cumulative since boot or
// since the cgroup was created, so its raw value says nothing about this
// session. Each is sampled twice — once when the run begins, once when it ends
// — and only the DELTA is reported, alongside the wall between the two samples
// (RunnerEnvironment.Window) that the delta must be read against. A delta that
// comes out negative means the counter was reset or the two samples came from
// different cgroups; that block is dropped rather than clamped, because a
// clamped counter is indistinguishable from a real zero.

// runnerEnvironmentRoot is the filesystem the probes read. It is a parameter
// rather than a constant baked into each path so the tests can point the whole
// capture at a fixture tree containing exactly the files a given runner has —
// which is the only way to test the DEGRADATION paths on a developer machine
// that has none of them.
const runnerEnvironmentRoot = "/"

// cpuBandwidthCores is how many whole CPUs this sample's cgroup permits, or 0
// when nothing bounds the process below the core count.
//
// It answers the only question the scheduler needs before it starts handing out
// grants — how much parallelism the host will actually deliver — from the same
// cgroup bandwidth reading the session reports as CPUAllocationCgroupQuota.
// Deriving it from the runtime instead is a trap: putnami exports GOMAXPROCS to
// its own child jobs, so inside a nested run (putnami testing putnami)
// runtime.GOMAXPROCS(0) reports the PARENT's grant rather than machine
// capacity, exactly the contamination applyCPUBudget documents for
// PUTNAMI_CPU_BUDGET.
//
// A fractional quota rounds down but never to zero: half a CPU still runs jobs,
// one at a time.
func (s environmentSample) cpuBandwidthCores() int {
	if !s.hasCgroup || s.quotaUs <= 0 || s.periodUs <= 0 {
		return 0
	}
	return max(int(s.quotaUs/s.periodUs), 1)
}

// environmentSample is one reading of the runner's cumulative counters. Each
// block carries its own presence flag, because "the file was not there" and
// "the file said zero" are different facts and only one of them is publishable.
type environmentSample struct {
	at time.Time

	hasCgroup   bool
	periodUs    int64
	quotaUs     int64
	hasUsage    bool
	usageUs     int64
	hasThrottle bool
	periods     int64
	throttled   int64
	throttledUs int64

	hasPressure   bool
	someStalledUs int64

	hasHostCPU  bool
	stealTicks  int64
	ioWaitTicks int64
	totalTicks  int64

	memoryCapacity memoryCapacityReading
	memory         memorySample
	memoryPressure memoryPressureSample
}

// captureEnvironmentSample reads every counter the runner exposes, once.
//
// It never fails and never blocks on anything but a procfs/sysfs read. A path
// that does not exist leaves its block's presence flag false, which is what
// carries "this platform cannot measure this" all the way to an absent member
// on the wire.
func captureEnvironmentSample(root string) environmentSample {
	sample := environmentSample{at: time.Now()}
	readCgroupCPU(root, &sample)
	readCPUPressure(root, &sample)
	readHostCPUTime(root, &sample)
	readMemoryEnvironment(root, &sample)
	return sample
}

// resolveRunnerEnvironment folds an opening and a closing sample into the
// session's environment: static facts as they are, cumulative counters as
// deltas over the window between the two.
//
// A block is published only when BOTH samples carried it and the delta is
// non-negative. A counter that went backwards means the samples do not describe
// one window — the cgroup was replaced, or the kernel reset the counter — and
// reporting the clamped difference would present a broken measurement as a
// legitimate zero.
func resolveRunnerEnvironment(root string, begin, end environmentSample) RunnerEnvironment {
	environment := RunnerEnvironment{
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		LogicalCPUs: runtime.NumCPU(),
		CPUModel:    hostCPUModel(root),
		Window:      max(end.at.Sub(begin.at), 0),
	}
	environment.Cgroup = resolveCgroupCPU(begin, end)
	environment.CPUPressure = resolveCPUPressure(begin, end)
	environment.HostCPU = resolveHostCPUTime(begin, end)
	environment.MemoryCapacity = resolveMemoryCapacity(begin.memoryCapacity)
	environment.CgroupMemory = resolveCgroupMemory(begin.memory, end.memory)
	environment.MemoryPressure = resolveMemoryPressure(begin.memoryPressure, end.memoryPressure, environment.CgroupMemory)
	return environment
}

// resolveCgroupCPU windows the cgroup counters. The quota and period are read
// from the CLOSING sample: they are configuration, not counters, and the
// closing value is the one that was in force for the tail of the run — but a
// mid-run change is rare enough that either would do, and taking one of them
// deliberately avoids publishing two numbers a reader would have to reconcile.
func resolveCgroupCPU(begin, end environmentSample) *CgroupCPU {
	if !begin.hasCgroup || !end.hasCgroup || end.periodUs <= 0 {
		return nil
	}
	cgroup := &CgroupCPU{PeriodUs: end.periodUs, QuotaUs: max(end.quotaUs, 0)}
	if begin.hasUsage && end.hasUsage {
		if delta, ok := windowDelta(begin.usageUs, end.usageUs); ok {
			cgroup.UsageUs, cgroup.HasUsage = delta, true
		}
	}
	if begin.hasThrottle && end.hasThrottle {
		periods, periodsOK := windowDelta(begin.periods, end.periods)
		throttled, throttledOK := windowDelta(begin.throttled, end.throttled)
		throttledUs, usOK := windowDelta(begin.throttledUs, end.throttledUs)
		// A throttled-period count above the period count would mean the two
		// members came from different windows; the pair is dropped rather than
		// published as an impossible ratio.
		if periodsOK && throttledOK && usOK && throttled <= periods {
			cgroup.Throttle = &CgroupThrottle{
				Periods:          periods,
				ThrottledPeriods: throttled,
				ThrottledUs:      throttledUs,
			}
		}
	}
	return cgroup
}

func resolveCPUPressure(begin, end environmentSample) *CPUPressure {
	if !begin.hasPressure || !end.hasPressure {
		return nil
	}
	stalled, ok := windowDelta(begin.someStalledUs, end.someStalledUs)
	if !ok {
		return nil
	}
	return &CPUPressure{SomeStalledUs: stalled}
}

// resolveHostCPUTime windows the /proc/stat aggregate. Steal and iowait are
// COLUMNS of the same total, so a delta where either exceeds the total is not a
// runner fact but a sampling error, and the block is dropped.
func resolveHostCPUTime(begin, end environmentSample) *HostCPUTime {
	if !begin.hasHostCPU || !end.hasHostCPU {
		return nil
	}
	steal, stealOK := windowDelta(begin.stealTicks, end.stealTicks)
	ioWait, ioWaitOK := windowDelta(begin.ioWaitTicks, end.ioWaitTicks)
	total, totalOK := windowDelta(begin.totalTicks, end.totalTicks)
	if !stealOK || !ioWaitOK || !totalOK || steal > total || ioWait > total {
		return nil
	}
	return &HostCPUTime{StealTicks: steal, IOWaitTicks: ioWait, TotalTicks: total}
}

// windowDelta subtracts two readings of one cumulative counter. A negative
// result is reported as unusable rather than clamped: the counter was reset or
// the samples came from different sources, and a clamped zero would be
// indistinguishable from a measured one.
func windowDelta(begin, end int64) (int64, bool) {
	if end < begin {
		return 0, false
	}
	return end - begin, true
}

// readCgroupCPU reads the CPU controller from the cgroup this process is in.
//
// cgroup v2 is preferred when its cpu.max is readable. A v1-only runner falls
// back to the exact cpu and cpuacct controller paths: production gVisor returns
// an empty cgroup-root directory listing even though those direct reads work, so
// controller discovery must never depend on listing the mount — nor on the
// membership directory /proc/self/cgroup advertises actually existing, which is
// the second way that substrate misreports its own topology.
func readCgroupCPU(root string, sample *environmentSample) {
	readCgroupCPUFrom(root, sample, readFile)
}

// readCgroupCPUFrom carries the filesystem read as a parameter so tests can
// model gVisor faithfully: exact paths have content while directory discovery
// has no surface at all.
func readCgroupCPUFrom(root string, sample *environmentSample, read func(string) string) {
	if readCgroupV2CPU(root, sample, read) {
		return
	}
	readCgroupV1CPU(root, sample, read)
}

func readCgroupV2CPU(root string, sample *environmentSample, read func(string) string) bool {
	mount := filepath.Join(root, "sys/fs/cgroup")
	candidates := make([]string, 0, 2)
	if relative := unifiedCgroupPath(read(filepath.Join(root, "proc/self/cgroup"))); relative != "" {
		candidates = append(candidates, filepath.Join(mount, filepath.FromSlash(relative)))
	}
	candidates = append(candidates, mount)
	for _, dir := range candidates {
		period, quota, ok := parseCPUMax(read(filepath.Join(dir, "cpu.max")))
		if !ok {
			continue
		}
		sample.hasCgroup, sample.periodUs, sample.quotaUs = true, period, quota

		stat := parseKeyedCounters(read(filepath.Join(dir, "cpu.stat")))
		if usage, present := stat["usage_usec"]; present {
			sample.hasUsage, sample.usageUs = true, usage
		}
		periods, hasPeriods := stat["nr_periods"]
		throttled, hasThrottled := stat["nr_throttled"]
		throttledUs, hasThrottledUs := stat["throttled_usec"]
		if hasPeriods && hasThrottled && hasThrottledUs {
			sample.hasThrottle = true
			sample.periods, sample.throttled, sample.throttledUs = periods, throttled, throttledUs
		}
		return true
	}
	return false
}

type cgroupV1Membership struct {
	path      string
	hierarchy string
}

func readCgroupV1CPU(root string, sample *environmentSample, read func(string) string) {
	memberships := cgroupV1Memberships(read(filepath.Join(root, "proc/self/cgroup")))
	cpuMembership, ok := memberships["cpu"]
	if !ok {
		return
	}
	mountInfo := read(filepath.Join(root, "proc/self/mountinfo"))

	var period, quota int64
	hasQuota := false
	for _, dir := range cgroupV1ControllerDirs(root, "cpu", cpuMembership, mountInfo) {
		period, quota, ok = parseCgroupV1CPUQuota(
			read(filepath.Join(dir, "cpu.cfs_quota_us")),
			read(filepath.Join(dir, "cpu.cfs_period_us")),
		)
		if ok {
			hasQuota = true
			break
		}
	}
	if !hasQuota {
		return
	}
	sample.hasCgroup, sample.periodUs, sample.quotaUs = true, period, quota

	cpuacctMembership, ok := memberships["cpuacct"]
	if !ok {
		return
	}
	for _, dir := range cgroupV1ControllerDirs(root, "cpuacct", cpuacctMembership, mountInfo) {
		usageNs, err := strconv.ParseInt(strings.TrimSpace(read(filepath.Join(dir, "cpuacct.usage"))), 10, 64)
		if err != nil || usageNs < 0 {
			continue
		}
		sample.hasUsage, sample.usageUs = true, usageNs/1000
		break
	}
}

// cgroupV1Memberships maps each scheduler-relevant controller to this process's
// directory in that hierarchy. The third /proc/self/cgroup field is relative to the
// hierarchy mount point, so reading the mount root for a non-root membership
// would report a parent quota and size the scheduler incorrectly.
func cgroupV1Memberships(content string) map[string]cgroupV1Membership {
	memberships := make(map[string]cgroupV1Membership)
	for _, line := range strings.Split(content, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
			continue
		}
		memberPath := path.Clean("/" + strings.TrimPrefix(strings.TrimSpace(parts[2]), "/"))
		hierarchy := strings.TrimSpace(parts[1])
		for _, controller := range strings.Split(hierarchy, ",") {
			controller = strings.TrimSpace(controller)
			if controller != "cpu" && controller != "cpuacct" && controller != "memory" {
				continue
			}
			memberships[controller] = cgroupV1Membership{
				path:      memberPath,
				hierarchy: hierarchy,
			}
		}
	}
	return memberships
}

// cgroupV1ControllerDirs resolves exact directories without listing the cgroup
// root. mountinfo is authoritative when it names the controller; the fixed
// mount names preserve the gVisor fallback where mountinfo may be unavailable
// and direct lookups work despite an empty directory listing.
//
// Two independent things can make a membership path unjoinable, and they need
// opposite treatments.
//
// The mount may expose only a SUBTREE of the hierarchy. /proc/self/cgroup always
// states the membership in the hierarchy's own namespace, so a container whose
// cgroupfs is attached at /docker/abc would send a naive join looking for
// <mount>/docker/abc/... under a mount that already IS /docker/abc. mountinfo
// names the attached directory, so the membership is REBASED onto it — and a
// mount exposing a cgroup that is not an ancestor of ours is dropped, since no
// path under it can name this process's cgroup.
//
// Or the runtime may advertise a membership directory it never created. gVisor
// is the production case: it names `/container0` in every hierarchy, creates
// none of them, and leaves the real quota at the controller root. That one is
// answered by falling back to the mount point — but ONLY for a mount attached at
// the hierarchy root, and only after every membership candidate on every mount
// has missed. A mount attached deeper already IS a specific cgroup, so reading
// it after a failed rebase would publish an ANCESTOR's quota under this
// process's name, overstating capacity and oversizing the scheduler's grant.
// An absent block beats a confident wrong one.
func cgroupV1ControllerDirs(
	root string,
	controller string,
	membership cgroupV1Membership,
	mountInfo string,
) []string {
	memberPath := path.Clean("/" + strings.TrimPrefix(membership.path, "/"))

	if mounts := cgroupV1ControllerMounts(mountInfo, controller); len(mounts) > 0 {
		dirs := make([]string, 0, 2*len(mounts))
		for _, mount := range mounts {
			if relative, ok := cgroupPathUnder(mount.root, memberPath); ok {
				dirs = appendUniqueString(
					dirs,
					filepath.Join(rootedProcPath(root, mount.point), filepath.FromSlash(relative)),
				)
			}
		}
		for _, mount := range mounts {
			if path.Clean("/"+strings.TrimPrefix(mount.root, "/")) == "/" {
				dirs = appendUniqueString(dirs, rootedProcPath(root, mount.point))
			}
		}
		return dirs
	}

	// No mountinfo evidence at all: the fixed mount names, with nothing to
	// rebase against. The mount points are assumed to be hierarchy roots, which
	// is what /sys/fs/cgroup/<controller> is on every runner that gets here.
	var rootedMounts []string
	for _, name := range []string{membership.hierarchy, controller, "cpu,cpuacct", "cpuacct,cpu"} {
		if name == "" {
			continue
		}
		rootedMounts = appendUniqueString(
			rootedMounts,
			filepath.Join(root, "sys/fs/cgroup", filepath.FromSlash(name)),
		)
	}

	dirs := make([]string, 0, 2*len(rootedMounts))
	relative := strings.TrimPrefix(memberPath, "/")
	for _, mountPoint := range rootedMounts {
		dirs = appendUniqueString(dirs, filepath.Join(mountPoint, filepath.FromSlash(relative)))
	}
	for _, mountPoint := range rootedMounts {
		dirs = appendUniqueString(dirs, mountPoint)
	}
	return dirs
}

// cgroupPathUnder rebases a /proc/self/cgroup membership onto a mount that
// exposes `mountRoot`, returning the remainder RELATIVE to the mount point.
//
// The second result is false when the membership is not inside the mounted
// subtree — a sibling container's mount, say — which makes that mount unusable
// for this process rather than merely inexact.
func cgroupPathUnder(mountRoot, memberPath string) (string, bool) {
	mountRoot = path.Clean("/" + strings.TrimPrefix(mountRoot, "/"))
	if mountRoot == "/" {
		return strings.TrimPrefix(memberPath, "/"), true
	}
	if memberPath == mountRoot {
		return "", true
	}
	return strings.CutPrefix(memberPath, mountRoot+"/")
}

// cgroupV1Mount is one cgroup v1 hierarchy mount: where it is attached, and
// which cgroup directory was attached there.
type cgroupV1Mount struct {
	// point is the mount point, as an absolute path in this process's view.
	point string
	// root is the directory within the hierarchy that the mount exposes. "/" is
	// the whole hierarchy; anything deeper means the mount point IS that cgroup.
	root string
}

func cgroupV1ControllerMounts(content, controller string) []cgroupV1Mount {
	var mounts []cgroupV1Mount
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 6 || separator+3 >= len(fields) || fields[separator+1] != "cgroup" {
			continue
		}
		options := fields[separator+2] + "," + fields[separator+3]
		if !commaListContains(options, controller) {
			continue
		}
		mount := cgroupV1Mount{
			point: unescapeMountInfoPath(fields[4]),
			root:  unescapeMountInfoPath(fields[3]),
		}
		if !strings.HasPrefix(mount.point, "/") || !strings.HasPrefix(mount.root, "/") {
			continue
		}
		if !slices.Contains(mounts, mount) {
			mounts = append(mounts, mount)
		}
	}
	return mounts
}

func commaListContains(content, wanted string) bool {
	for _, value := range strings.Split(content, ",") {
		if value == wanted {
			return true
		}
	}
	return false
}

func rootedProcPath(root, absolute string) string {
	relative := strings.TrimPrefix(path.Clean(absolute), "/")
	return filepath.Join(root, filepath.FromSlash(relative))
}

func unescapeMountInfoPath(value string) string {
	return strings.NewReplacer(
		"\\040", " ",
		"\\011", "\t",
		"\\012", "\n",
		"\\134", "\\",
	).Replace(value)
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// unifiedCgroupPath extracts the cgroup v2 ("0::") path from /proc/self/cgroup.
// A v1-only host has no such line and yields "".
func unifiedCgroupPath(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if after, found := strings.CutPrefix(strings.TrimSpace(line), "0::"); found {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// parseCPUMax reads cgroup v2's `cpu.max`: "<quota|max> <period>". A quota of
// "max" is UNLIMITED and comes back as 0, which the caller publishes by
// omitting the member rather than by writing a ceiling of nothing.
func parseCPUMax(content string) (periodUs, quotaUs int64, ok bool) {
	fields := strings.Fields(content)
	if len(fields) < 2 {
		return 0, 0, false
	}
	period, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || period <= 0 {
		return 0, 0, false
	}
	if fields[0] == "max" {
		return period, 0, true
	}
	quota, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || quota <= 0 {
		return period, 0, true
	}
	return period, quota, true
}

// parseCgroupV1CPUQuota reads cgroup v1's separate quota and period files.
// Linux spells an unlimited v1 quota as -1; the internal zero value carries the
// same meaning as cpu.max's "max" and is omitted by the projection.
func parseCgroupV1CPUQuota(quotaContent, periodContent string) (periodUs, quotaUs int64, ok bool) {
	period, err := strconv.ParseInt(strings.TrimSpace(periodContent), 10, 64)
	if err != nil || period <= 0 {
		return 0, 0, false
	}
	quota, err := strconv.ParseInt(strings.TrimSpace(quotaContent), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return period, max(quota, 0), true
}

// parseKeyedCounters reads the "<name> <value>" line format cgroup stat files
// use. Unparseable lines are skipped: a kernel that adds a member this CLI does
// not know must not cost it the members it does.
func parseKeyedCounters(content string) map[string]int64 {
	counters := make(map[string]int64)
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || value < 0 {
			continue
		}
		counters[fields[0]] = value
	}
	return counters
}

// readCPUPressure reads PSI's cumulative CPU stall total.
//
// Only the `some` line's `total=` is taken. The avg10/avg60/avg300 columns on
// the same line are decaying averages over windows that are not this session's,
// so nothing else in the recorded document could be reconciled against them;
// the cumulative total, delta'd over the run, can.
func readCPUPressure(root string, sample *environmentSample) {
	for _, line := range strings.Split(readFile(filepath.Join(root, "proc/pressure/cpu")), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		for _, field := range fields[1:] {
			value, found := strings.CutPrefix(field, "total=")
			if !found {
				continue
			}
			stalled, err := strconv.ParseInt(value, 10, 64)
			if err != nil || stalled < 0 {
				return
			}
			sample.hasPressure, sample.someStalledUs = true, stalled
			return
		}
	}
}

// procStatColumns is the aggregate "cpu" line's column layout, up to and
// including the one this slice exists for.
const (
	procStatIOWait = 4
	procStatSteal  = 7
	// procStatSummed bounds the columns folded into the total. It stops after
	// steal on purpose: `guest` and `guest_nice` follow, and the kernel already
	// counts them inside `user` and `nice`, so summing them would inflate the
	// denominator every steal ratio is read against.
	procStatSummed = 8
)

// readHostCPUTime reads the /proc/stat aggregate line — the hypervisor's view
// of this machine, whose `steal` column is the only place a shared host admits
// it took our ticks away.
func readHostCPUTime(root string, sample *environmentSample) {
	for _, line := range strings.Split(readFile(filepath.Join(root, "proc/stat")), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "cpu" {
			continue
		}
		values := fields[1:]
		if len(values) <= procStatSteal {
			return
		}
		var total int64
		for i := 0; i < procStatSummed && i < len(values); i++ {
			ticks, err := strconv.ParseInt(values[i], 10, 64)
			if err != nil || ticks < 0 {
				return
			}
			total += ticks
			switch i {
			case procStatIOWait:
				sample.ioWaitTicks = ticks
			case procStatSteal:
				sample.stealTicks = ticks
			}
		}
		// gVisor exposes a syntactically complete but all-zero aggregate. It
		// cannot measure host steal, so publishing a zero block would claim a
		// reassuring measurement that the substrate never made.
		if total <= 0 {
			return
		}
		sample.hasHostCPU, sample.totalTicks = true, total
		return
	}
}

// hostCPUModel is the processor's brand string, or "" where the platform
// publishes none. It is the one static fact with no portable source, so it has
// one reader per platform and no fabricated fallback: an unknown model is
// absent, not "unknown".
func hostCPUModel(root string) string {
	if model := procCPUInfoModel(readFile(filepath.Join(root, "proc/cpuinfo"))); model != "" {
		return model
	}
	// sysctl is consulted only for the REAL filesystem: a test pointing the
	// capture at a fixture tree is describing a machine, and must not have the
	// developer's own CPU leak into it.
	if root == runnerEnvironmentRoot && runtime.GOOS == "darwin" {
		out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	return ""
}

// procCPUInfoModel reads the first processor's brand string. x86 spells it
// "model name"; arm64 kernels usually publish no brand at all, and "" is the
// correct answer there rather than a synthesized one.
func procCPUInfoModel(content string) string {
	for _, line := range strings.Split(content, "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "model name", "hardware":
			if model := strings.TrimSpace(value); model != "" {
				return model
			}
		}
	}
	return ""
}

// readFile is the probes' only filesystem access: a missing, unreadable or
// permission-denied file is empty content, never an error to propagate. Nothing
// this package measures is worth failing a build over.
func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
