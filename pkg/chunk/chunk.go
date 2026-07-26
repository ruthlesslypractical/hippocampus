// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

// Package chunk splits long content into manageable pieces for storage.
// Chunks are designed to be stored as linked entries, enabling
// fine-grained semantic search while maintaining document coherence.
package chunk

import (
	"fmt"
	"strings"
)

// Chunk represents a piece of a larger document.
type Chunk struct {
	// Index is the position of this chunk (0-based).
	Index int `json:"index"`
	// Total is the total number of chunks.
	Total int `json:"total"`
	// Content is the text of this chunk.
	Content string `json:"content"`
	// Heading is the section heading this chunk falls under (if any).
	Heading string `json:"heading,omitempty"`
}

// Options controls chunking behavior.
type Options struct {
	// MaxChunkSize is the maximum character count per chunk. Default 3000.
	MaxChunkSize int
	// MinChunkSize is the minimum chunk size before merging with previous. Default 200.
	MinChunkSize int
	// PreferHeadings: if true, split at markdown headings when possible.
	PreferHeadings bool
}

// DefaultOptions returns sensible chunking defaults.
func DefaultOptions() Options {
	return Options{
		MaxChunkSize:   3000,
		MinChunkSize:   200,
		PreferHeadings: true,
	}
}

// Split breaks content into chunks, preferring natural boundaries.
// It tries to split at (in priority order): headings, double newlines, single newlines, sentences.
func Split(content string, opts Options) []Chunk {
	if opts.MaxChunkSize == 0 {
		opts = DefaultOptions()
	}

	// If content fits in one chunk, return as-is
	if len(content) <= opts.MaxChunkSize {
		return []Chunk{{Index: 0, Total: 1, Content: content}}
	}

	var rawChunks []rawChunk

	if opts.PreferHeadings {
		rawChunks = splitByHeadings(content, opts)
	} else {
		rawChunks = splitByParagraphs(content, opts)
	}

	// Merge tiny trailing chunks
	rawChunks = mergeTiny(rawChunks, opts)

	// Convert to Chunk structs
	total := len(rawChunks)
	result := make([]Chunk, total)
	for i, rc := range rawChunks {
		result[i] = Chunk{
			Index:   i,
			Total:   total,
			Content: rc.content,
			Heading: rc.heading,
		}
	}

	return result
}

// NeedsChunking returns true if content exceeds the threshold.
func NeedsChunking(content string, maxSize int) bool {
	if maxSize == 0 {
		maxSize = DefaultOptions().MaxChunkSize
	}
	return len(content) > maxSize
}

type rawChunk struct {
	content string
	heading string
}

func splitByHeadings(content string, opts Options) []rawChunk {
	lines := strings.Split(content, "\n")
	var chunks []rawChunk
	var current strings.Builder
	currentHeading := ""

	for _, line := range lines {
		// Detect markdown headings
		trimmed := strings.TrimSpace(line)
		isHeading := strings.HasPrefix(trimmed, "#")

		// If we hit a heading and current chunk is non-trivial, flush
		if isHeading && current.Len() > opts.MinChunkSize {
			chunks = append(chunks, rawChunk{
				content: strings.TrimSpace(current.String()),
				heading: currentHeading,
			})
			current.Reset()
			currentHeading = trimmed
		} else if isHeading {
			currentHeading = trimmed
		}

		current.WriteString(line)
		current.WriteString("\n")

		// If chunk exceeds max, force-split at paragraph boundary
		if current.Len() > opts.MaxChunkSize {
			text := current.String()
			splitPoint := findSplitPoint(text, opts.MaxChunkSize)
			chunks = append(chunks, rawChunk{
				content: strings.TrimSpace(text[:splitPoint]),
				heading: currentHeading,
			})
			current.Reset()
			current.WriteString(text[splitPoint:])
		}
	}

	// Flush remaining
	if current.Len() > 0 {
		chunks = append(chunks, rawChunk{
			content: strings.TrimSpace(current.String()),
			heading: currentHeading,
		})
	}

	return chunks
}

func splitByParagraphs(content string, opts Options) []rawChunk {
	paragraphs := strings.Split(content, "\n\n")
	var chunks []rawChunk
	var current strings.Builder

	for _, para := range paragraphs {
		if current.Len()+len(para)+2 > opts.MaxChunkSize && current.Len() > 0 {
			chunks = append(chunks, rawChunk{content: strings.TrimSpace(current.String())})
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteString("\n\n")
		}
		current.WriteString(para)
	}

	if current.Len() > 0 {
		chunks = append(chunks, rawChunk{content: strings.TrimSpace(current.String())})
	}

	return chunks
}

func findSplitPoint(text string, maxSize int) int {
	// Try to split at double newline
	if idx := strings.LastIndex(text[:maxSize], "\n\n"); idx > 0 {
		return idx + 2
	}
	// Try single newline
	if idx := strings.LastIndex(text[:maxSize], "\n"); idx > 0 {
		return idx + 1
	}
	// Try sentence boundary
	if idx := strings.LastIndex(text[:maxSize], ". "); idx > 0 {
		return idx + 2
	}
	// Give up, split at max
	return maxSize
}

func mergeTiny(chunks []rawChunk, opts Options) []rawChunk {
	if len(chunks) <= 1 {
		return chunks
	}

	var merged []rawChunk
	for i, ch := range chunks {
		if i > 0 && len(ch.content) < opts.MinChunkSize {
			// Merge with previous
			prev := &merged[len(merged)-1]
			prev.content = fmt.Sprintf("%s\n\n%s", prev.content, ch.content)
		} else {
			merged = append(merged, ch)
		}
	}

	return merged
}
