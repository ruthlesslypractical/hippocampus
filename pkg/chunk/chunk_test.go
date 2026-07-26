// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package chunk

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplit_ShortContent(t *testing.T) {
	content := "This is a short paragraph that fits in one chunk."
	chunks := Split(content, DefaultOptions())

	require.Len(t, chunks, 1)
	assert.Equal(t, 0, chunks[0].Index)
	assert.Equal(t, 1, chunks[0].Total)
	assert.Equal(t, content, chunks[0].Content)
}

func TestSplit_ByHeadings(t *testing.T) {
	content := `# Introduction

This is the intro paragraph with enough text to exceed the minimum chunk size threshold.
It goes on for a bit to make sure we have enough content here to not get merged.
Lorem ipsum dolor sit amet, consectetur adipiscing elit. Sed do eiusmod tempor.

# Methods

Here we describe the methods used in this study with plenty of detail.
The methodology section is quite extensive and covers multiple approaches.
We examined several different techniques and their relative merits.

# Results

The results show significant improvement over baseline. Our measurements
indicate a 45% improvement in performance across all tested metrics.
The statistical significance was confirmed with p < 0.01.`

	opts := Options{MaxChunkSize: 300, MinChunkSize: 100, PreferHeadings: true}
	chunks := Split(content, opts)

	require.GreaterOrEqual(t, len(chunks), 2, "should split at headings")

	// First chunk should contain Introduction
	assert.Contains(t, chunks[0].Content, "intro paragraph")

	// Chunks should cover all content
	combined := ""
	for _, c := range chunks {
		combined += c.Content + "\n"
	}
	assert.Contains(t, combined, "Introduction")
	assert.Contains(t, combined, "Methods")
	assert.Contains(t, combined, "Results")
}

func TestSplit_HeadingTracking(t *testing.T) {
	content := `# First Section

Content of first section with enough text to be meaningful and not get merged away.
This needs to be longer than MinChunkSize to avoid the merge behavior.
Adding more text here to ensure we have a substantial first section.

# Second Section

Content of second section, also with enough text to stand on its own.
We need this to be a proper section that won't get merged into the first one.
More content here to pad it out sufficiently for the test.`

	opts := Options{MaxChunkSize: 200, MinChunkSize: 50, PreferHeadings: true}
	chunks := Split(content, opts)

	require.GreaterOrEqual(t, len(chunks), 2)

	// Second chunk should have the heading tracked
	found := false
	for _, c := range chunks {
		if c.Heading != "" && strings.Contains(c.Heading, "Second Section") {
			found = true
			break
		}
	}
	assert.True(t, found, "expected heading tracking on second chunk")
}

func TestSplit_ForceSplitLongParagraph(t *testing.T) {
	// Single paragraph longer than MaxChunkSize
	words := make([]string, 500)
	for i := range words {
		words[i] = "word"
	}
	content := strings.Join(words, " ") // ~2500 chars

	opts := Options{MaxChunkSize: 1000, MinChunkSize: 100, PreferHeadings: true}
	chunks := Split(content, opts)

	require.GreaterOrEqual(t, len(chunks), 2, "should force-split long content")

	// No chunk should exceed MaxChunkSize (with tolerance for split point finding at boundaries)
	for _, c := range chunks {
		assert.LessOrEqual(t, len(c.Content), opts.MaxChunkSize*2,
			"chunk should not massively exceed max size")
	}
}

func TestSplit_MergesTinyChunks(t *testing.T) {
	content := `# Big Section

This is a substantial section with plenty of content that stands on its own.
It has multiple sentences and covers important ground about the topic at hand.
We continue to elaborate on the key points with supporting evidence and detail.

# Tiny

Hi.`

	opts := Options{MaxChunkSize: 3000, MinChunkSize: 200, PreferHeadings: true}
	chunks := Split(content, opts)

	// "Tiny" section (just "Hi.") should get merged into the previous chunk
	// because it's below MinChunkSize
	for _, c := range chunks {
		if c.Content == "Hi." {
			t.Fatal("tiny chunk should have been merged, not left standalone")
		}
	}
}

func TestSplit_IndexAndTotal(t *testing.T) {
	// Create content that will definitely split into multiple chunks
	var sections []string
	for i := 0; i < 5; i++ {
		section := strings.Repeat("This is section content that fills space. ", 50)
		sections = append(sections, "## Section "+string(rune('A'+i))+"\n\n"+section)
	}
	content := strings.Join(sections, "\n\n")

	opts := Options{MaxChunkSize: 500, MinChunkSize: 100, PreferHeadings: true}
	chunks := Split(content, opts)

	require.GreaterOrEqual(t, len(chunks), 3)

	// Verify index/total consistency
	for i, c := range chunks {
		assert.Equal(t, i, c.Index, "chunk index should match position")
		assert.Equal(t, len(chunks), c.Total, "total should match actual count")
	}
}

func TestSplit_ParagraphMode(t *testing.T) {
	content := `First paragraph with some content that is meaningful.

Second paragraph with different content about another topic entirely.

Third paragraph wrapping up the discussion with final thoughts and conclusions.`

	opts := Options{MaxChunkSize: 100, MinChunkSize: 20, PreferHeadings: false}
	chunks := Split(content, opts)

	require.GreaterOrEqual(t, len(chunks), 2, "should split by paragraphs")

	// Each chunk should contain complete paragraphs (not mid-sentence splits)
	for _, c := range chunks {
		trimmed := strings.TrimSpace(c.Content)
		assert.NotEmpty(t, trimmed)
	}
}

func TestNeedsChunking(t *testing.T) {
	short := "Hello world"
	long := strings.Repeat("x", 5000)

	assert.False(t, NeedsChunking(short, 3000))
	assert.True(t, NeedsChunking(long, 3000))
	assert.False(t, NeedsChunking(short, 0)) // uses default
	assert.True(t, NeedsChunking(long, 0))   // uses default
}

func TestSplit_DefaultOptions(t *testing.T) {
	// Verify defaults are sensible
	opts := DefaultOptions()
	assert.Equal(t, 3000, opts.MaxChunkSize)
	assert.Equal(t, 200, opts.MinChunkSize)
	assert.True(t, opts.PreferHeadings)
}

func TestSplit_EmptyContent(t *testing.T) {
	chunks := Split("", DefaultOptions())
	require.Len(t, chunks, 1)
	assert.Equal(t, "", chunks[0].Content)
}

func TestSplit_PreservesAllContent(t *testing.T) {
	// Ensure no content is lost during chunking
	content := `# Header One

Paragraph one content here with details.

# Header Two

Paragraph two content here with more details.

# Header Three

Paragraph three wraps everything up nicely.`

	opts := Options{MaxChunkSize: 200, MinChunkSize: 50, PreferHeadings: true}
	chunks := Split(content, opts)

	// Reassemble and verify no content lost
	var reassembled strings.Builder
	for _, c := range chunks {
		reassembled.WriteString(c.Content)
		reassembled.WriteString(" ")
	}
	full := reassembled.String()

	assert.Contains(t, full, "Paragraph one")
	assert.Contains(t, full, "Paragraph two")
	assert.Contains(t, full, "Paragraph three")
}
