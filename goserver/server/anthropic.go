package server

import (
	"fmt"
	"strings"
	"time"

	"splash/goserver/ojson"
)

func postMessages(x *exchange, body *ojson.Object, deadline time.Time) error {
	app := x.s.App
	streamed, flags, err := generationFields(body)
	if err != nil {
		return err
	}
	chat, omitted, err := toChatBody(body, app.Codec)
	if err != nil {
		return err
	}
	job, err := app.ChatJob(chat, deadline, flags, "max_tokens", "stop_sequences")
	if err != nil {
		return err
	}
	if err := submit(x, job); err != nil {
		return err
	}
	if streamed {
		return streamMessages(x, job, omitted)
	}
	return completeMessages(x, job, omitted)
}

func postMessagesCountTokens(x *exchange, body *ojson.Object, deadline time.Time) error {
	chat, _, err := anthropicChat(body, x.s.App.Codec)
	if err != nil {
		return err
	}
	n, err := x.s.App.CountTokens(chat, deadline)
	if err != nil {
		return err
	}
	x.sendJSON(200, ojson.O("input_tokens", int64(n)))
	return nil
}

func systemText(value any, label string) (string, error) {
	if s, ok := value.(string); ok {
		value = []any{ojson.O("type", "text", "text", s)}
	}
	list, ok := value.([]any)
	if !ok {
		return "", apiErr(400, label+" must be text content")
	}
	var parts []string
	for _, p := range list {
		part, _ := p.(*ojson.Object)
		text, isStr := part.Value("text").(string)
		if part == nil || part.Value("type") != "text" || !isStr {
			return "", apiErr(400, label+" contains unsupported content")
		}
		if !strings.HasPrefix(text, "x-anthropic-billing-header") {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, ""), nil
}

func anthropicContent(value any, label string) (any, error) {
	if s, ok := value.(string); ok {
		return s, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, apiErr(400, label+" must be text or content blocks")
	}
	var b strings.Builder
	for _, item := range list {
		block, _ := item.(*ojson.Object)
		kind, _ := block.Value("type").(string)
		text, isStr := block.Value("text").(string)
		switch {
		case kind == "text" && isStr:
			b.WriteString(text)
		case kind == "image":
			return nil, apiErr(400, "image input "+mediaUnsupported)
		case kind == "document":
			return nil, apiErr(400, "document input "+mediaUnsupported)
		default:
			return nil, apiErr(400, label+" contains unsupported content")
		}
	}
	return b.String(), nil
}

func toChatBody(body *ojson.Object, codec *ThinkingCodec) (*ojson.Object, bool, error) {
	mt, ok := body.Value("max_tokens").(int64)
	if !ok || mt <= 0 {
		return nil, false, apiErr(400, "max_tokens must be a positive integer")
	}
	chat, omitted, err := anthropicChat(body, codec)
	if err != nil {
		return nil, false, err
	}
	msgs := chat.Value("messages").([]any)
	if len(msgs) > 0 && msgs[len(msgs)-1].(*ojson.Object).Value("role") == "assistant" {
		return nil, false, apiErr(400, "a final assistant message (prefill) is not supported; end messages with a user turn")
	}
	stops, err := stopSequences(body.Value("stop_sequences"))
	if err != nil {
		return nil, false, err
	}
	for _, kv := range []struct {
		k string
		v any
	}{{"max_completion_tokens", mt}, {"stop", stops}, {"temperature", body.Value("temperature")},
		{"top_p", body.Value("top_p")}, {"top_k", body.Value("top_k")}, {"seed", body.Value("seed")},
		{"priority", body.Value("priority")}, {"cache_salt", body.Value("cache_salt")}} {
		if kv.v != nil {
			chat.Set(kv.k, kv.v)
		}
	}
	return chat, omitted, nil
}

func stopSequences(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, apiErr(400, "stop_sequences must be an array of strings")
	}
	for _, s := range list {
		str, ok := s.(string)
		if !ok {
			return nil, apiErr(400, "stop_sequences must be an array of strings")
		}
		if strings.TrimSpace(str) == "" {
			return nil, apiErr(400, "each of stop_sequences must contain non-whitespace")
		}
	}
	return list, nil
}

func anthropicChat(body *ojson.Object, codec *ThinkingCodec) (*ojson.Object, bool, error) {
	model, ok := body.Value("model").(string)
	if !ok || model == "" {
		return nil, false, apiErr(400, "model must be a non-empty string")
	}
	var preserve any
	if cm, ok := body.Value("context_management").(*ojson.Object); ok {
		if edits, ok := cm.Value("edits").([]any); ok {
			for _, e := range edits {
				if eo, ok := e.(*ojson.Object); ok && eo.Value("type") == "clear_thinking_20251015" && eo.Value("keep") == "all" {
					preserve = true
				}
			}
		}
	}
	output := ojson.NewObject()
	if v := body.Value("output_config"); v != nil {
		o, ok := v.(*ojson.Object)
		if !ok {
			return nil, false, apiErr(400, "output_config must be an object")
		}
		output = o
	}
	effort, ok := fieldValue(output, "effort", "high").(string)
	if !ok || !contains([]string{"low", "medium", "high", "xhigh", "max"}, effort) {
		return nil, false, apiErr(400, "output_config.effort is invalid")
	}
	responseFormat, err := anthropicResponseFormat(body, output)
	if err != nil {
		return nil, false, err
	}
	messages, ok := body.Value("messages").([]any)
	if !ok || len(messages) == 0 {
		return nil, false, apiErr(400, "messages must be a non-empty array")
	}
	var translated []any
	for _, m := range messages {
		out, err := anthropicMessage(m, codec)
		if err != nil {
			return nil, false, err
		}
		translated = append(translated, out...)
	}
	if sys := body.Value("system"); sys != nil {
		text, err := systemText(sys, "system")
		if err != nil {
			return nil, false, err
		}
		if text != "" {
			translated = append([]any{ojson.O("role", "system", "content", text)}, translated...)
		}
	}
	reasoningEffort, omitted, err := anthropicThinking(body.Value("thinking"), effort)
	if err != nil {
		return nil, false, err
	}
	tools, err := offeredTools(body.Value("tools"), messages)
	if err != nil {
		return nil, false, err
	}
	budget, err := anthropicBudget(body.Value("thinking"))
	if err != nil {
		return nil, false, err
	}
	chat := ojson.O("model", model, "messages", translated)
	for _, kv := range []struct {
		k string
		v any
	}{{"tools", tools}, {"response_format", responseFormat}, {"preserve_thinking", preserve}, {"reasoning_effort", reasoningEffort},
		{"chat_template_kwargs", body.Value("chat_template_kwargs")}, {"thinking_token_budget", budget}} {
		if kv.v != nil {
			if l, isList := kv.v.([]any); isList && l == nil {
				continue
			}
			chat.Set(kv.k, kv.v)
		}
	}
	if tc := body.Value("tool_choice"); tc != nil {
		choice, ok := tc.(*ojson.Object)
		if !ok {
			return nil, false, apiErr(400, "tool_choice must be an object")
		}
		disable, ok := fieldValue(choice, "disable_parallel_tool_use", false).(bool)
		if !ok {
			return nil, false, apiErr(400, "disable_parallel_tool_use must be a boolean")
		}
		var toolChoice any
		switch choice.Value("type") {
		case "auto":
			toolChoice = "auto"
		case "any":
			toolChoice = "required"
		case "none":
			toolChoice = "none"
		case "tool":
			name, ok := choice.Value("name").(string)
			if !ok {
				return nil, false, apiErr(400, "invalid Anthropic tool_choice")
			}
			toolChoice = ojson.O("type", "function", "function", ojson.O("name", name))
		default:
			return nil, false, apiErr(400, "invalid Anthropic tool_choice")
		}
		chat.Set("parallel_tool_calls", !disable)
		chat.Set("tool_choice", toolChoice)
	}
	return chat, omitted, nil
}

func anthropicThinking(v any, effort string) (any, bool, error) {
	if v == nil {
		return "none", false, nil
	}
	t, ok := v.(*ojson.Object)
	kind, _ := t.Value("type").(string)
	if !ok || !contains([]string{"enabled", "disabled", "adaptive"}, kind) {
		return nil, false, apiErr(400, "thinking.type must be enabled, disabled, or adaptive")
	}
	display := t.Value("display")
	if display != nil {
		if kind == "disabled" {
			return nil, false, apiErr(400, "thinking.display requires enabled or adaptive thinking")
		}
		if d, _ := display.(string); !contains([]string{"summarized", "omitted", "updates"}, d) {
			return nil, false, apiErr(400, "thinking.display must be summarized, omitted, updates, or null")
		}
	}
	omitted := display == "omitted" || display == "updates"
	if kind == "disabled" {
		return "none", omitted, nil
	}
	return effort, omitted, nil
}

func anthropicBudget(v any) (any, error) {
	t, ok := v.(*ojson.Object)
	if !ok || t.Value("type") != "enabled" {
		return nil, nil
	}
	b, ok := t.Value("budget_tokens").(int64)
	if !ok || b < 1024 {
		return nil, apiErr(400, "thinking.budget_tokens must be an integer of at least 1024")
	}
	return b, nil
}

func isToolChange(v any) bool {
	o, ok := v.(*ojson.Object)
	return ok && (o.Value("type") == "tool_addition" || o.Value("type") == "tool_removal")
}

func anthropicMessage(m any, codec *ThinkingCodec) ([]any, error) {
	message, ok := m.(*ojson.Object)
	if !ok {
		return nil, apiErr(400, "Anthropic messages must be objects")
	}
	role, _ := message.Value("role").(string)
	content := message.Value("content")
	if role != "user" && role != "assistant" && role != "system" {
		return nil, apiErr(400, "Anthropic messages require user/assistant/system roles")
	}
	if role == "system" {
		if list, ok := content.([]any); ok {
			var kept []any
			for _, b := range list {
				if !isToolChange(b) {
					kept = append(kept, b)
				}
			}
			if kept == nil {
				kept = []any{}
			}
			content = kept
		}
		text, err := systemText(content, "system message")
		if err != nil {
			return nil, err
		}
		if text == "" {
			return nil, nil
		}
		return []any{ojson.O("role", "system", "content", text)}, nil
	}
	if s, ok := content.(string); ok {
		return []any{ojson.O("role", role, "content", s)}, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, apiErr(400, "Anthropic message content must be text or blocks")
	}
	var text, reasoning []string
	var userBlocks, calls, results []any
	for _, b := range blocks {
		block, ok := b.(*ojson.Object)
		if !ok {
			return nil, apiErr(400, "Anthropic content blocks must be objects")
		}
		kind, _ := block.Value("type").(string)
		t, isText := block.Value("text").(string)
		switch {
		case kind == "text" && isText:
			text = append(text, t)
			userBlocks = append(userBlocks, block)
		case role == "user" && (kind == "image" || kind == "document"):
			userBlocks = append(userBlocks, block)
		case role == "assistant" && kind == "thinking":
			thinking, isStr := block.Value("thinking").(string)
			if !isStr {
				return nil, apiErr(400, fmt.Sprintf("unsupported Anthropic content block: %s", kind))
			}
			sig, ok := fieldValue(block, "signature", "").(string)
			if !ok {
				return nil, apiErr(400, "invalid thinking signature")
			}
			if sig != "" {
				r, err := codec.Decode(sig)
				if err != nil {
					r = thinking
				}
				if r != "" {
					reasoning = append(reasoning, r)
				}
			} else {
				reasoning = append(reasoning, thinking)
			}
		case role == "assistant" && kind == "redacted_thinking":
			if d, ok := block.Value("data").(string); !ok || d == "" {
				return nil, apiErr(400, "invalid redacted_thinking data")
			}
		case role == "assistant" && kind == "tool_use":
			id, ok1 := block.Value("id").(string)
			name, ok2 := block.Value("name").(string)
			input, ok3 := block.Value("input").(*ojson.Object)
			if !ok1 || !ok2 || !ok3 {
				return nil, apiErr(400, "invalid Anthropic tool_use block")
			}
			calls = append(calls, ojson.O("id", id, "type", "function", "function", ojson.O("name", name, "arguments", input)))
		case role == "user" && kind == "tool_result":
			r, err := toolResult(block)
			if err != nil {
				return nil, err
			}
			results = append(results, r)
		default:
			return nil, apiErr(400, fmt.Sprintf("unsupported Anthropic content block: %s", kind))
		}
	}
	if role == "assistant" {
		item := ojson.O("role", role, "content", strings.Join(text, ""))
		if len(reasoning) > 0 {
			item.Set("reasoning_content", strings.Join(reasoning, ""))
		}
		if len(calls) > 0 {
			item.Set("tool_calls", calls)
		}
		return []any{item}, nil
	}
	if len(userBlocks) > 0 || len(results) == 0 {
		if userBlocks == nil {
			userBlocks = []any{}
		}
		c, err := anthropicContent(userBlocks, "user message")
		if err != nil {
			return nil, err
		}
		return append(results, ojson.O("role", role, "content", c)), nil
	}
	return results, nil
}

func toolResult(block *ojson.Object) (*ojson.Object, error) {
	id, ok := block.Value("tool_use_id").(string)
	if !ok {
		return nil, apiErr(400, "invalid Anthropic tool_result block")
	}
	isError, ok := fieldValue(block, "is_error", false).(bool)
	if !ok {
		return nil, apiErr(400, "tool_result.is_error must be a boolean")
	}
	raw := fieldValue(block, "content", "")
	if list, ok := raw.([]any); ok {
		converted := make([]any, len(list))
		for i, p := range list {
			if po, ok := p.(*ojson.Object); ok && po.Value("type") == "tool_reference" {
				name := po.Value("tool_name")
				if !isTagName(name) {
					return nil, apiErr(400, "tool_reference.tool_name must be "+tagNameRule)
				}
				converted[i] = ojson.O("type", "text", "text", fmt.Sprintf("\nAvailable tool: %s\n", name))
			} else {
				converted[i] = p
			}
		}
		raw = converted
	}
	content, err := anthropicContent(raw, "tool_result")
	if err != nil {
		return nil, err
	}
	if isError {
		content = "Tool execution failed:\n" + content.(string)
	}
	return ojson.O("role", "tool", "tool_call_id", id, "content", content), nil
}

func anthropicTools(tools any) ([]any, error) {
	list, ok := tools.([]any)
	if !ok {
		return nil, apiErr(400, "tools must be an array")
	}
	functions := []any{}
	for _, t := range list {
		tool, isObj := t.(*ojson.Object)
		if isObj {
			if k := fieldValue(tool, "type", "custom"); k != "custom" {
				return nil, apiErr(400, fmt.Sprintf("only custom function tools are supported; received type %s", reprPy(k)))
			}
		}
		name, okName := tool.Value("name").(string)
		schema := fieldValue(tool, "input_schema", ojson.NewObject())
		_, schemaObj := schema.(*ojson.Object)
		_, schemaBool := schema.(bool)
		if !isObj || !okName || !(schemaObj || schemaBool) {
			return nil, apiErr(400, "invalid Anthropic tool")
		}
		fn := ojson.O("name", name, "parameters", schema)
		if d, ok := tool.Value("description").(string); ok {
			fn.Set("description", d)
		}
		if tool.Has("strict") {
			fn.Set("strict", tool.Value("strict"))
		}
		functions = append(functions, ojson.O("type", "function", "function", fn))
	}
	return functions, nil
}

func reprPy(v any) string {
	if s, ok := v.(string); ok {
		return "'" + s + "'"
	}
	return fmt.Sprint(v)
}

func offeredTools(tools any, messages []any) ([]any, error) {
	var declared []any
	if tools != nil {
		var err error
		if declared, err = anthropicTools(tools); err != nil {
			return nil, err
		}
	}
	var changes []*ojson.Object
	for _, m := range messages {
		mo, _ := m.(*ojson.Object)
		if mo == nil || mo.Value("role") != "system" {
			continue
		}
		if list, ok := mo.Value("content").([]any); ok {
			for _, b := range list {
				if isToolChange(b) {
					changes = append(changes, b.(*ojson.Object))
				}
			}
		}
	}
	if len(changes) == 0 {
		if tools == nil {
			return nil, nil
		}
		return declared, nil
	}
	offered := ojson.NewObject()
	for _, t := range declared {
		name := t.(*ojson.Object).Value("function").(*ojson.Object).Value("name").(string)
		if offered.Has(name) {
			return nil, apiErr(400, "duplicate tool name: "+name)
		}
		offered.Set(name, t)
	}
	removed := map[string]bool{}
	for _, block := range changes {
		kind := block.Value("type").(string)
		tool, _ := block.Value("tool").(*ojson.Object)
		form := tool.Value("type")
		var name string
		var definition any
		if kind == "tool_addition" && form == "tool_definition" {
			defs, err := anthropicTools([]any{tool.Value("definition")})
			if err != nil {
				return nil, err
			}
			definition = defs[0]
			name = defs[0].(*ojson.Object).Value("function").(*ojson.Object).Value("name").(string)
		} else {
			n, ok := tool.Value("name").(string)
			if form != "tool_reference" || !ok {
				forms := "a named tool_reference"
				if kind == "tool_addition" {
					forms = "a named tool_reference or a tool_definition"
				}
				return nil, apiErr(400, fmt.Sprintf("%s.tool must be %s", kind, forms))
			}
			name = n
		}
		if definition != nil {
			offered.Set(name, definition)
		} else if !offered.Has(name) {
			return nil, apiErr(400, fmt.Sprintf("%s names tool '%s', which is not declared", kind, name))
		}
		if kind == "tool_removal" {
			removed[name] = true
		} else {
			delete(removed, name)
		}
	}
	out := []any{}
	for _, k := range offered.Keys() {
		if !removed[k] {
			out = append(out, offered.Value(k))
		}
	}
	return out, nil
}

func stopReason(c *Collected, job *Job) string {
	switch {
	case c.Called():
		return "tool_use"
	case c.Result.Reason == "length":
		if job.ClampedToContext || c.Result.Bounded {
			return "model_context_window_exceeded"
		}
		return "max_tokens"
	case c.Result.StopSequence != nil:
		return "stop_sequence"
	}
	return "end_turn"
}

func messageUsage(prompt, output int, cache CacheInfo, thinking *int) *ojson.Object {
	u := ojson.O("input_tokens", int64(prompt-cache.Matched), "cache_read_input_tokens", int64(cache.Matched), "output_tokens", int64(output))
	if thinking != nil {
		u.Set("output_tokens_details", ojson.O("thinking_tokens", int64(*thinking)))
	}
	return u
}

func contentBlock(b *Block, omitted bool, signature string) *ojson.Object {
	switch b.Kind {
	case "reasoning":
		thinking := b.Text()
		if omitted {
			thinking = ""
		}
		return ojson.O("type", "thinking", "thinking", thinking, "signature", signature)
	case "text":
		return ojson.O("type", "text", "text", b.Text())
	}
	var input any = ojson.NewObject()
	if b.Status != "in_progress" {
		if v, err := ojson.DecodeString(b.Text()); err == nil {
			input = v
		}
	}
	return ojson.O("type", "tool_use", "id", b.CallID, "name", b.Name, "input", input)
}

func stopSequenceValue(r *Result) any {
	if r.StopSequence == nil {
		return nil
	}
	return *r.StopSequence
}

func completeMessages(x *exchange, job *Job, omitted bool) error {
	app := x.s.App
	seq := &BlockSequencer{}
	c, err := x.collect(job, callbacks{onText: seq.Text, onTool: seq.Tool})
	if err != nil {
		app.Backend.Cancel(job)
		return err
	}
	blocks := seq.Finish(c.Cut(), c.ReasoningOpen)
	signature := ""
	if c.Reasoning != "" && omitted {
		if signature, err = app.Codec.Encode(c.Reasoning); err != nil {
			return err
		}
	}
	content := []any{}
	allThinking := true
	for _, b := range blocks {
		if c.CutCall != "" && b.CallID == c.CutCall {
			continue
		}
		cb := contentBlock(b, omitted, signature)
		if cb.Value("type") != "thinking" {
			allThinking = false
		}
		content = append(content, cb)
	}
	if allThinking {
		content = append(content, ojson.O("type", "text", "text", ""))
	}
	tt := job.ReasoningTokens
	x.sendJSON(200, ojson.O("id", "msg_"+job.PublicID, "type", "message", "role", "assistant", "model", app.ResponseModel,
		"content", content, "stop_reason", stopReason(c, job), "stop_sequence", stopSequenceValue(c.Result),
		"usage", messageUsage(c.Result.PromptTokens, c.Result.CompletionTokens, c.Result.Cache, &tt),
		"timings", c.Result.Timings()))
	return nil
}

func streamMessages(x *exchange, job *Job, omitted bool) error {
	app := x.s.App
	send := func(name string, payload *ojson.Object) {
		o := ojson.O("type", name)
		for _, k := range payload.Keys() {
			o.Set(k, payload.Value(k))
		}
		x.eventSSE(name, o)
	}
	seq := &BlockSequencer{
		OnOpen: func(i int, b *Block) {
			send("content_block_start", ojson.O("index", int64(i), "content_block", contentBlock(b, omitted, "")))
			if b.Kind == "reasoning" && omitted {
				send("content_block_delta", ojson.O("index", int64(i), "delta", ojson.O("type", "thinking_delta", "thinking", "")))
			}
		},
		OnDelta: func(i int, b *Block, text string) {
			var delta *ojson.Object
			switch b.Kind {
			case "reasoning":
				if omitted {
					return
				}
				delta = ojson.O("type", "thinking_delta", "thinking", text)
			case "text":
				delta = ojson.O("type", "text_delta", "text", text)
			default:
				delta = ojson.O("type", "input_json_delta", "partial_json", text)
			}
			send("content_block_delta", ojson.O("index", int64(i), "delta", delta))
		},
		OnClose: func(i int, b *Block) {
			if b.Kind == "reasoning" && omitted {
				sig, _ := app.Codec.Encode(b.Text())
				send("content_block_delta", ojson.O("index", int64(i), "delta", ojson.O("type", "signature_delta", "signature", sig)))
			}
			send("content_block_stop", ojson.O("index", int64(i)))
		},
	}
	run := func() error {
		c, err := x.collect(job, callbacks{
			onStart: func() {
				x.startStream()
				send("message_start", ojson.O("message", ojson.O("id", "msg_"+job.PublicID, "type", "message", "role", "assistant",
					"model", app.ResponseModel, "content", []any{}, "stop_reason", nil, "stop_sequence", nil,
					"usage", messageUsage(len(job.Prompt), 0, job.Cache(), nil))))
			},
			onText: seq.Text,
			onTool: seq.Tool,
			onIdle: func() {
				x.startStream()
				send("ping", ojson.NewObject())
			},
			onProgress: func(p *ojson.Object) { send("ping", ojson.O("prompt_progress", p)) },
		})
		if err != nil {
			return err
		}
		seq.Finish(c.Cut(), c.ReasoningOpen)
		send("message_delta", ojson.O("delta", ojson.O("stop_reason", stopReason(c, job), "stop_sequence", stopSequenceValue(c.Result)),
			"usage", ojson.O("output_tokens", int64(c.Result.CompletionTokens), "output_tokens_details", ojson.O("thinking_tokens", int64(job.ReasoningTokens))),
			"timings", c.Result.Timings()))
		send("message_stop", ojson.NewObject())
		return nil
	}
	return x.guarded(job, run, func(e *APIError) { x.eventSSE("error", x.errorPayload(e)) })
}

// anthropicResponseFormat is a Messages request's output format, in
// output_config.format or output_format, as a Chat response_format.
func anthropicResponseFormat(body, output *ojson.Object) (any, error) {
	format, legacy := output.Value("format"), body.Value("output_format")
	if format != nil && legacy != nil {
		return nil, apiErr(400, "output_config.format and output_format cannot be combined")
	}
	if format == nil {
		format = legacy
	}
	if format == nil {
		return nil, nil
	}
	o, ok := format.(*ojson.Object)
	if ok && o.Value("type") == "json_schema" {
		switch o.Value("schema").(type) {
		case *ojson.Object, bool:
			return ojson.O("type", "json_schema", "json_schema", ojson.O("schema", o.Value("schema"))), nil
		}
	}
	return nil, apiErr(400, "output format must be a json_schema object with a schema")
}
