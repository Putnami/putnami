package jobs

import "encoding/json"

// BatchResultsDataKey is the result-data key under which a shared batch
// subprocess reports one entry per member: the typed batch wire, whose "data"
// member is that member's result data.
const BatchResultsDataKey = "batchResults"

// BatchNumber coerces any JSON-decoded numeric spelling into a float64.
func BatchNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case json.Number:
		value, err := number.Float64()
		return value, err == nil
	default:
		return 0, false
	}
}
