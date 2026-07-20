// Package safeguard provides content scanning for prompt injection detection.
// This is Layer 2 of the security model: heuristic detection of adversarial content
// that might attempt to manipulate an LLM when recalled from memory.
package safeguard

import (
	"regexp"
	"strings"
)

// ScanResult contains the results of a content safety scan.
type ScanResult struct {
	// Safe is true if no injection patterns were detected.
	Safe bool `json:"safe"`
	// Flags lists all detected patterns/concerns.
	Flags []Flag `json:"flags,omitempty"`
	// RiskScore from 0.0 (clean) to 1.0 (very suspicious). Computed from flags.
	RiskScore float64 `json:"risk_score"`
}

// Flag represents a single detected concern in the content.
type Flag struct {
	Pattern     string `json:"pattern"`
	Description string `json:"description"`
	Severity    string `json:"severity"` // "low", "medium", "high"
	Offset      int    `json:"offset"`   // Character position where pattern was found
}

// injection patterns ordered by severity
var injectionPatterns = []struct {
	re          *regexp.Regexp
	description string
	severity    string
	score       float64
}{
	// High severity: direct instruction override attempts
	{regexp.MustCompile(`(?i)ignore\s+(all\s+)?previous\s+(instructions?|prompts?|context)`), "instruction override attempt", "high", 0.9},
	{regexp.MustCompile(`(?i)disregard\s+(all\s+)?(previous|prior|above)\s+(instructions?|prompts?|context)`), "instruction override attempt", "high", 0.9},
	{regexp.MustCompile(`(?i)forget\s+(everything|all)\s+(you|that|above)`), "memory wipe attempt", "high", 0.9},
	{regexp.MustCompile(`(?i)you\s+are\s+now\s+(a|an)\s+`), "role reassignment attempt", "high", 0.85},
	{regexp.MustCompile(`(?i)new\s+instructions?:\s*`), "instruction injection", "high", 0.85},
	{regexp.MustCompile(`(?i)system\s*prompt\s*:`), "system prompt injection", "high", 0.9},
	{regexp.MustCompile(`(?i)\[system\]`), "system tag injection", "high", 0.8},
	{regexp.MustCompile(`(?i)<\s*system\s*>`), "system XML injection", "high", 0.8},
	{regexp.MustCompile(`(?i)IMPORTANT:\s*ignore`), "urgent override attempt", "high", 0.85},

	// Medium severity: suspicious instruction-like content
	{regexp.MustCompile(`(?i)from\s+now\s+on,?\s+(you|always|never)`), "behavioral modification", "medium", 0.6},
	{regexp.MustCompile(`(?i)do\s+not\s+(mention|reveal|tell|disclose)\s+(that|this|the\s+fact)`), "secrecy instruction", "medium", 0.6},
	{regexp.MustCompile(`(?i)pretend\s+(that\s+)?(you|this|we)`), "pretend instruction", "medium", 0.5},
	{regexp.MustCompile(`(?i)act\s+as\s+(if|though)\s+`), "behavioral override", "medium", 0.5},
	{regexp.MustCompile(`(?i)when\s+(the\s+)?(user|human)\s+asks?\s+about`), "conditional behavior manipulation", "medium", 0.6},
	{regexp.MustCompile(`(?i)respond\s+(only\s+)?with\s+`), "output control attempt", "medium", 0.5},
	{regexp.MustCompile(`(?i)output\s+(only|exactly)\s*:`), "output control attempt", "medium", 0.5},
	{regexp.MustCompile(`(?i)your\s+(true|real|actual)\s+(purpose|goal|objective)`), "goal redirection", "medium", 0.6},

	// Low severity: worth noting but not necessarily malicious
	{regexp.MustCompile(`(?i)<!--.*?(instruction|ignore|system|prompt).*?-->`), "suspicious HTML comment", "low", 0.3},
	{regexp.MustCompile(`(?i)(\\u0000|\\u200b|\\ufeff)`), "zero-width/null characters", "low", 0.4},
	{regexp.MustCompile(`(?i)base64[:\s]+[A-Za-z0-9+/=]{50,}`), "embedded base64 payload", "low", 0.4},
}

// Scan examines content for potential prompt injection patterns.
// Returns a ScanResult indicating whether the content is safe to store.
func Scan(content string) ScanResult {
	var flags []Flag
	var maxScore float64

	for _, p := range injectionPatterns {
		locs := p.re.FindAllStringIndex(content, -1)
		for _, loc := range locs {
			flags = append(flags, Flag{
				Pattern:     p.re.String(),
				Description: p.description,
				Severity:    p.severity,
				Offset:      loc[0],
			})
			if p.score > maxScore {
				maxScore = p.score
			}
		}
	}

	// Additional heuristic: unusually high ratio of instruction-like sentences
	sentences := strings.Split(content, ".")
	instructionCount := 0
	for _, s := range sentences {
		trimmed := strings.TrimSpace(s)
		if len(trimmed) > 5 && (strings.HasPrefix(trimmed, "You ") ||
			strings.HasPrefix(trimmed, "Always ") ||
			strings.HasPrefix(trimmed, "Never ") ||
			strings.HasPrefix(trimmed, "Do not ") ||
			strings.HasPrefix(trimmed, "Remember ")) {
			instructionCount++
		}
	}
	if len(sentences) > 0 {
		ratio := float64(instructionCount) / float64(len(sentences))
		if ratio > 0.3 && instructionCount > 3 {
			flags = append(flags, Flag{
				Pattern:     "instruction_density",
				Description: "unusually high density of instruction-like sentences",
				Severity:    "medium",
				Offset:      0,
			})
			if 0.5 > maxScore {
				maxScore = 0.5
			}
		}
	}

	return ScanResult{
		Safe:      maxScore < 0.5,
		Flags:     flags,
		RiskScore: maxScore,
	}
}

// Sanitize removes or neutralizes detected injection patterns from content.
// Returns the cleaned content and a list of what was removed.
// Use this for content that scored medium risk — high risk should be rejected entirely.
func Sanitize(content string) (string, []string) {
	var removed []string

	for _, p := range injectionPatterns {
		if p.severity == "high" || p.severity == "medium" {
			matches := p.re.FindAllString(content, -1)
			if len(matches) > 0 {
				for _, m := range matches {
					removed = append(removed, m)
				}
				content = p.re.ReplaceAllString(content, "[REDACTED: "+p.description+"]")
			}
		}
	}

	return content, removed
}

// IsSafe is a convenience function: returns true if content passes scan.
func IsSafe(content string) bool {
	return Scan(content).Safe
}
