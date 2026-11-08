package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"splash/goserver/wire"
)

// Supervision budgets (runtime/Supervision.hpp, protocol.SUPERVISION).
const (
	startTimeout  = 600 * time.Second
	stopGrace     = 15 * time.Second
	statusTimeout = 5 * time.Second
	writeProgress = 120 * time.Second
)

// EngineError is an engine failure: the process stopped, broke the protocol
// or reported itself unhealthy.
type EngineError struct {
	Code string
	Msg  string
}

func (e *EngineError) Error() string { return e.Msg }

// StartFailedError is a start the engine refused (StartFailedEvent).
type StartFailedError struct{ Event wire.StartFailed }

func (e *StartFailedError) Error() string {
	return fmt.Sprintf("native engine start failed (%s): %s", kindName(e.Event.Kind), e.Event.Message)
}

func kindName(k uint8) string {
	if int(k) < len(wire.StartFailureKinds) {
		return wire.StartFailureKinds[k]
	}
	return "internal"
}

// RequestFailed is a request the engine failed (ErrorEvent of a request).
type RequestFailed struct {
	Code       string
	Msg        string
	Retryable  bool
	RetryAfter int
}

func (e *RequestFailed) Error() string { return e.Msg }

// errClosed is the runtime after Close.
var errClosed = &EngineError{"runtime_closed", "runtime is closed"}

// CallHandler receives a call's events on the engine's reader goroutine.
type CallHandler interface {
	OnStart(wire.Start)
	OnProgress(wire.PromptProgress)
	OnTokens(wire.Tokens)
	// OnEnd ends the call: a Done, or the error that failed it.
	OnEnd(*wire.Done, error)
}

// Call is one request in flight.
type Call struct {
	id         uint64
	generation int
	handler    CallHandler
	prompt     int
	logicalMax int
	started    bool
	next       int
	ended      bool
	endSent    bool
	rt         *Runtime
}

// Runtime is the engine process and its multiplexed protocol stream.
type Runtime struct {
	command func(first *wire.Ready) []string

	mu         sync.Mutex
	writeMu    sync.Mutex
	proc       *exec.Cmd
	stdin      io.WriteCloser
	generation int
	ready      *wire.Ready
	first      *wire.Ready
	latest     *wire.Ready
	pending    map[uint64]*Call
	status     map[uint64]chan wire.StatusJSON
	lastStatus uint64
	nextID     uint64
	nextCorr   uint64
	closed     bool
	restarts   int
	readyCh    chan error
	exited     chan struct{}
	spawnedAt  time.Time
	startSecs  float64
	pendingCap int

	// OnWarming runs when the first engine begins its warm-up runs.
	OnWarming func()
	// OnFailure runs when an engine that reached Ready fails.
	OnFailure func(error)
}

// NewRuntime returns a runtime that launches command (given the first
// engine's Ready for a relaunch, nil for the first start).
func NewRuntime(command func(first *wire.Ready) []string, pendingLimit int) *Runtime {
	return &Runtime{command: command, pending: map[uint64]*Call{}, status: map[uint64]chan wire.StatusJSON{}, pendingCap: pendingLimit}
}

// Capabilities is the latest engine's Ready.
func (rt *Runtime) Capabilities() *wire.Ready {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.latest
}

// Ready reports whether an engine is serving.
func (rt *Runtime) Ready() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.ready != nil && !rt.closed
}

// Restarts counts the engines started after the first.
func (rt *Runtime) Restarts() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.restarts
}

// Pending is the number of calls in flight.
func (rt *Runtime) Pending() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return len(rt.pending)
}

// Start launches an engine and waits for its Ready.
func (rt *Runtime) Start() error {
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return errClosed
	}
	old, oldExited := rt.proc, rt.exited
	rt.mu.Unlock()
	if old != nil {
		stopProcess(old, oldExited)
	}
	rt.mu.Lock()
	argv := rt.command(rt.first)
	if rt.first != nil {
		rt.restarts++
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		rt.mu.Unlock()
		return &EngineError{"engine_unhealthy", fmt.Sprintf("could not launch native engine: %v", err)}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		rt.mu.Unlock()
		return &EngineError{"engine_unhealthy", fmt.Sprintf("could not launch native engine: %v", err)}
	}
	if err := cmd.Start(); err != nil {
		rt.mu.Unlock()
		if errors.Is(err, os.ErrNotExist) {
			return &EngineError{"engine_unhealthy", "native engine executable is missing; the installation may have been upgraded or removed"}
		}
		return &EngineError{"engine_unhealthy", fmt.Sprintf("could not launch native engine: %v", err)}
	}
	rt.generation++
	gen := rt.generation
	rt.proc, rt.stdin, rt.ready = cmd, stdin, nil
	rt.readyCh = make(chan error, 1)
	rt.exited = make(chan struct{})
	rt.spawnedAt = time.Now()
	readyCh, exited := rt.readyCh, rt.exited
	rt.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	go rt.readLoop(gen, bufio.NewReaderSize(stdout, 64*1024))
	select {
	case err := <-readyCh:
		if err != nil {
			rt.mu.Lock()
			proc := rt.proc
			rt.mu.Unlock()
			stopProcess(proc, exited)
		}
		return err
	case <-time.After(startTimeout):
		rt.failGeneration(gen, &EngineError{"engine_unhealthy", "native ReadyEvent timed out"})
		stopProcess(cmd, exited)
		return &EngineError{"engine_unhealthy", "native ReadyEvent timed out"}
	}
}

// LastStartSeconds is how long the latest engine took to be Ready.
func (rt *Runtime) LastStartSeconds() float64 {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.startSecs
}

func (rt *Runtime) readLoop(gen int, r io.Reader) {
	var scratch []byte
	for {
		ev, err := wire.ReadEvent(r, &scratch)
		if err != nil {
			var pe *wire.ProtocolError
			if errors.As(err, &pe) {
				rt.failGeneration(gen, &EngineError{"protocol_fatal", pe.Error()})
			} else {
				rt.failGeneration(gen, &EngineError{"engine_unhealthy", "native protocol reached EOF"})
			}
			return
		}
		if err := rt.dispatch(gen, ev); err != nil {
			rt.failGeneration(gen, err)
			return
		}
	}
}

func (rt *Runtime) dispatch(gen int, ev any) error {
	rt.mu.Lock()
	if gen != rt.generation {
		rt.mu.Unlock()
		return nil
	}
	switch e := ev.(type) {
	case wire.Warming:
		first := rt.first == nil
		rt.mu.Unlock()
		if first && rt.OnWarming != nil {
			rt.OnWarming()
		}
		return nil
	case wire.Ready:
		if rt.ready != nil {
			rt.mu.Unlock()
			return &EngineError{"protocol_fatal", "native sent more than one ReadyEvent"}
		}
		ready := e
		rt.ready = &ready
		rt.latest = &ready
		if rt.first == nil {
			rt.first = &ready
		}
		rt.startSecs = time.Since(rt.spawnedAt).Seconds()
		ch := rt.readyCh
		rt.mu.Unlock()
		ch <- nil
		return nil
	case wire.StartFailed:
		if rt.ready != nil {
			rt.mu.Unlock()
			return &EngineError{"protocol_fatal", "native start failure arrived after ReadyEvent"}
		}
		ch := rt.readyCh
		rt.mu.Unlock()
		ch <- &StartFailedError{e}
		return &EngineError{"engine_unhealthy", "native engine start failed"}
	}
	if rt.ready == nil {
		rt.mu.Unlock()
		return &EngineError{"protocol_fatal", "native event arrived before ReadyEvent"}
	}
	switch e := ev.(type) {
	case wire.StatusJSON:
		ch := rt.status[e.Correlation]
		delete(rt.status, e.Correlation)
		known := e.Correlation <= rt.lastStatus
		rt.mu.Unlock()
		if ch != nil {
			ch <- e
		} else if !known {
			return &EngineError{"protocol_fatal", fmt.Sprintf("native status references unknown correlation %d", e.Correlation)}
		}
		return nil
	case wire.ErrorEvent:
		if e.Class != wire.RequestError {
			rt.mu.Unlock()
			code := "engine_unhealthy"
			if e.Class == wire.ProtocolFatal {
				code = "protocol_fatal"
			} else if e.Code != "" {
				code = e.Code
			}
			return &EngineError{code, e.Message}
		}
		call := rt.pending[e.RequestID]
		if call == nil {
			rt.mu.Unlock()
			return &EngineError{"protocol_fatal", fmt.Sprintf("native event references unknown request %d", e.RequestID)}
		}
		rt.finishLocked(call)
		rt.mu.Unlock()
		call.handler.OnEnd(nil, &RequestFailed{e.Code, e.Message, e.Retryable, int(e.RetryAfter)})
		return nil
	case wire.MaskRequest:
		rt.mu.Unlock()
		return &EngineError{"protocol_fatal", "native requested a token mask for an unconstrained request"}
	}
	var id uint64
	switch e := ev.(type) {
	case wire.Start:
		id = e.RequestID
	case wire.PromptProgress:
		id = e.RequestID
	case wire.Tokens:
		id = e.RequestID
	case wire.Done:
		id = e.RequestID
	}
	call := rt.pending[id]
	if call == nil {
		rt.mu.Unlock()
		return &EngineError{"protocol_fatal", fmt.Sprintf("native event references unknown request %d", id)}
	}
	switch e := ev.(type) {
	case wire.Start:
		if call.started {
			rt.mu.Unlock()
			return &EngineError{"protocol_fatal", "duplicate or late StartEvent"}
		}
		call.started = true
		rt.mu.Unlock()
		call.handler.OnStart(e)
	case wire.PromptProgress:
		rt.mu.Unlock()
		call.handler.OnProgress(e)
	case wire.Tokens:
		if !call.started || int(e.Offset) != call.next || call.next+len(e.Tokens) > call.logicalMax {
			rt.mu.Unlock()
			return &EngineError{"protocol_fatal", fmt.Sprintf("TokensEvent offset %d does not continue the stream at %d", e.Offset, call.next)}
		}
		call.next += len(e.Tokens)
		rt.mu.Unlock()
		call.handler.OnTokens(e)
	case wire.Done:
		if int(e.PromptTokens) != call.prompt || int(e.CompletionTokens) != call.next {
			rt.mu.Unlock()
			return &EngineError{"protocol_fatal", "DoneEvent counts do not match the request"}
		}
		rt.finishLocked(call)
		rt.mu.Unlock()
		done := e
		call.handler.OnEnd(&done, nil)
	default:
		rt.mu.Unlock()
	}
	return nil
}

func (rt *Runtime) finishLocked(call *Call) {
	call.ended = true
	delete(rt.pending, call.id)
}

// failGeneration fails every call of an engine and reports its failure.
func (rt *Runtime) failGeneration(gen int, err error) {
	rt.mu.Lock()
	if gen != rt.generation {
		rt.mu.Unlock()
		return
	}
	wasReady := rt.ready != nil
	rt.ready = nil
	calls := make([]*Call, 0, len(rt.pending))
	for _, c := range rt.pending {
		if c.generation == gen {
			calls = append(calls, c)
			rt.finishLocked(c)
		}
	}
	waiters := rt.status
	rt.status = map[uint64]chan wire.StatusJSON{}
	closed := rt.closed
	readyCh := rt.readyCh
	rt.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
	if !wasReady {
		select {
		case readyCh <- err:
		default:
		}
	}
	for _, c := range calls {
		c.handler.OnEnd(nil, err)
	}
	if wasReady && !closed && rt.OnFailure != nil {
		rt.OnFailure(err)
	}
}

func (rt *Runtime) write(frame []byte, gen int) error {
	rt.writeMu.Lock()
	defer rt.writeMu.Unlock()
	rt.mu.Lock()
	if gen != rt.generation || rt.stdin == nil {
		rt.mu.Unlock()
		return &EngineError{"engine_unhealthy", "native process generation is unavailable"}
	}
	stdin := rt.stdin
	rt.mu.Unlock()
	if _, err := stdin.Write(frame); err != nil {
		rt.failGeneration(gen, &EngineError{"engine_unhealthy", fmt.Sprintf("native write failed: %v", err)})
		return &EngineError{"engine_unhealthy", "native process is not ready"}
	}
	return nil
}

// ErrPendingLimit is a submission past --queue-size.
var ErrPendingLimit = errors.New("pending request limit is full")

// Submit writes a request; its events reach handler.
func (rt *Runtime) Submit(req *wire.Request, deadline time.Time, handler CallHandler) (*Call, error) {
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return nil, errClosed
	}
	if rt.ready == nil {
		rt.mu.Unlock()
		return nil, &EngineError{"engine_unhealthy", "native process is not ready"}
	}
	if len(rt.pending) >= rt.pendingCap {
		rt.mu.Unlock()
		return nil, ErrPendingLimit
	}
	rt.nextID++
	req.ID = rt.nextID
	now := time.Now()
	wall := uint64(now.UnixMicro())
	limit := math.MaxUint64 - wall
	micros := limit
	if !deadline.IsZero() {
		remaining := deadline.Sub(now)
		if remaining <= 0 {
			rt.mu.Unlock()
			return nil, errTimeout
		}
		micros = uint64(remaining.Microseconds())
		if micros < 1 {
			micros = 1
		}
	}
	req.AbsoluteDeadlineMicros, req.RemainingMicros = wall+micros, micros
	if err := req.Validate(); err != nil {
		rt.mu.Unlock()
		return nil, &RequestFailed{Code: "invalid_request", Msg: err.Error()}
	}
	frame := req.Encode()
	call := &Call{id: req.ID, generation: rt.generation, handler: handler, prompt: len(req.Prompt), logicalMax: int(req.MaxOutputTokens), rt: rt}
	rt.pending[req.ID] = call
	gen := rt.generation
	rt.mu.Unlock()
	if err := rt.write(frame, gen); err != nil {
		rt.mu.Lock()
		delete(rt.pending, req.ID)
		rt.mu.Unlock()
		return nil, err
	}
	return call, nil
}

var errTimeout = errors.New("request timed out")

func (c *Call) end(frame func(uint64) []byte) {
	rt := c.rt
	rt.mu.Lock()
	if c.ended || c.endSent {
		rt.mu.Unlock()
		return
	}
	c.endSent = true
	gen := c.generation
	rt.mu.Unlock()
	_ = rt.write(frame(c.id), gen)
}

// Cancel asks the engine to end the call; its Done still ends it.
func (c *Call) Cancel() { c.end(wire.CancelFrame) }

// Stop ends the call at a client stop sequence.
func (c *Call) Stop() { c.end(wire.StopFrame) }

// Status asks the engine for its status JSON.
func (rt *Runtime) Status(timeout time.Duration, release bool) ([]byte, error) {
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return nil, errClosed
	}
	if rt.ready == nil {
		rt.mu.Unlock()
		return nil, &EngineError{"engine_unhealthy", "native process is not ready"}
	}
	rt.nextCorr++
	corr := rt.nextCorr
	rt.lastStatus = corr
	ch := make(chan wire.StatusJSON, 1)
	rt.status[corr] = ch
	gen := rt.generation
	rt.mu.Unlock()
	frame := wire.StatusRequestFrame(corr)
	if release {
		frame = wire.ReleaseRequestFrame(corr)
	}
	if err := rt.write(frame, gen); err != nil {
		return nil, err
	}
	select {
	case ev, ok := <-ch:
		if !ok {
			return nil, &EngineError{"engine_unhealthy", "native engine failed"}
		}
		return ev.JSON, nil
	case <-time.After(timeout):
		rt.mu.Lock()
		delete(rt.status, corr)
		rt.mu.Unlock()
		return nil, errStatusTimeout
	}
}

var errStatusTimeout = errors.New("native status response timed out")

// Close fails every call and stops the engine.
func (rt *Runtime) Close() {
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return
	}
	rt.closed = true
	gen, proc, exited := rt.generation, rt.proc, rt.exited
	rt.mu.Unlock()
	rt.failGeneration(gen, errClosed)
	if proc != nil {
		stopProcess(proc, exited)
	}
}

// Kill sends SIGKILL to the engine at once.
func (rt *Runtime) Kill() {
	rt.mu.Lock()
	proc := rt.proc
	rt.mu.Unlock()
	if proc != nil && proc.Process != nil {
		_ = proc.Process.Kill()
	}
}

// stopProcess asks the engine to exit (stdin closed, SIGTERM), and kills it
// past the stop grace.
func stopProcess(cmd *exec.Cmd, exited chan struct{}) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	select {
	case <-exited:
		return
	default:
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
		return
	case <-time.After(stopGrace):
	}
	_ = cmd.Process.Kill()
	select {
	case <-exited:
	case <-time.After(stopGrace):
	}
}
