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
	DoneMarker    string // if the agent prints this line, finish immediately
	                      // (stable-timeout becomes a safety cap, not the signal)
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

	// Create a fresh tmux session. Include the PID so concurrent debri processes
	// (e.g. a spawned a2a team, or parallel batch runs) can never collide on the
	// name — UnixMilli alone duplicates when two starts land in the same
	// millisecond, and `tmux new-session` then fails with exit 1. Retry with a
	// bumped suffix as a further guard against a lingering same-name session.
	base := fmt.Sprintf("devin-debri-%d-%d", time.Now().UnixMilli(), os.Getpid())
	sessionName := base
	var newErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			sessionName = fmt.Sprintf("%s-%d", base, attempt)
		}
		if newErr = tmuxNew(sessionName, workDir); newErr == nil {
			break
		}
	}
	if newErr != nil {
		return "", fmt.Errorf("cannot create tmux session: %w", newErr)
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
		sawDevinRunning      bool // pane foreground was observed to be non-shell
	)

	for i := 0; i < maxPolls; i++ {
		time.Sleep(pollIntervalMs * time.Millisecond)

		rawPane, err := tmuxCapture(sessionName)
		// The session going missing (tmux errors, or reports no such session) is
		// never a legitimate completion path — devin's own exit hands the pane
		// back to the shell (see the process-exit check below) rather than
		// killing the session. This only happens when something external killed
		// it (crash, OOM, another process, host issue), so surface it as an
		// error instead of silently reporting success with whatever partial
		// output was collected — a caller (or a2a agent) needs to be able to
		// tell "finished" from "the session vanished mid-task".
		if err != nil || strings.Contains(rawPane, "can't find session") {
			return strings.Join(allSentLines, "\n"), fmt.Errorf(
				"devin tmux session disappeared unexpectedly after %d polls (crashed, killed externally, or host issue)", i)
		}
		if strings.TrimSpace(rawPane) == "" {
			fmt.Fprintln(os.Stderr, "[debri] pane empty, stopping poll")
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

		// Process-exit completion (primary signal): once we've positively seen
		// devin running (pane foreground = a non-shell), a return to an idle
		// shell means devin finished and handed the pane back. This needs no
		// cooperation from the agent and is immune to the `a2a recv --wait`
		// silence that forced the long stable-timeout cap. Gate on
		// sawDevinRunning so the shell state at startup (before devin launches)
		// can't fake an instant completion.
		if seenPrompt {
			if paneForegroundIsShell(sessionName) {
				if sawDevinRunning {
					if len(responseLines) > sentLineCount {
						for _, l := range responseLines[sentLineCount:] {
							if l != "" && (opts.DoneMarker == "" || !strings.Contains(l, opts.DoneMarker)) {
								if onChunk != nil {
									onChunk(l)
								}
								allSentLines = append(allSentLines, l)
							}
						}
					}
					fmt.Fprintf(os.Stderr, "[debri] devin exited (pane back to shell) at poll %d, finishing\n", i)
					break
				}
			} else {
				sawDevinRunning = true
			}
		}

		// Done-marker fast path: the agent signals completion explicitly (e.g. a
		// reactive a2a peer that has just marked itself `status done` and echoed
		// the marker). Scan the RAW pane, not the response-sliced output — devin
		// returns to a fresh shell prompt after the echo, and findResponseStart
		// would slice the marker line away. Require the marker on its own line
		// (trimmed line == marker) so it can't match narration that merely quotes
		// the echo command. stable-timeout stays a safety cap for when the agent
		// forgets to print it. Reactive work — like blocking on `a2a recv
		// --wait N` — no longer risks a mid-wait stable-timeout kill, because the
		// real exit is now the marker, not silence.
		if opts.DoneMarker != "" && paneHasMarkerLine(rawPane, opts.DoneMarker) {
			if len(responseLines) > sentLineCount {
				for _, l := range responseLines[sentLineCount:] {
					if l != "" && !strings.Contains(l, opts.DoneMarker) {
						if onChunk != nil {
							onChunk(l)
						}
						allSentLines = append(allSentLines, l)
					}
				}
			}
			fmt.Fprintf(os.Stderr, "[debri] done-marker seen at poll %d, finishing\n", i)
			break
		}

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

// knownShells are the foreground commands that mean the pane has returned to an
// idle prompt. devin runs in `-p` (single-prompt) mode and exits when the task
// is complete, handing the pane back to the shell — a far more reliable "done"
// signal than a model-emitted marker (which the agent may narrate instead of
// run) or a silence timer (which a blocking `a2a recv --wait` trips falsely).
var knownShells = map[string]bool{
	"zsh": true, "-zsh": true, "bash": true, "-bash": true,
	"sh": true, "-sh": true, "dash": true, "fish": true, "-fish": true,
}

// paneForegroundIsShell reports whether the pane's current foreground command is
// an idle shell (i.e. devin has exited). Returns false on any tmux error so a
// transient failure never fakes a completion.
func paneForegroundIsShell(session string) bool {
	out, err := exec.Command("tmux", "display-message", "-p", "-t", session,
		"#{pane_current_command}").Output()
	if err != nil {
		return false
	}
	return knownShells[strings.TrimSpace(string(out))]
}

// paneHasMarkerLine reports whether any line of the raw tmux pane, trimmed of
// surrounding whitespace, equals the marker exactly. Exact-line match (not
// substring) so a narration line that quotes the echo command does not trip it.
func paneHasMarkerLine(rawPane, marker string) bool {
	for _, line := range strings.Split(rawPane, "\n") {
		if strings.TrimSpace(line) == marker {
			return true
		}
	}
	return false
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
