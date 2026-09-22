package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// acpStub writes a fake `devin` that speaks just enough ACP to drive Execute: it echoes each
// request's own id back (so we are testing real id correlation, not a fixed sequence) and emits
// the notification kinds devin actually sends. extra is injected into the session/prompt branch.
func acpStub(t *testing.T, extra string) string {
	t.Helper()
	binDir := t.TempDir()
	stub := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1}}\n' "$id" ;;
    *'"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"sess-1"}}\n' "$id" ;;
    *'"session/set_mode"'*)
      printf '%s\n' "$line" >> MODELOG
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id" ;;
    *'"session/prompt"'*)
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking hard"}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello "}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Listed ./","kind":"execute","content":[{"type":"content","content":{"type":"text","text":"total 4"}}]}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"world"}}}}\n'
EXTRA
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id" ;;
  esac
done
`
	stub = strings.Replace(stub, "MODELOG", filepath.Join(binDir, "modes.txt"), 1)
	stub = strings.Replace(stub, "EXTRA", extra, 1)
	if err := os.WriteFile(filepath.Join(binDir, "devin"), []byte(stub), 0755); err != nil {
		t.Fatalf("write acp stub: %v", err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	return binDir
}

// TestACP_StreamsChunksIncrementally is the whole point of #15: the caller must receive the
// agent's text as separate chunks as they arrive, not one blob at the end like `devin -p`.
func TestACP_StreamsChunksIncrementally(t *testing.T) {
	acpStub(t, "")

	var mu sync.Mutex
	var chunks []string
	out, err := Execute("hi", ExecOptions{
		WorkingDir: t.TempDir(), StableTimeout: 30000,
	}, func(c string) {
		mu.Lock()
		defer mu.Unlock()
		chunks = append(chunks, c)
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "hello world" {
		t.Errorf("content = %q, want %q", out, "hello world")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(chunks) < 2 {
		t.Errorf("expected incremental chunks, got %d: %v", len(chunks), chunks)
	}
}

// TestACP_EmitsToolEvents: structured tool activity is surfaced, which pane scraping could only
// ever approximate. Thoughts stay off unless asked for.
func TestACP_EmitsToolEvents(t *testing.T) {
	acpStub(t, "")

	var mu sync.Mutex
	var events []map[string]any
	_, err := Execute("hi", ExecOptions{
		WorkingDir: t.TempDir(), StableTimeout: 30000,
		OnEvent: func(ev map[string]any) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		},
	}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var tools, thoughts int
	for _, ev := range events {
		switch ev["event"] {
		case "tool":
			tools++
		case "thought":
			thoughts++
		}
	}
	if tools < 2 { // tool_call + tool_call_update
		t.Errorf("expected tool events, got %d (%v)", tools, events)
	}
	if thoughts != 0 {
		t.Errorf("thoughts must stay off unless --thoughts is set, got %d", thoughts)
	}
}

// TestACP_ThoughtsOptIn: reasoning is streamed only when the caller asks.
func TestACP_ThoughtsOptIn(t *testing.T) {
	acpStub(t, "")

	var mu sync.Mutex
	thoughts := 0
	_, err := Execute("hi", ExecOptions{
		WorkingDir: t.TempDir(), StableTimeout: 30000, Thoughts: true,
		OnEvent: func(ev map[string]any) {
			mu.Lock()
			defer mu.Unlock()
			if ev["event"] == "thought" {
				thoughts++
			}
		},
	}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if thoughts == 0 {
		t.Error("expected thought events with Thoughts: true")
	}
}

// TestACP_PermissionModeMapsToSessionMode: --permission-mode is translated to a devin session
// mode, since ACP has no --permission-mode flag. dangerous -> bypass, auto -> ask (read-only).
func TestACP_PermissionModeMapsToSessionMode(t *testing.T) {
	for _, tc := range []struct{ perm, want string }{
		{"dangerous", "bypass"},
		{"auto", "ask"},
		{"", "bypass"},
	} {
		if got := sessionModeFor(tc.perm); got != tc.want {
			t.Errorf("sessionModeFor(%q) = %q, want %q", tc.perm, got, tc.want)
		}
	}

	binDir := acpStub(t, "")
	if _, err := Execute("hi", ExecOptions{
		WorkingDir: t.TempDir(), StableTimeout: 30000, PermMode: "auto",
	}, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	modes, err := os.ReadFile(filepath.Join(binDir, "modes.txt"))
	if err != nil {
		t.Fatalf("stub recorded no session/set_mode call: %v", err)
	}
	if !strings.Contains(string(modes), `"modeId":"ask"`) {
		t.Errorf("auto must map to ask mode, got: %s", modes)
	}
}

// TestACP_FallsBackWhenHandshakeFails: a devin build that cannot speak ACP must degrade to the
// -p path rather than failing the run.
func TestACP_FallsBackWhenHandshakeFails(t *testing.T) {
	binDir := t.TempDir()
	// Speaks no ACP at all: exits immediately, exactly like an older devin given a subcommand
	// it does not know. In -p mode the same stub answers normally.
	stub := `#!/bin/sh
case "$1" in
  acp) echo 'unknown subcommand: acp' >&2; exit 2 ;;
  *) echo fallback-answer ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "devin"), []byte(stub), 0755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	out, err := Execute("hi", ExecOptions{WorkingDir: t.TempDir(), StableTimeout: 30000}, nil)
	if err != nil {
		t.Fatalf("expected fallback to succeed, got: %v", err)
	}
	if out != "fallback-answer" {
		t.Errorf("content = %q, want fallback-answer", out)
	}
}

// TestACP_DoneMarkerStopsTheTurn: with real streaming a done-marker is meaningful again (under
// -p nothing arrives before exit, so it could never fire early).
func TestACP_DoneMarkerStopsTheTurn(t *testing.T) {
	// The stub blocks after the marker, so only an early stop can finish the run in time.
	acpStub(t, "      sleep 30\n")

	out, err := Execute("hi", ExecOptions{
		WorkingDir: t.TempDir(), StableTimeout: 20000, DoneMarker: "world",
	}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "world") {
		t.Errorf("content = %q, want it to contain the marker", out)
	}
}

// TestACP_ToolCallWithArrayContentIsNotDropped guards a bug found live: devin sends `content` as
// an object on message chunks but as an ARRAY on tool_call. Decoding it into one fixed struct
// made every tool_call carrying content fail to parse and vanish, taking its title/kind with it.
func TestACP_ToolCallWithArrayContentIsNotDropped(t *testing.T) {
	acpStub(t, "")

	var mu sync.Mutex
	var titled int
	_, err := Execute("hi", ExecOptions{
		WorkingDir: t.TempDir(), StableTimeout: 30000,
		OnEvent: func(ev map[string]any) {
			mu.Lock()
			defer mu.Unlock()
			if ev["event"] == "tool" && ev["title"] == "Listed ./" && ev["kind"] == "execute" {
				titled++
			}
		},
	}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if titled == 0 {
		t.Error("tool_call with array content was dropped; title/kind never surfaced")
	}
}
