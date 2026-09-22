package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecute_EmptyPromptRejected: an empty prompt is a caller bug, not something
// to hand to devin.
func TestExecute_EmptyPromptRejected(t *testing.T) {
	if _, err := Execute("", ExecOptions{}, nil); err == nil {
		t.Error("empty prompt must be rejected")
	}
}

// TestExecute_InvalidModelRejected guards the argv from obviously malformed model
// names before we spawn anything.
func TestExecute_InvalidModelRejected(t *testing.T) {
	_, err := Execute("hi", ExecOptions{Model: "bad model; rm -rf /", NoACP: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid model name") {
		t.Errorf("malformed model must be rejected, got: %v", err)
	}
}

// TestExecutePrint_WritesPromptToFileNotArgv (-p fallback path): the briefing is handed over as a file so a
// multi-KB prompt can never hit ARG_MAX. We assert the file is created under the
// working dir's .devin/ and cleaned up afterwards, by pointing devin at a stub that
// records the argv it was given.
func TestExecutePrint_WritesPromptToFileNotArgv(t *testing.T) {
	workDir := t.TempDir()
	binDir := t.TempDir()
	argvLog := filepath.Join(binDir, "argv.txt")

	stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvLog + "\necho stub-response\n"
	if err := os.WriteFile(filepath.Join(binDir, "devin"), []byte(stub), 0755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	prompt := strings.Repeat("x", 4096)
	out, err := Execute(prompt, ExecOptions{WorkingDir: workDir, StableTimeout: 30000, NoACP: true}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "stub-response" {
		t.Errorf("content = %q, want stub-response", out)
	}

	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("stub did not record argv: %v", err)
	}
	got := string(argv)
	if strings.Contains(got, prompt) {
		t.Error("prompt body must not be passed in argv")
	}
	for _, want := range []string{"--prompt-file", "-p", "--permission-mode", "--respect-workspace-trust", "false"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q\n  got: %s", want, got)
		}
	}

	// The prompt file is removed once devin has read it.
	entries, _ := os.ReadDir(filepath.Join(workDir, ".devin"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "debri-prompt-") {
			t.Errorf("prompt file %s was left behind", e.Name())
		}
	}
}

// TestExecute_SurfacesStderrOnFailure: when devin exits non-zero with nothing on
// stdout, the caller gets devin's own stderr rather than a bare "exit status 1".
func TestExecutePrint_SurfacesStderrOnFailure(t *testing.T) {
	workDir := t.TempDir()
	binDir := t.TempDir()
	stub := "#!/bin/sh\necho 'devin exploded' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "devin"), []byte(stub), 0755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	_, err := Execute("hi", ExecOptions{WorkingDir: workDir, StableTimeout: 30000, NoACP: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "devin exploded") {
		t.Errorf("stderr must be surfaced, got: %v", err)
	}
}

// TestExecute_HardTimeoutKillsDevin: --stable-timeout is a hard cap on the run now
// (it used to be a silence timer against a scraped pane). A hung devin must be
// killed and reported, not waited on forever.
func TestExecutePrint_HardTimeoutKillsDevin(t *testing.T) {
	workDir := t.TempDir()
	binDir := t.TempDir()
	stub := "#!/bin/sh\nsleep 60\n"
	if err := os.WriteFile(filepath.Join(binDir, "devin"), []byte(stub), 0755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	_, err := Execute("hi", ExecOptions{WorkingDir: workDir, StableTimeout: 300, NoACP: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("expected a timeout error, got: %v", err)
	}
}

// TestExecute_NoTmuxSessionsCreated is the regression guard for #14: the executor
// must not create tmux sessions at all, so there is nothing that can be orphaned
// when debri is SIGKILLed.
func TestExecutePrint_NoTmuxSessionsCreated(t *testing.T) {
	workDir := t.TempDir()
	binDir := t.TempDir()
	tmuxLog := filepath.Join(binDir, "tmux-was-called.txt")

	// Shadow tmux: if the executor still shells out to it, this records the call.
	tmuxStub := "#!/bin/sh\necho called > " + tmuxLog + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(tmuxStub), 0755); err != nil {
		t.Fatalf("write tmux stub: %v", err)
	}
	devinStub := "#!/bin/sh\necho ok\n"
	if err := os.WriteFile(filepath.Join(binDir, "devin"), []byte(devinStub), 0755); err != nil {
		t.Fatalf("write devin stub: %v", err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	if _, err := Execute("hi", ExecOptions{WorkingDir: workDir, StableTimeout: 30000, NoACP: true}, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(tmuxLog); err == nil {
		t.Error("executor invoked tmux — sessions can leak again (#14)")
	}
}
