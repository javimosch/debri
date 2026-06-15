package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ExecOptions configures a single debri execution.
type ExecOptions struct {
	Model         string // devin model name (empty = devin default)
	PermMode      string // "auto" or "dangerous"
	WorkingDir    string // working directory for the session
	StableTimeout int    // ms of silence before considering output done
}

const (
	pollIntervalMs    = 250
	maxPolls          = 14400          // 60 min hard cap
	thinkingTimeoutMs = 5 * 60 * 1000 // 5 min thinking timeout
)

var safeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Execute creates a fresh devin tmux session, runs the prompt, collects output,
// then kills the session. onChunk is called for each new output chunk (may be nil).
func Execute(prompt string, opts ExecOptions, onChunk func(string)) (string, error) {
	if prompt == "" {
		return "", fmt.Errorf("prompt cannot be empty")
	}

	workDir := opts.WorkingDir
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("cannot determine working directory: %w", err)
		}
	}

	// Validate model name to prevent shell injection
	if opts.Model != "" && !isValidModel(opts.Model) {
		return "", fmt.Errorf("invalid model name: %q", opts.Model)
	}

	// Write prompt to a temp file in working dir's .devin/ folder
	// (avoids workspace trust prompts)
	devinDir := workDir + "/.devin"
	if err := os.MkdirAll(devinDir, 0755); err != nil {
		// Fall back to /tmp if we can't write to the working dir
		devinDir = "/tmp"
	}
	tmpFile := fmt.Sprintf("%s/debri-prompt-%d.txt", devinDir, time.Now().UnixMilli())
	if err := os.WriteFile(tmpFile, []byte(prompt), 0644); err != nil {
		return "", fmt.Errorf("cannot write prompt file: %w", err)
	}
	// Cleanup at end (after devin has read it)
	defer func() {
		os.Remove(tmpFile) //nolint: errcheck
	}()

	// Create a fresh tmux session
	sessionName := fmt.Sprintf("devin-debri-%d", time.Now().UnixMilli())
	if err := tmuxNew(sessionName, workDir); err != nil {
		return "", fmt.Errorf("cannot create tmux session: %w", err)
	}
	defer tmuxKill(sessionName) //nolint: errcheck

	// Short pause for tmux to settle
	time.Sleep(500 * time.Millisecond)

	// Build the devin command
	cmd := buildDevinCommand(opts, tmpFile)
	fmt.Fprintf(os.Stderr, "[debri] session=%s cmd=%s\n", sessionName, cmd)

	// Capture pane snapshot before sending command
	preSnap, _ := tmuxCapture(sessionName)

	// Clear screen then run
	tmuxSend(sessionName, "C-l")   //nolint: errcheck
	time.Sleep(200 * time.Millisecond)
	tmuxSend(sessionName, cmd)     //nolint: errcheck
	tmuxSendEnter(sessionName)     //nolint: errcheck
	time.Sleep(2 * time.Second) // let devin read the prompt file and start

	stableMs := opts.StableTimeout
	if stableMs <= 0 {
		stableMs = 5000
	}
	stableThreshold := stableMs / pollIntervalMs

	// Two-phase wait: before first output, wait longer (up to 300s)
	initialThreshold := 300000 / pollIntervalMs

	var (
		sentLineCount        int
		stablePolls          int
		seenPrompt           bool
		gotFirstOutput       bool
		allSentLines         []string
		thinkingStart        time.Time
		lastRealOutputCount  int
		capturing            bool
	)

	for i := 0; i < maxPolls; i++ {
		time.Sleep(pollIntervalMs * time.Millisecond)

		rawPane, err := tmuxCapture(sessionName)
		if err != nil || strings.Contains(rawPane, "can't find session") || strings.TrimSpace(rawPane) == "" {
			fmt.Fprintln(os.Stderr, "[debri] session gone, stopping poll")
			break
		}

		// Auto-confirm workspace trust dialog
		if hasTrustDialog(rawPane) {
			fmt.Fprintln(os.Stderr, "[debri] trust dialog detected, confirming")
			tmuxSendEnter(sessionName) //nolint: errcheck
			time.Sleep(300 * time.Millisecond)
			continue
		}

		if !capturing && i > 5 {
			capturing = true
		}
		if !capturing {
			continue
		}

		// Detect devin is active
		if !seenPrompt {
			paneChanged := preSnap != "" && rawPane != preSnap
			stuckSec := getStuckSeconds(rawPane)
			if paneChanged || stuckSec > 0 {
				seenPrompt = true
				stablePolls = 0
				fmt.Fprintf(os.Stderr, "[debri] devin active at poll %d\n", i)
			} else {
				if i >= initialThreshold {
					return "", fmt.Errorf("devin did not start within %ds", initialThreshold*pollIntervalMs/1000)
				}
				continue
			}
		}

		cleanLines := extractCleanLines(rawPane)
		responseStart := findResponseStart(cleanLines)
		responseLines := cleanLines[responseStart:]

		// Thinking timeout
		stuckSec := getStuckSeconds(rawPane)
		if stuckSec > 0 {
			if thinkingStart.IsZero() {
				thinkingStart = time.Now()
				lastRealOutputCount = sentLineCount
			} else if time.Since(thinkingStart).Milliseconds() > int64(thinkingTimeoutMs) {
				fmt.Fprintln(os.Stderr, "[debri] thinking timeout, interrupting")
				tmuxSend(sessionName, "C-c") //nolint: errcheck
				thinkingStart = time.Time{}
			}
		} else {
			thinkingStart = time.Time{}
			_ = lastRealOutputCount
		}

		// Emit new lines beyond the watermark
		if len(responseLines) > sentLineCount {
			newLines := responseLines[sentLineCount:]
			for _, l := range newLines {
				if l != "" {
					if onChunk != nil {
						onChunk(l)
					}
					allSentLines = append(allSentLines, l)
				}
			}
			sentLineCount = len(responseLines)
			if len(newLines) > 0 {
				gotFirstOutput = true
				stablePolls = 0
			}
		}

		// Stability check
		if gotFirstOutput || seenPrompt {
			stablePolls++
			threshold := stableThreshold
			if !gotFirstOutput {
				threshold = initialThreshold
			}
			if stablePolls >= threshold {
				fmt.Fprintf(os.Stderr, "[debri] stable after %d polls\n", i)
				break
			}
		}
	}

	return strings.Join(allSentLines, "\n"), nil
}

// buildDevinCommand assembles the devin CLI invocation string.
func buildDevinCommand(opts ExecOptions, promptFile string) string {
	var parts []string

	// Resolve devin path
	devinPath, err := exec.LookPath("devin")
	if err != nil {
		devinPath = "devin" // fallback, will fail in tmux if not in PATH
	}
	parts = append(parts, devinPath)

	if opts.Model != "" {
		parts = append(parts, "--model", shellQuote(opts.Model))
	}

	permMode := opts.PermMode
	if permMode == "" {
		permMode = "dangerous"
	}
	parts = append(parts, "--permission-mode", shellQuote(permMode))

	if opts.WorkingDir != "" {
		cfgPath := opts.WorkingDir + "/.devin/config.json"
		if _, err := os.Stat(cfgPath); err == nil {
			parts = append(parts, "--config", shellQuote(cfgPath))
		}
	}

	parts = append(parts, "-p", "--prompt-file", shellQuote(promptFile))

	return strings.Join(parts, " ")
}

// isValidModel validates a model name to prevent shell injection.
func isValidModel(model string) bool {
	known := []string{"SWE-1.6", "Kimi K2.6", "claude-sonnet-4", "claude-opus-4.6", "opus", "codex", "adaptive"}
	for _, k := range known {
		if model == k {
			return true
		}
	}
	return safeNameRe.MatchString(model)
}

// shellQuote wraps s in single quotes and escapes embedded single quotes.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// --- tmux helpers ---

func tmuxNew(session, workDir string) error {
	return exec.Command("tmux", "new-session", "-d", "-s", session, "-c", workDir).Run()
}

func tmuxKill(session string) error {
	return exec.Command("tmux", "kill-session", "-t", session).Run()
}

func tmuxSend(session, keys string) error {
	// Target the session by name only — no :window.pane suffix — so the command
	// works regardless of the operator's tmux pane-base-index setting. Using
	// ":0.0" fails when pane-base-index=1, causing executor to see "session gone"
	// even though devin is still running.
	return exec.Command("tmux", "send-keys", "-t", session, keys).Run()
}

func tmuxSendEnter(session string) error {
	return exec.Command("tmux", "send-keys", "-t", session, "Enter").Run()
}

func tmuxCapture(session string) (string, error) {
	out, err := exec.Command("tmux", "capture-pane", "-t", session, "-p", "-J").Output()
	return string(out), err
}
