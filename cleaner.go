package main

import (
	"fmt"
	"regexp"
	"strings"
)

// chromePatterns are devin TUI chrome lines that should be stripped from output.
var chromePatterns = []*regexp.Regexp{
	regexp.MustCompile(`[\x{2800}-\x{28FF}]`),       // braille art (logo)
	regexp.MustCompile(`Devin for Terminal`),
	regexp.MustCompile(`^v20\d{2}\.\d+\.\d+`),        // version string
	regexp.MustCompile(`Pro\s*·\s*\d+%\s*remaining`),
	regexp.MustCompile(`resets in \d+[dhm]`),
	regexp.MustCompile(`^Context:\s*\d+k?\s*/\s*\d+k?\s*tokens`),
	regexp.MustCompile(`SWE-1\.6`),
	regexp.MustCompile(`claude-\w+`),
	regexp.MustCompile(`(?i)opus|codex`),
	regexp.MustCompile(`Looking for plan mode`),
	regexp.MustCompile(`Yapping`),
	regexp.MustCompile(`esc to interrupt`),
	regexp.MustCompile(`(?i)ctrl\+o`),
	regexp.MustCompile(`(?i)alt\+t`),
	regexp.MustCompile(`alt\+[↑↓]`),
	regexp.MustCompile(`Press alt\+`),
	regexp.MustCompile(`Did you know`),
	regexp.MustCompile(`^[─═╌┄─]+$`),
	regexp.MustCompile(`[─═╌┄].*(bypass permissions|tokens|SWE|Context)`),
	regexp.MustCompile(`^[\s│└├┤┬┴┼╭╮╯╰]+$`),
	regexp.MustCompile(`bypass permissions`),
	regexp.MustCompile(`^❭\s*Ask Devin`),
	regexp.MustCompile(`^❭\s*Guide Devin`),
	regexp.MustCompile(`OPENAI_API_KEY`),
	regexp.MustCompile(`platform\.openai\.com`),
	regexp.MustCompile(`export OPENAI_API_KEY`),
	regexp.MustCompile(`Get your API key from`),
	regexp.MustCompile(`Warning: OPENAI_API_KEY is not set`),
	regexp.MustCompile(`➜`), // shell prompt (anywhere in line)
	regexp.MustCompile(`^\d+ shell\s*·\s*[↓↑]\s*select`),
	regexp.MustCompile(`^Session:\s+\w+`),
	regexp.MustCompile(`◆ Trust`),
	regexp.MustCompile(`Thinking\s*·\s*\d+m\s*\d+s`),
	regexp.MustCompile(`Thinking\s*·\s*\d+s`),
	regexp.MustCompile(`Running tools\s*·\s*\d+m\s*\d+s`),
	regexp.MustCompile(`Running tools\s*·\s*\d+s`),
	regexp.MustCompile(`Running command\s*·\s*\d+m\s*\d+s`),
	regexp.MustCompile(`Running command\s*·\s*\d+s`),
}

var (
	boxPrefixRe   = regexp.MustCompile(`^[\s]*[│├└]\s+(.+)$`)
	toolStatusRe  = regexp.MustCompile(`^([⏺◔✗✓✱])\s+(.+)$`)
	stuckRe       = regexp.MustCompile(`(?:Thinking|Running tools|Running command)\s*·\s*(?:(?:(\d+)m\s*)?(\d+)s)`)
	trustDialogRe = regexp.MustCompile(`◆ Trust .+\? Yes, trust`)
)

// cleanLine strips devin TUI chrome from a single line.
// Returns empty string for blank lines, nil sentinel ("") for lines to drop.
// We use a second return bool: false = drop the line entirely.
func cleanLine(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)

	if trimmed == "" {
		return "", true // preserve blank lines
	}

	for _, pat := range chromePatterns {
		if pat.MatchString(trimmed) {
			return "", false // drop
		}
	}

	// Strip leading box-drawing prefix: "  │ output" → "output"
	if m := boxPrefixRe.FindStringSubmatch(line); m != nil {
		return strings.TrimRight(m[1], " \t"), true
	}

	// Strip tool status symbols: "⏺ Ran command" → "Ran command"
	if m := toolStatusRe.FindStringSubmatch(trimmed); m != nil {
		return strings.TrimRight(m[2], " \t"), true
	}

	// Drop lines that contain command artifacts
	if strings.Contains(line, "devin") || strings.Contains(line, "prompt-file") || strings.Contains(line, "dangerous") {
		return "", false
	}

	// Drop lines that contain .txt (file path fragments)
	if strings.Contains(line, ".txt") {
		return "", false
	}

	// Preserve the original line content
	return strings.TrimRight(line, " \t"), true
}

// extractCleanLines splits pane content into cleaned lines, dropping chrome.
func extractCleanLines(paneContent string) []string {
	raw := strings.Split(paneContent, "\n")
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		if cl, keep := cleanLine(l); keep {
			out = append(out, cl)
		}
	}
	return out
}

// findResponseStart returns the index after the last user-prompt indicator (❭).
// Everything from that index onward is the response.
// If no ❭ indicator is found (e.g., in -p mode), returns 0 (entire pane is response).
func findResponseStart(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if strings.HasPrefix(l, "❭") &&
			!strings.Contains(l, "Yes, trust") &&
			!strings.Contains(l, "No, exit") {
			return i + 1
		}
	}
	// No ❭ found: this is -p mode, return 0 (everything is response)
	return 0
}

// hasTrustDialog returns true when a workspace-trust dialog is active
// (visible in the last 10 lines of the pane).
func hasTrustDialog(paneContent string) bool {
	if paneContent == "" {
		return false
	}
	lines := strings.Split(paneContent, "\n")
	start := len(lines) - 10
	if start < 0 {
		start = 0
	}
	last := strings.Join(lines[start:], "\n")

	if trustDialogRe.MatchString(last) {
		return true
	}
	return strings.Contains(last, "Do you trust the authors of this directory?")
}

// getStuckSeconds returns how many seconds devin has been in a
// Thinking/Running state, or 0 if not stuck.
func getStuckSeconds(paneContent string) int {
	if paneContent == "" {
		return 0
	}
	lines := strings.Split(paneContent, "\n")
	start := len(lines) - 5
	if start < 0 {
		start = 0
	}
	last := strings.Join(lines[start:], "\n")

	m := stuckRe.FindStringSubmatch(last)
	if m == nil {
		return 0
	}
	minutes := 0
	seconds := 0
	if m[1] != "" {
		fmt.Sscanf(m[1], "%d", &minutes)
	}
	if m[2] != "" {
		fmt.Sscanf(m[2], "%d", &seconds)
	}
	return minutes*60 + seconds
}
