package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// acp.go drives `devin acp` -- devin's Agent Client Protocol server over stdio -- instead of
// shelling out to `devin -p`. See #15.
//
// The point is streaming. `devin -p` buffers its whole answer and emits it at exit (measured:
// every line of an 18s run arrives at +18s), so a -p child can only ever report progress once,
// at the end. ACP delivers `agent_message_chunk` notifications token by token, plus structured
// `tool_call` / `tool_call_update` events, so a caller can actually watch a long task work.
//
// This is not an exotic transport: `devin -p` itself runs its agent in a `devin acp` child (per
// devin's own --model help text). Speaking ACP directly removes the buffering client rather than
// working around it.
//
// Protocol shape, as observed against devin 3000.11.1 rather than assumed:
//   - newline-delimited JSON-RPC 2.0 (no Content-Length framing)
//   - initialize -> {"protocolVersion":1,"agentCapabilities":{...}}
//   - session/new {cwd,mcpServers} -> {"sessionId":"...","modes":{...}}
//   - session/set_mode {sessionId,modeId} -> {}
//   - session/prompt {sessionId,prompt:[{type,text}]} -> {"stopReason":"end_turn","usage":{...}}
//   - notifications arrive as session/update with params.update.sessionUpdate naming the kind

// errACPUnavailable marks a failure to *establish* an ACP session (binary too old, handshake
// broken/timed out). Only this is allowed to fall back to the -p path: once the agent is running,
// a real error must surface rather than silently re-running expensive work a second time.
var errACPUnavailable = errors.New("acp unavailable")

// acpHandshakeTimeout bounds only the initialize/session-setup round trips. devin's own startup
// measured 1-3s in practice, so a longer window would only delay a fallback that is already
// doomed -- and it is spent from the caller's overall budget.
const acpHandshakeTimeout = 12 * time.Second

type acpClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	writeM sync.Mutex

	mu      sync.Mutex
	nextID  int
	pending map[int]chan acpResponse

	opts    ExecOptions
	onChunk func(string)

	textM sync.Mutex
	text  strings.Builder

	doneMarkerHit chan struct{}
	markerOnce    sync.Once

	// closed fires when the agent's stdout ends, so a request never waits out the full
	// timeout for a process that has already died (e.g. a devin build with no `acp`).
	closed chan struct{}
}

type acpResponse struct {
	result json.RawMessage
	err    *acpError
}

type acpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type acpMessage struct {
	ID     *int            `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *acpError       `json:"error,omitempty"`
}

// sessionModeFor maps debri's --permission-mode onto a devin ACP session mode.
// Observed modes: accept-edits ("Code"), smart, ask, plan, bypass ("Bypass Permissions").
func sessionModeFor(permMode string) string {
	switch permMode {
	case "auto":
		// auto means read-only in debri's contract ("never edits"), which `ask` enforces:
		// the agent will not edit without approval, and we decline approvals in this mode.
		return "ask"
	default: // "dangerous" and anything unrecognised
		return "bypass"
	}
}

func (c *acpClient) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeM.Lock()
	defer c.writeM.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

func (c *acpClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan acpResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.send(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	}); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, fmt.Errorf("acp stream closed before %s was answered", method)
	case resp := <-ch:
		if resp.err != nil {
			return nil, fmt.Errorf("%s: %s (code %d)", method, resp.err.Message, resp.err.Code)
		}
		return resp.result, nil
	}
}

func (c *acpClient) notify(method string, params any) {
	c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params}) //nolint:errcheck
}

// readLoop demultiplexes the agent's stdout: responses go to their waiting request, agent->client
// requests get answered (never left hanging, which would deadlock the turn), and notifications
// become streamed events.
func (c *acpClient) readLoop(stdout io.Reader, done chan<- error) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 32*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg acpMessage
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue // not JSON-RPC (stray output); ignore rather than abort the turn
		}
		switch {
		case msg.ID != nil && msg.Method == "": // response to one of our requests
			c.mu.Lock()
			ch := c.pending[*msg.ID]
			c.mu.Unlock()
			if ch != nil {
				ch <- acpResponse{result: msg.Result, err: msg.Error}
			}
		case msg.ID != nil && msg.Method != "": // agent -> client request: must always answer
			c.answerAgentRequest(*msg.ID, msg.Method, msg.Params)
		default:
			c.handleNotification(msg.Method, msg.Params)
		}
	}
	close(c.closed)
	done <- sc.Err()
}

func (c *acpClient) answerAgentRequest(id int, method string, params json.RawMessage) {
	reply := func(result any) {
		c.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) //nolint:errcheck
	}
	switch method {
	case "session/request_permission":
		// In bypass mode approve; in ask mode (debri's read-only "auto") decline, so an edit
		// attempt is refused rather than silently allowed -- and never left hanging either way.
		if sessionModeFor(c.opts.PermMode) != "bypass" {
			reply(map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
			return
		}
		var p struct {
			Options []struct {
				OptionID string `json:"optionId"`
				Kind     string `json:"kind"`
			} `json:"options"`
		}
		json.Unmarshal(params, &p) //nolint:errcheck
		pick := ""
		for _, o := range p.Options {
			if o.Kind == "allow_always" || o.Kind == "allow_once" {
				pick = o.OptionID
				break
			}
		}
		if pick == "" && len(p.Options) > 0 {
			pick = p.Options[0].OptionID
		}
		if pick == "" {
			reply(map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
			return
		}
		reply(map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": pick}})
	case "fs/read_text_file", "fs/write_text_file":
		// We advertise no client filesystem; the agent has its own tools for this.
		c.send(map[string]any{"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": -32601, "message": "client filesystem not provided"}}) //nolint:errcheck
	default:
		reply(map[string]any{})
	}
}

func (c *acpClient) handleNotification(method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	// `content` is not one shape: an object {"type":"text","text":...} on message/thought
	// chunks, but an ARRAY on tool_call. Decoding it into a fixed struct made every tool_call
	// that carried content fail to unmarshal and be dropped silently, losing its title/kind.
	// Keep it raw and decode per kind.
	var p struct {
		Update struct {
			SessionUpdate string          `json:"sessionUpdate"`
			Content       json.RawMessage `json:"content"`
			ToolCallID    string          `json:"toolCallId"`
			Title         string          `json:"title"`
			Kind          string          `json:"kind"`
			Status        string          `json:"status"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	u := p.Update
	text := func() string {
		var c struct {
			Text string `json:"text"`
		}
		if len(u.Content) == 0 || json.Unmarshal(u.Content, &c) != nil {
			return ""
		}
		return c.Text
	}

	switch u.SessionUpdate {
	case "agent_message_chunk":
		t := text()
		if t == "" {
			return
		}
		c.textM.Lock()
		c.text.WriteString(t)
		full := c.text.String()
		c.textM.Unlock()
		if c.onChunk != nil {
			c.onChunk(t)
		}
		// With real streaming a done-marker is meaningful again: stop as soon as it appears.
		if c.opts.DoneMarker != "" && strings.Contains(full, c.opts.DoneMarker) {
			c.markerOnce.Do(func() { close(c.doneMarkerHit) })
		}
	case "agent_thought_chunk":
		if t := text(); c.opts.Thoughts && c.opts.OnEvent != nil && t != "" {
			c.opts.OnEvent(map[string]any{"event": "thought", "content": t})
		}
	case "tool_call":
		if c.opts.OnEvent != nil {
			c.opts.OnEvent(map[string]any{
				"event": "tool", "tool_call_id": u.ToolCallID,
				"title": u.Title, "kind": u.Kind, "status": "started",
			})
		}
	case "tool_call_update":
		if c.opts.OnEvent != nil && u.Status != "" {
			c.opts.OnEvent(map[string]any{
				"event": "tool", "tool_call_id": u.ToolCallID, "status": u.Status,
			})
		}
	}
}

// executeACP runs one prompt through a devin ACP session and returns the agent's message text.
func executeACP(ctx context.Context, prompt string, opts ExecOptions, workDir string, onChunk func(string)) (string, error) {
	devinPath, err := exec.LookPath("devin")
	if err != nil {
		return "", fmt.Errorf("%w: devin not in PATH: %v", errACPUnavailable, err)
	}

	args := []string{"acp"}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}

	cmd := exec.CommandContext(ctx, devinPath, args...)
	cmd.Dir = workDir
	// Own process group so a cancel/timeout takes the agent and anything it spawned with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("%w: %v", errACPUnavailable, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("%w: %v", errACPUnavailable, err)
	}
	cmd.Stderr = nil // devin logs INFO chatter to stderr; it is not part of the protocol

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("%w: cannot start devin acp: %v", errACPUnavailable, err)
	}
	killGroup := func() {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck
		}
	}
	defer func() {
		killGroup()
		cmd.Wait() //nolint:errcheck
	}()
	stopSignals := killOnSignal(func() int {
		if cmd.Process != nil {
			return cmd.Process.Pid
		}
		return 0
	})
	defer stopSignals()

	c := &acpClient{
		cmd: cmd, stdin: stdin, pending: map[int]chan acpResponse{},
		opts: opts, onChunk: onChunk, doneMarkerHit: make(chan struct{}),
		closed: make(chan struct{}),
	}
	readDone := make(chan error, 1)
	go c.readLoop(stdout, readDone)

	// --- handshake (the only stage allowed to fall back to -p) ---
	hctx, hcancel := context.WithTimeout(ctx, acpHandshakeTimeout)
	defer hcancel()

	if _, err := c.request(hctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}},
	}); err != nil {
		return "", fmt.Errorf("%w: initialize: %v", errACPUnavailable, err)
	}

	newRaw, err := c.request(hctx, "session/new", map[string]any{"cwd": workDir, "mcpServers": []any{}})
	if err != nil {
		return "", fmt.Errorf("%w: session/new: %v", errACPUnavailable, err)
	}
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(newRaw, &sess) != nil || sess.SessionID == "" {
		return "", fmt.Errorf("%w: session/new returned no sessionId", errACPUnavailable)
	}

	mode := sessionModeFor(opts.PermMode)
	if _, err := c.request(hctx, "session/set_mode", map[string]any{
		"sessionId": sess.SessionID, "modeId": mode,
	}); err != nil {
		// Not fatal: the session still runs in its default mode, but the caller asked for a
		// specific posture, so say so rather than silently running under the wrong one.
		fmt.Fprintf(os.Stderr, "[debri] warning: could not set session mode %q: %v\n", mode, err)
	}
	fmt.Fprintf(os.Stderr, "[debri] acp session=%s mode=%s\n", sess.SessionID, mode)

	// --- the turn ---
	promptDone := make(chan error, 1)
	go func() {
		_, perr := c.request(ctx, "session/prompt", map[string]any{
			"sessionId": sess.SessionID,
			"prompt":    []any{map[string]any{"type": "text", "text": prompt}},
		})
		promptDone <- perr
	}()

	finish := func() (string, error) {
		c.textM.Lock()
		defer c.textM.Unlock()
		return strings.TrimSpace(c.text.String()), nil
	}

	select {
	case err := <-promptDone:
		if err != nil {
			c.textM.Lock()
			partial := strings.TrimSpace(c.text.String())
			c.textM.Unlock()
			if partial != "" {
				// The agent produced real output before failing; returning it beats discarding
				// minutes of work over a late transport error.
				fmt.Fprintf(os.Stderr, "[debri] prompt ended with error after output: %v\n", err)
				return partial, nil
			}
			return "", fmt.Errorf("devin acp: %w", err)
		}
		return finish()
	case <-c.doneMarkerHit:
		c.notify("session/cancel", map[string]any{"sessionId": sess.SessionID})
		return finish()
	case err := <-readDone:
		c.textM.Lock()
		partial := strings.TrimSpace(c.text.String())
		c.textM.Unlock()
		if partial != "" {
			return partial, nil
		}
		if err != nil {
			return "", fmt.Errorf("%w: acp stream ended: %v", errACPUnavailable, err)
		}
		return "", fmt.Errorf("%w: acp stream closed during handshake", errACPUnavailable)
	case <-ctx.Done():
		// Ask the agent to stop before killing it, so it can wind down cleanly.
		c.notify("session/cancel", map[string]any{"sessionId": sess.SessionID})
		time.Sleep(200 * time.Millisecond)
		c.textM.Lock()
		partial := strings.TrimSpace(c.text.String())
		c.textM.Unlock()
		return partial, fmt.Errorf("devin exceeded the %dms cap", opts.StableTimeout)
	}
}
