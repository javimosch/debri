package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

type ExecOptions struct {
	Model         string // devin model name (empty = devin default)
	PermMode      string // "auto" or "dangerous"
	WorkingDir    string // working directory for the session
	StableTimeout int    // ms: hard cap on the whole run (see note below)
	DoneMarker    string // retained for CLI compatibility; see note below
}

// defaultRunTimeoutMs bounds a run when the caller does not pass --stable-timeout.
// This used to be a 5s *silence* timer against a scraped tmux pane; as a hard cap
// on the process, 5s would kill every real task, so the default is the old
// pane-scraper's 10 minute tick ceiling instead.
const defaultRunTimeoutMs = 600000

var safeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Execute runs devin as an ordinary child process and returns its response.
//
// It used to drive devin inside a tmux session and scrape the pane. That existed
// to solve two problems, neither of which is real any more:
//
//   - Clean output. Already not tmux's job: the old code redirected devin's stdout
//     to a file precisely because the redrawing pane was unreliable to scrape.
//     devin's `-p` mode writes plain text to stdout when stdout is not a TTY, so a
//     pipe gives the same clean result directly.
//   - Liveness / completion. This is exactly what cmd.Wait() reports, exactly, with
//     no poll interval, no silence heuristics and no "crashed session looked like
//     success" failure mode.
//
// Dropping tmux also removes the orphaned-session leak (#14) by construction: a
// SIGKILLed debri could never clean up its session, because SIGKILL cannot be
// trapped. There is now no session to leak, and killing debri kills devin with it
// (own process group) instead of leaving it running and burning credits unseen.
//
// Trade-off, stated plainly: devin `-p` buffers its entire response and emits it at
// exit (measured: every line of an 18s run arrives at +18s). So onChunk fires once,
// at the end, and callers no longer see intermediate progress the way pane scraping
// showed it. Genuine incremental streaming needs `devin acp` (Agent Client Protocol
// over stdio), not a TUI scraper -- that is the right follow-up, not this layer.
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
	if opts.Model != "" && !isValidModel(opts.Model) {
		return "", fmt.Errorf("invalid model name: %q", opts.Model)
	}

	// The prompt goes to a file rather than an argv entry so a multi-KB briefing
	// cannot hit ARG_MAX. Prefer the working dir's .devin/ (devin already owns that
	// path), falling back to /tmp when the workspace is not writable.
	devinDir := filepath.Join(workDir, ".devin")
	if err := os.MkdirAll(devinDir, 0755); err != nil {
		devinDir = os.TempDir()
	}
	promptFile := filepath.Join(devinDir, fmt.Sprintf("debri-prompt-%d.txt", time.Now().UnixMilli()))
	if err := os.WriteFile(promptFile, []byte(prompt), 0644); err != nil {
		return "", fmt.Errorf("cannot write prompt file: %w", err)
	}
	defer os.Remove(promptFile) //nolint:errcheck

	devinPath, err := exec.LookPath("devin")
	if err != nil {
		return "", fmt.Errorf("devin not found in PATH: %w", err)
	}

	// argv, not a shell string: no quoting, and therefore no shell-injection surface
	// for the model/path values to defend against.
	args := []string{}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	permMode := opts.PermMode
	if permMode == "" {
		permMode = "dangerous"
	}
	args = append(args, "--permission-mode", permMode)
	if cfg := filepath.Join(workDir, ".devin", "config.json"); fileExists(cfg) {
		args = append(args, "--config", cfg)
	}
	// Non-interactive mode cannot answer devin's workspace-trust dialog, and debri
	// always owns --working-dir, so the trust check can only ever deadlock us here.
	args = append(args, "--respect-workspace-trust", "false", "-p", "--prompt-file", promptFile)

	timeoutMs := opts.StableTimeout
	if timeoutMs <= 0 {
		timeoutMs = defaultRunTimeoutMs
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, devinPath, args...)
	cmd.Dir = workDir
	// Own process group: a timeout or a signal kills devin and anything it spawned,
	// rather than orphaning a running agent.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}

	fmt.Fprintf(os.Stderr, "[debri] exec %s %s\n", devinPath, strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("cannot start devin: %w", err)
	}

	killGroup := func() {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck
		}
	}
	// SIGTERM/SIGINT from a caller must take devin down too; deferred cleanup does
	// not run for a signal-terminated process.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)
	go func() {
		if sig, ok := <-sigCh; ok {
			fmt.Fprintf(os.Stderr, "[debri] received %v, terminating devin\n", sig)
			killGroup()
			os.Exit(130)
		}
	}()

	var out strings.Builder
	var stderrBuf strings.Builder
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // stderr is diagnostics only; devin's answer is on stdout
		defer wg.Done()
		b, _ := io.ReadAll(stderrPipe)
		stderrBuf.Write(b)
	}()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 32*1024*1024) // a long answer is one long line
	for sc.Scan() {
		line := sc.Text()
		out.WriteString(line)
		out.WriteString("\n")
		if onChunk != nil {
			onChunk(line)
		}
		// DoneMarker is retained for flag compatibility, but devin -p emits nothing
		// before exit, so in practice the process has already finished by the time a
		// marker could match. Honour it anyway for any caller/mode that does stream.
		if opts.DoneMarker != "" && strings.Contains(line, opts.DoneMarker) {
			killGroup()
			break
		}
	}
	wg.Wait()

	waitErr := cmd.Wait()
	content := strings.TrimSpace(out.String())

	if ctx.Err() == context.DeadlineExceeded {
		return content, fmt.Errorf("devin exceeded the %dms cap", timeoutMs)
	}
	if waitErr != nil && content == "" {
		msg := strings.TrimSpace(stderrBuf.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return "", fmt.Errorf("devin failed: %s", msg)
	}
	return content, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// isValidModel keeps obviously malformed model names out of the argv. With no shell
// involved this is plain validation, not an injection defence.
func isValidModel(model string) bool {
	known := []string{"SWE-1.6", "Kimi K2.6", "claude-sonnet-4", "claude-opus-4.6", "opus", "codex", "adaptive"}
	for _, k := range known {
		if model == k {
			return true
		}
	}
	return safeNameRe.MatchString(model)
}
