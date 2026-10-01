package shared

// AppendUnique appends item to slice unless it is already present, so the
// result never carries a duplicate value.
func AppendUnique(slice []string, item string) []string {
	for _, s := range slice {
		if s == item {
			return slice
		}
	}
	return append(slice, item)
}
