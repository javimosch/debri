package main

import "strings"

import "testing"

// TestBuildDevinCommand_RedirectsStdout guards the content-capture fix: devin
// `-p` buffers its whole response and only prints it at exit, and renders a
// redrawing TUI to the pane in the meantime — so the response is captured by
// redirecting devin's stdout to a file, NOT by scraping the pane. The built
// command must therefore append `> '<outFile>'`, keep `-p --prompt-file`, and
// shell-quote every path/flag value.
func TestBuildDevinCommand_RedirectsStdout(t *testing.T) {
	opts := ExecOptions{Model: "SWE-1.7", PermMode: "dangerous"}
	cmd := buildDevinCommand(opts, "/w/.devin/prompt.txt", "/w/.devin/out.txt")

	wants := []string{
		"-p --prompt-file '/w/.devin/prompt.txt'",
		"> '/w/.devin/out.txt'",
		"--model 'SWE-1.7'",
		"--permission-mode 'dangerous'",
	}
	for _, w := range wants {
		if !strings.Contains(cmd, w) {
			t.Errorf("built command missing %q\n  got: %s", w, cmd)
		}
	}
	// The redirect must come after the prompt-file so the shell applies it to the
	// devin invocation, not to a flag argument.
	if strings.Index(cmd, ">") < strings.Index(cmd, "--prompt-file") {
		t.Errorf("redirect must follow --prompt-file\n  got: %s", cmd)
	}
}

// TestBuildDevinCommand_NoOutFile: an empty outFile means no redirect is added
// (fallback to pane-scraped output), so downstream callers that don't want a
// file still get a valid command.
func TestBuildDevinCommand_NoOutFile(t *testing.T) {
	cmd := buildDevinCommand(ExecOptions{}, "/w/p.txt", "")
	if strings.Contains(cmd, ">") {
		t.Errorf("no redirect expected when outFile empty\n  got: %s", cmd)
	}
	if !strings.Contains(cmd, "-p --prompt-file '/w/p.txt'") {
		t.Errorf("prompt-file flag missing\n  got: %s", cmd)
	}
}
