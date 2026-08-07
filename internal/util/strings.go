// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

// Package util provides shared utility functions used across Hippocampus.
package util

// Truncate shortens s to maxLen characters, appending "..." if truncated.
func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// Dedupe removes duplicate strings from a slice, preserving order.
func Dedupe(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	result := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

// EscapeRedisTag escapes special characters in a RediSearch TAG value.
// TAG field queries use {} delimiters and certain characters must be escaped
// with a backslash: , . < > { } [ ] " ' : ; ! @ # $ % ^ & * ( ) - + = ~
func EscapeRedisTag(tag string) string {
	var b []byte
	for i := 0; i < len(tag); i++ {
		ch := tag[i]
		switch ch {
		case ',', '.', '<', '>', '{', '}', '[', ']', '"', '\'',
			':', ';', '!', '@', '#', '$', '%', '^', '&', '*',
			'(', ')', '-', '+', '=', '~', ' ', '/', '\\':
			b = append(b, '\\', ch)
		default:
			b = append(b, ch)
		}
	}
	return string(b)
}
