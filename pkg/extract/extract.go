// Package extract provides web page content extraction.
// It fetches URLs, strips non-content elements (ads, nav, scripts),
// and returns clean markdown text suitable for storage.
// This is Layer 1 of the security model: aggressive sanitization.
package extract

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	readability "github.com/go-shiori/go-readability"
	md "github.com/JohannesKaufmann/html-to-markdown/v2"
)

// Result holds the extracted content from a web page.
type Result struct {
	Title       string `json:"title"`
	Byline      string `json:"byline,omitempty"`
	SiteName    string `json:"site_name,omitempty"`
	Excerpt     string `json:"excerpt,omitempty"`
	Content     string `json:"content"`      // Clean markdown text
	ContentHTML string `json:"content_html"` // Clean HTML (post-readability, pre-markdown)
	URL         string `json:"url"`
	FetchedAt   time.Time `json:"fetched_at"`
	WordCount   int    `json:"word_count"`
}

// Options controls extraction behavior.
type Options struct {
	// UserAgent for HTTP requests. Defaults to a standard browser UA.
	UserAgent string
	// Timeout for HTTP fetch. Default 30s.
	Timeout time.Duration
	// MaxBodySize caps the raw HTML download. Default 10MB.
	MaxBodySize int64
}

// DefaultOptions returns sensible defaults.
func DefaultOptions() Options {
	return Options{
		UserAgent:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		Timeout:     30 * time.Second,
		MaxBodySize: 10 * 1024 * 1024, // 10MB
	}
}

// FromURL fetches a URL and extracts readable content.
func FromURL(rawURL string, opts Options) (*Result, error) {
	if opts.Timeout == 0 {
		opts = DefaultOptions()
	}

	// Validate URL
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme: %s (only http/https)", parsed.Scheme)
	}

	// Fetch
	client := &http.Client{Timeout: opts.Timeout}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", opts.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	// Limit body size
	body := io.LimitReader(resp.Body, opts.MaxBodySize)

	// Use go-readability to extract article content
	article, err := readability.FromReader(body, parsed)
	if err != nil {
		return nil, fmt.Errorf("readability extraction failed: %w", err)
	}

	// Convert clean HTML to markdown
	markdown, err := md.ConvertString(article.Content)
	if err != nil {
		// Fallback: use the text content directly
		markdown = article.TextContent
	}

	// Count words
	wordCount := len(strings.Fields(markdown))

	return &Result{
		Title:       article.Title,
		Byline:      article.Byline,
		SiteName:    article.SiteName,
		Excerpt:     article.Excerpt,
		Content:     markdown,
		ContentHTML: article.Content,
		URL:         rawURL,
		FetchedAt:   time.Now(),
		WordCount:   wordCount,
	}, nil
}

// FromHTML extracts content from already-fetched HTML.
// Useful when you already have the page content.
func FromHTML(html string, sourceURL string) (*Result, error) {
	parsed, err := url.Parse(sourceURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	article, err := readability.FromReader(strings.NewReader(html), parsed)
	if err != nil {
		return nil, fmt.Errorf("readability extraction failed: %w", err)
	}

	markdown, err := md.ConvertString(article.Content)
	if err != nil {
		markdown = article.TextContent
	}

	wordCount := len(strings.Fields(markdown))

	return &Result{
		Title:       article.Title,
		Byline:      article.Byline,
		SiteName:    article.SiteName,
		Excerpt:     article.Excerpt,
		Content:     markdown,
		ContentHTML: article.Content,
		URL:         sourceURL,
		FetchedAt:   time.Now(),
		WordCount:   wordCount,
	}, nil
}
