// Package coverage provides shared helpers for enforcing a minimum test
// coverage threshold across language extensions. The threshold is a percentage
// (0–100) configurable at the workspace, language (extension), or project level
// through the standard Putnami option-merging rules, so each extension only has
// to evaluate the resolved value the same way.
package coverage

import "fmt"

// thresholdEpsilon absorbs floating-point rounding so a coverage value that is
// mathematically equal to the threshold (e.g. 80.0 stored as 79.99999999) is
// not spuriously reported as below it.
const thresholdEpsilon = 1e-9

// CheckThreshold reports whether measured coverage satisfies the configured
// minimum threshold (a percentage in the range 0–100).
//
// A threshold of 0 (or negative) disables the gate and always passes. When the
// gate is active but no coverage data was produced (hasCoverage is false), it
// fails: a threshold the run cannot evaluate is treated as unmet rather than
// silently ignored. The returned message is non-empty only when the gate fails
// and is suitable for an error diagnostic.
func CheckThreshold(threshold, actual float64, hasCoverage bool) (ok bool, message string) {
	if threshold <= 0 {
		return true, ""
	}
	if !hasCoverage {
		return false, fmt.Sprintf("coverage threshold %.1f%% is set but no coverage data was produced", threshold)
	}
	if actual+thresholdEpsilon < threshold {
		return false, fmt.Sprintf("coverage %.1f%% is below the required threshold of %.1f%%", actual, threshold)
	}
	return true, ""
}
