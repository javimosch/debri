package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// runProbe verifies that devin is functional without starting a real coding
// session. It runs "devin --version" inside a fresh tmux session (the same
// execution path as a normal debri run) and scans the output for auth-failure
// patterns. AM calls this via `debri probe` before reporting the harness ready.
//
// Exit 0 = healthy.
// Exit 1 = unhealthy (reason on stderr, one line).
func runProbe(timeoutSecs int) int {
	// 1. Check devin CLI is in PATH.
	devinPath, err := exec.LookPath("devin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe: devin not found in PATH — install the Devin CLI and ensure it is executable")
		return 1
	}

	// 2. Check tmux is available (debri requires it for all runs).
	if _, err := exec.LookPath("tmux"); err != nil {
		fmt.Fprintln(os.Stderr, "probe: tmux not found in PATH — install tmux")
		return 1
	}

	// 3. Run "devin --version" inside a fresh tmux session.
	//    Using the full tmux path mirrors what Execute() does so any tmux-level
	//    permission or config issue is caught here rather than at run time.
	session := fmt.Sprintf("debri-probe-%d", time.Now().UnixMilli())
	if err := tmuxNew(session, os.TempDir()); err != nil {
		fmt.Fprintf(os.Stderr, "probe: failed to create tmux session: %v\n", err)
		return 1
	}
	defer tmuxKill(session) //nolint:errcheck

	// Use the session name as target (no :window.pane suffix) so the probe is
	// not sensitive to the operator's tmux pane-base-index setting.
	probeSend := func(keys string) error {
		return exec.Command("tmux", "send-keys", "-t", session, keys).Run()
	}
	probeCapture := func() (string, error) {
		out, err := exec.Command("tmux", "capture-pane", "-t", session, "-p", "-J").Output()
		return string(out), err
	}

	cmd := shellQuote(devinPath) + " --version"
	if err := probeSend(cmd); err != nil {
		fmt.Fprintf(os.Stderr, "probe: send command: %v\n", err)
		return 1
	}
	if err := probeSend("Enter"); err != nil {
		fmt.Fprintf(os.Stderr, "probe: send Enter: %v\n", err)
		return 1
	}

	// 4. Poll for output until stable or timeout.
	deadline := time.Now().Add(time.Duration(timeoutSecs) * time.Second)
	prev := ""
	stableSince := time.Time{}
	const stableWindow = 500 * time.Millisecond
	var captured string
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		out, err := probeCapture()
		if err != nil {
			continue
		}
		trimmed := strings.TrimSpace(out)
		if trimmed == "" {
			continue
		}
		if trimmed == prev {
			if !stableSince.IsZero() && time.Since(stableSince) >= stableWindow {
				captured = trimmed
				break
			}
		} else {
			prev = trimmed
			stableSince = time.Now()
		}
	}
	if captured == "" {
		captured = strings.TrimSpace(prev)
	}

	// 5. No output at all — devin didn't respond.
	if captured == "" {
		fmt.Fprintf(os.Stderr, "probe: devin produced no output within %ds\n", timeoutSecs)
		return 1
	}

	// 6. Scan for auth/credential error patterns.
	lower := strings.ToLower(captured)
	authPatterns := []string{
		"unauthorized", "unauthenticated",
		"401", "403",
		"not logged in", "not authenticated",
		"please log in", "please sign in",
		"login required", "sign in required",
		"invalid token", "token expired", "token invalid",
		"session expired", "session limit",
		"quota exceeded", "rate limit",
		"permission denied", "access denied",
	}
	for _, pat := range authPatterns {
		if strings.Contains(lower, pat) {
			// Surface first 120 chars of output so operators see the real error.
			excerpt := captured
			if len(excerpt) > 120 {
				excerpt = excerpt[:120] + "…"
			}
			fmt.Fprintf(os.Stderr, "probe: devin auth error — %s\n", excerpt)
			return 1
		}
	}

	// 7. Output present and no error patterns — devin is responsive.
	fmt.Printf("probe: ok (devin responsive: %s)\n", firstLine(captured))
	return 0
}

// firstLine returns the first non-empty line of s.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return strings.TrimSpace(s)
}
