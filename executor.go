package main

import (
	"bufio"
	"context"
	"errors"
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
	PermMode      string // "auto" (read-only) or "dangerous"
	WorkingDir    string // working directory for the session
	StableTimeout int    // ms: hard cap on the whole run
	DoneMarker    string // finish as soon as this text appears (meaningful again under ACP)
	Thoughts      bool   // stream the agent's reasoning as {"event":"thought"}
	NoACP         bool   // force the legacy `devin -p` path instead of ACP
	// OnEvent receives the richer structured events ACP makes available (tool calls,
	// thoughts). nil is fine; plain text chunks still go to Execute's onChunk.
	OnEvent func(map[string]any)
}

// defaultRunTimeoutMs bounds a run when the caller does not pass --stable-timeout. Pre-1.3.0
// this was a 5s *silence* timer against a scraped tmux pane; as a hard cap on the run, 5s would
// kill every real task, so the default is the old pane-scraper's 10 minute tick ceiling.
const defaultRunTimeoutMs = 600000

var safeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Execute runs one prompt through devin and returns the agent's answer.
//
// Preferred path is ACP (acp.go): `devin acp` speaks JSON-RPC over stdio and streams
// `agent_message_chunk` notifications token by token, plus structured tool-call events. The
// fallback is `devin -p`, which is correct but buffers its entire answer until exit -- so it can
// only ever report progress once, at the end.
//
// Neither path uses tmux. That layer existed to scrape a TUI pane for output and liveness;
// output was already being redirected to a file because the pane was unreliable, and liveness is
// what cmd.Wait() / the ACP turn result report exactly. Removing it also removed the orphaned
// session leak (#14) by construction, since SIGKILL can never run a deferred cleanup.
func Execute(prompt string, opts ExecOptions, onChunk func(string)) (string, error) {
	if prompt == "" {
		return "", fmt.Errorf("prompt cannot be empty")
	}
	workDir, err := resolveWorkDir(opts)
	if err != nil {
		return "", err
	}
	if opts.Model != "" && !isValidModel(opts.Model) {
		return "", fmt.Errorf("invalid model name: %q", opts.Model)
	}

	timeoutMs := opts.StableTimeout
	if timeoutMs <= 0 {
		timeoutMs = defaultRunTimeoutMs
	}
	opts.StableTimeout = timeoutMs
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	if !opts.NoACP {
		content, err := executeACP(ctx, prompt, opts, workDir, onChunk)
		if err == nil {
			return content, nil
		}
		// Only a failure to *establish* the session falls back. Once the agent is running, a
		// real error must surface rather than silently re-running expensive work on -p.
		if !errors.Is(err, errACPUnavailable) {
			return content, err
		}
		fmt.Fprintf(os.Stderr, "[debri] acp unavailable (%v) — falling back to devin -p\n", err)
		if ctx.Err() != nil {
			// The handshake consumed the whole budget; report that plainly rather than
			// letting the fallback fail with an opaque "cannot start devin".
			return "", fmt.Errorf("devin exceeded the %dms cap", opts.StableTimeout)
		}
	}
	return executePrint(ctx, prompt, opts, workDir, onChunk)
}

func resolveWorkDir(opts ExecOptions) (string, error) {
	if opts.WorkingDir != "" {
		return opts.WorkingDir, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cannot determine working directory: %w", err)
	}
	return wd, nil
}

// killOnSignal arranges for the child's process group to die with us. Deferred cleanup does not
// run for a signal-terminated process, so without this a SIGTERMed debri would leave devin
// running and burning credits unseen.
func killOnSignal(pid func() int) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-ch:
			fmt.Fprintf(os.Stderr, "[debri] received %v, terminating devin\n", sig)
			if p := pid(); p > 0 {
				syscall.Kill(-p, syscall.SIGKILL) //nolint:errcheck
			}
			os.Exit(130)
		case <-done:
		}
	}()
	return func() { signal.Stop(ch); close(done) }
}

// executePrint is the pre-ACP path: run `devin -p` as a child process and read its stdout. Kept
// as the fallback for a devin build whose `acp` surface we cannot speak to.
func executePrint(ctx context.Context, prompt string, opts ExecOptions, workDir string, onChunk func(string)) (string, error) {
	// The prompt goes to a file rather than an argv entry so a multi-KB briefing cannot hit
	// ARG_MAX. Prefer the working dir's .devin/ (devin already owns that path), falling back to
	// /tmp when the workspace is not writable.
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

	// argv, not a shell string: no quoting, and therefore no shell-injection surface.
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
	// Non-interactive mode cannot answer devin's workspace-trust dialog, and debri always owns
	// --working-dir, so the trust check can only ever deadlock us here.
	args = append(args, "--respect-workspace-trust", "false", "-p", "--prompt-file", promptFile)

	cmd := exec.CommandContext(ctx, devinPath, args...)
	cmd.Dir = workDir
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
	stopSignals := killOnSignal(func() int {
		if cmd.Process != nil {
			return cmd.Process.Pid
		}
		return 0
	})
	defer stopSignals()

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
		if opts.DoneMarker != "" && strings.Contains(line, opts.DoneMarker) {
			if cmd.Process != nil {
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck
			}
			break
		}
	}
	wg.Wait()

	waitErr := cmd.Wait()
	content := strings.TrimSpace(out.String())

	if ctx.Err() == context.DeadlineExceeded {
		return content, fmt.Errorf("devin exceeded the %dms cap", opts.StableTimeout)
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

// isValidModel keeps obviously malformed model names out of the argv. With no shell involved
// this is plain validation, not an injection defence.
func isValidModel(model string) bool {
	known := []string{"SWE-1.6", "Kimi K2.6", "claude-sonnet-4", "claude-opus-4.6", "opus", "codex", "adaptive"}
	for _, k := range known {
		if model == k {
			return true
		}
	}
	return safeNameRe.MatchString(model)
}
