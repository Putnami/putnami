package jobs

// DefaultTimeoutMs is the default subprocess timeout (5 minutes).
const DefaultTimeoutMs = 300_000

// PublishConcurrencyReporter is an optional renderer capability for machine
// consumers that report Docker publication facts. The scheduler owns the
// resolved worker cap; the renderer owns observing the registry phases that
// actually ran, so this narrow seam keeps the two measurements distinct.
type PublishConcurrencyReporter interface {
	SetPublishConcurrency(configuredCap int)
}
