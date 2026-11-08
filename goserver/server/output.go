package server

import (
	"fmt"
	"strings"

	"splash/goserver/ojson"
)

const (
	thinkStart       = "<think>"
	thinkEnd         = "</think>"
	toolCallOpen     = "<tool_call>"
	toolCallClose    = "</tool_call>"
	maxNameLength    = 256
	nameSpace        = " \t\n\r"
	argumentDeltaLen = 16 * 1024
	maxStopChars     = 16 * 1024
)

// holdPartial splits text before its longest end that begins one of markers.
func holdPartial(text string, markers ...string) (string, string) {
	longest := 0
	for _, m := range markers {
		if len(m) > longest {
			longest = len(m)
		}
	}
	n := len(text)
	if longest-1 < n {
		n = longest - 1
	}
	for length := n; length > 0; length-- {
		for _, m := range markers {
			if length < len(m) && strings.HasSuffix(text, m[:length]) {
				return text[:len(text)-length], text[len(text)-length:]
			}
		}
	}
	return text, ""
}

// StopSearch finds the first stop sequence the output contains, as the
// output arrives (Knuth-Morris-Pratt per stop, over bytes).
type StopSearch struct {
	stops     []string
	fallbacks [][]int
	states    []int
}

func NewStopSearch(stops []string) *StopSearch {
	s := &StopSearch{stops: stops, states: make([]int, len(stops))}
	for _, stop := range stops {
		s.fallbacks = append(s.fallbacks, prefixFunction(stop))
	}
	return s
}

func prefixFunction(text string) []int {
	lengths := make([]int, len(text))
	length := 0
	for i := 1; i < len(text); i++ {
		for length > 0 && text[i] != text[length] {
			length = lengths[length-1]
		}
		if text[i] == text[length] {
			length++
		}
		lengths[i] = length
	}
	return lengths
}

// Search returns the stop the output, text appended, contains first and how
// many bytes before the end of text it begins; ok false when none.
func (s *StopSearch) Search(text string) (stop string, back int, ok bool) {
	best := -1
	for i, stopText := range s.stops {
		state := s.states[i]
		fb := s.fallbacks[i]
		for pos := 0; pos < len(text); pos++ {
			c := text[pos]
			for state > 0 && stopText[state] != c {
				state = fb[state-1]
			}
			if stopText[state] == c {
				state++
			}
			if state == len(stopText) {
				b := len(text) - pos - 1 + len(stopText)
				if best < 0 || b > back {
					best, back, stop = i, b, stopText
				}
				break
			}
		}
		s.states[i] = state
	}
	return stop, back, best >= 0
}

// Held is the length of the output's longest end that begins a stop.
func (s *StopSearch) Held() int {
	m := 0
	for _, v := range s.states {
		if v > m {
			m = v
		}
	}
	return m
}

// textEvent is a piece of output: field "reasoning_content" or "content",
// or a tool delta.
type textEvent struct {
	field string
	text  string
	tool  *ojson.Object
}

// ReasoningSplitter separates the reasoning from the answer as the output
// arrives (output.ReasoningSplitter).
type ReasoningSplitter struct {
	reasoning bool
	opening   bool
	pending   string
	separator bool
	ends      []string
}

func NewReasoningSplitter(thinking, toolCalls bool, cf *ChatFormat) *ReasoningSplitter {
	s := &ReasoningSplitter{reasoning: thinking}
	s.opening = !thinking && cf != nil && cf.ThinkStartID >= 0
	s.ends = []string{thinkEnd}
	if cf != nil && toolCalls && cf.callsEndReasoning() {
		s.ends = append(s.ends, cf.Syntax.callOpen())
	}
	return s
}

func (s *ReasoningSplitter) content(text string) []textEvent {
	if s.separator {
		text = strings.TrimLeft(text, "\n")
		if text == "" {
			return nil
		}
		s.separator = false
	}
	return []textEvent{{field: "content", text: text}}
}

func (s *ReasoningSplitter) Put(text string) []textEvent {
	if s.opening {
		s.pending += text
		if len(s.pending) < len(thinkStart) && strings.HasPrefix(thinkStart, s.pending) {
			return nil
		}
		s.opening = false
		text, s.pending = s.pending, ""
		if strings.HasPrefix(text, thinkStart) {
			s.reasoning = true
			text = text[len(thinkStart):]
		}
	}
	if !s.reasoning {
		return s.content(text)
	}
	s.pending += text
	end, marker := -1, ""
	for _, m := range s.ends {
		if i := strings.Index(s.pending, m); i >= 0 && (end < 0 || i < end || (i == end && m < marker)) {
			end, marker = i, m
		}
	}
	if end >= 0 {
		reasoning := s.pending[:end]
		content := s.pending[end:]
		if marker == thinkEnd {
			content = content[len(thinkEnd):]
		}
		s.pending = ""
		s.reasoning = false
		s.separator = marker == thinkEnd
		var out []textEvent
		if reasoning != "" {
			out = append(out, textEvent{field: "reasoning_content", text: reasoning})
		}
		return append(out, s.content(content)...)
	}
	ready, held := holdPartial(s.pending, s.ends...)
	s.pending = held
	if ready != "" {
		return []textEvent{{field: "reasoning_content", text: ready}}
	}
	return nil
}

func (s *ReasoningSplitter) Finish() []textEvent {
	if s.pending == "" {
		return nil
	}
	kind := "content"
	if s.reasoning {
		kind = "reasoning_content"
	}
	text := s.pending
	s.pending = ""
	return []textEvent{{field: kind, text: text}}
}

// ToolCall is a call the output made.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

func (c ToolCall) JSON() *ojson.Object {
	return ojson.O("id", c.ID, "type", "function", "function", ojson.O("name", c.Name, "arguments", c.Arguments))
}

// Projector parses tool calls out of the output as it arrives into OpenAI
// argument deltas (output.StreamingToolCallProjector).
type Projector struct {
	types         *ParameterTypes
	requestID     string
	syntax        ToolCallSyntax
	textTags      []string
	pending       string
	state         string
	callIndex     int
	callID        string
	hasCall       bool
	functionName  string
	paramTypes    map[string]typeSet
	otherTypes    typeSet
	paramNames    map[string]bool
	argFragments  []string
	paramCount    int
	paramName     string
	hasParam      bool
	valueTypes    typeSet
	valueStreams  bool
	valueStarted  bool
	valueParts    []string
	contentFrags  []string
	streamedCount int
	textVisible   bool
	closed        []ToolCall
}

func NewProjector(policy *ToolPolicy, requestID string, structured bool, syntax ToolCallSyntax) *Projector {
	p := &Projector{requestID: requestID, syntax: syntax, state: "content"}
	if policy != nil {
		p.types = NewParameterTypes(policy)
		p.textTags = []string{syntax.callOpen(), thinkEnd}
	} else {
		p.textTags = []string{thinkEnd}
		p.textVisible = true
	}
	if structured {
		p.state = "output"
	}
	return p
}

func (p *Projector) emitContent(value string, events *[]textEvent) {
	if value == "" {
		return
	}
	p.contentFrags = append(p.contentFrags, value)
	if !p.textVisible {
		if strings.TrimSpace(value) == "" {
			return
		}
		p.textVisible = true
	}
	unsent := strings.Join(p.contentFrags[p.streamedCount:], "")
	p.streamedCount = len(p.contentFrags)
	*events = append(*events, textEvent{field: "content", text: unsent})
}

func (p *Projector) streamedContent() string {
	return strings.Join(p.contentFrags[:p.streamedCount], "")
}

func (p *Projector) emitArgument(fragment string, events *[]textEvent) {
	p.argFragments = append(p.argFragments, fragment)
	for off := 0; off < len(fragment); off += argumentDeltaLen {
		end := off + argumentDeltaLen
		if end > len(fragment) {
			end = len(fragment)
		}
		*events = append(*events, textEvent{tool: ojson.O("index", int64(p.callIndex), "function", ojson.O("arguments", fragment[off:end]))})
	}
}

func (p *Projector) beginCall(name string, events *[]textEvent) {
	if strings.Trim(name, nameSpace) == "" {
		p.state = "content"
		p.emitContent(p.syntax.callOpen()+name, events)
		return
	}
	p.state = "arguments"
	p.argFragments = nil
	p.paramCount = 0
	p.paramNames = map[string]bool{}
	name = strings.Trim(name, nameSpace)
	if p.streamedCount == 0 {
		p.contentFrags = nil
	}
	p.functionName = name
	p.callID = fmt.Sprintf("call_%s_%d", p.requestID, p.callIndex)
	p.hasCall = true
	p.paramTypes, p.otherTypes = p.types.Of(name)
	*events = append(*events, textEvent{tool: ojson.O("index", int64(p.callIndex), "id", p.callID, "type", "function", "function", ojson.O("name", name))})
	p.emitArgument("{", events)
}

func (p *Projector) key(name string) string {
	sep := ""
	if p.paramCount > 0 {
		sep = ","
	}
	p.paramCount++
	return sep + ojson.MarshalString(name) + ":"
}

func (p *Projector) beginParameter(name string, events *[]textEvent) {
	name = strings.Trim(name, nameSpace)
	kept := name != "" && !p.paramNames[name]
	p.paramNames[name] = true
	p.hasParam = kept
	p.paramName = name
	if t, ok := p.paramTypes[name]; ok {
		p.valueTypes = t
	} else {
		p.valueTypes = p.otherTypes
	}
	p.valueStreams = p.valueTypes == nil || p.valueTypes.only("string")
	p.valueStarted = false
	p.valueParts = nil
	if p.valueStreams && p.hasParam {
		p.emitArgument(p.key(name)+`"`, events)
	}
	p.state = "value"
}

func (p *Projector) putValue(text string, events *[]textEvent) {
	if !p.hasParam {
		return
	}
	if !p.valueStarted {
		if text == "" {
			return
		}
		p.valueStarted = true
		text = strings.TrimPrefix(text, p.syntax.ValueOpen)
	}
	if text == "" {
		return
	}
	if p.valueStreams {
		q := ojson.MarshalString(text)
		p.emitArgument(q[1:len(q)-1], events)
	} else {
		p.valueParts = append(p.valueParts, text)
	}
}

func (p *Projector) endParameter(text string, events *[]textEvent) {
	if p.hasParam {
		if !p.valueStarted {
			p.valueStarted = true
			text = strings.TrimPrefix(text, p.syntax.ValueOpen)
		}
		p.putValue(text, events)
		if p.valueStreams {
			p.emitArgument(`"`, events)
		} else {
			value := convertValue(strings.Join(p.valueParts, ""), p.valueTypes)
			p.emitArgument(p.key(p.paramName)+ojson.MarshalString(value), events)
		}
	}
	p.hasParam = false
	p.valueParts = nil
	p.state = "arguments"
}

func (p *Projector) finishCall(events *[]textEvent) {
	p.emitArgument("}", events)
	p.closed = append(p.closed, ToolCall{p.callID, p.functionName, strings.Join(p.argFragments, "")})
	p.callIndex++
	p.hasCall = false
	p.callID = ""
	p.functionName = ""
	p.argFragments = nil
	p.textVisible = false
	p.state = "closing"
}

func (p *Projector) nextTag(tags ...string) (int, string) {
	best, tag := -1, ""
	for _, t := range tags {
		if i := strings.Index(p.pending, t); i >= 0 && (best < 0 || i < best || (i == best && t < tag)) {
			best, tag = i, t
		}
	}
	return best, tag
}

// name returns the name the pending text spells up to end; "" (with ok)
// for one too long, and ok false while it may yet end.
func (p *Projector) name(end string, keep bool) (string, bool) {
	limit := maxNameLength + len(end)
	search := p.pending
	if len(search) > limit {
		search = search[:limit]
	}
	i := strings.Index(search, end)
	if i < 0 {
		if len([]rune(p.pending)) < limit {
			return "", false
		}
		return "", true
	}
	name := p.pending[:i]
	if keep {
		p.pending = p.pending[i:]
	} else {
		p.pending = p.pending[i+len(end):]
	}
	return name, true
}

func (p *Projector) Put(text string) []textEvent {
	p.pending += text
	var events []textEvent
	syn := p.syntax
	for p.pending != "" {
		switch p.state {
		case "output":
			first := strings.TrimLeft(p.pending, " \t\n\r\f\v")
			if first == "" {
				return events
			}
			if strings.HasPrefix(first, "<") {
				p.state = "content"
			} else {
				p.state = "json"
			}
			continue
		case "json":
			p.emitContent(p.pending, &events)
			p.pending = ""
			return events
		case "content":
			start, tag := p.nextTag(p.textTags...)
			if tag == "" {
				ready, held := holdPartial(p.pending, p.textTags...)
				p.pending = held
				p.emitContent(ready, &events)
				return events
			}
			p.emitContent(p.pending[:start], &events)
			p.pending = p.pending[start+len(tag):]
			if tag != thinkEnd {
				p.state = "name"
			}
		case "name":
			name, ok := p.name(string(syn.nameEnd()), true)
			if !ok {
				return events
			}
			p.beginCall(name, &events)
		case "arguments":
			tags := syn.argumentTags()
			start, tag := p.nextTag(tags...)
			if tag == "" {
				_, p.pending = holdPartial(p.pending, tags...)
				return events
			}
			p.pending = p.pending[start+len(tag):]
			if tag == syn.KeyOpen {
				p.state = "parameter"
			} else {
				p.finishCall(&events)
				if tag == toolCallClose {
					p.state = "content"
				}
			}
		case "parameter":
			name, ok := p.name(syn.KeyClose, false)
			if !ok {
				return events
			}
			p.beginParameter(name, &events)
		case "value":
			if !p.valueStarted && len(p.pending) < len(syn.ValueOpen) && strings.HasPrefix(syn.ValueOpen, p.pending) {
				return events
			}
			ends := syn.valueEnds()
			start, tag := p.nextTag(ends...)
			if tag == "" {
				_, held := holdPartial(p.pending, ends...)
				p.putValue(p.pending[:len(p.pending)-len(held)], &events)
				p.pending = held
				return events
			}
			p.endParameter(p.pending[:start], &events)
			p.pending = p.pending[start+len(syn.ValueClose):]
		case "closing":
			rest := strings.TrimLeft(p.pending, nameSpace)
			if strings.HasPrefix(rest, toolCallClose) {
				p.pending = rest[len(toolCallClose):]
			} else if strings.HasPrefix(toolCallClose, rest) {
				return events
			}
			p.state = "content"
		}
	}
	return events
}

// InCall reports whether the output so far ends inside a call.
func (p *Projector) InCall() bool {
	switch p.state {
	case "name", "arguments", "parameter", "value":
		return true
	}
	return false
}

// Finish returns the output's content and calls, and the events the stream
// still owes.
func (p *Projector) Finish(incomplete bool) (string, []ToolCall, []textEvent) {
	var events []textEvent
	if !incomplete {
		if p.state == "name" {
			name := ""
			if len([]rune(p.pending)) <= maxNameLength {
				name, p.pending = p.pending, ""
			}
			p.beginCall(name, &events)
		}
		if p.state == "value" {
			p.pending = ""
			p.endParameter("", &events)
		}
		if p.state == "arguments" || p.state == "parameter" {
			p.pending = ""
			p.finishCall(&events)
		}
	}
	if p.state == "closing" {
		p.pending = ""
		p.state = "content"
	}
	partialTag := false
	if incomplete {
		for _, t := range p.textTags {
			if strings.HasPrefix(t, p.pending) {
				partialTag = true
			}
		}
	}
	if (p.state == "content" || p.state == "output" || p.state == "json") && p.pending != "" && !partialTag {
		p.contentFrags = append(p.contentFrags, p.pending)
	} else if len(p.closed) > 0 {
		p.contentFrags = p.contentFrags[:p.streamedCount]
	}
	p.pending = ""
	calls := append([]ToolCall(nil), p.closed...)
	if p.hasCall {
		calls = append(calls, ToolCall{p.callID, p.functionName, strings.Join(p.argFragments, "")})
	}
	streamed := p.streamedContent()
	content := strings.Join(p.contentFrags, "")
	if len(calls) > 0 && incomplete {
		content = streamed
	}
	if len(content) > len(streamed) {
		events = append(events, textEvent{field: "content", text: content[len(streamed):]})
	}
	return content, calls, events
}

// CallID is the open call's id, "" without one.
func (p *Projector) CallID() string {
	if p.hasCall {
		return p.callID
	}
	return ""
}

// Block is one block of output: the reasoning, a run of text or a call.
type Block struct {
	Kind   string // reasoning, text, tool
	Parts  []string
	CallID string
	Name   string
	Status string
}

func (b *Block) Text() string { return strings.Join(b.Parts, "") }

// BlockSequencer orders the output into blocks (output.BlockSequencer).
type BlockSequencer struct {
	OnOpen  func(int, *Block)
	OnDelta func(int, *Block, string)
	OnClose func(int, *Block)
	Blocks  []*Block
	open    *Block
}

func (s *BlockSequencer) start(b *Block) {
	s.close("completed")
	s.Blocks = append(s.Blocks, b)
	s.open = b
	if s.OnOpen != nil {
		s.OnOpen(len(s.Blocks)-1, b)
	}
}

func (s *BlockSequencer) appendText(text string) {
	s.open.Parts = append(s.open.Parts, text)
	if s.OnDelta != nil {
		s.OnDelta(len(s.Blocks)-1, s.open, text)
	}
}

func (s *BlockSequencer) close(status string) {
	b := s.open
	s.open = nil
	if b == nil {
		return
	}
	b.Status = status
	if s.OnClose != nil {
		s.OnClose(len(s.Blocks)-1, b)
	}
}

func (s *BlockSequencer) Text(field, text string) {
	kind := "text"
	if field == "reasoning_content" {
		kind = "reasoning"
	}
	if s.open == nil || s.open.Kind != kind {
		s.start(&Block{Kind: kind, Status: "in_progress"})
	}
	s.appendText(text)
}

func (s *BlockSequencer) Tool(delta *ojson.Object) {
	fn := delta.Value("function").(*ojson.Object)
	if name, ok := fn.Get("name"); ok {
		s.start(&Block{Kind: "tool", CallID: delta.Value("id").(string), Name: name.(string), Status: "in_progress"})
	}
	if args, _ := fn.Value("arguments").(string); args != "" {
		s.appendText(args)
	}
}

func (s *BlockSequencer) Finish(incomplete, reasoningOpen bool) []*Block {
	if s.open != nil {
		cut := incomplete && (s.open.Kind != "reasoning" || reasoningOpen)
		if cut {
			s.close("incomplete")
		} else {
			s.close("completed")
		}
	}
	allReasoning := true
	for _, b := range s.Blocks {
		if b.Kind != "reasoning" {
			allReasoning = false
		}
	}
	if allReasoning {
		s.start(&Block{Kind: "text", Status: "in_progress"})
		if incomplete {
			s.close("incomplete")
		} else {
			s.close("completed")
		}
	}
	return s.Blocks
}
