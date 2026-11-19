package telemetry

// Log record conventions. Every runtime counts each record its loggers
// receive under MetricLogRecords, labeled with the record's level and whether
// the minimum level let it through, so an operator sees how many records a
// level hides. fixtures/conventions/log-records.json holds the same values;
// log_conventions_test.go keeps them equal.
const (
	// MetricLogRecords counts log records by level and outcome.
	MetricLogRecords = "log.records"
	// AttrLogLevel labels a record with its level: debug, info, warn or error.
	AttrLogLevel = "level"
	// AttrLogOutcome labels a record with LogOutcomeEmitted or LogOutcomeDropped.
	AttrLogOutcome = "outcome"
	// LogOutcomeEmitted is the outcome of a record the logger wrote.
	LogOutcomeEmitted = "emitted"
	// LogOutcomeDropped is the outcome of a record the minimum level filtered.
	LogOutcomeDropped = "dropped"
)

// LogLevels returns the values of AttrLogLevel, from the lowest level.
func LogLevels() []string {
	return []string{"debug", "info", "warn", "error"}
}

// LogOutcomes returns the values of AttrLogOutcome.
func LogOutcomes() []string {
	return []string{LogOutcomeEmitted, LogOutcomeDropped}
}

// LogRecordMetrics returns the log record metric conventions.
func LogRecordMetrics() []MetricConvention {
	return []MetricConvention{{
		Name:        MetricLogRecords,
		Kind:        MetricKindCounter,
		Description: "Log records by level and outcome",
		Attributes:  []string{AttrLogLevel, AttrLogOutcome},
	}}
}
