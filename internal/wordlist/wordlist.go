// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

// Package wordlist provides a curated 4096-word list for passphrases and verbal hashes.
// Based on the EFF long diceware list, filtered for ≤8 chars, no homophones, no profanity.
// 4096 words = 12 bits per word. 6 words = 72 bits of entropy.
package wordlist

import (
	_ "embed"
	"strings"
)

//go:embed words.txt
var wordsFile string

// Words returns the full 4096-word list.
func Words() []string {
	return strings.Split(strings.TrimSpace(wordsFile), "\n")
}

// Size returns the number of words in the list (4096).
func Size() int {
	return 4096
}
