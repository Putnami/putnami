package fmtx

func Label(s string) string {
	if s == "" {
		return "[empty]"
	}
	return "[" + s + "]"
}

func Unused(s string) string {
	return s + s
}
