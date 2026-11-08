package server

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"splash/goserver/ojson"
)

const (
	sseKeepalive      = 2 * time.Second
	disconnectPoll    = 100 * time.Millisecond
	httpIOTimeout     = 30 * time.Second
	uploadBytesPerSec = 512 * 1024
	requestBodyBudget = 512 * 1024 * 1024
)

// Server is the HTTP side.
type Server struct {
	App            *Frontend
	Host           string
	Port           int
	APIKey         string
	WebUI          bool
	MaxRequest     int64
	AllowedHosts   map[string]bool
	AllowedOrigins map[string]bool // "*" or "scheme://host:port"
	assets         map[string][]byte
	requests       chan struct{}
	tokenCounts    chan struct{}
	instanceID     string
	started        time.Time
	refusedMu      sync.Mutex
	refused        map[string]bool
	bodyMu         sync.Mutex
	bodyBytes      int64
	bodyBudget     int64
}

// NewServer prepares the HTTP side; root is the Splash checkout or install.
func NewServer(app *Frontend, root string, queue int) *Server {
	s := &Server{App: app, assets: map[string][]byte{}, requests: make(chan struct{}, queue),
		tokenCounts: make(chan struct{}, queue), instanceID: randomHex(12), started: time.Now(), refused: map[string]bool{}}
	dir := filepath.Join(root, "server")
	read := func(name string) []byte {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil
		}
		return data
	}
	s.assets["/"] = read("chat.html")
	s.assets["/favicon.ico"] = read("favicon.svg")
	for _, name := range []string{"markdown-it-15.0.2.min.js", "highlight-11.12.0.min.js", "markdown-links.js",
		"github-11.12.0.min.css", "github-dark-11.12.0.min.css"} {
		s.assets["/chat-assets/"+name] = read(filepath.Join("chat-assets", name))
	}
	return s
}

type exchange struct {
	s           *Server
	w           http.ResponseWriter
	r           *http.Request
	path        string
	anthropic   bool
	started     bool
	lastWrite   time.Time
	allowOrigin string
	flusher     http.Flusher
}

func normalizePath(raw string) string {
	p, _, _ := strings.Cut(raw, "?")
	p, _, _ = strings.Cut(p, "#")
	if d, err := url.PathUnescape(p); err == nil {
		p = d
	}
	if !strings.HasPrefix(p, "/") {
		return p
	}
	trailing := strings.HasSuffix(p, "/") && len(p) > 1
	var segs []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
		default:
			segs = append(segs, seg)
		}
	}
	out := "/" + strings.Join(segs, "/")
	if trailing && out != "/" {
		out += "/"
	}
	return out
}

func isAnthropicPath(p string) bool {
	return p == "/v1/messages" || strings.HasPrefix(p, "/v1/messages/")
}

var publicPaths = map[string]bool{"/health": true, "/ready": true}

func (s *Server) isPage(p string) bool {
	if p == "/" || p == "/index.html" || p == "/favicon.ico" {
		return true
	}
	_, ok := s.assets[p]
	return ok && strings.HasPrefix(p, "/chat-assets/")
}

func parseAuthority(v string) (string, string, error) {
	if v == "" {
		return "", "", errors.New("invalid authority")
	}
	for _, c := range []byte(v) {
		if c <= 32 || c >= 127 {
			return "", "", errors.New("invalid authority")
		}
	}
	u, err := url.Parse("//" + v)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("invalid authority")
	}
	return strings.TrimRight(strings.ToLower(u.Hostname()), "."), u.Port(), nil
}

func parseOrigin(v string) (string, error) {
	if strings.Contains(v, "*") {
		return "", errors.New("invalid origin")
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid origin")
	}
	host, port, err := parseAuthority(u.Host)
	if err != nil {
		return "", err
	}
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return u.Scheme + "://" + host + ":" + port, nil
}

// ParseAllowedOrigin normalizes an --allowed-origin value.
func ParseAllowedOrigin(v string) (string, error) {
	if v == "*" {
		return v, nil
	}
	if strings.Contains(v, "*") {
		return "", fmt.Errorf("%s is not an origin: only a bare '*' admits every origin", v)
	}
	o, err := parseOrigin(v)
	if err != nil {
		return "", fmt.Errorf("%s is not an origin: expected a scheme and a host, as in tauri://localhost or http://localhost:3000, or '*' for every origin", v)
	}
	return o, nil
}

func (x *exchange) validateHeaders() *APIError {
	hosts := x.r.Header.Values("Host")
	if x.r.Host != "" {
		hosts = []string{x.r.Host}
	}
	origins := x.r.Header.Values("Origin")
	if len(hosts) != 1 || len(origins) > 1 {
		return apiErrCode(403, "expected one Host header and at most one Origin header", "forbidden")
	}
	host, port, err := parseAuthority(hosts[0])
	if err != nil {
		return apiErrCode(403, "invalid Host header", "forbidden")
	}
	allowed := x.s.AllowedHosts[host]
	if !allowed {
		if local, ok := x.r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			if h, _, err := net.SplitHostPort(local.String()); err == nil && strings.ToLower(h) == host {
				allowed = true
			}
		}
	}
	if !allowed {
		return apiErrCode(403, fmt.Sprintf("Host %s is not allowed; restart the server with --allowed-host %s to accept it", host, host), "forbidden")
	}
	if len(origins) == 0 {
		return nil
	}
	if x.s.AllowedOrigins["*"] {
		x.allowOrigin = "*"
		return nil
	}
	origin, err := parseOrigin(origins[0])
	if err != nil {
		return apiErrCode(403, "invalid Origin header", "forbidden")
	}
	scheme, _, _ := strings.Cut(origin, "://")
	def := map[string]string{"http": "80", "https": "443"}[scheme]
	if def != "" {
		p := port
		if p == "" {
			p = def
		}
		if origin == scheme+"://"+host+":"+p {
			return nil
		}
	}
	if !x.s.AllowedOrigins[origin] {
		x.s.refusedMu.Lock()
		if !x.s.refused[origins[0]] && len(x.s.refused) <= 32 {
			x.s.refused[origins[0]] = true
			printStatus(fmt.Sprintf("Refused · Origin %s · restart with --allowed-origin %s to accept it", origins[0], origins[0]), true)
		}
		x.s.refusedMu.Unlock()
		return apiErrCode(403, fmt.Sprintf("Origin %s is not allowed; restart the server with --allowed-origin %s to accept it", origins[0], origins[0]), "forbidden")
	}
	x.allowOrigin = origins[0]
	return nil
}

func (x *exchange) authenticate() *APIError {
	key := x.s.APIKey
	if key == "" {
		return nil
	}
	auth := x.r.Header.Values("Authorization")
	keys := x.r.Header.Values("X-Api-Key")
	supplied := append([]string(nil), keys...)
	valid := true
	for _, h := range auth {
		scheme, value, ok := strings.Cut(h, " ")
		if ok && strings.EqualFold(scheme, "bearer") {
			supplied = append(supplied, value)
		} else {
			valid = false
		}
	}
	if len(supplied) == 0 || len(auth) > 1 || len(keys) > 1 || !valid {
		return apiErrCode(401, "invalid or missing API key", "authentication_error")
	}
	for _, v := range supplied {
		if subtle.ConstantTimeCompare([]byte(v), []byte(key)) != 1 {
			return apiErrCode(401, "invalid or missing API key", "authentication_error")
		}
	}
	return nil
}

var anthropicTypes = map[int]string{401: "authentication_error", 403: "permission_error", 404: "not_found_error",
	408: "timeout_error", 413: "request_too_large", 429: "rate_limit_error", 503: "overloaded_error", 504: "timeout_error"}

func (x *exchange) errorPayload(e *APIError) *ojson.Object {
	if x.anthropic {
		msg := e.Message
		if e.contextLength {
			msg = fmt.Sprintf("prompt is too long: %d tokens > %d maximum input tokens", e.InputTokens, e.MaxInputTokens)
		}
		t, ok := anthropicTypes[e.Status]
		if !ok {
			t = "invalid_request_error"
			if e.Status >= 500 {
				t = "api_error"
			}
		}
		return ojson.O("type", "error", "error", ojson.O("type", t, "message", msg))
	}
	t := "invalid_request_error"
	if e.Status >= 500 {
		t = "server_error"
	}
	return ojson.O("error", ojson.O("message", e.Message, "type", t, "code", e.Code))
}

func (x *exchange) headers() http.Header {
	h := x.w.Header()
	if x.allowOrigin != "" {
		h.Set("Access-Control-Allow-Origin", x.allowOrigin)
		if x.allowOrigin != "*" {
			h.Set("Vary", "Origin")
		}
		h.Set("Access-Control-Expose-Headers", "Retry-After, WWW-Authenticate")
	}
	h.Set("Server", "Splash")
	return h
}

func (x *exchange) send(status int, data []byte, contentType string, retryAfter int) {
	if x.started {
		return
	}
	x.started = true
	h := x.headers()
	if status == 503 || status == 529 {
		if retryAfter < 1 {
			retryAfter = 1
		}
		h.Set("Retry-After", strconv.Itoa(retryAfter))
	}
	if status == 401 {
		h.Set("WWW-Authenticate", "Bearer")
	}
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("Connection", "close")
	if strings.HasPrefix(x.path, "/v1/models") || x.path == "/v1/systemone" {
		h.Set("X-Request-Id", "req_"+randomHex(12))
	}
	x.w.WriteHeader(status)
	if x.r.Method != http.MethodHead {
		_, _ = x.w.Write(data)
	}
}

func (x *exchange) sendJSON(status int, payload any) {
	x.send(status, ojson.Marshal(payload), "application/json", 1)
}

func (x *exchange) fail(err error, log bool) {
	var e *APIError
	if !errors.As(err, &e) {
		printStatus("Error · internal_server_error · "+fmt.Sprintf("%T", err), true)
		e = apiErrCode(500, "internal server error", "internal_server_error")
		log = false
	}
	if x.started {
		return
	}
	if log {
		p := x.path
		if len(p) > 256 {
			p = p[:256]
		}
		printStatus(fmt.Sprintf("Error · %s · %s %s", e.Code, x.r.Method, strings.Map(func(r rune) rune {
			if r < 32 || r == 127 {
				return '?'
			}
			return r
		}, p)), true)
	}
	x.send(e.Status, ojson.Marshal(x.errorPayload(e)), "application/json", e.RetryAfter)
}

// ---------------------------------------------------------------- SSE

func (x *exchange) startStream() {
	if x.started {
		return
	}
	x.started = true
	h := x.headers()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "close")
	x.w.WriteHeader(200)
	x.flush()
}

func (x *exchange) flush() {
	if x.flusher != nil {
		x.flusher.Flush()
	}
	x.lastWrite = time.Now()
}

func (x *exchange) sse(payload any) {
	var data []byte
	if s, ok := payload.(string); ok {
		data = []byte(s)
	} else {
		data = ojson.Marshal(payload)
	}
	_, _ = x.w.Write(append(append([]byte("data: "), data...), '\n', '\n'))
	x.flush()
}

func (x *exchange) eventSSE(name string, payload any) {
	_, _ = x.w.Write([]byte("event: " + name + "\ndata: "))
	_, _ = x.w.Write(ojson.Marshal(payload))
	_, _ = x.w.Write([]byte("\n\n"))
	x.flush()
}

func (x *exchange) sseKeepalive() {
	x.startStream()
	_, _ = x.w.Write([]byte(": splash-keepalive\n\n"))
	x.flush()
}

// ---------------------------------------------------------------- collection

var errDisconnected = errors.New("client disconnected")

func (x *exchange) nextEvent(j *Job, onIdle func()) (event, error) {
	for {
		select {
		case <-x.r.Context().Done():
			x.s.App.Backend.Cancel(j)
			return event{}, errDisconnected
		default:
		}
		wait := disconnectPoll
		if !j.Deadline.IsZero() {
			left := time.Until(j.Deadline)
			if left <= 0 {
				x.s.App.Backend.Cancel(j)
				return event{}, apiErrCode(504, "request timed out", "request_timeout")
			}
			if left < wait {
				wait = left
			}
		}
		e, ok := j.events.get(wait)
		if ok && e.kind == "error" {
			return event{}, e.err
		}
		if onIdle != nil && time.Since(x.lastWrite) >= sseKeepalive {
			onIdle()
		}
		if ok {
			return e, nil
		}
	}
}

// Collected is a generation as collect gathered it.
type Collected struct {
	Reasoning     string
	Content       string
	ToolCalls     []ToolCall
	Result        *Result
	ReasoningOpen bool
	CutInCall     bool
	CutCall       string
}

func (c *Collected) Cut() bool { return c.Result.Reason == "length" || c.CutCall != "" }
func (c *Collected) Called() bool {
	return len(c.ToolCalls) > 0 && !c.CutInCall && c.Result.Reason != "length"
}

type callbacks struct {
	onStart    func()
	onText     func(field, text string)
	onTool     func(*ojson.Object)
	onIdle     func()
	onProgress func(*ojson.Object)
}

func (x *exchange) collect(j *Job, cb callbacks) (*Collected, error) {
	splitter := NewReasoningSplitter(j.Thinking, j.MayCallTools(), j.Format)
	var projector *Projector
	if j.ToolPolicy != nil || j.Reasoning != nil {
		projector = NewProjector(j.ToolPolicy, j.PublicID, false, j.Format.Syntax)
	}
	var reasoning, content strings.Builder
	var result *Result
	callsSeen := 0
	publish := func(events []textEvent) {
		for _, e := range events {
			if e.tool != nil {
				if j.FirstCallOnly {
					idx, _ := e.tool.Value("index").(int64)
					if idx > 0 {
						continue
					}
					if e.tool.Has("id") {
						callsSeen++
					}
				}
				if cb.onTool != nil {
					cb.onTool(e.tool)
				}
			} else if cb.onText != nil {
				cb.onText(e.field, e.text)
			}
		}
	}
	appendText := func(field, text string) {
		if field == "content" && projector != nil {
			publish(projector.Put(text))
			return
		}
		if field == "reasoning_content" {
			reasoning.WriteString(text)
		} else {
			content.WriteString(text)
		}
		publish([]textEvent{{field: field, text: text}})
	}
	for result == nil {
		e, err := x.nextEvent(j, cb.onIdle)
		if err != nil {
			return nil, err
		}
		switch e.kind {
		case "start":
			if cb.onStart != nil {
				cb.onStart()
			}
		case "text":
			for _, piece := range splitter.Put(e.text) {
				appendText(piece.field, piece.text)
			}
		case "progress":
			if cb.onProgress != nil {
				cb.onProgress(e.progress)
			}
		case "done":
			result = e.result
		}
	}
	for _, piece := range splitter.Finish() {
		appendText(piece.field, piece.text)
	}
	c := &Collected{Reasoning: reasoning.String(), Result: result, ReasoningOpen: splitter.reasoning}
	if projector == nil {
		c.Content = content.String()
		return c, nil
	}
	c.CutInCall = projector.InCall() && (result.Reason == "length" || result.StopSequence != nil)
	text, calls, events := projector.Finish(result.Reason == "length" || c.CutInCall)
	publish(events)
	if j.FirstCallOnly && len(calls) > 1 {
		calls = calls[:1]
	}
	c.Content, c.ToolCalls, c.CutCall = text, calls, projector.CallID()
	return c, nil
}

// guarded runs a stream; a failure after the response began is written as
// the dialect's error event.
func (x *exchange) guarded(j *Job, run func() error, sendError func(*APIError)) error {
	err := run()
	if err == nil {
		return nil
	}
	x.s.App.Backend.Cancel(j)
	if errors.Is(err, errDisconnected) {
		return nil
	}
	if !x.started {
		return err
	}
	var e *APIError
	if !errors.As(err, &e) {
		e = apiErrCode(500, "internal server error", "internal_server_error")
	}
	if e.Status >= 500 {
		printStatus("Error · "+e.Code, true)
	}
	sendError(e)
	return nil
}

// ---------------------------------------------------------------- routing

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	x := &exchange{s: s, w: w, r: r, path: normalizePath(r.URL.RequestURI()), lastWrite: time.Now()}
	x.flusher, _ = w.(http.Flusher)
	x.anthropic = isAnthropicPath(x.path)
	if r.ProtoMajor != 1 {
		x.fail(apiErrCode(505, "HTTP version not supported", "http_version_not_supported"), false)
		return
	}
	if e := x.validateHeaders(); e != nil {
		x.fail(e, false)
		return
	}
	public := r.Method == http.MethodOptions || publicPaths[x.path] || s.isPage(x.path)
	if !public {
		if e := x.authenticate(); e != nil {
			x.fail(e, false)
			return
		}
	}
	switch r.Method {
	case http.MethodOptions:
		h := x.headers()
		methods := "GET, HEAD, POST, DELETE, OPTIONS"
		h.Set("Allow", methods)
		if x.allowOrigin != "" && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", methods)
			if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
				h.Set("Access-Control-Allow-Headers", req)
			}
			h.Set("Access-Control-Max-Age", "600")
		}
		h.Set("Content-Length", "0")
		h.Set("Connection", "close")
		w.WriteHeader(204)
		return
	case http.MethodGet, http.MethodHead:
		s.get(x)
	case http.MethodPost:
		s.post(x)
	case http.MethodDelete:
		if strings.HasPrefix(x.path, "/v1/responses/") {
			x.fail(notImplemented("the Responses API"), true)
			return
		}
		x.fail(apiErrCode(404, "not found", "not_found"), true)
	default:
		x.fail(apiErrCode(501, "Unsupported method", "not_implemented"), false)
	}
}

func notImplemented(what string) *APIError {
	return apiErrCode(501, what+" is not implemented by the Go server yet", "not_implemented")
}

func (s *Server) get(x *exchange) {
	p := x.path
	switch {
	case s.isPage(p):
		if !s.WebUI {
			x.fail(apiErrCode(404, "not found", "not_found"), true)
			return
		}
		switch {
		case p == "/favicon.ico":
			x.send(200, s.assets["/favicon.ico"], "image/svg+xml", 1)
		case strings.HasPrefix(p, "/chat-assets/"):
			mime := "text/javascript"
			if strings.HasSuffix(p, ".css") {
				mime = "text/css"
			}
			x.send(200, s.assets[p], mime+"; charset=utf-8", 1)
		default:
			x.send(200, s.assets["/"], "text/html; charset=utf-8", 1)
		}
	case p == "/health":
		x.sendJSON(200, ojson.O("status", "ok"))
	case p == "/ready":
		if s.App.Backend.Status().Value("ready") == true {
			x.sendJSON(200, ojson.O("status", "ready"))
		} else {
			x.sendJSON(503, ojson.O("status", "unavailable"))
		}
	case p == "/status":
		x.sendJSON(200, s.status())
	case p == "/metrics":
		x.send(200, []byte(prometheusMetrics(s.status())), "text/plain; version=0.0.4; charset=utf-8", 1)
	case p == "/v1/models":
		models := s.models()
		x.sendJSON(200, ojson.O("object", "list", "data", models, "models", typedModels(models)))
	case strings.HasPrefix(p, "/v1/models/"):
		name := strings.TrimPrefix(p, "/v1/models/")
		for _, m := range s.models() {
			if m.(*ojson.Object).Value("id") == name {
				x.sendJSON(200, m)
				return
			}
		}
		x.fail(apiErrCode(404, "model not found", "model_not_found"), true)
	case strings.HasPrefix(p, "/v1/responses/"):
		x.fail(notImplemented("the Responses API"), true)
	default:
		x.fail(apiErrCode(404, "not found", "not_found"), true)
	}
}

func (s *Server) models() []any {
	app := s.App
	var out []any
	modalities := []any{"text"}
	for _, name := range app.ModelNames {
		m := ojson.O("id", name, "object", "model", "created", int64(0), "owned_by", "splash",
			"max_model_len", int64(app.MaxContext()), "context_length", int64(app.MaxContext()),
			"vision", false, "input_modalities", modalities, "reasoning", app.ReasoningReport())
		if name != app.ResponseModel {
			m.Set("root", app.ResponseModel)
		}
		out = append(out, m)
	}
	return out
}

// typedModels is the scoring dialect's catalog; the Go server serves no
// typed judgments, so it lists none.
func typedModels(_ []any) []any { return []any{} }

func (s *Server) status() *ojson.Object {
	app := s.App
	st := app.Backend.Status()
	st.Set("capabilities", ojson.O("max_context_tokens", int64(app.MaxContext())))
	st.Set("vision", false)
	st.Set("input_modalities", []any{"text"})
	st.Set("reasoning_switch", app.Format.reasoningSwitch())
	st.Set("chat_template", app.Templates.Status())
	app.prepMu.Lock()
	st.Set("frontend", ojson.O("preparation_capacity", int64(cap(app.prepSlots)), "active", int64(app.prepActive),
		"waiting", int64(app.prepWaiting), "implementation", "go"))
	app.prepMu.Unlock()
	st.Set("instance", ojson.O("id", s.instanceID, "pid", int64(os.Getpid()), "model", app.Model, "host", s.Host,
		"port", int64(s.Port), "started_at", float64(s.started.UnixNano())/1e9))
	st.Set("http", ojson.O("requests", ojson.O("active", int64(len(s.requests)), "capacity", int64(cap(s.requests))),
		"max_request_bytes", s.MaxRequest,
		"token_counts", ojson.O("active", int64(len(s.tokenCounts)), "capacity", int64(cap(s.tokenCounts)))))
	return st
}

type postRoute struct {
	handle     func(*exchange, *ojson.Object, time.Time) error
	promptOnly bool
	fieldless  bool
}

func (s *Server) postRoutes() map[string]postRoute {
	return map[string]postRoute{
		"/v1/chat/completions":              {handle: postChat},
		"/v1/chat/completions/count_tokens": {handle: postChatCountTokens, promptOnly: true},
		"/v1/completions":                   {handle: postCompletion},
		"/v1/messages":                      {handle: postMessages},
		"/v1/messages/count_tokens":         {handle: postMessagesCountTokens, promptOnly: true},
		"/tokenize": {handle: func(x *exchange, body *ojson.Object, _ time.Time) error {
			ids, err := x.s.App.Tokenize(body)
			if err != nil {
				return err
			}
			x.sendJSON(200, ojson.O("tokens", ids))
			return nil
		}, promptOnly: true},
		"/apply-template": {handle: func(x *exchange, body *ojson.Object, deadline time.Time) error {
			text, err := x.s.App.ApplyTemplate(body, deadline)
			if err != nil {
				return err
			}
			x.sendJSON(200, ojson.O("prompt", text))
			return nil
		}, promptOnly: true},
		"/v1/memory/release": {handle: func(x *exchange, _ *ojson.Object, _ time.Time) error {
			st, err := x.s.App.Backend.ReleaseWeights()
			if err != nil {
				return err
			}
			weights, _ := st.Value("weights").(*ojson.Object)
			if weights == nil {
				weights = ojson.NewObject()
			}
			if weights.Value("released") == true && weights.Value("restoring") == false {
				x.sendJSON(200, ojson.O("released", true, "weights", weights))
				return nil
			}
			x.fail(apiErrCode(409, "the engine is serving requests or restoring its weights; it releases them only while idle", "engine_busy"), false)
			return nil
		}, fieldless: true},
	}
}

func (s *Server) post(x *exchange) {
	started := time.Now()
	route, ok := s.postRoutes()[x.path]
	if !ok {
		switch x.path {
		case "/v1/responses":
			x.fail(notImplemented("the Responses API"), true)
		case "/v1/judgments", "/v1/systemone":
			x.fail(notImplemented(x.path), true)
		default:
			x.fail(apiErrCode(404, "not found", "not_found"), true)
		}
		return
	}
	if !route.promptOnly {
		if e := s.App.Backend.Refusal(); e != nil {
			x.fail(e, false)
			return
		}
	}
	gate := s.requests
	if route.promptOnly {
		gate = s.tokenCounts
	}
	select {
	case gate <- struct{}{}:
	default:
		x.fail(apiErrCode(503, "frontend request capacity is exhausted", "frontend_overloaded"), true)
		return
	}
	defer func() { <-gate }()
	var body *ojson.Object
	if route.fieldless && x.r.ContentLength <= 0 {
		body = ojson.NewObject()
	} else {
		b, err := s.readBody(x, started)
		if err != nil {
			x.fail(err, true)
			return
		}
		body = ojson.NewObject()
		for _, k := range b.Keys() {
			if v := b.Value(k); v != nil {
				body.Set(k, v)
			}
		}
		if route.fieldless && body.Len() > 0 {
			x.fail(apiErr(400, "this endpoint takes no fields"), true)
			return
		}
	}
	deadline, err := s.App.requestDeadline(body, started)
	if err != nil {
		x.fail(err, true)
		return
	}
	if err := route.handle(x, body, deadline); err != nil && !errors.Is(err, errDisconnected) {
		x.fail(err, true)
	}
}

func (s *Server) readBody(x *exchange, started time.Time) (*ojson.Object, error) {
	r := x.r
	if len(r.TransferEncoding) > 0 {
		return nil, apiErr(400, "transfer encoding is not supported")
	}
	encodings := r.Header.Values("Content-Encoding")
	if len(encodings) > 1 || (len(encodings) == 1 && strings.ToLower(strings.TrimSpace(encodings[0])) != "identity") {
		return nil, apiErr(415, "content encoding is not supported")
	}
	types := r.Header.Values("Content-Type")
	ct := ""
	if len(types) == 1 {
		ct, _, _ = strings.Cut(strings.ToLower(types[0]), ";")
		ct = strings.TrimSpace(ct)
	}
	if len(types) != 1 || !(ct == "application/json" || (strings.HasPrefix(ct, "application/") && strings.HasSuffix(ct, "+json"))) {
		return nil, apiErr(415, "Content-Type must be application/json")
	}
	if len(r.Header.Values("Content-Length")) != 1 {
		return nil, apiErr(400, "exactly one Content-Length header is required")
	}
	length := r.ContentLength
	if length <= 0 {
		return nil, apiErr(400, "request body must not be empty")
	}
	if length > s.MaxRequest {
		return nil, apiErrCode(413, fmt.Sprintf("request body is %d bytes; limit is %d bytes (--max-request-size)", length, s.MaxRequest), "request_too_large")
	}
	s.bodyMu.Lock()
	if s.bodyBytes+length > s.bodyBudget {
		s.bodyMu.Unlock()
		return nil, apiErrCode(503, "frontend request body capacity is exhausted", "frontend_overloaded")
	}
	s.bodyBytes += length
	s.bodyMu.Unlock()
	defer func() {
		s.bodyMu.Lock()
		s.bodyBytes -= length
		s.bodyMu.Unlock()
	}()
	limit := httpIOTimeout + time.Duration(float64(length)/uploadBytesPerSec*float64(time.Second))
	rc := http.NewResponseController(x.w)
	_ = rc.SetReadDeadline(started.Add(limit))
	data := make([]byte, length)
	_, err := io.ReadFull(r.Body, data)
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, apiErrCode(408, "HTTP I/O timed out", "request_timeout")
		}
		return nil, apiErr(400, "request body ended before Content-Length")
	}
	if !utf8.Valid(data) && len(data) >= 2 && (data[0] == 0 || data[1] == 0) {
		return nil, apiErr(400, "invalid JSON request body")
	}
	v, err := ojson.Decode(data)
	data = nil
	if err != nil {
		return nil, apiErr(400, "invalid JSON request body")
	}
	o, ok := v.(*ojson.Object)
	if !ok {
		return nil, apiErr(400, "request body must be an object")
	}
	return o, nil
}

// Serve listens and serves until the listener closes.
func (s *Server) Serve(l net.Listener) error {
	s.bodyBudget = requestBodyBudget
	if 2*s.MaxRequest > s.bodyBudget {
		s.bodyBudget = 2 * s.MaxRequest
	}
	srv := &http.Server{Handler: s, ReadHeaderTimeout: httpIOTimeout, IdleTimeout: httpIOTimeout,
		ErrorLog: nil, MaxHeaderBytes: 1 << 16}
	return srv.Serve(l)
}

var _ = path.Clean
