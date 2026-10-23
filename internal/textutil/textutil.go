// Package textutil measures and cuts text by character (rune) rather than by byte, so limits like "the first
// 200 characters" never split a character.
package textutil

import (
	"strings"
	"unicode/utf8"
)

// Len is the number of characters in s.
func Len(s string) int { return utf8.RuneCountInString(s) }

// Substr returns characters [start, end) of s, clamped to the text; end < 0 means "to the end".
func Substr(s string, start, end int) string {
	r := []rune(s)
	if start < 0 {
		start = 0
	}
	if start > len(r) {
		start = len(r)
	}
	if end < 0 || end > len(r) {
		end = len(r)
	}
	if end < start {
		return ""
	}
	return string(r[start:end])
}

// Truncate returns the first n characters of s.
func Truncate(s string, n int) string { return Substr(s, 0, n) }

// DecodeUTF8 turns bytes into text, replacing each invalid byte with U+FFFD.
func DecodeUTF8(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	var b strings.Builder
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size <= 1 {
			b.WriteRune(utf8.RuneError)
			data = data[1:]
			continue
		}
		b.WriteRune(r)
		data = data[size:]
	}
	return b.String()
}
