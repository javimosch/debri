<p align="center">
  <img src="https://img.shields.io/badge/version-1.0.0-blue" alt="Version">
  <img src="https://img.shields.io/badge/license-MIT-green" alt="License">
  <img src="https://img.shields.io/badge/go-1.21-orange" alt="Go">
</p>

<h1 align="center">debri 🗑️ Fresh Devin Session Invoker</h1>

<p align="center">
  <b>Go CLI for fresh devin sessions with streaming JSONL output.</b><br>
  <b>Avoids session-reuse slowdowns.</b>
</p>

> Think: "devin, but fresh sessions every time"

## ⚡ TL;DR

> Single-shot devin CLI with streaming JSONL output — designed for automation.

```bash
# Fresh session, clean output
debri "List the files in src/"

# Streaming JSONL events
debri --stream "Analyze the codebase"

# Single JSON output
debri --json "Summarize this"

# Read prompt from file
debri --file prompt.txt

# With model selection
debri --model "SWE-1.6" "Create a file"
```

👉 Streaming JSONL events for monitoring
👉 Auto-strips devin TUI chrome
👉 Fresh sessions avoid slowdowns
👉 Single binary, no dependencies

## The Problem

Devin sessions get slower over time:
- **Session reuse** → accumulated state slows down each prompt
- **TUI chrome** → braille logos, status bars clutter output
- **No structured output** → parsing text responses is fragile
- **No automation** → interactive mode doesn't work in scripts

Without debri, automation struggles to:
1. Get clean, structured output from devin
2. Avoid session-reuse slowdowns
3. Monitor progress in real-time
4. Integrate with CI/CD pipelines

## The Solution

debri gives automation what it needs:
- **Fresh sessions** — new tmux session per prompt, auto-cleanup
- **Streaming JSONL** — structured events: init, chunk, done, error
- **TUI chrome stripping** — clean output, no braille logos or status bars
- **Single binary** — statically compiled Go, no runtime dependencies
- **Model support** — SWE-1.6, Kimi K2.6, and other devin-supported models
- **Auto-confirm dialogs** — handles workspace trust prompts automatically

With debri:
1. **Execute** with `debri "prompt"` — fresh session, clean output
2. **Monitor** with `--stream` — real-time JSONL events
3. **Integrate** with `--json` — single structured JSON response
4. **Automate** with `--file` — read prompts from files
5. **Customize** with `--model`, `--permission-mode`, `--working-dir`

---

## ⚡ Quick Start

```bash
# Quick install (curl)
curl -fsSL https://github.com/javimosch/debri/releases/download/v1.0.0/debri -o /usr/local/bin/debri && chmod +x /usr/local/bin/debri

# Fresh session, clean output
debri "What is Zig?"

# Streaming JSONL events
debri --stream "Analyze the project structure"

# Single JSON output
debri --json "Summarize this error"

# Read prompt from file
debri --file prompt.txt

# With model selection
debri --model "SWE-1.6" "Create a file"

# With working directory
debri --working-dir ~/project "List files"
```

---

## For Humans

| Instead of... | You do... |
|--------------|-----------|
| Slow devin sessions | `debri "prompt"` — fresh session every time |
| TUI chrome in output | Auto-stripped — clean output only |
| Text parsing | JSONL streaming or single JSON output |
| Manual trust prompts | Auto-confirmed automatically |

What this means day-to-day:
- **No session slowdowns** — fresh session every prompt
- **No TUI chrome** — clean output, no braille logos
- **No text parsing** — structured JSONL or JSON output
- **No manual prompts** — workspace trust auto-confirmed

## For Automation

- 🎯 **Fresh sessions** — New tmux session per prompt, auto-cleanup
- 📡 **Streaming JSONL** — Real-time events: init, chunk, done, error
- 🧹 **TUI chrome stripping** — Clean output, no braille logos or status bars
- 📦 **Single binary** — Statically compiled Go, no runtime dependencies
- 🤖 **Model support** — SWE-1.6, Kimi K2.6, and other devin-supported models
- 🔐 **Auto-confirm dialogs** — Handles workspace trust prompts automatically
- ⚙️ **Configurable** — Model, permission mode, working directory, timeout

```bash
# Automation workflow: fresh session → stream → json → file
debri "prompt"                    # Fresh session, clean output
debri --stream "task"            # JSONL streaming events
debri --json "task"              # Single JSON response
debri --file prompt.txt          # Read prompt from file
```

---

## What You Get

debri gives automation a fresh devin session invoker:

- 🎯 **Fresh sessions** — New tmux session per prompt, auto-cleanup
- 📡 **Streaming JSONL** — Real-time events: init, chunk, done, error
- 🧹 **TUI chrome stripping** — Clean output, no braille logos or status bars
- 📦 **Single binary** — Statically compiled Go, no runtime dependencies
- 🤖 **Model support** — SWE-1.6, Kimi K2.6, and other devin-supported models
- 🔐 **Auto-confirm dialogs** — Handles workspace trust prompts automatically
- ⚙️ **Configurable** — Model, permission mode, working directory, timeout
- 📝 **Prompt files** — Read prompts from files for complex workflows

---

## 🛠️ CLI Usage Examples

```bash
# Fresh session, clean output
debri "What is Zig?"
debri --model "SWE-1.6" "Explain this error"

# Streaming JSONL events
debri --stream "Run: echo hello"
debri --stream "Analyze src/"

# Single JSON output
debri --json "Summarize this"
debri --json "What is 2+2?"

# Read prompt from file
debri --file prompt.txt
debri --file complex-prompt.txt

# With model selection
debri --model "SWE-1.6" "Create a file"
debri --model "Kimi K2.6" "Analyze this"

# With working directory
debri --working-dir ~/project "List files"
debri --working-dir ~/project --stream "Analyze code"

# With permission mode
debri --permission-mode dangerous "Execute command"
debri --permission-mode auto "Read-only task"

# With stability timeout
debri --stable-timeout 10000 "Long-running task"
```

---

## 🏗️ Architecture

### Core Design

debri is a **fresh session invoker** for devin:

- **Fresh sessions** — New tmux session per prompt, auto-cleanup
- **Streaming JSONL** — Structured events for monitoring
- **TUI chrome stripping** — Clean output, no braille logos or status bars
- **Single binary** — Statically compiled Go, no runtime dependencies

### Session Lifecycle

1. **Create tmux session** — Unique name: `devin-debri-<timestamp>`
2. **Write prompt file** — `<workdir>/.devin/debri-prompt-<timestamp>.txt`
3. **Invoke devin** — `devin -p --permission-mode dangerous --prompt-file <file>`
4. **Poll pane** — Every 250ms using watermark approach
5. **Strip chrome** — Remove TUI chrome from output
6. **Cleanup** — Kill tmux session, remove prompt file

### TUI Chrome Stripping

Automatically strips devin TUI chrome:
- Braille art (logo)
- Version strings
- Status bars (Pro · X% remaining)
- Context indicators
- Thinking/Running indicators
- Workspace trust prompts (auto-confirmed)
- Shell prompts

### Streaming JSONL Events

```json
{"event":"init","status":"ok"}
{"event":"chunk","content":"..."}
{"event":"done","content":"...","elapsed_ms":1234}
{"event":"error","error":"...","elapsed_ms":1234}
```

---

## 📦 Installation

### Quick Install (curl)

```bash
curl -fsSL https://github.com/javimosch/debri/releases/download/v1.0.0/debri -o /usr/local/bin/debri && chmod +x /usr/local/bin/debri
```

### Build from Source

```bash
git clone https://github.com/javimosch/debri.git
cd debri
go build -o debri .
sudo mv debri /usr/local/bin/
```

### Requirements

- Go 1.21+ (for building from source)
- tmux (for session management)
- devin CLI (for execution)

---

## 🎯 CLI Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--model <model>` | string | devin default | Model to use (e.g., "SWE-1.6", "Kimi K2.6") |
| `--permission-mode <mode>` | string | dangerous | Permission mode: "auto" or "dangerous" |
| `--working-dir <dir>` | string | cwd | Working directory for the session |
| `--stream` | flag | false | Emit streaming JSONL events |
| `--json` | flag | false | Emit final result as single JSON object |
| `--file <path>` | string | — | Read prompt from file |
| `--stable-timeout <ms>` | int | 5000 | Stability timeout in ms (silence = done) |
| `--version` | flag | — | Print version |
| `-h, --help` | flag | — | Show help |

---

## 🚨 Exit Codes

| Code | Meaning |
|------|---------|
| `0` | Success |
| `80` | User error (bad args) |
| `100` | Integration error (devin failed) |

---

## 📚 Documentation

- [Landing Page](docs/index.html) — Visual overview and architecture
- [Changelog](docs/changelog-2026-06.html) — Product and technical changes
- [AGENTS.md](AGENTS.md) — Agent guide for contributing

---

## 🤝 Contributing

debri is designed for automation and CI/CD integration. Contributions are welcome:

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Submit a pull request

---

## 📄 License

MIT License — see LICENSE file for details.

---

## 🙏 Acknowledgments

- Built with Go 1.21
- Inspired by devin-bridge TypeScript implementation
- Uses tmux for session management
