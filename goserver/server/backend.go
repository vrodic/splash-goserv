package server

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"splash/goserver/ojson"
	"splash/goserver/tokenizer"
	"splash/goserver/wire"
)

// APIError is an error an API answers with.
type APIError struct {
	Status     int
	Message    string
	Code       string
	RetryAfter int
	// Context length details, which Messages words its own way.
	InputTokens, MaxInputTokens int
	contextLength               bool
}

func (e *APIError) Error() string { return e.Message }

func apiErr(status int, message string) *APIError {
	return &APIError{Status: status, Message: message, Code: "invalid_request_error", RetryAfter: 1}
}

func apiErrCode(status int, message, code string) *APIError {
	return &APIError{Status: status, Message: message, Code: code, RetryAfter: 1}
}

func contextLengthError(input, max int) *APIError {
	e := apiErrCode(400, "prompt exceeds the context window", "context_length_exceeded")
	e.InputTokens, e.MaxInputTokens, e.contextLength = input, max, true
	return e
}

// CacheInfo is a request's prefix-cache outcome.
type CacheInfo struct {
	Status       string
	Matched      int
	Lane         int
	Deduplicated int
}

// Result is a generation's end.
type Result struct {
	Reason            string
	PromptTokens      int
	CompletionTokens  int
	PrefillMS         float64
	DecodeMS          float64
	WallMS            float64
	PrefillTokens     int
	Cache             CacheInfo
	StopSequence      *string
	FirstBatchTokens  int
	Drafted, Accepted int
	Cycles            int
	Bounded           bool
	metrics           *ojson.Object
}

// Metrics is the per-request metrics object (metrics.metrics_dict).
func (r *Result) Metrics() *ojson.Object {
	if r.metrics != nil {
		return r.metrics
	}
	decode := r.CompletionTokens - r.FirstBatchTokens
	if decode < 0 {
		decode = 0
	}
	latency := ojson.NewObject()
	for _, iv := range []struct {
		name string
		v    float64
	}{{"start_to_first_token_ms", r.PrefillMS}, {"first_token_to_done_ms", r.DecodeMS}, {"wall_ms", r.WallMS}} {
		if !math.IsInf(iv.v, 0) && !math.IsNaN(iv.v) && iv.v >= 0 {
			latency.Set(iv.name, iv.v)
		}
	}
	if r.CompletionTokens > 0 && latency.Has("wall_ms") && latency.Has("first_token_to_done_ms") && r.WallMS >= r.DecodeMS {
		ttft := r.WallMS - r.DecodeMS
		latency.Set("ttft_ms", ttft)
		if latency.Has("start_to_first_token_ms") && ttft >= r.PrefillMS {
			latency.Set("queue_to_start_ms", ttft-r.PrefillMS)
		}
	}
	if r.FirstBatchTokens > 0 && decode > 0 && r.DecodeMS > 0 {
		latency.Set("stream_tokens_per_second", float64(decode)*1000/r.DecodeMS)
	}
	var lane any
	if r.Cache.Lane >= 0 {
		lane = int64(r.Cache.Lane)
	}
	m := ojson.O(
		"prefill", ojson.O("tokens", int64(r.PrefillTokens)),
		"decode", ojson.O("tokens", int64(decode)),
		"request_latency", latency,
		"cache", ojson.O("status", r.Cache.Status, "matched_tokens", int64(r.Cache.Matched),
			"deduplicated_tokens", int64(r.Cache.Deduplicated), "lane", lane),
	)
	if r.Drafted > 0 {
		m.Set("speculation", ojson.O("drafted_tokens", int64(r.Drafted), "accepted_draft_tokens", int64(r.Accepted), "cycles", int64(r.Cycles)))
	}
	r.metrics = m
	return m
}

// Timings is llama-server's timings object (metrics.timings_dict).
func (r *Result) Timings() *ojson.Object {
	latency := r.Metrics().Value("request_latency").(*ojson.Object)
	get := func(k string) float64 {
		v, _ := latency.Value(k).(float64)
		return v
	}
	promptMS := get("start_to_first_token_ms")
	rate := 0.0
	if promptMS != 0 {
		rate = float64(r.PrefillTokens) * 1000 / promptMS
	}
	t := ojson.O("prompt_n", int64(r.PromptTokens), "prompt_ms", promptMS, "prompt_per_second", rate,
		"predicted_n", int64(r.CompletionTokens), "predicted_ms", get("first_token_to_done_ms"),
		"predicted_per_second", get("stream_tokens_per_second"), "cache_n", int64(r.Cache.Matched))
	if r.Drafted > 0 {
		t.Set("draft_n", int64(r.Drafted))
		t.Set("draft_n_accepted", int64(r.Accepted))
	}
	return t
}

func (r *Result) Usage(j *Job) *ojson.Object {
	return ojson.O("prompt_tokens", int64(r.PromptTokens), "completion_tokens", int64(r.CompletionTokens),
		"total_tokens", int64(r.PromptTokens+r.CompletionTokens),
		"prompt_tokens_details", ojson.O("cached_tokens", int64(r.Cache.Matched)),
		"completion_tokens_details", ojson.O("reasoning_tokens", int64(j.ReasoningTokens)))
}

// Reasoning is how a chat request asked to reason.
type Reasoning struct {
	Sent, Effort   *string
	EnableThinking *bool
}

// event is one item of a job's event queue.
type event struct {
	kind     string // start, text, progress, done, error
	text     string
	progress *ojson.Object
	result   *Result
	err      error
}

// eventQueue is an unbounded queue: the engine's reader never blocks on a
// slow client.
type eventQueue struct {
	mu     sync.Mutex
	items  []event
	notify chan struct{}
}

func newEventQueue() *eventQueue { return &eventQueue{notify: make(chan struct{}, 1)} }

func (q *eventQueue) put(e event) {
	q.mu.Lock()
	q.items = append(q.items, e)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// get waits up to timeout for an event; ok false on timeout.
func (q *eventQueue) get(timeout time.Duration) (event, bool) {
	deadline := time.Now().Add(timeout)
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			e := q.items[0]
			q.items[0] = event{}
			q.items = q.items[1:]
			q.mu.Unlock()
			return e, true
		}
		q.mu.Unlock()
		wait := time.Until(deadline)
		if wait <= 0 {
			return event{}, false
		}
		select {
		case <-q.notify:
		case <-time.After(wait):
		}
	}
}

// Job is one prepared generation.
type Job struct {
	Prompt           []uint32
	MaxNewTokens     int
	Seed             uint64
	Sampling         wire.Sampling
	Deadline         time.Time
	Priority         uint8
	StopSequences    []string
	Thinking         bool
	Reasoning        *Reasoning
	Format           *ChatFormat
	ThinkingBudget   *int
	ReasoningTokens  int
	ToolPolicy       *ToolPolicy
	ToolsCount       int
	ToolsSignature   string
	GenerationPrompt int
	Flags            uint32
	ClampedToContext bool
	CacheSaltLo      uint64
	CacheSaltHi      uint64
	PublicID         string
	Created          int64
	FirstCallOnly    bool
	// The grammar the output generates under, nil for none, and whether
	// the output answers a response schema.
	Constraint *TokenConstraint
	Structured bool
	cache      CacheInfo
	cacheMu    sync.Mutex
	events     *eventQueue
	call       *Call
	cancelled  bool
	callMu     sync.Mutex
	started    time.Time
}

func (j *Job) MayCallTools() bool { return j.ToolPolicy.MayCall() }

func (j *Job) Cache() CacheInfo {
	j.cacheMu.Lock()
	defer j.cacheMu.Unlock()
	return j.cache
}

// streamer decodes a job's tokens and applies its stop sequences
// (backend.CallbackStreamer).
type streamer struct {
	tok     *tokenizer.Tokenizer
	emit    func(string)
	stops   *StopSearch
	onStop  func()
	stream  *tokenizer.DecodeStream
	ids     []int
	emitted strings.Builder
	pending string
	stopSeq *string
}

func (s *streamer) send(text string) {
	if text != "" {
		s.emit(text)
		s.emitted.WriteString(text)
	}
}

func (s *streamer) emitText(text string) {
	if text == "" || s.stopSeq != nil {
		return
	}
	if s.stops == nil {
		s.send(text)
		return
	}
	s.pending += text
	if stop, back, ok := s.stops.Search(text); ok {
		s.stopSeq = &stop
		cut := len(s.pending) - back
		if cut < 0 {
			cut = 0
		}
		s.send(s.pending[:cut])
		s.pending = ""
		if s.onStop != nil {
			s.onStop()
		}
		return
	}
	held := s.stops.Held()
	if held > len(s.pending) {
		held = len(s.pending)
	}
	ready := s.pending[:len(s.pending)-held]
	s.pending = s.pending[len(s.pending)-held:]
	s.send(ready)
}

func (s *streamer) putTokens(ids []uint32) {
	if s.stopSeq != nil {
		return
	}
	for _, id := range ids {
		s.ids = append(s.ids, int(id))
		if text := s.stream.Step(int(id)); text != "" {
			s.emitText(text)
			if s.stopSeq != nil {
				break
			}
		}
	}
}

func (s *streamer) end() error {
	if s.stopSeq != nil {
		return nil
	}
	decoded := s.tok.Decode(s.ids, true)
	handled := s.emitted.String() + s.pending
	if !strings.HasPrefix(decoded, handled) {
		return errors.New("incremental tokenizer output diverged")
	}
	s.emitText(strings.TrimRight(decoded[len(handled):], "�"))
	if s.stopSeq == nil {
		s.send(s.pending)
		s.pending = ""
	}
	return nil
}

// reasoningTokens counts the reasoning's tokens (count_reasoning_tokens).
func (s *streamer) reasoningTokens(cf *ChatFormat, thinking, toolCalls bool) int {
	if cf == nil {
		return 0
	}
	start := 0
	if !thinking {
		if cf.ThinkStartID >= 0 && len(s.ids) > 0 && s.ids[0] == cf.ThinkStartID {
			start = 1
		} else {
			return 0
		}
	}
	callsEnd := toolCalls && cf.callsEndReasoning()
	for i := start; i < len(s.ids); i++ {
		t := s.ids[i]
		if t == cf.ThinkEndID || (callsEnd && t == cf.CallOpenID && s.opensCall(i, cf.Syntax.callOpen())) {
			return i - start
		}
	}
	return len(s.ids) - start
}

func (s *streamer) opensCall(index int, callOpen string) bool {
	text := ""
	for end := index + 1; end <= len(s.ids); end++ {
		text = s.tok.Decode(s.ids[index:end], false)
		if len(text) >= len(callOpen) || !strings.HasPrefix(callOpen, text) {
			break
		}
	}
	return strings.HasPrefix(text, callOpen)
}

// jobHandler connects a job to its call.
type jobHandler struct {
	b          *Backend
	job        *Job
	streamer   *streamer
	firstBatch int
}

func (h *jobHandler) OnStart(e wire.Start) {
	status := "miss"
	if e.Matched > 0 {
		status = "hit"
	}
	h.job.cacheMu.Lock()
	h.job.cache = CacheInfo{Status: status, Matched: int(e.Matched), Lane: int(e.Lane)}
	h.job.cacheMu.Unlock()
	h.b.logStarted(h.job, int(e.Matched))
	h.job.events.put(event{kind: "start"})
}

func (h *jobHandler) OnProgress(e wire.PromptProgress) {
	h.job.events.put(event{kind: "progress", progress: ojson.O("total", int64(len(h.job.Prompt)),
		"cache", int64(h.job.Cache().Matched), "processed", int64(e.Processed), "time_ms", float64(e.ElapsedMicros)/1000)})
}

func (h *jobHandler) OnTokens(e wire.Tokens) {
	if e.Offset == 0 {
		h.firstBatch = len(e.Tokens)
	}
	if c := h.job.Constraint; c != nil {
		if err := c.Commit(e.Tokens); err != nil {
			h.job.callMu.Lock()
			call := h.job.call
			h.job.callMu.Unlock()
			if call != nil {
				go call.maskFailed(&MaskFailed{Msg: err.Error()})
			}
			return
		}
	}
	h.streamer.putTokens(e.Tokens)
}

func (h *jobHandler) OnEnd(done *wire.Done, err error) {
	job := h.job
	if err != nil {
		apiError := h.b.apiError(err)
		h.b.logEnd(job, nil, apiError)
		job.events.put(event{kind: "error", err: apiError})
		return
	}
	if c := job.Constraint; c != nil && done.Reason != wire.FinishCancelled && h.streamer.stopSeq == nil {
		// The grammar checks what was generated after the last mask.
		if err := c.Finish(); err != nil {
			apiError := apiErrCode(400, err.Error(), "constraint_error")
			h.b.logEnd(job, nil, apiError)
			job.events.put(event{kind: "error", err: apiError})
			return
		}
	}
	if err := h.streamer.end(); err != nil {
		apiError := apiErrCode(500, err.Error(), "runtime_error")
		h.b.logEnd(job, nil, apiError)
		job.events.put(event{kind: "error", err: apiError})
		return
	}
	job.ReasoningTokens = h.streamer.reasoningTokens(job.Format, job.Thinking, job.MayCallTools())
	reasons := map[uint8]string{wire.FinishStop: "stop", wire.FinishLength: "length", wire.FinishCancelled: "cancelled"}
	cache := job.Cache()
	cache.Deduplicated = int(done.Deduplicated)
	r := &Result{Reason: reasons[done.Reason], PromptTokens: int(done.PromptTokens), CompletionTokens: int(done.CompletionTokens),
		PrefillMS: float64(done.PrefillMicros) / 1000, DecodeMS: float64(done.DecodeMicros) / 1000, WallMS: float64(done.WallMicros) / 1000,
		Cache: cache, StopSequence: h.streamer.stopSeq, FirstBatchTokens: h.firstBatch,
		Drafted: int(done.Drafted), Accepted: int(done.Accepted), Cycles: int(done.Cycles), Bounded: done.Bounded}
	r.PrefillTokens = r.PromptTokens - cache.Matched
	if r.PrefillTokens < 0 {
		r.PrefillTokens = 0
	}
	if h.streamer.stopSeq != nil {
		r.Reason = "stop"
		r.CompletionTokens = len(h.streamer.ids)
	}
	h.b.logEnd(job, r, nil)
	job.events.put(event{kind: "done", result: r})
}

// Backend submits jobs to the engine and supervises it.
type Backend struct {
	rt          *Runtime
	tok         *tokenizer.Tokenizer
	thinkingEnd []uint32

	mu          sync.Mutex
	closing     bool
	engineError string
	stopReason  string
	recovering  bool
	failures    []time.Time
	snapshot    *ojson.Object
	snapshotAt  time.Time
	logs        chan func()
}

func NewBackend(rt *Runtime, tok *tokenizer.Tokenizer, thinkingEnd []uint32) *Backend {
	b := &Backend{rt: rt, tok: tok, thinkingEnd: thinkingEnd, logs: make(chan func(), 4096)}
	go func() {
		for f := range b.logs {
			f()
		}
	}()
	rt.OnFailure = b.engineFailed
	return b
}

const crashLoopFailures = 3
const shortLived = 120 * time.Second

func (b *Backend) engineFailed(err error) {
	b.mu.Lock()
	if b.closing || b.stopReason != "" {
		b.mu.Unlock()
		return
	}
	b.engineError = err.Error()
	now := time.Now()
	b.failures = append(b.failures, now)
	recent := 0
	for _, t := range b.failures {
		if now.Sub(t) < shortLived*time.Duration(crashLoopFailures) {
			recent++
		}
	}
	if recent >= crashLoopFailures {
		b.stopReason = fmt.Sprintf("Splash stopped restarting the engine: it failed %d times in a row (%s). Restart the Splash server after fixing the cause.", crashLoopFailures, err)
		b.engineError = b.stopReason
		b.mu.Unlock()
		printStatus("Engine failed · "+err.Error(), true)
		printStatus("Engine stopped · "+b.stopReason, true)
		return
	}
	delay := time.Duration(1<<uint(recent-1)) * time.Second
	b.recovering = true
	b.mu.Unlock()
	printStatus("Engine failed · "+err.Error(), true)
	go func() {
		time.Sleep(delay)
		if err := b.rt.Start(); err != nil {
			b.mu.Lock()
			b.recovering = false
			b.mu.Unlock()
			printStatus("Engine restart failed · "+err.Error(), true)
			b.engineFailed(err)
			return
		}
		b.mu.Lock()
		b.recovering = false
		restarted := b.engineError != ""
		b.engineError = ""
		b.mu.Unlock()
		if restarted {
			printStatus("Engine restarted", false)
		}
	}()
}

// Refusal is the error a request not yet admitted gets now; nil while the
// engine serves.
func (b *Backend) Refusal() *APIError {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closing {
		return apiErrCode(503, "server is shutting down", "server_shutdown")
	}
	if b.stopReason != "" {
		return apiErrCode(500, b.stopReason, "engine_failed")
	}
	if b.rt.Ready() {
		return nil
	}
	msg := "engine is recovering; retry shortly"
	if b.engineError != "" {
		msg += " (last failure: " + b.engineError + ")"
	}
	e := apiErrCode(503, msg, "engine_recovering")
	e.RetryAfter = int(math.Max(1, math.Min(60, math.Ceil(b.rt.LastStartSeconds()))))
	return e
}

func (b *Backend) thinkingBudget(j *Job) *wire.ThinkingBudget {
	if j.ThinkingBudget == nil {
		return nil
	}
	end := b.thinkingEnd
	if end == nil {
		end = []uint32{uint32(j.Format.ThinkEndID)}
	}
	also := uint32(j.Format.ThinkEndID)
	if j.MayCallTools() && j.Format.callsEndReasoning() {
		also = uint32(j.Format.CallOpenID)
	}
	return &wire.ThinkingBudget{Tokens: uint32(*j.ThinkingBudget), End: end, AlsoEnds: also}
}

// Submit writes the job to the engine.
func (b *Backend) Submit(j *Job) error {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return apiErrCode(503, "server is shutting down", "server_shutdown")
	}
	b.mu.Unlock()
	j.events = newEventQueue()
	h := &jobHandler{b: b, job: j}
	h.streamer = &streamer{tok: b.tok, emit: func(text string) { j.events.put(event{kind: "text", text: text}) },
		stream: b.tok.NewDecodeStream()}
	if len(j.StopSequences) > 0 {
		h.streamer.stops = NewStopSearch(j.StopSequences)
		h.streamer.onStop = func() {
			j.callMu.Lock()
			c := j.call
			j.callMu.Unlock()
			if c != nil {
				go c.Stop()
			}
		}
	}
	req := &wire.Request{Priority: j.Priority, MaxOutputTokens: uint32(j.MaxNewTokens), Prompt: j.Prompt,
		Sampling: j.Sampling, Seed: j.Seed, GenerationPromptTokens: uint32(j.GenerationPrompt), Flags: j.Flags,
		CacheSaltLo: j.CacheSaltLo, CacheSaltHi: j.CacheSaltHi, Thinking: b.thinkingBudget(j)}
	var masks func([]uint32) ([]byte, error)
	if c := j.Constraint; c != nil {
		masks = func(sim []uint32) ([]byte, error) {
			m, err := c.Masks(sim)
			if err != nil {
				return nil, &MaskFailed{Msg: err.Error()}
			}
			return m, nil
		}
	}
	j.callMu.Lock()
	call, err := b.rt.SubmitConstrained(req, j.Deadline, h, masks)
	if err == nil {
		j.call = call
		if j.cancelled {
			go call.Cancel()
		}
	}
	j.callMu.Unlock()
	if err != nil {
		var ee *EngineError
		if errors.As(err, &ee) {
			if r := b.Refusal(); r != nil {
				return r
			}
		}
		return b.apiError(err)
	}
	return nil
}

// Cancel asks the engine to end the job.
func (b *Backend) Cancel(j *Job) {
	j.callMu.Lock()
	j.cancelled = true
	c := j.call
	j.callMu.Unlock()
	if c != nil {
		c.Cancel()
	}
}

var requestCodes = map[string]bool{"invalid_constraint": true, "invalid_count": true, "invalid_deadline": true,
	"invalid_enum_value": true, "invalid_request": true, "invalid_sampling": true, "limit_exceeded": true}

func (b *Backend) apiError(err error) *APIError {
	var ae *APIError
	var rf *RequestFailed
	var ee *EngineError
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.As(err, &rf):
		if rf.Code == "deadline_exceeded" {
			return apiErrCode(504, rf.Msg, "request_timeout")
		}
		if rf.Code == "capacity_exhausted" || rf.Code == "kv_disk_quota" {
			return apiErrCode(400, rf.Msg, rf.Code)
		}
		status := 500
		if rf.Retryable {
			status = 503
		} else if requestCodes[rf.Code] {
			status = 400
		}
		e := apiErrCode(status, rf.Msg, rf.Code)
		if rf.RetryAfter > 0 {
			e.RetryAfter = rf.RetryAfter
		}
		return e
	case errors.As(err, new(*MaskFailed)):
		var mf *MaskFailed
		errors.As(err, &mf)
		if mf.Retryable {
			return apiErrCode(503, mf.Msg, "runtime_busy")
		}
		return apiErrCode(400, mf.Msg, "constraint_error")
	case errors.Is(err, ErrPendingLimit):
		return apiErrCode(503, "request queue is full", "frontend_overloaded")
	case errors.Is(err, errTimeout):
		return apiErrCode(504, "request timed out", "request_timeout")
	case errors.As(err, &ee):
		b.mu.Lock()
		stop := b.stopReason
		closing := b.closing
		b.mu.Unlock()
		if closing {
			return apiErrCode(503, "server is shutting down", "server_shutdown")
		}
		if stop != "" {
			return apiErrCode(500, stop, "engine_failed")
		}
		e := apiErrCode(503, "the inference engine stopped unexpectedly and is restarting; retry the request", "runtime_unavailable")
		e.RetryAfter = int(math.Max(1, math.Ceil(b.rt.LastStartSeconds())))
		return e
	}
	return apiErrCode(500, err.Error(), "runtime_error")
}

// Status is the engine's status JSON with the transport's state.
func (b *Backend) Status() *ojson.Object {
	data, err := b.rt.Status(50*time.Millisecond, false)
	var snapshot *ojson.Object
	stale := err != nil
	b.mu.Lock()
	if err == nil {
		if v, derr := ojson.Decode(data); derr == nil {
			if o, ok := v.(*ojson.Object); ok && o.Value("schema_version") == int64(wire.StatusSchemaVersion) {
				snapshot = o
				b.snapshot, b.snapshotAt = o.Copy(), time.Now()
			}
		}
		if snapshot == nil {
			stale = true
		}
	}
	age := 0.0
	if snapshot == nil {
		if b.snapshot != nil {
			snapshot = b.snapshot.Copy()
			age = float64(time.Since(b.snapshotAt).Microseconds()) / 1000
			// A busy loop keeps its last readiness for a while.
			busy := errors.Is(err, errStatusTimeout)
			snapshot.Set("ready", busy && snapshot.Value("ready") == true && time.Since(b.snapshotAt) < 30*time.Second)
		} else {
			snapshot = ojson.O("schema_version", int64(wire.StatusSchemaVersion), "ready", false)
		}
	}
	ready := !b.closing && b.rt.Ready()
	transport := ojson.O("ready", ready, "recovering", !b.closing && b.stopReason == "" && !ready,
		"stopped", b.stopReason != "", "pending", int64(b.rt.Pending()), "pending_limit", int64(b.rt.pendingCap),
		"restarts", int64(b.rt.Restarts()), "last_crash_trace", nil, "status_stale", stale, "status_age_ms", age)
	if stale {
		msg := b.engineError
		if msg == "" && err != nil {
			msg = err.Error()
		}
		transport.Set("error", msg)
	}
	b.mu.Unlock()
	snapshot.Set("transport", transport)
	if !ready {
		snapshot.Set("ready", false)
	}
	return snapshot
}

// ReleaseWeights asks the engine to give its weights back now.
func (b *Backend) ReleaseWeights() (*ojson.Object, error) {
	data, err := b.rt.Status(5*time.Second, true)
	if err != nil {
		if errors.Is(err, errStatusTimeout) {
			return nil, apiErrCode(503, "the engine did not answer within 5 s; retry shortly", "engine_busy")
		}
		if r := b.Refusal(); r != nil {
			return nil, r
		}
		return nil, b.apiError(err)
	}
	v, err := ojson.Decode(data)
	if err != nil {
		return nil, apiErrCode(500, "native status does not match the current schema", "runtime_error")
	}
	o, _ := v.(*ojson.Object)
	return o, nil
}

// Close stops the engine; requests in flight end with server_shutdown.
func (b *Backend) Close() {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return
	}
	b.closing = true
	b.mu.Unlock()
	b.rt.Close()
}

func (b *Backend) logStarted(j *Job, cached int) {
	line := fmt.Sprintf("Started · input %s · cached %s", commas(len(j.Prompt)), commas(cached))
	if r := j.Reasoning; r != nil {
		if r.Sent != nil {
			line += " · effort " + *r.Sent
		} else if r.Effort != nil {
			line += " · effort absent → " + *r.Effort
		} else {
			line += " · effort absent"
		}
		if r.EnableThinking != nil {
			line += fmt.Sprintf(" · enable_thinking %t", *r.EnableThinking)
		}
		if j.Thinking {
			line += " · thinking on"
		} else {
			line += " · thinking off"
		}
	}
	b.log(func() { printStatus(line, false) })
}

var engineResourceCodes = map[string]bool{"capacity_exhausted": true, "kv_disk_quota": true, "kv_write_budget": true, "resource_timeout": true}

func (b *Backend) logEnd(j *Job, r *Result, e *APIError) {
	if e != nil {
		line := "Error · " + e.Code
		if engineResourceCodes[e.Code] {
			line += " · " + e.Message
		}
		b.log(func() { printStatus(line, true) })
		return
	}
	head := "Done"
	if r.Reason == "cancelled" {
		head = "Cancelled"
	}
	parts := []string{head, "input " + commas(len(j.Prompt)), "cached " + commas(r.Cache.Matched)}
	if r.Cache.Deduplicated > 0 {
		parts = append(parts, "deduplicated "+commas(r.Cache.Deduplicated))
	}
	parts = append(parts, "output "+commas(r.CompletionTokens))
	if j.ToolsCount > 0 {
		parts = append(parts, fmt.Sprintf("tools %d·%s", j.ToolsCount, j.ToolsSignature))
	}
	latency := r.Metrics().Value("request_latency").(*ojson.Object)
	if ttft, ok := latency.Value("ttft_ms").(float64); ok {
		parts = append(parts, fmt.Sprintf("TTFT %.1fs", ttft/1000))
	}
	if speed, ok := latency.Value("stream_tokens_per_second").(float64); ok {
		parts = append(parts, fmt.Sprintf("%.1f tok/s", speed))
	}
	if r.Cycles > 0 {
		parts = append(parts, fmt.Sprintf("%.1f tok/cycle", 1+float64(r.Accepted)/float64(r.Cycles)))
	}
	line := strings.Join(parts, " · ")
	b.log(func() { printStatus(line, false) })
}

func (b *Backend) log(f func()) {
	select {
	case b.logs <- f:
	default:
	}
}

func commas(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func printStatus(message string, isErr bool) {
	stream := stdout
	if isErr {
		stream = stderr
	}
	stream.write(time.Now().Format("15:04:05") + " " + message + "\n")
}
