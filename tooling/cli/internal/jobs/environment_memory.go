package jobs

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	maxSafeJSONInteger        = uint64((1 << 53) - 1)
	cgroupV1UnlimitedSentinel = uint64(0x7ffffffffffff000)
)

// memoryCapacityReading keeps the scheduler's full-width inputs separate from
// the safe-integer public model. Admission has always used uint64 bytes; a
// substrate value that cannot be published safely must not silently change it.
type memoryCapacityReading struct {
	physicalBytes uint64
	hasPhysical   bool
	cgroupBytes   uint64
	hasCgroup     bool
	cgroupSource  string
}

func (r memoryCapacityReading) effectiveBytes() (uint64, string, bool) {
	if !r.hasPhysical && !r.hasCgroup {
		return 0, "", false
	}
	if r.hasPhysical && (!r.hasCgroup || r.physicalBytes <= r.cgroupBytes) {
		return r.physicalBytes, MemoryCapacityPhysical, true
	}
	return r.cgroupBytes, MemoryCapacityCgroupLimit, true
}

type memorySample struct {
	present  bool
	source   string
	identity string
	current  int64

	composition *CgroupMemoryComposition
	peak        *CgroupMemoryLifetimePeak
	events      *CgroupMemoryEvents
}

type memoryPressureSample struct {
	present  bool
	scope    string
	identity string
	some     int64
	full     int64
}

func readMemoryEnvironment(root string, sample *environmentSample) {
	if physical := totalMemoryBytesAt(root, readFile); physical > 0 {
		sample.memoryCapacity.physicalBytes = physical
		sample.memoryCapacity.hasPhysical = true
	}
	readCgroupMemoryFrom(root, sample, readFile)
	if !sample.memoryPressure.present {
		readHostMemoryPressureFrom(root, sample, readFile)
	}
}

// readCgroupMemoryFrom performs bounded direct reads only. It resolves at most
// one v2 or v1 directory and never walks or lists the cgroup tree.
func readCgroupMemoryFrom(root string, sample *environmentSample, read func(string) string) {
	cgroup := read(filepath.Join(root, "proc/self/cgroup"))
	if readCgroupV2Memory(root, cgroup, sample, read) {
		return
	}
	readCgroupV1Memory(root, cgroup, sample, read)
}

func readCgroupV2Memory(
	root string,
	cgroup string,
	sample *environmentSample,
	read func(string) string,
) bool {
	mount := filepath.Join(root, "sys/fs/cgroup")
	var dirs []string
	if relative := unifiedCgroupPath(cgroup); relative != "" {
		dirs = appendUniqueString(dirs, filepath.Join(mount, filepath.FromSlash(relative)))
	}
	dirs = appendUniqueString(dirs, mount)

	for _, dir := range dirs {
		currentContent := read(filepath.Join(dir, "memory.current"))
		limitContent := read(filepath.Join(dir, "memory.max"))
		// Non-empty content proves this exact cgroup exposes the v2 memory
		// controller. Malformed content must degrade here, not fall through to
		// an ancestor root or an unrelated v1 hierarchy.
		if currentContent == "" && limitContent == "" {
			continue
		}

		if current, ok := parseSafeMemoryValue(currentContent); ok {
			sample.memory = memorySample{
				present:  true,
				source:   CgroupMemoryV2,
				identity: filepath.Clean(dir),
				current:  current,
			}
			if peak, peakOK := parseSafeMemoryValue(read(filepath.Join(dir, "memory.peak"))); peakOK {
				sample.memory.peak = &CgroupMemoryLifetimePeak{Bytes: peak}
			}
			if stat, statOK := parseCompleteCounters(
				read(filepath.Join(dir, "memory.stat")), []string{"anon", "file", "shmem"},
			); statOK && stat["shmem"] <= stat["file"] {
				sample.memory.composition = &CgroupMemoryComposition{
					AnonBytes: stat["anon"], FileBytes: stat["file"], ShmemBytes: stat["shmem"],
				}
			}
			if events, eventsOK := parseCompleteCounters(
				read(filepath.Join(dir, "memory.events")), []string{"low", "high", "max", "oom", "oom_kill"},
			); eventsOK {
				sample.memory.events = &CgroupMemoryEvents{
					Low: events["low"], High: events["high"], Max: events["max"],
					OOM: events["oom"], OOMKill: events["oom_kill"],
				}
			}
		}

		if limit, finite := parseFiniteMemoryLimit(limitContent, CgroupMemoryV2); finite {
			sample.memoryCapacity.cgroupBytes = limit
			sample.memoryCapacity.hasCgroup = true
			sample.memoryCapacity.cgroupSource = CgroupMemoryV2
		} else if value := strings.TrimSpace(limitContent); filepath.Clean(dir) != filepath.Clean(mount) &&
			(value == "" || value == "max") {
			// memory.max is hierarchical. Preserve the leaf gauges and identity,
			// but retain the scheduler's historical mount-root fallback when the
			// leaf delegates its effective ceiling to an ancestor. A malformed
			// leaf limit deliberately does not enter this branch.
			if limit, rootFinite := parseFiniteMemoryLimit(
				read(filepath.Join(mount, "memory.max")), CgroupMemoryV2,
			); rootFinite {
				sample.memoryCapacity.cgroupBytes = limit
				sample.memoryCapacity.hasCgroup = true
				sample.memoryCapacity.cgroupSource = CgroupMemoryV2
			}
		}

		if some, full, ok := parseMemoryPressure(read(filepath.Join(dir, "memory.pressure"))); sample.memory.present && ok {
			sample.memoryPressure = memoryPressureSample{
				present: true, scope: MemoryPressureCgroup,
				identity: filepath.Clean(dir), some: some, full: full,
			}
		}
		return true
	}
	return false
}

func readCgroupV1Memory(
	root string,
	cgroup string,
	sample *environmentSample,
	read func(string) string,
) {
	membership, ok := cgroupV1Memberships(cgroup)["memory"]
	if !ok {
		return
	}
	mountInfo := read(filepath.Join(root, "proc/self/mountinfo"))
	for _, dir := range cgroupV1ControllerDirs(root, "memory", membership, mountInfo) {
		currentContent := read(filepath.Join(dir, "memory.usage_in_bytes"))
		limitContent := read(filepath.Join(dir, "memory.limit_in_bytes"))
		if currentContent == "" && limitContent == "" {
			continue
		}

		if current, currentOK := parseSafeMemoryValue(currentContent); currentOK {
			sample.memory = memorySample{
				present:  true,
				source:   CgroupMemoryV1,
				identity: filepath.Clean(dir),
				current:  current,
			}
			if peak, peakOK := parseSafeMemoryValue(
				read(filepath.Join(dir, "memory.max_usage_in_bytes")),
			); peakOK {
				sample.memory.peak = &CgroupMemoryLifetimePeak{Bytes: peak}
			}
		}
		if limit, finite := parseFiniteMemoryLimit(limitContent, CgroupMemoryV1); finite {
			sample.memoryCapacity.cgroupBytes = limit
			sample.memoryCapacity.hasCgroup = true
			sample.memoryCapacity.cgroupSource = CgroupMemoryV1
		}
		return
	}
}

func readHostMemoryPressureFrom(root string, sample *environmentSample, read func(string) string) {
	if some, full, ok := parseMemoryPressure(read(filepath.Join(root, "proc/pressure/memory"))); ok {
		sample.memoryPressure = memoryPressureSample{
			present: true, scope: MemoryPressureHost, identity: "host", some: some, full: full,
		}
	}
}

func resolveMemoryCapacity(reading memoryCapacityReading) *MemoryCapacity {
	effective, source, ok := reading.effectiveBytes()
	if !ok || effective == 0 || effective > maxSafeJSONInteger {
		return nil
	}
	capacity := &MemoryCapacity{EffectiveBytes: int64(effective), EffectiveSource: source}
	if reading.hasPhysical && reading.physicalBytes <= maxSafeJSONInteger {
		capacity.PhysicalBytes = int64(reading.physicalBytes)
	}
	if reading.hasCgroup && reading.cgroupBytes <= maxSafeJSONInteger {
		capacity.CgroupLimit = &CgroupMemoryLimit{
			Bytes: int64(reading.cgroupBytes), Source: reading.cgroupSource,
		}
	}
	return capacity
}

func resolveCgroupMemory(begin, end memorySample) *CgroupMemory {
	if !begin.present || !end.present || begin.source != end.source || begin.identity != end.identity {
		return nil
	}
	memory := &CgroupMemory{
		Source: end.source,
		Closing: CgroupMemoryClosing{
			CurrentBytes: end.current, Composition: end.composition, LifetimePeak: end.peak,
		},
	}
	if end.source == CgroupMemoryV2 && begin.events != nil && end.events != nil {
		low, lowOK := windowDelta(begin.events.Low, end.events.Low)
		high, highOK := windowDelta(begin.events.High, end.events.High)
		maximum, maxOK := windowDelta(begin.events.Max, end.events.Max)
		oom, oomOK := windowDelta(begin.events.OOM, end.events.OOM)
		oomKill, killOK := windowDelta(begin.events.OOMKill, end.events.OOMKill)
		if lowOK && highOK && maxOK && oomOK && killOK {
			memory.Events = &CgroupMemoryEvents{
				Low: low, High: high, Max: maximum, OOM: oom, OOMKill: oomKill,
			}
		}
	}
	return memory
}

func resolveMemoryPressure(
	begin, end memoryPressureSample,
	memory *CgroupMemory,
) *MemoryPressure {
	if !begin.present || !end.present || begin.scope != end.scope || begin.identity != end.identity {
		return nil
	}
	if end.scope == MemoryPressureCgroup && (memory == nil || memory.Source != CgroupMemoryV2) {
		return nil
	}
	some, someOK := windowDelta(begin.some, end.some)
	full, fullOK := windowDelta(begin.full, end.full)
	if !someOK || !fullOK || full > some {
		return nil
	}
	return &MemoryPressure{Scope: end.scope, SomeStalledUs: some, FullStalledUs: full}
}

func parseSafeMemoryValue(content string) (int64, bool) {
	value := strings.TrimSpace(content)
	if value == "" || strings.ContainsAny(value, " \t\r\n") {
		return 0, false
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed > maxSafeJSONInteger {
		return 0, false
	}
	return int64(parsed), true
}

func parseFiniteMemoryLimit(content, source string) (uint64, bool) {
	value := strings.TrimSpace(content)
	if value == "" || value == "max" || strings.ContainsAny(value, " \t\r\n") {
		return 0, false
	}
	limit, err := strconv.ParseUint(value, 10, 64)
	if err != nil || limit == 0 {
		return 0, false
	}
	if source == CgroupMemoryV1 && limit >= cgroupV1UnlimitedSentinel {
		return 0, false
	}
	return limit, true
}

// parseCompleteCounters returns exactly the requested subset only when every
// member occurs once and parses as a non-negative safe JSON integer. Unknown
// kernel members are ignored; a malformed requested member invalidates all.
func parseCompleteCounters(content string, wanted []string) (map[string]int64, bool) {
	requested := make(map[string]bool, len(wanted))
	for _, key := range wanted {
		requested[key] = true
	}
	values := make(map[string]int64, len(wanted))
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !requested[fields[0]] {
			continue
		}
		if len(fields) != 2 {
			return nil, false
		}
		if _, duplicate := values[fields[0]]; duplicate {
			return nil, false
		}
		value, ok := parseSafeMemoryValue(fields[1])
		if !ok {
			return nil, false
		}
		values[fields[0]] = value
	}
	if len(values) != len(wanted) {
		return nil, false
	}
	return values, true
}

func parseMemoryPressure(content string) (some, full int64, ok bool) {
	seenSome, seenFull := false, false
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || (fields[0] != "some" && fields[0] != "full") {
			continue
		}
		if (fields[0] == "some" && seenSome) || (fields[0] == "full" && seenFull) {
			return 0, 0, false
		}
		totalFound := false
		var total int64
		for _, field := range fields[1:] {
			value, found := strings.CutPrefix(field, "total=")
			if !found {
				continue
			}
			if totalFound {
				return 0, 0, false
			}
			parsed, parsedOK := parseSafeMemoryValue(value)
			if !parsedOK {
				return 0, 0, false
			}
			total, totalFound = parsed, true
		}
		if !totalFound {
			return 0, 0, false
		}
		if fields[0] == "some" {
			some, seenSome = total, true
		} else {
			full, seenFull = total, true
		}
	}
	return some, full, seenSome && seenFull && full <= some
}

func totalMemoryBytesAt(root string, read func(string) string) uint64 {
	for _, line := range strings.Split(read(filepath.Join(root, "proc/meminfo")), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kib, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || kib > ^uint64(0)/1024 {
			return 0
		}
		return kib * 1024
	}
	if root == runnerEnvironmentRoot && runtime.GOOS == "darwin" {
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0
		}
		bytes, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
		if err == nil {
			return bytes
		}
	}
	// The platform reader, like sysctl above, is consulted only for the real
	// filesystem: a fixture tree describes a machine of its own.
	if root == runnerEnvironmentRoot {
		return platformPhysicalMemoryBytes()
	}
	return 0
}
