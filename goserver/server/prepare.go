package server

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"

	"splash/goserver/jinja"
	"splash/goserver/ojson"
	"splash/goserver/tokenizer"
	"splash/goserver/wire"
)

const completionDefaultMaxTokens = 16
const minSamplingTemperature = 0.01

// Frontend prepares API requests for generation.
type Frontend struct {
	Tok             *tokenizer.Tokenizer
	Backend         *Backend
	Model           string
	ResponseModel   string
	ModelNames      []string
	Templates       *ChatTemplates
	Renderer        *Renderer
	Format          *ChatFormat
	SamplingDefault map[string]any
	DefaultEffort   *string
	DefaultBudget   *int
	RequestTimeout  time.Duration // 0 for none
	Codec           *ThinkingCodec
	// Constraints compiles grammars once the first constrained request
	// loads llguidance; nil where it cannot be loaded.
	Constraints     *ConstraintFactory
	loadConstraints func() (*ConstraintFactory, error)
	constraintsOnce sync.Once
	prepSlots       chan struct{}
	prepMu          sync.Mutex
	prepActive      int
	prepWaiting     int
}

func (f *Frontend) init(capacity int) {
	f.prepSlots = make(chan struct{}, capacity)
}

func (f *Frontend) constraints() *ConstraintFactory {
	f.prepMu.Lock()
	defer f.prepMu.Unlock()
	return f.Constraints
}

func (f *Frontend) MaxContext() int { return int(f.Backend.rt.Capabilities().MaxContext) }

func (f *Frontend) acceptsModel(model any) bool {
	if model == nil || model == "" {
		return true
	}
	s, ok := model.(string)
	if !ok {
		return false
	}
	for _, n := range f.ModelNames {
		if n == s {
			return true
		}
	}
	return false
}

func (f *Frontend) checkModel(body *ojson.Object) error {
	if !f.acceptsModel(body.Value("model")) {
		return apiErrCode(404, fmt.Sprintf("model %v not found", body.Value("model")), "model_not_found")
	}
	return nil
}

// preparation holds a preparation slot (one per engine lane) while a
// request prepares.
func (f *Frontend) preparation(deadline time.Time) (func(), error) {
	f.prepMu.Lock()
	f.prepWaiting++
	f.prepMu.Unlock()
	wait := 30 * time.Second
	if !deadline.IsZero() && time.Until(deadline) < wait {
		wait = time.Until(deadline)
	}
	var acquired bool
	select {
	case f.prepSlots <- struct{}{}:
		acquired = true
	case <-time.After(wait):
	}
	f.prepMu.Lock()
	f.prepWaiting--
	if acquired {
		f.prepActive++
	}
	f.prepMu.Unlock()
	if !acquired {
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, apiErrCode(504, "request timed out", "request_timeout")
		}
		return nil, apiErrCode(503, "frontend preparation capacity is exhausted", "frontend_overloaded")
	}
	return func() {
		f.prepMu.Lock()
		f.prepActive--
		f.prepMu.Unlock()
		<-f.prepSlots
	}, nil
}

func remaining(deadline time.Time) error {
	if !deadline.IsZero() && time.Now().After(deadline) {
		return apiErrCode(504, "request timed out", "request_timeout")
	}
	return nil
}

// ---------------------------------------------------------------- messages

var textParts = map[string]bool{"text": true, "input_text": true, "output_text": true, "refusal": true}

func fieldValue(o *ojson.Object, key string, def any) any {
	v := o.Value(key)
	if v == nil {
		return def
	}
	return v
}

func partText(part *ojson.Object) (string, error) {
	key := "text"
	if part.Value("type") == "refusal" {
		key = "refusal"
	}
	s, ok := fieldValue(part, key, "").(string)
	if !ok {
		return "", apiErr(400, "text message content must be a string")
	}
	return s, nil
}

func textContent(content any) (string, error) {
	switch c := content.(type) {
	case nil:
		return "", nil
	case string:
		return c, nil
	case []any:
		var b strings.Builder
		for _, p := range c {
			part, _ := p.(*ojson.Object)
			kind, _ := part.Value("type").(string)
			if part == nil || !textParts[kind] {
				return "", apiErr(400, "only text message content is supported")
			}
			s, err := partText(part)
			if err != nil {
				return "", err
			}
			b.WriteString(s)
		}
		return b.String(), nil
	}
	return "", apiErr(400, "message content must be text")
}

const mediaUnsupported = "is not supported by the Go server yet"

func userContent(content any) (any, error) {
	list, ok := content.([]any)
	if !ok {
		return textContent(content)
	}
	var b strings.Builder
	for _, p := range list {
		part, ok := p.(*ojson.Object)
		if !ok {
			return nil, apiErr(400, "message content parts must be objects")
		}
		kind, _ := part.Value("type").(string)
		switch {
		case kind == "image_url" || kind == "input_image":
			return nil, apiErr(400, "image input "+mediaUnsupported)
		case textParts[kind]:
			s, err := partText(part)
			if err != nil {
				return nil, err
			}
			b.WriteString(s)
		case kind == "file":
			return nil, apiErr(400, "PDF input "+mediaUnsupported)
		case kind == "video" || kind == "video_url" || kind == "input_audio":
			return nil, apiErr(400, kind+" content is not supported")
		default:
			return nil, apiErr(400, "unsupported message content part")
		}
	}
	return b.String(), nil
}

func callArguments(arguments any) any {
	switch a := arguments.(type) {
	case nil:
		return ojson.NewObject()
	case string:
		text := strings.TrimSpace(a)
		if text == "" {
			text = "{}"
		}
		v, err := ojson.DecodeString(text)
		if err != nil {
			return a
		}
		if o, ok := v.(*ojson.Object); ok {
			return o
		}
		return a
	case *ojson.Object:
		return a
	}
	return ojson.MarshalString(arguments)
}

func normalizeMessages(messages any) ([]any, error) {
	list, ok := messages.([]any)
	if !ok || len(list) == 0 {
		return nil, apiErr(400, "messages must be a non-empty array")
	}
	var out []any
	hasUser := false
	for _, m := range list {
		message, ok := m.(*ojson.Object)
		if !ok {
			return nil, apiErr(400, "each message must be an object")
		}
		role, _ := message.Value("role").(string)
		if role == "system" || role == "developer" {
			text, err := textContent(message.Value("content"))
			if err != nil {
				return nil, err
			}
			if len(out) == 1 && out[0].(*ojson.Object).Value("role") == "system" {
				first := out[0].(*ojson.Object)
				var parts []string
				for _, p := range []string{first.Value("content").(string), text} {
					if p != "" {
						parts = append(parts, p)
					}
				}
				first.Set("content", strings.Join(parts, "\n\n"))
			} else {
				out = append(out, ojson.O("role", "system", "content", text))
			}
			continue
		}
		if role != "user" && role != "assistant" && role != "tool" {
			return nil, apiErr(400, fmt.Sprintf("unsupported message role: %v", message.Value("role")))
		}
		hasUser = hasUser || role == "user"
		var content any
		var err error
		if role == "assistant" {
			content, err = textContent(message.Value("content"))
		} else {
			content, err = userContent(message.Value("content"))
		}
		if err != nil {
			return nil, err
		}
		item := ojson.O("role", role, "content", content)
		if role == "assistant" {
			reasoning := message.Value("reasoning_content")
			if reasoning == nil {
				if s, ok := message.Value("reasoning").(string); ok {
					reasoning = s
				}
			}
			if reasoning != nil {
				r, err := textContent(reasoning)
				if err != nil {
					return nil, err
				}
				item.Set("reasoning_content", r)
			}
			if tc := message.Value("tool_calls"); tc != nil {
				calls, ok := tc.([]any)
				if !ok {
					return nil, apiErr(400, "assistant tool_calls must be an array")
				}
				var outCalls []any
				for _, c := range calls {
					call, _ := c.(*ojson.Object)
					fn, _ := call.Value("function").(*ojson.Object)
					name, ok := fn.Value("name").(string)
					if fn == nil || !ok {
						return nil, apiErr(400, "invalid assistant tool call")
					}
					oc := ojson.O("type", "function", "function", ojson.O("name", name, "arguments", callArguments(fn.Value("arguments"))))
					if id := call.Value("id"); id != nil {
						s, ok := id.(string)
						if !ok {
							return nil, apiErr(400, "tool call id must be a string")
						}
						oc.Set("id", s)
					}
					outCalls = append(outCalls, oc)
				}
				if outCalls == nil {
					outCalls = []any{}
				}
				item.Set("tool_calls", outCalls)
			}
		}
		if role == "tool" {
			if id := message.Value("tool_call_id"); id != nil {
				s, ok := id.(string)
				if !ok {
					return nil, apiErr(400, "tool_call_id must be a string")
				}
				item.Set("tool_call_id", s)
			}
		}
		out = append(out, item)
	}
	if !hasUser {
		return nil, apiErr(400, "messages must include a user message")
	}
	return out, nil
}

// templateMessages keeps a call whose arguments are text as text.
func templateMessages(messages []any) []any {
	var result []any
	for _, m := range messages {
		message := m.(*ojson.Object)
		calls, _ := message.Value("tool_calls").([]any)
		hasText := false
		for _, c := range calls {
			if _, ok := c.(*ojson.Object).Value("function").(*ojson.Object).Value("arguments").(string); ok {
				hasText = true
			}
		}
		if !hasText {
			result = append(result, message)
			continue
		}
		current := message.Copy()
		current.Delete("tool_calls")
		nonEmpty := func(o *ojson.Object) bool {
			for _, k := range []string{"content", "reasoning_content", "tool_calls"} {
				if jinjaTruthy(o.Value(k)) {
					return true
				}
			}
			return false
		}
		for _, c := range calls {
			call := c.(*ojson.Object)
			fn := call.Value("function").(*ojson.Object)
			if _, isText := fn.Value("arguments").(string); !isText {
				existing, _ := current.Value("tool_calls").([]any)
				current.Set("tool_calls", append(existing, call))
				continue
			}
			if nonEmpty(current) {
				result = append(result, current)
			}
			result = append(result, ojson.O("role", "assistant", "content", "Incomplete tool call:\n"+ojson.PyDumps(fn)))
			current = ojson.O("role", "assistant", "content", "")
		}
		if nonEmpty(current) {
			result = append(result, current)
		}
	}
	return result
}

func jinjaTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case *ojson.Object:
		return x.Len() > 0
	case bool:
		return x
	}
	return true
}

// ---------------------------------------------------------------- options

type samplingRule struct {
	name    string
	def     float64
	ok      func(float64) bool
	require string
}

var samplingRules = []samplingRule{
	{"temperature", 1.0, func(v float64) bool { return v >= 0 && v <= 2 }, "a number in [0, 2]"},
	{"top_p", 1.0, func(v float64) bool { return v >= 0x1p-149 && v <= 1 }, "a number in (0, 1]"},
	{"presence_penalty", 0, func(v float64) bool { return v >= -2 && v <= 2 }, "a number in [-2, 2]"},
	{"frequency_penalty", 0, func(v float64) bool { return v >= -2 && v <= 2 }, "a number in [-2, 2]"},
	{"repetition_penalty", 1.0, func(v float64) bool { return v >= 0x1p-149 && v <= math.MaxFloat32 }, "a positive number"},
	{"min_p", 0, func(v float64) bool { return v >= 0 && v <= 1 }, "a number in [0, 1]"},
}

func finiteNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, !math.IsInf(x, 0) && !math.IsNaN(x)
	case *big.Int:
		f, _ := new(big.Float).SetInt(x).Float64()
		return f, !math.IsInf(f, 0)
	}
	return 0, false
}

func isInt(v any) (int64, bool) {
	i, ok := v.(int64)
	return i, ok
}

// GenerationOptions are sampling, stop and scheduling options.
type GenerationOptions struct {
	Sampling       wire.Sampling
	Stops          []string
	IgnoreEOS      bool
	Seed           uint64
	Priority       uint8
	SaltLo         uint64
	SaltHi         uint64
	ThinkingBudget *int
}

func (f *Frontend) generationOptions(body *ojson.Object, stopField string) (*GenerationOptions, error) {
	numbers := map[string]float64{}
	for _, rule := range samplingRules {
		v := body.Value(rule.name)
		if v == nil {
			if d, ok := f.SamplingDefault[rule.name]; ok {
				v = d
			} else {
				v = rule.def
			}
		}
		n, ok := finiteNumber(v)
		if !ok || !rule.ok(n) {
			return nil, apiErr(400, rule.name+" must be "+rule.require)
		}
		numbers[rule.name] = n
	}
	if t := numbers["temperature"]; t > 0 && t < minSamplingTemperature {
		numbers["temperature"] = minSamplingTemperature
	}
	topKValue := body.Value("top_k")
	if topKValue == nil {
		if d, ok := f.SamplingDefault["top_k"]; ok {
			topKValue = d
		} else {
			topKValue = int64(0)
		}
	}
	topK, ok := isInt(topKValue)
	if !ok || topK < -1 {
		if b, isBig := topKValue.(*big.Int); isBig && b.Sign() > 0 {
			topK, ok = math.MaxUint32, true
		} else {
			return nil, apiErr(400, "top_k must be 0 or -1 (disabled) or a positive integer")
		}
	}
	if topK < 0 {
		topK = 0
	}
	if topK > math.MaxUint32 {
		topK = math.MaxUint32
	}
	var stops []string
	switch s := body.Value("stop").(type) {
	case nil:
	case string:
		if s == "" {
			return nil, apiErr(400, "stop must be a string or up to four strings")
		}
		stops = []string{s}
	case []any:
		for _, item := range s {
			str, ok := item.(string)
			if !ok || str == "" {
				return nil, apiErr(400, "stop must be a string or up to four strings")
			}
			stops = append(stops, str)
		}
	default:
		return nil, apiErr(400, "stop must be a string or up to four strings")
	}
	if len(stops) > 4 {
		return nil, apiErr(400, stopField+" must be up to four strings")
	}
	for _, s := range stops {
		if len([]rune(s)) > maxStopChars {
			return nil, apiErr(400, fmt.Sprintf("each of %s must be at most %d characters", stopField, maxStopChars))
		}
	}
	if lb := body.Value("logit_bias"); lb != nil {
		if o, ok := lb.(*ojson.Object); !ok || o.Len() > 0 {
			return nil, apiErr(400, "logit_bias is not supported")
		}
	}
	ignoreEOS := false
	if v := body.Value("ignore_eos"); v != nil {
		b, ok := v.(bool)
		if !ok {
			return nil, apiErr(400, "ignore_eos must be a boolean")
		}
		ignoreEOS = b
	}
	var seed uint64
	switch s := body.Value("seed").(type) {
	case nil:
		seed = randomUint64()
	case int64:
		if s == -1 {
			seed = randomUint64()
		} else {
			seed = uint64(s)
		}
	case *big.Int:
		if s.Sign() < 0 || s.BitLen() > 64 {
			return nil, apiErr(400, "seed must be a 64-bit integer")
		}
		seed = s.Uint64()
	default:
		return nil, apiErr(400, "seed must be a 64-bit integer")
	}
	var budget *int
	switch b := body.Value("thinking_token_budget").(type) {
	case nil:
		budget = f.DefaultBudget
	case int64:
		if b < -1 {
			return nil, apiErr(400, "thinking_token_budget must be a non-negative integer or -1 for no cap")
		}
		if b >= 0 {
			v := int(b)
			budget = &v
		}
	default:
		return nil, apiErr(400, "thinking_token_budget must be a non-negative integer or -1 for no cap")
	}
	priority := uint8(wire.PriorityNormal)
	if p := body.Value("priority"); p != nil {
		switch p {
		case "foreground":
			priority = wire.PriorityForeground
		case "normal":
			priority = wire.PriorityNormal
		case "background":
			priority = wire.PriorityBackground
		default:
			return nil, apiErr(400, "priority must be foreground, normal, or background")
		}
	}
	lo, hi, err := cacheSalt(body)
	if err != nil {
		return nil, err
	}
	return &GenerationOptions{
		Sampling: wire.Sampling{Temperature: float32(numbers["temperature"]), TopP: float32(numbers["top_p"]), TopK: uint32(topK),
			PresencePenalty: float32(numbers["presence_penalty"]), FrequencyPenalty: float32(numbers["frequency_penalty"]),
			RepetitionPenalty: float32(numbers["repetition_penalty"]), MinP: float32(numbers["min_p"])},
		Stops: stops, IgnoreEOS: ignoreEOS, Seed: seed, Priority: priority, SaltLo: lo, SaltHi: hi, ThinkingBudget: budget}, nil
}

func cacheSalt(body *ojson.Object) (uint64, uint64, error) {
	v := body.Value("cache_salt")
	if v == nil {
		return 0, 0, nil
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return 0, 0, apiErr(400, "cache_salt must be a non-empty string")
	}
	d := blake2bPersonal([]byte(s), 16, []byte("splash-salt"))
	return binary.LittleEndian.Uint64(d[:8]), binary.LittleEndian.Uint64(d[8:]), nil
}

func randomUint64() uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return binary.LittleEndian.Uint64(b[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (f *Frontend) requestDeadline(body *ojson.Object, started time.Time) (time.Time, error) {
	timeout := f.RequestTimeout
	if v := body.Value("timeout"); v != nil {
		n, ok := finiteNumber(v)
		if !ok || n <= 0 {
			return time.Time{}, apiErr(400, "timeout must be positive")
		}
		d := time.Duration(n * float64(time.Second))
		if timeout == 0 || d < timeout {
			timeout = d
		}
	}
	if timeout == 0 {
		return time.Time{}, nil
	}
	return started.Add(timeout), nil
}

// ---------------------------------------------------------------- prompts

type prompt struct {
	messages       []any
	tools          []any
	policy         *ToolPolicy
	effort         *string
	sentEffort     *string
	preserve       *bool
	enableThinking *bool
	kwargs         *ojson.Object
	// The response schema, nil without one.
	responseSchema any
}

func (f *Frontend) preparePrompt(body *ojson.Object) (*prompt, error) {
	if err := f.checkModel(body); err != nil {
		return nil, err
	}
	p := &prompt{}
	if v := body.Value("reasoning_effort"); v != nil {
		s, ok := v.(string)
		if !ok || !contains(reasoningEfforts, s) {
			return nil, apiErr(400, "invalid reasoning_effort")
		}
		p.effort, p.sentEffort = &s, &s
	} else if f.DefaultEffort != nil {
		p.effort = f.DefaultEffort
	}
	if v := body.Value("preserve_thinking"); v != nil {
		b, ok := v.(bool)
		if !ok {
			return nil, apiErr(400, "preserve_thinking must be a boolean")
		}
		p.preserve = &b
	}
	p.kwargs = ojson.NewObject()
	if v := body.Value("chat_template_kwargs"); v != nil {
		o, ok := v.(*ojson.Object)
		if !ok {
			return nil, apiErr(400, "chat_template_kwargs must be an object")
		}
		var reserved []string
		for _, k := range o.Keys() {
			if reservedTemplateKwargs[k] {
				reserved = append(reserved, k)
			}
		}
		if len(reserved) > 0 {
			return nil, apiErr(400, "chat_template_kwargs cannot set "+minString(reserved))
		}
		p.kwargs = o.Copy()
		if et, has := p.kwargs.Get("enable_thinking"); has {
			p.kwargs.Delete("enable_thinking")
			if et != nil {
				b, ok := et.(bool)
				if !ok {
					return nil, apiErr(400, "chat_template_kwargs enable_thinking must be a boolean")
				}
				p.enableThinking = &b
			}
		}
	}
	messages, err := normalizeMessages(body.Value("messages"))
	if err != nil {
		return nil, err
	}
	tools, policy, err := normalizeTools(body.Value("tools"), body.Value("tool_choice"), body.Value("parallel_tool_calls"))
	if err != nil {
		return nil, err
	}
	if p.responseSchema, err = normalizeResponseFormat(body.Value("response_format")); err != nil {
		return nil, err
	}
	p.messages, p.tools, p.policy = templateMessages(messages), tools, policy
	return p, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func minString(list []string) string {
	m := list[0]
	for _, s := range list[1:] {
		if s < m {
			m = s
		}
	}
	return m
}

type renderedPrompt struct {
	text             string
	tokens           []int
	thinking         bool
	generationPrompt int
}

func (f *Frontend) renderPrompt(p *prompt, addGenerationPrompt bool) (*renderedPrompt, error) {
	ct := f.Templates.Select(p.tools)
	if !ct.Accepts(p.messages) {
		return nil, apiErr(400, laterSystemUnsupported)
	}
	options := templateOptions(p.effort, p.preserve, p.tools, addGenerationPrompt, p.enableThinking)
	for _, k := range p.kwargs.Keys() {
		options.Set(k, p.kwargs.Value(k))
	}
	text, err := f.Renderer.Render(ct.Template, p.messages, options)
	if err != nil {
		var je *jinja.Error
		reason := ""
		if errors.As(err, &je) {
			reason = ": " + je.Error()
			printStatus(fmt.Sprintf("Template error · TemplateError · <template>:%d", je.Line), true)
		}
		return nil, apiErr(400, "messages could not be rendered"+reason)
	}
	tokens := f.Tok.Encode(text)
	rp := &renderedPrompt{text: text, tokens: tokens}
	if addGenerationPrompt {
		gp := ct.GenerationPrompt(options)
		if gp.text == "" || !strings.HasSuffix(text, gp.text) {
			return nil, apiErr(400, "chat template must end with an assistant generation prefix")
		}
		rp.thinking = strings.LastIndex(gp.text, thinkStart) > strings.LastIndex(gp.text, thinkEnd)
		if n := len(gp.ids); n < len(tokens) && intsEqual(tokens[len(tokens)-n:], gp.ids) {
			rp.generationPrompt = n
		}
	}
	return rp, nil
}

func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (f *Frontend) outputBudget(requested any, promptTokens int, field string) (int, error) {
	maxContext := f.MaxContext()
	if promptTokens >= maxContext {
		return 0, contextLengthError(promptTokens, maxContext-1)
	}
	room := maxContext - promptTokens
	if requested == nil {
		return room, nil
	}
	n, ok := requested.(int64)
	if !ok || n <= 0 {
		return 0, apiErr(400, field+" must be a positive integer")
	}
	if int(n) < room {
		return int(n), nil
	}
	return room, nil
}

// errConstrained refuses what only a grammar can do where llguidance could
// not be loaded.
func errConstrained(what string) *APIError {
	return apiErrCode(400, what+" needs constrained decoding, and llguidance could not be loaded (see the startup log)", "constraint_unsupported")
}

// ChatJob prepares a Chat body.
func (f *Frontend) ChatJob(body *ojson.Object, deadline time.Time, flags uint32, outputField, stopField string) (*Job, error) {
	release, err := f.preparation(deadline)
	if err != nil {
		return nil, err
	}
	defer release()
	opts, err := f.generationOptions(body, stopField)
	if err != nil {
		return nil, err
	}
	n := body.Value("n")
	if n != nil && n != int64(1) {
		return nil, apiErr(400, "n and logprobs are not currently supported")
	}
	if lp := body.Value("logprobs"); lp != nil && lp != false {
		return nil, apiErr(400, "n and logprobs are not currently supported")
	}
	p, err := f.preparePrompt(body)
	if err != nil {
		return nil, err
	}
	firstCallOnly := false
	// Constrained tool calls and structured output generate under a
	// grammar, which decides where the output ends; tools beside a response
	// schema share one.
	toolConstrained := p.policy != nil && (p.policy.Constrained || p.responseSchema != nil)
	constrained := toolConstrained || p.responseSchema != nil
	if opts.IgnoreEOS && constrained {
		return nil, apiErr(400, "ignore_eos cannot be combined with constrained tool calls, tool_choice none or structured output")
	}
	if constrained {
		f.constraintsOnce.Do(func() {
			if f.loadConstraints == nil {
				return
			}
			factory, err := f.loadConstraints()
			if err != nil {
				printStatus("Warning · constrained output is unavailable: "+err.Error(), true)
				return
			}
			f.prepMu.Lock()
			f.Constraints = factory
			f.prepMu.Unlock()
		})
	}
	if constrained && f.constraints() == nil {
		pol := p.policy
		switch {
		case p.responseSchema != nil:
			return nil, errConstrained("response_format")
		case pol.Required:
			return nil, errConstrained("tool_choice required or a named tool")
		case len(pol.Strict) > 0:
			return nil, errConstrained("a strict tool")
		case pol.Schemas.Len() == 0:
			p.policy = nil
		default:
			firstCallOnly = true
		}
		constrained, toolConstrained = false, false
	}
	rendered, err := f.renderPrompt(p, true)
	if err != nil {
		return nil, err
	}
	if err := remaining(deadline); err != nil {
		return nil, err
	}
	var constraint *TokenConstraint
	if constrained {
		var grammar string
		var prefixes func() []prefixCheck
		if toolConstrained {
			if grammar, err = toolGrammar(p.policy, rendered.thinking, p.responseSchema, f.Format); err != nil {
				return nil, err
			}
			prefixes = func() []prefixCheck { return f.callOpenings(p.policy, rendered.thinking) }
		} else {
			grammar = jsonGrammar(p.responseSchema, rendered.thinking, f.Format)
		}
		if constraint, err = f.constraints().Create(grammar, prefixes); err != nil {
			return nil, err
		}
		if err := remaining(deadline); err != nil {
			return nil, err
		}
	}
	requested := body.Value("max_completion_tokens")
	if requested == nil {
		requested = body.Value("max_tokens")
	}
	if outputField == "" {
		outputField = "max_tokens"
		if body.Has("max_completion_tokens") {
			outputField = "max_completion_tokens"
		}
	}
	maxNew, err := f.outputBudget(requested, len(rendered.tokens), outputField)
	if err != nil {
		return nil, err
	}
	budget := opts.ThinkingBudget
	if budget != nil && requested != nil {
		req := int(requested.(int64))
		lim := *budget + maxNew
		if req < lim {
			lim = req
		}
		v := *budget * maxNew / lim
		budget = &v
	}
	job := f.newJob(opts, rendered.tokens, maxNew, deadline, flags)
	job.Thinking = rendered.thinking
	job.Reasoning = &Reasoning{Sent: p.sentEffort, Effort: p.effort, EnableThinking: p.enableThinking}
	job.Format = f.Format
	if rendered.thinking && budget != nil && *budget < maxNew {
		job.ThinkingBudget = budget
	}
	job.ToolPolicy = p.policy
	job.FirstCallOnly = firstCallOnly
	job.Constraint = constraint
	job.Structured = p.responseSchema != nil
	if len(p.tools) > 0 {
		sum := sha1.Sum([]byte(sortedJSON(p.tools)))
		job.ToolsCount, job.ToolsSignature = len(p.tools), hex.EncodeToString(sum[:])[:8]
	}
	job.GenerationPrompt = rendered.generationPrompt
	if requested != nil && int64(maxNew) < requested.(int64) {
		job.ClampedToContext = true
	}
	return job, nil
}

func sortedJSON(v any) string {
	s, _ := ojson.Dumps(v, ojson.Options{ItemSep: ",", KeySep: ":", SortKeys: true, EnsureASCII: true})
	return s
}

func (f *Frontend) newJob(opts *GenerationOptions, tokens []int, maxNew int, deadline time.Time, flags uint32) *Job {
	if opts.IgnoreEOS {
		flags |= wire.FlagIgnoreEOS
	}
	prompt := make([]uint32, len(tokens))
	for i, t := range tokens {
		prompt[i] = uint32(t)
	}
	return &Job{Prompt: prompt, MaxNewTokens: maxNew, Seed: opts.Seed, Sampling: opts.Sampling, Deadline: deadline,
		Priority: opts.Priority, StopSequences: opts.Stops, Flags: flags, CacheSaltLo: opts.SaltLo, CacheSaltHi: opts.SaltHi,
		PublicID: randomHex(16), Created: time.Now().Unix()}
}

// callOpenings are the tokens that begin each strict tool's call, after the
// close of the reasoning the generation prompt opened.
func (f *Frontend) callOpenings(p *ToolPolicy, thinking bool) []prefixCheck {
	var out []prefixCheck
	for _, name := range p.Schemas.Keys() {
		if !p.Strict[name] {
			continue
		}
		var tokens []uint32
		if thinking {
			tokens = append(tokens, uint32(f.Format.ThinkEndID))
		}
		for _, id := range f.Tok.Encode(toolCallOpen + f.Format.Syntax.functionOpening(name)) {
			tokens = append(tokens, uint32(id))
		}
		out = append(out, prefixCheck{tokens, "tool " + name + " has too many parameters to constrain"})
	}
	return out
}

// CompletionJob prepares a text completion.
func (f *Frontend) CompletionJob(body *ojson.Object, deadline time.Time, flags uint32) (*Job, error) {
	release, err := f.preparation(deadline)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := f.checkModel(body); err != nil {
		return nil, err
	}
	for _, field := range []string{"suffix", "logprobs"} {
		if body.Has(field) {
			return nil, apiErr(400, field+" is not supported")
		}
	}
	if e := body.Value("echo"); e != nil && e != false {
		return nil, apiErr(400, "echo is not supported")
	}
	for _, field := range []string{"best_of", "n"} {
		if v := body.Value(field); v != nil && v != int64(1) {
			return nil, apiErr(400, field+" must be 1")
		}
	}
	opts, err := f.generationOptions(body, "stop")
	if err != nil {
		return nil, err
	}
	if b := body.Value("thinking_token_budget"); b != nil && b != int64(-1) {
		return nil, apiErr(400, "thinking_token_budget is not supported for text completions")
	}
	var tokens []int
	switch p := body.Value("prompt").(type) {
	case string:
		tokens = f.Tok.Encode(p)
	case []any:
		for _, t := range p {
			id, ok := t.(int64)
			if !ok {
				return nil, apiErr(400, "prompt must be one string or one array of token ids")
			}
			if id < 0 || int(id) >= f.Tok.VocabSize() {
				return nil, apiErr(400, "prompt token ids must be in the vocabulary")
			}
			tokens = append(tokens, int(id))
		}
	default:
		return nil, apiErr(400, "prompt must be one string or one array of token ids")
	}
	if len(tokens) == 0 {
		return nil, apiErr(400, "prompt must not be empty")
	}
	requested := body.Value("max_tokens")
	if requested == nil {
		requested = int64(completionDefaultMaxTokens)
	}
	maxNew, err := f.outputBudget(requested, len(tokens), "max_tokens")
	if err != nil {
		return nil, err
	}
	return f.newJob(opts, tokens, maxNew, deadline, flags), nil
}

// CountTokens counts a Chat body's prompt tokens.
func (f *Frontend) CountTokens(body *ojson.Object, deadline time.Time) (int, error) {
	release, err := f.preparation(deadline)
	if err != nil {
		return 0, err
	}
	defer release()
	p, err := f.preparePrompt(body)
	if err != nil {
		return 0, err
	}
	r, err := f.renderPrompt(p, true)
	if err != nil {
		return 0, err
	}
	return len(r.tokens), nil
}

// ApplyTemplate renders a Chat body's prompt.
func (f *Frontend) ApplyTemplate(body *ojson.Object, deadline time.Time) (string, error) {
	add := true
	if v := body.Value("add_generation_prompt"); v != nil {
		b, ok := v.(bool)
		if !ok {
			return "", apiErr(400, "add_generation_prompt must be a boolean")
		}
		add = b
	}
	release, err := f.preparation(deadline)
	if err != nil {
		return "", err
	}
	defer release()
	p, err := f.preparePrompt(body)
	if err != nil {
		return "", err
	}
	r, err := f.renderPrompt(p, add)
	if err != nil {
		return "", err
	}
	return r.text, nil
}

// Tokenize encodes content.
func (f *Frontend) Tokenize(body *ojson.Object) ([]any, error) {
	content, ok := body.Value("content").(string)
	if !ok {
		return nil, apiErr(400, "content must be a string")
	}
	if v := body.Value("add_special"); v != nil {
		if _, ok := v.(bool); !ok {
			return nil, apiErr(400, "add_special must be a boolean")
		}
	}
	if v := body.Value("parse_special"); v != nil && v != true {
		return nil, apiErr(400, "only parse_special=true is supported")
	}
	if v := body.Value("with_pieces"); v != nil && v != false {
		return nil, apiErr(400, "only with_pieces=false is supported")
	}
	ids := f.Tok.Encode(content)
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = int64(id)
	}
	return out, nil
}

// Reasoning is what /v1/models reports of reasoning.
func (f *Frontend) ReasoningReport() *ojson.Object {
	if !f.Format.reasoningSwitch() {
		return ojson.O("thinks", nil, "efforts", []any{}, "default", nil)
	}
	t := f.Templates.Select(nil)
	r := t.Reasoning.Copy()
	if f.DefaultEffort != nil {
		r.Set("thinks", *f.DefaultEffort != "none")
		if n, ok := t.EffortNames[*f.DefaultEffort]; ok {
			r.Set("default", n)
		} else {
			r.Set("default", nil)
		}
	}
	return r
}
