package errors

// Category classifies the nature of an error for operational decisions
// (retry, log level, alert, user visibility).
type Category string

// Error categories for operational classification.
const (
	CategoryInfra     Category = "infra"     // infrastructure failures (DB, network, disk)
	CategoryUser      Category = "user"      // user input / request errors
	CategoryTransient Category = "transient" // temporary, retryable failures
	CategoryBug       Category = "bug"       // invariant violations, programmer errors
	CategorySecurity  Category = "security"  // authentication / authorization failures
)
