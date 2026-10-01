// Package lib provides string manipulation utilities.
package lib

import "strings"

// Reverse reverses a string
func Reverse(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}

	return string(runes)
}

// Capitalize capitalizes the first letter of a string
func Capitalize(s string) string {
	if len(s) == 0 {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// IsPalindrome checks if a string is a palindrome
func IsPalindrome(s string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(s, " ", ""))
	return normalized == Reverse(normalized)
}
