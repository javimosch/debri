package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

const Version = "1.2.0"

func main() {
	// Subcommand dispatch — checked before flag parsing so flags don't interfere.
	if len(os.Args) >= 2 && os.Args[1] == "probe" {
		// `debri probe [--timeout <s>]` — lightweight harness health check.
		// Used by automaintainer's probeHarness("debri") to verify devin is
		// functional (auth valid, tmux available) without starting a real session.
		timeout := 10
		for i := 2; i < len(os.Args)-1; i++ {
			if os.Args[i] == "--timeout" {
				fmt.Sscanf(os.Args[i+1], "%d", &timeout)
			}
		}
		os.Exit(runProbe(timeout))
	}

	fs := flag.NewFlagSet("debri", flag.ContinueOnError)
	fs.Usage = printHelp

	model := fs.String("model", "", `Model to use (e.g. "SWE-1.6", "Kimi K2.6", "claude-sonnet-4")`)
	permMode := fs.String("permission-mode", "dangerous", `Permission mode: "auto" or "dangerous"`)
	workingDir := fs.String("working-dir", "", "Working directory for the devin session")
	stream := fs.Bool("stream", false, "Emit streaming JSONL events to stdout")
	jsonOut := fs.Bool("json", false, "Emit final result as single JSON object")
	promptFile := fs.String("file", "", "Read prompt from file instead of argument")
	stableTimeout := fs.Int("stable-timeout", 5000, "Stability timeout in ms (silence = done)")
	doneMarker := fs.String("done-marker", "", "Finish as soon as the agent prints this line (stable-timeout becomes a safety cap). Useful for reactive agents that block on I/O and would otherwise trip the silence timer.")
	ver := fs.Bool("version", false, "Print version and exit")

	if err := fs.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(80)
	}

	if *ver {
		fmt.Printf("debri v%s\n", Version)
		os.Exit(0)
	}

	// Resolve prompt
	var prompt string
	if *promptFile != "" {
		data, err := os.ReadFile(*promptFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading prompt file: %v\n", err)
			os.Exit(80)
		}
		prompt = strings.TrimSpace(string(data))
	} else {
		args := fs.Args()
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "Error: prompt required (pass as argument or use --file)")
			printHelp()
			os.Exit(80)
		}
		prompt = strings.Join(args, " ")
	}

	if prompt == "" {
		fmt.Fprintln(os.Stderr, "Error: prompt cannot be empty")
		os.Exit(80)
	}

	opts := ExecOptions{
		Model:         *model,
		PermMode:      *permMode,
		WorkingDir:    *workingDir,
		StableTimeout: *stableTimeout,
		DoneMarker:    *doneMarker,
	}

	start := time.Now()

	if *stream {
		emitEvent(map[string]interface{}{"event": "init", "status": "ok"})
	}

	onChunk := func(chunk string) {
		if *stream {
			emitEvent(map[string]interface{}{"event": "chunk", "content": chunk})
		}
	}

	result, err := Execute(prompt, opts, onChunk)
	elapsed := int(time.Since(start).Milliseconds())

	if err != nil {
		if *stream {
			emitEvent(map[string]interface{}{"event": "error", "error": err.Error(), "elapsed_ms": elapsed})
		} else if *jsonOut {
			emitJSON(map[string]interface{}{"error": err.Error(), "elapsed_ms": elapsed})
		} else {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		os.Exit(100)
	}

	if *stream {
		emitEvent(map[string]interface{}{"event": "done", "content": result, "elapsed_ms": elapsed})
	} else if *jsonOut {
		emitJSON(map[string]interface{}{"content": result, "elapsed_ms": elapsed})
	} else {
		fmt.Println(result)
	}
}

func emitEvent(v map[string]interface{}) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func emitJSON(v map[string]interface{}) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func printHelp() {
	fmt.Fprintf(os.Stderr, `debri v%s - Fresh devin session invoker

Usage:
  debri [options] "<prompt>"
  debri [options] --file prompt.txt
  debri probe [--timeout <s>]   Verify devin is functional (used by AM harness probe)

Options:
  --model <model>            Model to use (default: devin default)
  --permission-mode <mode>   "auto" or "dangerous" (default: dangerous)
  --working-dir <dir>        Working directory for the session
  --stream                   Emit streaming JSONL events
  --json                     Emit final result as single JSON object
  --file <path>              Read prompt from file
  --stable-timeout <ms>      Stability timeout in ms (default: 5000)
  --done-marker <str>        Finish the instant the agent prints this line
                             (stable-timeout becomes a safety cap). For reactive
                             agents that block on I/O (e.g. a2a recv --wait).
  --version                  Print version
  -h, --help                 Show this help

JSONL stream events:
  {"event":"init","status":"ok"}
  {"event":"chunk","content":"..."}
  {"event":"done","content":"...","elapsed_ms":1234}
  {"event":"error","error":"...","elapsed_ms":1234}

Examples:
  debri "say hi"
  debri --model "SWE-1.6" --permission-mode dangerous "create a file"
  debri --working-dir ~/project --stream "list files"
  debri --json "summarize this"
  debri --file prompt.txt

Exit codes:
  0    Success
  80   User error (bad args)
  100  Integration error (devin failed)

probe exit codes:
  0    Healthy (devin responsive, no auth errors detected)
  1    Unhealthy (reason on stderr)
`, Version)
}
