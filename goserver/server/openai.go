package server

import (
	"time"

	"splash/goserver/ojson"
	"splash/goserver/wire"
)

// generationFields reads stream and return_progress (request_fields.generation).
func generationFields(body *ojson.Object) (bool, uint32, error) {
	stream := false
	if v := body.Value("stream"); v != nil {
		b, ok := v.(bool)
		if !ok {
			return false, 0, apiErr(400, "stream must be a boolean")
		}
		stream = b
	}
	var flags uint32
	if v := body.Value("return_progress"); v != nil {
		b, ok := v.(bool)
		if !ok || (b && !stream) {
			return false, 0, apiErr(400, "return_progress requires stream: true and must be a boolean")
		}
		if b {
			flags = wire.FlagReturnProgress
		}
	}
	return stream, flags, nil
}

func includeUsage(body *ojson.Object) (bool, error) {
	v := body.Value("stream_options")
	if v == nil {
		return false, nil
	}
	o, ok := v.(*ojson.Object)
	if !ok {
		return false, apiErr(400, "invalid streaming options")
	}
	iu := o.Value("include_usage")
	if iu == nil {
		return false, nil
	}
	b, ok := iu.(bool)
	if !ok {
		return false, apiErr(400, "invalid streaming options")
	}
	return b, nil
}

func postChat(x *exchange, body *ojson.Object, deadline time.Time) error {
	return postOpenAI(x, body, deadline, true)
}

func postCompletion(x *exchange, body *ojson.Object, deadline time.Time) error {
	return postOpenAI(x, body, deadline, false)
}

func postChatCountTokens(x *exchange, body *ojson.Object, deadline time.Time) error {
	n, err := x.s.App.CountTokens(body, deadline)
	if err != nil {
		return err
	}
	x.sendJSON(200, ojson.O("input_tokens", int64(n)))
	return nil
}

func submit(x *exchange, j *Job) error {
	if err := remaining(j.Deadline); err != nil {
		return err
	}
	select {
	case <-x.r.Context().Done():
		return errDisconnected
	default:
	}
	j.started = time.Now()
	return x.s.App.Backend.Submit(j)
}

func postOpenAI(x *exchange, body *ojson.Object, deadline time.Time, chat bool) error {
	streamed, flags, err := generationFields(body)
	if err != nil {
		return err
	}
	usage, err := includeUsage(body)
	if err != nil {
		return err
	}
	var job *Job
	if chat {
		job, err = x.s.App.ChatJob(body, deadline, flags, "", "stop")
	} else {
		job, err = x.s.App.CompletionJob(body, deadline, flags)
	}
	if err != nil {
		return err
	}
	if err := submit(x, job); err != nil {
		return err
	}
	if streamed {
		return streamOpenAI(x, job, usage, chat)
	}
	c, err := x.collect(job, callbacks{})
	if err != nil {
		x.s.App.Backend.Cancel(job)
		return err
	}
	model := x.s.App.ResponseModel
	if !chat {
		x.sendJSON(200, ojson.O("id", "cmpl-"+job.PublicID, "object", "text_completion", "created", job.Created, "model", model,
			"choices", []any{ojson.O("index", int64(0), "text", c.Content, "logprobs", nil, "finish_reason", c.Result.Reason)},
			"usage", c.Result.Usage(job), "metrics", c.Result.Metrics(), "timings", c.Result.Timings()))
		return nil
	}
	var content any
	if c.Content != "" {
		content = c.Content
	}
	message := ojson.O("role", "assistant", "content", content)
	if c.Reasoning != "" {
		message.Set("reasoning_content", c.Reasoning)
	}
	if len(c.ToolCalls) > 0 {
		calls := make([]any, len(c.ToolCalls))
		for i, tc := range c.ToolCalls {
			calls[i] = tc.JSON()
		}
		message.Set("tool_calls", calls)
	}
	x.sendJSON(200, ojson.O("id", "chatcmpl-"+job.PublicID, "object", "chat.completion", "created", job.Created, "model", model,
		"choices", []any{ojson.O("index", int64(0), "message", message, "finish_reason", finishReason(c))},
		"usage", c.Result.Usage(job), "metrics", c.Result.Metrics(), "timings", c.Result.Timings()))
	return nil
}

func finishReason(c *Collected) string {
	if c.Called() {
		return "tool_calls"
	}
	return c.Result.Reason
}

func streamOpenAI(x *exchange, job *Job, usage, chat bool) error {
	model := x.s.App.ResponseModel
	chunk := func(delta any, finish any, extra ...any) *ojson.Object {
		var o *ojson.Object
		if chat {
			o = ojson.O("id", "chatcmpl-"+job.PublicID, "object", "chat.completion.chunk", "created", job.Created, "model", model,
				"choices", []any{ojson.O("index", int64(0), "delta", delta, "finish_reason", finish)})
		} else {
			o = ojson.O("id", "cmpl-"+job.PublicID, "object", "text_completion", "created", job.Created, "model", model,
				"choices", []any{ojson.O("index", int64(0), "text", delta, "logprobs", nil, "finish_reason", finish)})
		}
		for i := 0; i+1 < len(extra); i += 2 {
			k := extra[i].(string)
			if k == "usage" {
				o.Set("choices", []any{})
			}
			o.Set(k, extra[i+1])
		}
		return o
	}
	empty := func() any {
		if chat {
			return ojson.NewObject()
		}
		return ""
	}
	payload := func(field, text string) any {
		if chat {
			return ojson.O(field, text)
		}
		return text
	}
	started := false
	run := func() error {
		c, err := x.collect(job, callbacks{
			onStart: func() {
				started = true
				x.startStream()
				if chat {
					x.sse(chunk(ojson.O("role", "assistant", "content", ""), nil))
				}
			},
			onText: func(field, text string) { x.sse(chunk(payload(field, text), nil)) },
			onTool: func(delta *ojson.Object) { x.sse(chunk(ojson.O("tool_calls", []any{delta}), nil)) },
			onIdle: func() {
				if started {
					x.sse(chunk(empty(), nil))
				} else {
					x.sseKeepalive()
				}
			},
			onProgress: func(p *ojson.Object) {
				c := chunk(empty(), nil)
				c.Set("prompt_progress", p)
				x.sse(c)
			},
		})
		if err != nil {
			return err
		}
		x.startStream()
		x.sse(chunk(empty(), finishReason(c), "timings", c.Result.Timings()))
		if usage {
			x.sse(chunk(empty(), nil, "usage", c.Result.Usage(job), "metrics", c.Result.Metrics()))
		}
		x.sse("[DONE]")
		return nil
	}
	return x.guarded(job, run, func(e *APIError) {
		x.sse(x.errorPayload(e))
		x.sse("[DONE]")
	})
}
