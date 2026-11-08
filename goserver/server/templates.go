package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"splash/goserver/jinja"
	"splash/goserver/ojson"
	"splash/goserver/tokenizer"
)

var reasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
var effortAliases = map[string]string{"high": "xhigh", "max": "xhigh", "minimal": "low"}

// reservedTemplateKwargs are template arguments a request cannot set.
var reservedTemplateKwargs = map[string]bool{"add_generation_prompt": true, "chat_template": true,
	"continue_final_message": true, "conversation": true, "documents": true, "messages": true,
	"return_dict": true, "tokenize": true, "tools": true}

const laterSystemUnsupported = "this model's chat template does not accept system messages after the first message"

// templateOptions are the template variables request preparation passes
// (chat_templates.template_options), as an ordered object.
func templateOptions(effort *string, preserve *bool, tools []any, addGenerationPrompt bool, enableThinking *bool) *ojson.Object {
	o := ojson.O("add_generation_prompt", addGenerationPrompt)
	var enable *bool
	if enableThinking != nil {
		enable = enableThinking
	} else if effort != nil {
		v := *effort != "none"
		enable = &v
	}
	if enable != nil {
		o.Set("enable_thinking", *enable)
		if !*enable {
			o.Set("reasoning_effort", "none")
		} else if effort != nil && *effort != "none" {
			o.Set("reasoning_effort", *effort)
		}
	}
	if preserve != nil {
		o.Set("preserve_thinking", *preserve)
	}
	if len(tools) > 0 {
		o.Set("tools", tools)
	}
	return o
}

// Renderer renders templates with the tokenizer's special tokens.
type Renderer struct {
	special map[string]jinja.Value
}

func (r *Renderer) render(t *jinja.Template, messages []any, options *ojson.Object) (string, error) {
	vars := map[string]jinja.Value{}
	for k, v := range r.special {
		vars[k] = v
	}
	vars["messages"] = jinja.FromJSON(messages)
	vars["tools"] = nil
	vars["documents"] = nil
	vars["add_generation_prompt"] = false
	for _, k := range options.Keys() {
		vars[k] = jinja.FromJSON(options.Value(k))
	}
	return t.Render(vars)
}

// Render renders as requests and the probe do: a template that rejects the
// effort renders its alias, and one that rejects "none" with thinking off
// renders by enable_thinking alone.
func (r *Renderer) Render(t *jinja.Template, messages []any, options *ojson.Object) (string, error) {
	out, err := r.render(t, messages, options)
	if err == nil {
		return out, nil
	}
	var je *jinja.Error
	if !errors.As(err, &je) {
		return "", err
	}
	effort, _ := options.Value("reasoning_effort").(string)
	retry := options.Copy()
	if alias, ok := effortAliases[effort]; ok {
		retry.Set("reasoning_effort", alias)
	} else if effort == "none" && options.Value("enable_thinking") == false {
		retry.Delete("reasoning_effort")
	} else {
		return "", err
	}
	if out, err2 := r.render(t, messages, retry); err2 == nil {
		return out, nil
	}
	return "", err
}

type generationPrompt struct {
	text string
	ids  []int
}

// ChatTemplate is one probed template.
type ChatTemplate struct {
	Template    *jinja.Template
	LaterSystem string
	Original    string
	prompts     map[string]*generationPrompt
	Reasoning   *ojson.Object
	EffortNames map[string]string
	ThinkingEnd *string

	r      *Renderer
	tok    *tokenizer.Tokenizer
	mu     sync.Mutex
	probed map[string]*generationPrompt
}

func (c *ChatTemplate) Accepts(messages []any) bool {
	if c.LaterSystem != "unsupported" {
		return true
	}
	for _, m := range messages[1:] {
		if m.(*ojson.Object).Value("role") == "system" {
			return false
		}
	}
	return true
}

// GenerationPrompt is what add_generation_prompt appends under options.
func (c *ChatTemplate) GenerationPrompt(options *ojson.Object) *generationPrompt {
	key := generationKey(options)
	if p, ok := c.prompts[key]; ok {
		return p
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.probed[key]; ok {
		return p
	}
	o, _ := ojson.DecodeString(key)
	opts := o.(*ojson.Object)
	opts.Set("add_generation_prompt", true)
	p := probeGenerationPrompt(c.r, c.tok, c.Template, opts)
	if p == nil {
		p = &generationPrompt{}
	}
	if len(c.probed) >= 64 {
		c.probed = map[string]*generationPrompt{}
	}
	c.probed[key] = p
	return p
}

var historyOptions = map[string]bool{"preserve_thinking": true}

func generationKey(options *ojson.Object) string {
	o := ojson.NewObject()
	for _, k := range options.Keys() {
		if !reservedTemplateKwargs[k] && !historyOptions[k] {
			o.Set(k, options.Value(k))
		}
	}
	s, _ := ojson.Dumps(o, ojson.Options{SortKeys: true})
	return s
}

// ChatTemplates are the tokenizer's templates.
type ChatTemplates struct {
	byName map[string]*ChatTemplate
	named  bool
}

func (t *ChatTemplates) Select(tools []any) *ChatTemplate {
	if !t.named {
		return t.byName[""]
	}
	if len(tools) > 0 {
		if c, ok := t.byName["tool_use"]; ok {
			return c
		}
	}
	return t.byName["default"]
}

func (t *ChatTemplates) Status() *ojson.Object {
	if !t.named {
		return ojson.O("later_system", t.byName[""].LaterSystem)
	}
	o := ojson.NewObject()
	names := make([]string, 0, len(t.byName))
	for n := range t.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		o.Set(n, t.byName[n].LaterSystem)
	}
	return ojson.O("later_system", o)
}

func (t *ChatTemplates) Describe() string {
	var parts []string
	names := make([]string, 0, len(t.byName))
	for n := range t.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := t.byName[n]
		var d string
		switch c.LaterSystem {
		case "native":
			d = "renders later system messages in place"
		case "patched":
			d = fmt.Sprintf("patched to render later system messages in place (the original template %s them)", c.Original)
		default:
			d = fmt.Sprintf("requests with later system messages are rejected (the original template %s them)", c.Original)
		}
		counts := map[int]bool{}
		for _, p := range c.prompts {
			counts[len(p.ids)] = true
		}
		var cs []int
		for k := range counts {
			cs = append(cs, k)
		}
		sort.Ints(cs)
		g := "generation prompt unknown"
		if len(cs) == 1 {
			unit := "tokens"
			if cs[0] == 1 {
				unit = "token"
			}
			g = fmt.Sprintf("generation prompt %d %s", cs[0], unit)
		} else if len(cs) > 1 {
			g = fmt.Sprintf("generation prompt %d-%d tokens", cs[0], cs[len(cs)-1])
		}
		prefix := ""
		if n != "" {
			prefix = n + " "
		}
		parts = append(parts, prefix+d+", "+g)
	}
	return strings.Join(parts, " · ")
}

// LoadTokenizerFiles reads the tokenizer directory's chat templates and
// special tokens, as transformers' AutoTokenizer does.
func LoadTokenizerFiles(dir string) (map[string]string, map[string]jinja.Value, error) {
	config := ojson.NewObject()
	if data, err := os.ReadFile(filepath.Join(dir, "tokenizer_config.json")); err == nil {
		v, err := ojson.Decode(data)
		if err != nil {
			return nil, nil, fmt.Errorf("tokenizer_config.json: %w", err)
		}
		if o, ok := v.(*ojson.Object); ok {
			config = o
		}
	}
	special := map[string]jinja.Value{}
	tokenText := func(v any) (string, bool) {
		switch x := v.(type) {
		case string:
			return x, true
		case *ojson.Object:
			s, ok := x.Value("content").(string)
			return s, ok
		}
		return "", false
	}
	for _, k := range []string{"bos_token", "eos_token", "unk_token", "sep_token", "pad_token", "cls_token", "mask_token"} {
		if s, ok := tokenText(config.Value(k)); ok {
			special[k] = s
		}
	}
	if extra, ok := config.Value("extra_special_tokens").(*ojson.Object); ok {
		for _, k := range extra.Keys() {
			if s, ok := tokenText(extra.Value(k)); ok {
				special[k] = s
			}
		}
	}
	templates := map[string]string{}
	if data, err := os.ReadFile(filepath.Join(dir, "chat_template.jinja")); err == nil {
		templates[""] = string(data)
	} else if entries, err := filepath.Glob(filepath.Join(dir, "additional_chat_templates", "*.jinja")); err == nil && len(entries) > 0 {
		_ = entries
	}
	if len(templates) == 0 {
		switch t := config.Value("chat_template").(type) {
		case string:
			templates[""] = t
		case []any:
			for _, item := range t {
				if o, ok := item.(*ojson.Object); ok {
					name, _ := o.Value("name").(string)
					src, _ := o.Value("template").(string)
					templates[name] = src
				}
			}
		}
	}
	return templates, special, nil
}

// NewChatTemplates probes every template once.
func NewChatTemplates(sources map[string]string, r *Renderer, tok *tokenizer.Tokenizer) (*ChatTemplates, error) {
	named := len(sources) > 1 || sources[""] == ""
	if named {
		if _, ok := sources["default"]; !ok {
			return nil, &ModelError{"the tokenizer's named chat templates have no default"}
		}
	}
	for _, s := range sources {
		if s == "" {
			return nil, &ModelError{"the tokenizer defines no chat template"}
		}
	}
	cts := &ChatTemplates{byName: map[string]*ChatTemplate{}, named: named}
	prepared := map[string]*ChatTemplate{}
	for name, src := range sources {
		if _, ok := prepared[src]; !ok {
			c, err := prepareTemplate(r, tok, src)
			if err != nil {
				return nil, err
			}
			prepared[src] = c
		}
		cts.byName[name] = prepared[src]
	}
	return cts, nil
}

// ModelError is a model whose files cannot be served.
type ModelError struct{ Msg string }

func (e *ModelError) Error() string { return e.Msg }

// Probe conversations (chat_templates.py).
const canaryMarker = "Splash later-system canary 5d0c8e"

func msg(pairs ...any) *ojson.Object { return ojson.O(pairs...) }

var (
	pSystem  = msg("role", "system", "content", "Leading instructions")
	pAsk     = msg("role", "user", "content", "First question")
	pAnswer  = msg("role", "assistant", "content", "First answer")
	pLater   = msg("role", "system", "content", canaryMarker)
	pNext    = msg("role", "user", "content", "Next question")
	pThought = msg("role", "assistant", "content", "First answer", "reasoning_content", "Hm")
	pCall    = msg("role", "assistant", "content", "", "reasoning_content", "Look it up", "tool_calls",
		[]any{ojson.O("id", "call_1", "type", "function", "function", ojson.O("name", "lookup", "arguments", ojson.O("key", "alpha")))})
	pResult = msg("role", "tool", "tool_call_id", "call_1", "content", "beta")
	pImage  = msg("role", "user", "content", []any{
		ojson.O("type", "image_url", "image_url", ojson.O("url", "data:image/png;base64,")),
		ojson.O("type", "text", "text", "Describe it")})
	pTools = []any{ojson.O("type", "function", "function", ojson.O("name", "lookup", "description", "Look up a key.",
		"parameters", ojson.O("type", "object", "properties", ojson.O("key", ojson.O("type", "string")), "required", []any{"key"})))}
)

func conv(ms ...*ojson.Object) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

var canaries = [][2][]any{
	{conv(pSystem, pAsk, pAnswer, pLater, pNext), conv(pSystem, pAsk, pAnswer, pNext)},
	{conv(pAsk, pAnswer, pLater, pNext), conv(pAsk, pAnswer, pNext)},
}
var ordinary = [][]any{conv(pAsk), conv(pSystem, pAsk), conv(pSystem, pAsk, pThought, pNext),
	conv(pSystem, pAsk, pCall, pResult), conv(pSystem, pAsk, pCall, pResult, pThought, pNext), conv(pSystem, pImage)}
var followed = [][]any{conv(pAsk), conv(pSystem, pAsk, pThought, pNext), conv(pSystem, pAsk, pCall, pResult)}

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

func probeOptions() []*ojson.Object {
	var out []*ojson.Object
	efforts := append([]*string{nil}, func() []*string {
		var e []*string
		for _, x := range reasoningEfforts {
			e = append(e, strp(x))
		}
		return e
	}()...)
	for _, tools := range [][]any{nil, pTools} {
		for _, effort := range efforts {
			for _, preserve := range []*bool{nil, boolp(true), boolp(false)} {
				out = append(out, templateOptions(effort, preserve, tools, true, nil))
			}
		}
	}
	return append(out, ojson.O("add_generation_prompt", false))
}

type outcome struct {
	text string
	err  string
	ok   bool
}

func render(r *Renderer, t *jinja.Template, messages []any, options *ojson.Object) outcome {
	s, err := r.Render(t, messages, options)
	if err != nil {
		return outcome{err: err.Error()}
	}
	return outcome{text: s, ok: true}
}

func inPlace(rendered string) bool {
	marker := strings.Index(rendered, canaryMarker)
	before := strings.Index(rendered, "First answer")
	after := strings.Index(rendered, "Next question")
	return strings.Count(rendered, canaryMarker) == 1 && before >= 0 &&
		before+len("First answer") <= marker && marker+len(canaryMarker) <= after
}

func originalTreatment(r *Renderer, t *jinja.Template) string {
	for _, c := range canaries {
		out := render(r, t, c[0], ojson.O("add_generation_prompt", true))
		if !out.ok {
			return "rejects"
		}
		if !strings.Contains(out.text, canaryMarker) {
			return "drops"
		}
		if !inPlace(out.text) {
			return "misplaces"
		}
	}
	return "renders"
}

func systemBlock(r *Renderer, t *jinja.Template) (string, string, bool) {
	options := templateOptions(strp("none"), nil, nil, false, nil)
	with := render(r, t, conv(msg("role", "system", "content", canaryMarker), pAsk), options)
	without := render(r, t, conv(pAsk), options)
	if !with.ok || !without.ok || !strings.HasSuffix(with.text, without.text) {
		return "", "", false
	}
	block := with.text[:len(with.text)-len(without.text)]
	if strings.Count(block, canaryMarker) != 1 {
		return "", "", false
	}
	prefix, suffix, _ := strings.Cut(block, canaryMarker)
	return prefix, suffix, true
}

func verified(r *Renderer, original, patched *jinja.Template) bool {
	for _, options := range probeOptions() {
		for _, m := range ordinary {
			if render(r, original, m, options) != render(r, patched, m, options) {
				return false
			}
		}
		for _, c := range canaries {
			if !render(r, original, c[1], options).ok {
				continue
			}
			out := render(r, patched, c[0], options)
			if !out.ok || !inPlace(out.text) {
				return false
			}
		}
	}
	return true
}

func probeGenerationPrompt(r *Renderer, tok *tokenizer.Tokenizer, t *jinja.Template, options *ojson.Object) *generationPrompt {
	texts := map[string]bool{}
	history := options.Copy()
	history.Set("add_generation_prompt", false)
	for _, m := range followed {
		prompt := render(r, t, m, options)
		hist := render(r, t, m, history)
		if !prompt.ok || !hist.ok || !strings.HasPrefix(prompt.text, hist.text) {
			return nil
		}
		texts[prompt.text[len(hist.text):]] = true
	}
	if len(texts) != 1 {
		return nil
	}
	for text := range texts {
		if text == "" {
			return nil
		}
		return &generationPrompt{text, tok.Encode(text)}
	}
	return nil
}

func effortOptions(effort *string) *ojson.Object {
	return templateOptions(effort, nil, nil, true, nil)
}

func prepareTemplate(r *Renderer, tok *tokenizer.Tokenizer, src string) (*ChatTemplate, error) {
	t, err := jinja.Parse(src)
	if err != nil {
		return nil, &ModelError{fmt.Sprintf("the chat template cannot be parsed: %v", err)}
	}
	c := &ChatTemplate{Template: t, r: r, tok: tok, probed: map[string]*generationPrompt{}}
	c.Original = originalTreatment(r, t)
	c.LaterSystem = "unsupported"
	if c.Original == "renders" {
		c.LaterSystem = "native"
	} else if c.Original == "rejects" || c.Original == "drops" {
		if prefix, suffix, ok := systemBlock(r, t); ok {
			normalized := strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\r", "\n")
			if patched, ok := jinja.PatchLaterSystem(normalized, c.Original, prefix, suffix); ok && verified(r, t, patched) {
				c.Template, c.LaterSystem = patched, "patched"
			}
		}
	}
	// Generation prompts for each effort a request can pass.
	c.prompts = map[string]*generationPrompt{}
	for _, effort := range append([]*string{nil}, effortPointers()...) {
		options := effortOptions(effort)
		if p := probeGenerationPrompt(r, tok, c.Template, options); p != nil {
			c.prompts[generationKey(options)] = p
		}
	}
	c.Reasoning, c.EffortNames = reasoningReport(r, c.Template)
	c.ThinkingEnd = thinkingEndText(r, c.Template)
	return c, nil
}

func effortPointers() []*string {
	var out []*string
	for _, e := range reasoningEfforts {
		out = append(out, strp(e))
	}
	return out
}

func reasoningReport(r *Renderer, t *jinja.Template) (*ojson.Object, map[string]string) {
	rendered := func(effort *string) [2]outcome {
		options := effortOptions(effort)
		return [2]outcome{render(r, t, conv(pAsk), options), render(r, t, conv(pSystem, pAsk), options)}
	}
	type group struct {
		key     [2]outcome
		efforts []string
	}
	var groups []*group
	for _, effort := range reasoningEfforts {
		if effort == "none" {
			continue
		}
		prompts := rendered(strp(effort))
		if !prompts[0].ok || !prompts[1].ok {
			continue
		}
		found := false
		for _, g := range groups {
			if g.key == prompts {
				g.efforts = append(g.efforts, effort)
				found = true
			}
		}
		if !found {
			groups = append(groups, &group{prompts, []string{effort}})
		}
	}
	names := map[string]string{}
	if len(groups) > 1 {
		for _, g := range groups {
			name := ""
			for _, e := range g.efforts {
				alias, hasAlias := effortAliases[e]
				inGroup := false
				if hasAlias {
					for _, x := range g.efforts {
						if x == alias {
							inGroup = true
						}
					}
				}
				if !inGroup {
					name = e
					break
				}
			}
			for _, e := range g.efforts {
				names[e] = name
			}
		}
	}
	def := rendered(nil)
	var efforts []any
	seen := map[string]bool{}
	for _, e := range reasoningEfforts {
		if n, ok := names[e]; ok && !seen[n] {
			seen[n] = true
			efforts = append(efforts, n)
		}
	}
	if efforts == nil {
		efforts = []any{}
	}
	var defaultName any
	for _, g := range groups {
		if g.key == def {
			if n, ok := names[g.efforts[0]]; ok {
				defaultName = n
			}
		}
	}
	return ojson.O("thinks", def != rendered(strp("none")), "efforts", efforts, "default", defaultName), names
}

func thinkingEndText(r *Renderer, t *jinja.Template) *string {
	out := render(r, t, conv(pSystem, pAsk, pCall, pResult), templateOptions(nil, nil, pTools, false, nil))
	if !out.ok {
		return nil
	}
	reasoning := "Look it up"
	start := strings.Index(out.text, reasoning)
	if start < 0 {
		return nil
	}
	end := strings.Index(out.text[start:], thinkEnd)
	if end < 0 {
		return nil
	}
	s := out.text[start+len(reasoning) : start+end+len(thinkEnd)]
	return &s
}
