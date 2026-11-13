package solari

import (
	"context"
	"encoding/base64"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Commands is the process-execution namespace on a Sandbox.
type Commands struct {
	sb *Sandbox
}

// Run executes a command to completion and returns its terminal result.
// OnStdout/OnStderr (if set) receive output as it streams.
func (c *Commands) Run(ctx context.Context, cmd string, opts CommandOptions) (*CommandResult, error) {
	return c.sb.runCommand(ctx, cmd, opts)
}

// Start launches a command and returns a handle immediately (does not wait for
// exit). Output streams as cmd.data frames; the handle exposes Stdin, OnData,
// Wait, and Kill.
func (c *Commands) Start(ctx context.Context, cmd string, opts CommandOptions) (*CommandHandle, error) {
	return c.sb.startCommand(ctx, cmd, opts, nil)
}

type dataChunk struct {
	stream string
	data   string
}

type exitResult struct {
	code int
	err  error
}

// CommandHandle is a started command from Commands.Start.
//
// Output callbacks and the exit are delivered by a goroutine of the handle's own, in the order they arrived, never on the
// control socket's read loop: a callback may therefore make calls of its own (Stdin, Kill, a file read) without stalling the
// loop that has to deliver the reply.
type CommandHandle struct {
	CmdID string
	ch    *channel

	mu       sync.Mutex
	dataCbs  []func(stream, data string)
	buffered []dataChunk

	evMu     sync.Mutex
	evCond   *sync.Cond
	events   []handleEvent
	evClosed bool

	done     chan struct{} // closed once the exit result is set
	result   exitResult
	exitOnce sync.Once
}

type handleEvent struct {
	chunk *dataChunk
	exit  *exitResult
}

func newCommandHandle(cmdID string, ch *channel) *CommandHandle {
	h := &CommandHandle{CmdID: cmdID, ch: ch, done: make(chan struct{})}
	h.evCond = sync.NewCond(&h.evMu)
	go h.run()
	return h
}

// push queues an event for the handle's goroutine; events after the exit are dropped.
func (h *CommandHandle) push(ev handleEvent) {
	h.evMu.Lock()
	if !h.evClosed {
		h.events = append(h.events, ev)
		h.evCond.Signal()
	}
	h.evMu.Unlock()
}

// run delivers queued events in order and ends with the exit.
func (h *CommandHandle) run() {
	for {
		h.evMu.Lock()
		for len(h.events) == 0 {
			h.evCond.Wait()
		}
		ev := h.events[0]
		h.events = h.events[1:]
		h.evMu.Unlock()
		if ev.chunk != nil {
			h.deliver(*ev.chunk)
			continue
		}
		h.evMu.Lock()
		h.evClosed = true
		h.events = nil
		h.evMu.Unlock()
		h.result = *ev.exit
		close(h.done)
		return
	}
}

// OnData subscribes to stdout/stderr chunks. Any output buffered before the
// first subscriber is replayed so early output is never dropped.
func (h *CommandHandle) OnData(cb func(stream, data string)) {
	// Replay buffered output in order and register cb only once nothing is left
	// to replay. Registering after a single replay would lose (a chunk delivered
	// between "buffered = nil" and the append lands in h.buffered with no one to
	// read it) or reorder (a live chunk overtakes the replay) output that arrives
	// while the replay runs; chunks that arrive meanwhile are buffered again and
	// drained by the next pass.
	for {
		h.mu.Lock()
		if len(h.buffered) == 0 {
			h.dataCbs = append(h.dataCbs, cb)
			h.mu.Unlock()
			return
		}
		buffered := h.buffered
		h.buffered = nil
		h.mu.Unlock()
		for _, c := range buffered {
			cb(c.stream, c.data)
		}
	}
}

// Wait blocks until the command exits (or the channel drops / ctx is cancelled)
// and returns the exit code. It may be called more than once, and from several
// goroutines: each gets the same result.
func (h *CommandHandle) Wait(ctx context.Context) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-h.done:
		return h.result.code, h.result.err
	}
}

// Stdin writes bytes/text to the command's stdin.
func (h *CommandHandle) Stdin(ctx context.Context, data []byte) error {
	_, err := h.ch.call(ctx, "cmd.stdin", map[string]interface{}{
		"cmdId":  h.CmdID,
		"base64": base64.StdEncoding.EncodeToString(data),
	}, callOptions{})
	return err
}

// Kill sends a signal (default SIGTERM when signal <= 0) to the command.
func (h *CommandHandle) Kill(ctx context.Context, signal int) error {
	params := map[string]interface{}{"cmdId": h.CmdID}
	if signal > 0 {
		params["signal"] = signal
	}
	_, err := h.ch.call(ctx, "cmd.kill", params, callOptions{})
	return err
}

// cancelKillGrace is how long a cancelled command gets to exit after SIGTERM before it is SIGKILLed.
const cancelKillGrace = 2 * time.Second

// CloseStdin closes the command's stdin (an EOF to a program reading it, such as cat or a shell). Write what the command
// should read with Stdin first.
func (h *CommandHandle) CloseStdin(ctx context.Context) error {
	_, err := h.ch.call(ctx, "cmd.stdin", map[string]interface{}{"cmdId": h.CmdID, "base64": "", "eof": true}, callOptions{})
	return err
}

// stopAfterCancel is what a cancelled Commands.Run does to the guest process: SIGTERM and close stdin now, SIGKILL after
// cancelKillGrace if it has not exited. Every call is best effort (a command that already finished answers with an error),
// and runs on its own goroutine (the caller starts it with `go`) so the cancelled caller returns at once.
func (h *CommandHandle) stopAfterCancel() {
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = h.Kill(bg, 15)
	_ = h.CloseStdin(bg)
	cancel()
	select {
	case <-h.done:
		return
	case <-time.After(cancelKillGrace):
	}
	kctx, kcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer kcancel()
	_ = h.Kill(kctx, 9)
}

func (h *CommandHandle) deliver(chunk dataChunk) {
	h.mu.Lock()
	if len(h.dataCbs) == 0 {
		h.buffered = append(h.buffered, chunk)
		h.mu.Unlock()
		return
	}
	cbs := append([]func(stream, data string){}, h.dataCbs...)
	h.mu.Unlock()
	for _, cb := range cbs {
		cb(chunk.stream, chunk.data)
	}
}

func (h *CommandHandle) finishExit(code int, err error) {
	h.exitOnce.Do(func() {
		h.push(handleEvent{exit: &exitResult{code: code, err: err}})
	})
}

// startCommand issues cmd.start and wires the async cmd.data/cmd.exit handlers.
func (s *Sandbox) startCommand(ctx context.Context, cmd string, opts CommandOptions, acc *cmdAccumulator) (*CommandHandle, error) {
	params := map[string]interface{}{"cmd": cmd}
	if opts.Args != nil {
		params["args"] = opts.Args
	}
	if opts.Cwd != "" {
		params["cwd"] = opts.Cwd
	}
	if opts.Env != nil {
		params["env"] = opts.Env
	}
	if opts.User != "" {
		params["user"] = opts.User
	}

	raw, err := s.ch.call(ctx, "cmd.start", params, callOptions{})
	if err != nil {
		return nil, execFailed(err)
	}
	var res struct {
		CmdID string `json:"cmdId"`
	}
	if err := unmarshal(raw, &res); err != nil {
		return nil, err
	}
	cmdID := res.CmdID

	h := newCommandHandle(cmdID, s.ch)

	// The result accumulator is subscribed BEFORE the frame handlers are registered:
	// registering them flushes frames that beat the cmd.start reply, and a non-empty
	// subscriber list skips the replay buffer, so an accumulator attached afterwards
	// (via OnData) would lose that early output whenever a callback is set.
	if acc != nil {
		h.dataCbs = append(h.dataCbs, acc.add)
	}
	// Route streaming callbacks straight through (before returning the handle),
	// mirroring the reference SDK so early output reaches OnStdout/OnStderr.
	if opts.OnStdout != nil || opts.OnStderr != nil {
		h.dataCbs = append(h.dataCbs, func(stream, data string) {
			if stream == "stdout" {
				if opts.OnStdout != nil {
					opts.OnStdout(data)
				}
			} else if opts.OnStderr != nil {
				opts.OnStderr(data)
			}
		})
	}

	// If the channel drops before cmd.exit, wake Wait() with the error.
	s.ch.onStreamClose(cmdID, func(err error) { h.finishExit(0, err) })

	s.ch.onFrame("cmd.data", cmdID, func(f asyncFrame) {
		stream := f.Stream
		if stream == "" {
			stream = "stdout"
		}
		data := ""
		if f.Base64 != "" {
			if b, derr := base64.StdEncoding.DecodeString(f.Base64); derr == nil {
				data = string(b)
			}
		}
		h.push(handleEvent{chunk: &dataChunk{stream: stream, data: data}})
	})
	var exited atomic.Bool
	s.ch.onFrame("cmd.exit", cmdID, func(f asyncFrame) {
		exited.Store(true)
		s.ch.offFrame("cmd.data", cmdID)
		s.ch.offFrame("cmd.exit", cmdID)
		s.ch.offStreamClose(cmdID)
		code := 0
		if f.ExitCode != nil {
			code = *f.ExitCode
		}
		h.finishExit(code, nil)
	})
	// A command that exited before its handle existed had its buffered cmd.exit replayed by the registration above, and onFrame
	// registers a handler only after the replay: the handlers it just ran to unregister were registered afterwards and stayed.
	// Unregister them now, or the channel counts the socket as in use for good and never idle-closes.
	if exited.Load() {
		s.ch.offFrame("cmd.data", cmdID)
		s.ch.offFrame("cmd.exit", cmdID)
		s.ch.offStreamClose(cmdID)
	}

	return h, nil
}

// runCommand runs a command to completion. It prefers the one-shot HTTP fast
// path (no cold WS handshake) for a plain run-to-completion, falling back to the
// control-WS path when the fast path is unavailable or unsuitable.
func (s *Sandbox) runCommand(ctx context.Context, cmd string, opts CommandOptions) (*CommandResult, error) {
	// The deadline is the same on both paths: TimeoutMs, else the tier maximum the gateway accepts. Validated BEFORE a transport
	// is chosen, with the gateway's own wording and class, so a bad value fails the same way whichever path it would take.
	timeoutMs := opts.TimeoutMs
	if timeoutMs < 0 {
		return nil, newValidationError("timeoutMs must be at least 1000 (one second), or 0 for no deadline")
	}
	if timeoutMs == 0 {
		timeoutMs = defaultCommandTimeoutMs
	}
	// Over the cap is refused on both paths, as REST refuses it (retest C-08). The live connection used to run it: its deadline is
	// this SDK's own timer, so nothing else would have said no.
	if timeoutMs > defaultCommandTimeoutMs {
		return nil, newValidationError("timeoutMs must be at most " + strconv.Itoa(defaultCommandTimeoutMs))
	}
	if opts.User != "" && !validUser(opts.User) {
		return nil, newValidationError("user: must be a unix user name or root")
	}
	// REST only for a plain run-to-completion, a deadline the route can express, and a call that cannot be cancelled: aborting
	// POST /exec does NOT stop the process in the guest, whereas cancelling a run on the live connection sends cmd.kill.
	// A shared sandbox is the exception: it refuses command streams, so REST is its only route and a cancellable call uses it too
	// (cancelling then abandons the request; the guest process is not stopped, which is the best that route can do).
	restPlain := s.execHook != nil &&
		!opts.Background &&
		opts.OnStdout == nil &&
		opts.OnStderr == nil &&
		opts.Env == nil &&
		opts.User == "" &&
		timeoutMs >= minRESTTimeoutMs && timeoutMs <= defaultCommandTimeoutMs
	restEligible := restPlain && (ctx.Done() == nil || s.shared.Load())
	viaREST := func() (*CommandResult, error) {
		res, err := s.execHook(ctx, s.ID, execOneShotRequest{
			Cmd:       cmd,
			Args:      opts.Args,
			Cwd:       opts.Cwd,
			TimeoutMs: timeoutMs,
		})
		return res, execFailed(err)
	}
	// A shared sandbox refuses command streams (cmd.start) but runs commands over REST, so once we know the sandbox is
	// shared the REST route is the only one that works, whether or not the control channel is open.
	if restEligible && (!s.ch.isConnected() || s.shared.Load()) {
		res, err := viaREST()
		if err == nil {
			return res, nil
		}
		// Fall back to the WS path ONLY when /exec isn't served (older gateway).
		// Any other failure propagates so we never silently double-execute.
		// Also the one refusal that proves nothing ran: a host pool without exec.user answers 409 HostCapabilityMissing; the live
		// connection reaches the guest directly. Retried once, there.
		if !isRouteUnavailable(err) && !isHostCapabilityMissing(err) {
			return nil, err
		}
	}

	// The socket is what this run needs: connect it, as the REST route needs no Connect() either (Commands.Start still
	// requires an open channel).
	if !s.ch.isConnected() {
		if err := s.ch.connect(ctx); err != nil {
			return nil, err
		}
	}

	acc := &cmdAccumulator{}
	handle, err := s.startCommand(ctx, cmd, opts, acc)
	if err != nil {
		if !isSharedRefusal(err) {
			return nil, err
		}
		// The sandbox is shared: remember it, and run this command over REST if it can be (the refusal means nothing
		// started, so nothing can run twice).
		s.shared.Store(true)
		if restPlain {
			return viaREST()
		}
		return nil, err
	}

	if opts.Background {
		return &CommandResult{ExitCode: 0, Stdout: "", Stderr: ""}, nil
	}

	// TimeoutMs is a deadline for the command, as on the REST /exec path: when it elapses the
	// command is killed and the result is exit -1 with the output so far (the guest's own
	// timeout result). Without this a hung command waited here until ctx was cancelled.
	waitCtx, cancel := ctx, context.CancelFunc(func() {})
	if timeoutMs > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	}
	code, err := handle.Wait(waitCtx)
	cancel()
	timedOut := false
	if err != nil {
		if timeoutMs > 0 && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			_ = handle.Kill(ctx, 9)
			code = -1
			timedOut = true
		} else {
			if ctx.Err() != nil {
				// Cancelled: the guest process would otherwise run on. Ask it to stop, close its stdin, and
				// escalate to SIGKILL if it is still there after 2 s.
				go handle.stopAfterCancel()
			}
			return nil, err
		}
	}
	res := acc.result(code)
	res.TimedOut = timedOut
	return res, nil
}

// defaultCommandTimeoutMs is the deadline of a run that sets none: the maximum the gateway accepts.
const defaultCommandTimeoutMs = 600000

// minRESTTimeoutMs is the smallest deadline the one-shot route can express.
const minRESTTimeoutMs = 1000

var unixUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func validUser(u string) bool { return u == "root" || unixUser.MatchString(u) }

func isHostCapabilityMissing(err error) bool {
	var ge *GatewayError
	return errors.As(err, &ge) && ge.Status == 409 && ge.Code == "HostCapabilityMissing"
}

// cmdAccumulator collects a command's stdout/stderr for the CommandResult.
type cmdAccumulator struct {
	mu             sync.Mutex
	stdout, stderr []byte
}

func (a *cmdAccumulator) add(stream, data string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if stream == "stdout" {
		a.stdout = append(a.stdout, data...)
	} else {
		a.stderr = append(a.stderr, data...)
	}
}

func (a *cmdAccumulator) result(code int) *CommandResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	return &CommandResult{
		ExitCode: code, Stdout: validUTF8(a.stdout), Stderr: validUTF8(a.stderr),
		StdoutBytes: append([]byte(nil), a.stdout...), StderrBytes: append([]byte(nil), a.stderr...),
	}
}

// validUTF8 decodes the whole stream at once, so a character split across chunks survives, and replaces each invalid byte
// with U+FFFD, which is what the REST route returns for the same output.
func validUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n == 1 {
			sb.WriteRune(utf8.RuneError)
		} else {
			sb.Write(b[:n])
		}
		b = b[n:]
	}
	return sb.String()
}

// execFailed gives "the command could not be started" one shape on both transports: *ActionError{Method: "cmd.start",
// Code: "ExecFailed"}. REST answers 400 {code: ExecFailed}; the control socket answers cmd.start with a bare message
// ("fork/exec ...", "chdir ...") and no code. Any other error is returned unchanged.
func execFailed(err error) error {
	if err == nil {
		return nil
	}
	var ae *ActionError
	if errors.As(err, &ae) && ae.Method == "cmd.start" && ae.Code == "" &&
		(strings.HasPrefix(ae.Message, "fork/exec ") || strings.HasPrefix(ae.Message, "chdir ")) {
		ae.Code = "ExecFailed"
		return ae
	}
	var ge *GatewayError
	if errors.As(err, &ge) && ge.Status == 400 && ge.Code == "ExecFailed" {
		return &ActionError{SolariError: SolariError{Message: ge.Message}, Method: "cmd.start", Code: "ExecFailed"}
	}
	return err
}
