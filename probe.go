package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// runProbe verifies that devin is functional without starting a real coding
// session: it runs `devin --version` and scans the output for auth-failure
// patterns. AM calls this via `debri probe` before reporting the harness ready.
//
// This used to run inside a fresh tmux session and poll the pane. That was the
// direct cause of #14: cleanup hung off `defer tmuxKill(session)`, and a caller
// enforcing its own timeout (Go's exec.CommandContext sends SIGKILL) skips defers
// entirely -- so every killed probe orphaned a session forever. Observed on rbm4:
// 26 stranded `debri-probe-*` sessions in six hours, one idle shell each.
//
// A plain child process has nothing to leak, reports its own exit status, and dies
// with its parent.
//
// Exit 0 = healthy. Exit 1 = unhealthy (reason on stderr, one line).
func runProbe(timeoutSecs int) int {
	devinPath, err := exec.LookPath("devin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe: devin not found in PATH — install the Devin CLI and ensure it is executable")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, devinPath, "--version").CombinedOutput()
	captured := strings.TrimSpace(string(out))

	if ctx.Err() == context.DeadlineExceeded {
		fmt.Fprintf(os.Stderr, "probe: devin produced no output within %ds\n", timeoutSecs)
		return 1
	}
	if err != nil && captured == "" {
		fmt.Fprintf(os.Stderr, "probe: devin --version failed: %v\n", err)
		return 1
	}
	if captured == "" {
		fmt.Fprintln(os.Stderr, "probe: devin produced no output")
		return 1
	}

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
			excerpt := captured
			if len(excerpt) > 120 {
				excerpt = excerpt[:120] + "…"
			}
			fmt.Fprintf(os.Stderr, "probe: devin auth error — %s\n", excerpt)
			return 1
		}
	}

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
