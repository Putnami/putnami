package scheduler

import (
	"sync"
	"time"

	model "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/jobs/machineprobe"
)

const (
	// capacitySampleInterval is the first spacing of capacity samples.
	capacitySampleInterval = 5 * time.Second
	// maxCapacitySamples bounds a run's samples: when full, every other sample
	// is dropped and the spacing doubles.
	maxCapacitySamples = 512
)

// capacitySampler reads the sum of the CPU ceilings of the running
// subprocesses and the one-minute load at a fixed spacing over a run. A nil
// sampler records nothing.
type capacitySampler struct {
	cpus, workers int
	started       time.Time
	load          func() (float64, bool)
	now           func() time.Time

	mu       sync.Mutex
	granted  int
	peak     int
	interval time.Duration
	samples  []model.CapacitySample

	stopOnce sync.Once
	ticking  bool
	stop     chan struct{}
	done     chan struct{}
}

func newCapacitySampler(cpus, workers int) *capacitySampler {
	now := time.Now()
	return &capacitySampler{
		cpus: cpus, workers: workers, started: now,
		load: machineprobe.OneMinuteLoad, now: time.Now,
		interval: capacitySampleInterval,
		stop:     make(chan struct{}), done: make(chan struct{}),
	}
}

// start takes the first sample and samples until finish.
func (c *capacitySampler) start() {
	if c == nil {
		return
	}
	c.sample()
	c.ticking = true
	go func() {
		defer close(c.done)
		ticker := time.NewTicker(capacitySampleInterval)
		defer ticker.Stop()
		for ticks := 1; ; ticks++ {
			select {
			case <-c.stop:
				return
			case <-ticker.C:
				if c.due(ticks) {
					c.sample()
				}
			}
		}
	}()
}

// due reports whether the tick-th tick of the base interval falls on the
// current spacing, which is a power-of-two multiple of it.
func (c *capacitySampler) due(tick int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	every := max(int(c.interval/capacitySampleInterval), 1)
	return tick%every == 0
}

// grant adds a running subprocess's ceiling; release removes it.
func (c *capacitySampler) grant(cpu int) {
	if c == nil || cpu <= 0 {
		return
	}
	c.mu.Lock()
	c.granted += cpu
	c.peak = max(c.peak, c.granted)
	c.mu.Unlock()
}

func (c *capacitySampler) release(cpu int) {
	if c == nil || cpu <= 0 {
		return
	}
	c.mu.Lock()
	c.granted = max(c.granted-cpu, 0)
	c.mu.Unlock()
}

// sample appends one reading. The load is read outside the lock.
func (c *capacitySampler) sample() {
	load, known := c.load()
	c.mu.Lock()
	defer c.mu.Unlock()
	at := c.now()
	if len(c.samples) == maxCapacitySamples {
		kept := c.samples[:0]
		for i := 0; i < len(c.samples); i += 2 {
			kept = append(kept, c.samples[i])
		}
		c.samples = kept
		c.interval *= 2
	}
	c.samples = append(c.samples, model.CapacitySample{
		At: at.Sub(c.started), GrantedCPU: c.granted, Load: load, LoadKnown: known,
	})
}

// finish stops sampling, takes the last sample and returns the report.
func (c *capacitySampler) finish() *model.CapacityReport {
	if c == nil {
		return nil
	}
	c.stopOnce.Do(func() {
		close(c.stop)
		if c.ticking {
			<-c.done
		}
		c.sample()
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	return &model.CapacityReport{
		CPUs: c.cpus, Workers: c.workers, Interval: c.interval,
		Samples:        append([]model.CapacitySample(nil), c.samples...),
		PeakGrantedCPU: c.peak,
	}
}
