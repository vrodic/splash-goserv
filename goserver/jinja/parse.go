// Package jinja renders Hugging Face chat templates: the subset of Jinja2
// they use, with Python's value semantics, under the environment
// transformers renders them in (trim_blocks, lstrip_blocks, loopcontrols,
// the generation tag, tojson, raise_exception and strftime_now).
package jinja

import (
	"fmt"
	"strconv"
	"strings"
)

// Error is a template's syntax or render error. Raised is set for
// raise_exception, a template's own refusal.
type Error struct {
	Msg    string
	Line   int
	Raised bool
}

func (e *Error) Error() string {
	if e.Line > 0 && !e.Raised {
		return fmt.Sprintf("%s (line %d)", e.Msg, e.Line)
	}
	return e.Msg
}

func errf(line int, format string, args ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, args...), Line: line}
}

// ---------------------------------------------------------------- lexing

type tokKind int

const (
	tText tokKind = iota
	tVarBegin
	tVarEnd
	tBlockBegin
	tBlockEnd
	tName
	tString
	tInt
	tFloat
	tOp
	tEOF
)

type token struct {
	kind tokKind
	val  string
	line int
}

type lexer struct {
	src    string
	pos    int
	line   int
	tokens []token
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isNameChar(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }

// lex splits source into text and tag tokens, applying whitespace control.
func lex(src string) ([]token, error) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	l := &lexer{src: src, line: 1}
	// Pending text and whether the tag after it strips it.
	type piece struct {
		text string
		line int
	}
	stripNextLeading := false // a "-" closed the previous tag
	trimNextNewline := false  // trim_blocks after a block tag
	for l.pos < len(src) {
		start := l.pos
		next := findTag(src, l.pos)
		text := src[start:next]
		textLine := l.line
		// Whether the text starts a line, as Jinja's line_starting reads
		// the match before it: the template's start, or a tag whose match
		// ended with the newline trim_blocks or "-" took.
		lineStarting := start == 0
		if trimNextNewline && strings.HasPrefix(text, "\n") {
			text = text[1:]
			textLine++
			lineStarting = true
		}
		if stripNextLeading {
			trimmed := strings.TrimLeft(text, " \t\n\r\f\v")
			if strings.HasSuffix(text[:len(text)-len(trimmed)], "\n") {
				lineStarting = true
			}
			text = trimmed
		}
		stripNextLeading, trimNextNewline = false, false
		l.line += strings.Count(src[start:next], "\n")
		if next >= len(src) {
			if text != "" {
				l.tokens = append(l.tokens, token{tText, text, textLine})
			}
			break
		}
		kind := src[next+1] // '{', '%' or '#'
		modifier := byte(0)
		if next+2 < len(src) && (src[next+2] == '-' || src[next+2] == '+') {
			modifier = src[next+2]
		}
		if modifier == '-' {
			text = strings.TrimRight(text, " \t\n\r\f\v")
		} else if (kind == '%' || kind == '#') && modifier != '+' {
			// lstrip_blocks: spaces and tabs from the line's start.
			i := strings.LastIndexByte(text, '\n')
			tail := text[i+1:]
			if tail != "" && strings.Trim(tail, " \t\f\v") == "" && (i >= 0 || lineStarting) {
				text = text[:i+1]
			}
		}
		if text != "" {
			l.tokens = append(l.tokens, token{tText, text, textLine})
		}
		l.pos = next + 2
		if modifier != 0 {
			l.pos++
		}
		switch kind {
		case '#':
			end := strings.Index(src[l.pos:], "#}")
			if end < 0 {
				return nil, errf(l.line, "unclosed comment")
			}
			body := src[l.pos : l.pos+end]
			l.line += strings.Count(body, "\n")
			l.pos += end + 2
			if strings.HasSuffix(body, "-") {
				stripNextLeading = true
			} else if !strings.HasSuffix(body, "+") {
				trimNextNewline = true
			}
		case '{':
			l.tokens = append(l.tokens, token{tVarBegin, "", l.line})
			strip, err := l.lexTag("}}")
			if err != nil {
				return nil, err
			}
			l.tokens = append(l.tokens, token{tVarEnd, "", l.line})
			stripNextLeading = strip
		case '%':
			// A raw block keeps its body as text.
			if rest, ok := rawStart(src[l.pos:]); ok {
				l.pos += rest
				body, consumed, stripBefore, stripAfter, err := rawBody(src[l.pos:])
				if err != nil {
					return nil, errf(l.line, "%s", err)
				}
				if stripBefore {
					body = strings.TrimRight(body, " \t\n\r\f\v")
				}
				if body != "" {
					l.tokens = append(l.tokens, token{tText, body, l.line})
				}
				l.line += strings.Count(src[l.pos:l.pos+consumed], "\n")
				l.pos += consumed
				stripNextLeading = stripAfter
				trimNextNewline = !stripAfter
				continue
			}
			l.tokens = append(l.tokens, token{tBlockBegin, "", l.line})
			strip, err := l.lexTag("%}")
			if err != nil {
				return nil, err
			}
			l.tokens = append(l.tokens, token{tBlockEnd, "", l.line})
			stripNextLeading = strip
			// The closing "+%}" keeps the newline; "-%}" strips anyway.
			trimNextNewline = !strip && !(l.pos >= 3 && src[l.pos-3] == '+')
		}
	}
	l.tokens = append(l.tokens, token{tEOF, "", l.line})
	return l.tokens, nil
}

func findTag(src string, from int) int {
	for i := from; i+1 < len(src); i++ {
		if src[i] == '{' && (src[i+1] == '{' || src[i+1] == '%' || src[i+1] == '#') {
			return i
		}
	}
	return len(src)
}

// rawStart reports whether a block tag's body opens a raw block, and the
// length of the tag's remainder.
func rawStart(s string) (int, bool) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n') {
		i++
	}
	if !strings.HasPrefix(s[i:], "raw") || (i+3 < len(s) && isNameChar(s[i+3])) {
		return 0, false
	}
	i += 3
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n') {
		i++
	}
	if strings.HasPrefix(s[i:], "-%}") {
		return i + 3, true
	}
	if strings.HasPrefix(s[i:], "%}") {
		return i + 2, true
	}
	return 0, false
}

func rawBody(s string) (body string, consumed int, stripBefore, stripAfter bool, err error) {
	for i := 0; i < len(s); i++ {
		if !strings.HasPrefix(s[i:], "{%") {
			continue
		}
		j := i + 2
		strip := false
		if j < len(s) && (s[j] == '-' || s[j] == '+') {
			strip = s[j] == '-'
			j++
		}
		for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n') {
			j++
		}
		if !strings.HasPrefix(s[j:], "endraw") {
			continue
		}
		j += 6
		for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n') {
			j++
		}
		after := false
		if strings.HasPrefix(s[j:], "-%}") {
			after = true
			j += 3
		} else if strings.HasPrefix(s[j:], "%}") {
			j += 2
		} else {
			continue
		}
		return s[:i], j, strip, after, nil
	}
	return "", 0, false, false, fmt.Errorf("unclosed raw block")
}

// lexTag tokenizes a tag's body up to its end ("}}" or "%}") and reports
// whether the end strips whitespace after it ("-}}" / "-%}").
func (l *lexer) lexTag(end string) (bool, error) {
	src := l.src
	for {
		for l.pos < len(src) && (src[l.pos] == ' ' || src[l.pos] == '\t' || src[l.pos] == '\n' || src[l.pos] == '\r') {
			if src[l.pos] == '\n' {
				l.line++
			}
			l.pos++
		}
		if l.pos >= len(src) {
			return false, errf(l.line, "unclosed tag")
		}
		if strings.HasPrefix(src[l.pos:], "-"+end) {
			l.pos += 1 + len(end)
			return true, nil
		}
		if strings.HasPrefix(src[l.pos:], "+"+end) {
			l.pos += 1 + len(end)
			return false, nil
		}
		if strings.HasPrefix(src[l.pos:], end) {
			l.pos += len(end)
			return false, nil
		}
		c := src[l.pos]
		switch {
		case isNameStart(c):
			s := l.pos
			for l.pos < len(src) && isNameChar(src[l.pos]) {
				l.pos++
			}
			l.tokens = append(l.tokens, token{tName, src[s:l.pos], l.line})
		case c >= '0' && c <= '9':
			s := l.pos
			isFloat := false
			for l.pos < len(src) && (src[l.pos] >= '0' && src[l.pos] <= '9' || src[l.pos] == '_') {
				l.pos++
			}
			if l.pos+1 < len(src) && src[l.pos] == '.' && src[l.pos+1] >= '0' && src[l.pos+1] <= '9' {
				isFloat = true
				l.pos++
				for l.pos < len(src) && (src[l.pos] >= '0' && src[l.pos] <= '9' || src[l.pos] == '_') {
					l.pos++
				}
			}
			if l.pos < len(src) && (src[l.pos] == 'e' || src[l.pos] == 'E') {
				p := l.pos + 1
				if p < len(src) && (src[p] == '+' || src[p] == '-') {
					p++
				}
				if p < len(src) && src[p] >= '0' && src[p] <= '9' {
					isFloat = true
					l.pos = p
					for l.pos < len(src) && src[l.pos] >= '0' && src[l.pos] <= '9' {
						l.pos++
					}
				}
			}
			text := strings.ReplaceAll(src[s:l.pos], "_", "")
			if isFloat {
				l.tokens = append(l.tokens, token{tFloat, text, l.line})
			} else {
				l.tokens = append(l.tokens, token{tInt, text, l.line})
			}
		case c == '"' || c == '\'':
			s, err := l.lexString(c)
			if err != nil {
				return false, err
			}
			l.tokens = append(l.tokens, token{tString, s, l.line})
		default:
			op := ""
			for _, candidate := range []string{"//", "**", "==", "!=", "<=", ">=", "+", "-", "*", "/", "%", "~", "<", ">", "(", ")", "[", "]", "{", "}", ",", ".", ":", "|", "=", "!"} {
				if strings.HasPrefix(src[l.pos:], candidate) {
					op = candidate
					break
				}
			}
			if op == "" {
				return false, errf(l.line, "unexpected character %q", c)
			}
			l.pos += len(op)
			l.tokens = append(l.tokens, token{tOp, op, l.line})
		}
	}
}

func (l *lexer) lexString(quote byte) (string, error) {
	src := l.src
	l.pos++
	var sb strings.Builder
	for l.pos < len(src) {
		c := src[l.pos]
		if c == quote {
			l.pos++
			return sb.String(), nil
		}
		if c == '\n' {
			l.line++
		}
		if c != '\\' {
			sb.WriteByte(c)
			l.pos++
			continue
		}
		l.pos++
		if l.pos >= len(src) {
			break
		}
		e := src[l.pos]
		l.pos++
		switch e {
		case 'n':
			sb.WriteByte('\n')
		case 't':
			sb.WriteByte('\t')
		case 'r':
			sb.WriteByte('\r')
		case '\\', '\'', '"':
			sb.WriteByte(e)
		case '0':
			sb.WriteByte(0)
		case 'a':
			sb.WriteByte(7)
		case 'b':
			sb.WriteByte(8)
		case 'f':
			sb.WriteByte(12)
		case 'v':
			sb.WriteByte(11)
		case '\n':
		case 'x', 'u', 'U':
			n := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
			if l.pos+n > len(src) {
				return "", errf(l.line, "invalid escape")
			}
			v, err := strconv.ParseUint(src[l.pos:l.pos+n], 16, 32)
			if err != nil {
				return "", errf(l.line, "invalid escape")
			}
			sb.WriteRune(rune(v))
			l.pos += n
		default:
			sb.WriteByte('\\')
			sb.WriteByte(e)
		}
	}
	return "", errf(l.line, "unclosed string")
}

// ---------------------------------------------------------------- AST

type node interface{}

type (
	textNode   struct{ text string }
	outputNode struct {
		expr expr
		line int
	}
	ifNode struct {
		conds  []expr
		bodies [][]node
		orelse []node
	}
	forNode struct {
		targets   []string
		iter      expr
		filter    expr
		body      []node
		orelse    []node
		recursive bool
		line      int
	}
	setNode struct {
		targets []expr // name or attribute targets
		value   expr
		body    []node // block set
		line    int
	}
	macroNode struct {
		name     string
		params   []string
		defaults []expr // aligned with params, nil for none
		body     []node
	}
	callBlockNode struct {
		call   expr
		params []string
		body   []node
	}
	filterBlockNode struct {
		filter *filterExpr
		body   []node
	}
	breakNode    struct{}
	continueNode struct{}
	scopeNode    struct{ body []node }
)

type expr interface{}

type (
	literal  struct{ v Value }
	nameExpr struct {
		name string
		line int
	}
	listExpr  struct{ items []expr }
	tupleExpr struct{ items []expr }
	dictExpr  struct{ keys, vals []expr }
	attrExpr  struct {
		obj  expr
		attr string
		line int
	}
	itemExpr struct {
		obj, key expr
		line     int
	}
	sliceExpr struct {
		obj               expr
		start, stop, step expr
	}
	callExpr struct {
		fn     expr
		args   []expr
		kwargs []kwarg
		line   int
	}
	filterExpr struct {
		value  expr // nil inside a filter block
		name   string
		args   []expr
		kwargs []kwarg
		line   int
	}
	testExpr struct {
		value  expr
		name   string
		args   []expr
		negate bool
		line   int
	}
	binExpr struct {
		op          string
		left, right expr
		line        int
	}
	unaryExpr struct {
		op   string
		x    expr
		line int
	}
	condExpr    struct{ cond, yes, no expr }
	compareExpr struct {
		first expr
		ops   []string
		rest  []expr
		line  int
	}
	concatExpr struct{ parts []expr }
)

type kwarg struct {
	name string
	val  expr
}

// ---------------------------------------------------------------- parsing

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}
func (p *parser) isOp(op string) bool {
	t := p.peek()
	return t.kind == tOp && t.val == op
}
func (p *parser) isName(name string) bool {
	t := p.peek()
	return t.kind == tName && t.val == name
}
func (p *parser) expectOp(op string) error {
	t := p.next()
	if t.kind != tOp || t.val != op {
		return errf(t.line, "expected %q, got %q", op, t.val)
	}
	return nil
}
func (p *parser) expectName() (string, error) {
	t := p.next()
	if t.kind != tName {
		return "", errf(t.line, "expected a name, got %q", t.val)
	}
	return t.val, nil
}
func (p *parser) expectBlockEnd() error {
	t := p.next()
	if t.kind != tBlockEnd {
		return errf(t.line, "expected end of statement, got %q", t.val)
	}
	return nil
}

// parseBody parses nodes until a block tag whose first name is in ends,
// which it leaves unconsumed after the begin token; it returns that name.
func (p *parser) parseBody(ends ...string) ([]node, string, error) {
	var nodes []node
	for {
		t := p.peek()
		switch t.kind {
		case tEOF:
			if len(ends) > 0 {
				return nil, "", errf(t.line, "unexpected end of template, expected %s", strings.Join(ends, " or "))
			}
			return nodes, "", nil
		case tText:
			p.next()
			nodes = append(nodes, &textNode{t.val})
		case tVarBegin:
			p.next()
			e, err := p.parseExpr()
			if err != nil {
				return nil, "", err
			}
			if t := p.next(); t.kind != tVarEnd {
				return nil, "", errf(t.line, "expected end of print statement, got %q", t.val)
			}
			nodes = append(nodes, &outputNode{e, t.line})
		case tBlockBegin:
			p.next()
			kw := p.peek()
			if kw.kind != tName {
				return nil, "", errf(kw.line, "expected a statement name")
			}
			for _, end := range ends {
				if kw.val == end {
					return nodes, end, nil
				}
			}
			n, err := p.parseStatement()
			if err != nil {
				return nil, "", err
			}
			if n != nil {
				nodes = append(nodes, n)
			}
		default:
			return nil, "", errf(t.line, "unexpected %q", t.val)
		}
	}
}

func (p *parser) parseStatement() (node, error) {
	kw := p.next()
	switch kw.val {
	case "if":
		return p.parseIf()
	case "for":
		return p.parseFor(kw.line)
	case "set":
		return p.parseSet(kw.line)
	case "macro":
		return p.parseMacro()
	case "call":
		return p.parseCallBlock()
	case "filter":
		return p.parseFilterBlock()
	case "break":
		return &breakNode{}, p.expectBlockEnd()
	case "continue":
		return &continueNode{}, p.expectBlockEnd()
	case "generation":
		if err := p.expectBlockEnd(); err != nil {
			return nil, err
		}
		body, _, err := p.parseBody("endgeneration")
		if err != nil {
			return nil, err
		}
		p.next()
		return &scopeNode{body}, p.expectBlockEnd()
	case "do":
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		return &setNode{value: e, line: kw.line}, p.expectBlockEnd()
	}
	return nil, errf(kw.line, "unknown statement %q", kw.val)
}

func (p *parser) parseIf() (node, error) {
	n := &ifNode{}
	cond, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if err := p.expectBlockEnd(); err != nil {
		return nil, err
	}
	for {
		body, end, err := p.parseBody("elif", "else", "endif")
		if err != nil {
			return nil, err
		}
		n.conds = append(n.conds, cond)
		n.bodies = append(n.bodies, body)
		p.next()
		switch end {
		case "elif":
			if cond, err = p.parseExpr(); err != nil {
				return nil, err
			}
			if err := p.expectBlockEnd(); err != nil {
				return nil, err
			}
		case "else":
			if err := p.expectBlockEnd(); err != nil {
				return nil, err
			}
			if n.orelse, _, err = p.parseBody("endif"); err != nil {
				return nil, err
			}
			p.next()
			return n, p.expectBlockEnd()
		case "endif":
			return n, p.expectBlockEnd()
		}
	}
}

func (p *parser) parseFor(line int) (node, error) {
	n := &forNode{line: line}
	for {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		n.targets = append(n.targets, name)
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	if !p.isName("in") {
		return nil, errf(p.peek().line, "expected 'in'")
	}
	p.next()
	iter, err := p.parseCondless()
	if err != nil {
		return nil, err
	}
	n.iter = iter
	if p.isName("if") {
		p.next()
		if n.filter, err = p.parseCondless(); err != nil {
			return nil, err
		}
	}
	if p.isName("recursive") {
		p.next()
		n.recursive = true
	}
	if err := p.expectBlockEnd(); err != nil {
		return nil, err
	}
	body, end, err := p.parseBody("else", "endfor")
	if err != nil {
		return nil, err
	}
	n.body = body
	p.next()
	if end == "else" {
		if err := p.expectBlockEnd(); err != nil {
			return nil, err
		}
		if n.orelse, _, err = p.parseBody("endfor"); err != nil {
			return nil, err
		}
		p.next()
	}
	return n, p.expectBlockEnd()
}

func (p *parser) parseSet(line int) (node, error) {
	n := &setNode{line: line}
	for {
		target, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		// A dotted target sets a namespace's attribute.
		for p.isOp(".") {
			p.next()
			attr, err := p.expectName()
			if err != nil {
				return nil, err
			}
			target = &attrExpr{obj: target, attr: attr}
		}
		n.targets = append(n.targets, target)
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	if p.peek().kind == tBlockEnd {
		p.next()
		body, _, err := p.parseBody("endset")
		if err != nil {
			return nil, err
		}
		p.next()
		n.body = body
		if n.body == nil {
			n.body = []node{}
		}
		return n, p.expectBlockEnd()
	}
	if err := p.expectOp("="); err != nil {
		return nil, err
	}
	value, err := p.parseTuple()
	if err != nil {
		return nil, err
	}
	n.value = value
	return n, p.expectBlockEnd()
}

func (p *parser) parseParams() ([]string, []expr, error) {
	var params []string
	var defaults []expr
	if err := p.expectOp("("); err != nil {
		return nil, nil, err
	}
	for !p.isOp(")") {
		name, err := p.expectName()
		if err != nil {
			return nil, nil, err
		}
		params = append(params, name)
		var def expr
		if p.isOp("=") {
			p.next()
			if def, err = p.parseExpr(); err != nil {
				return nil, nil, err
			}
		}
		defaults = append(defaults, def)
		if p.isOp(",") {
			p.next()
		}
	}
	p.next()
	return params, defaults, nil
}

func (p *parser) parseMacro() (node, error) {
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	params, defaults, err := p.parseParams()
	if err != nil {
		return nil, err
	}
	if err := p.expectBlockEnd(); err != nil {
		return nil, err
	}
	body, _, err := p.parseBody("endmacro")
	if err != nil {
		return nil, err
	}
	p.next()
	if p.peek().kind == tName { // {% endmacro name %}
		p.next()
	}
	return &macroNode{name, params, defaults, body}, p.expectBlockEnd()
}

func (p *parser) parseCallBlock() (node, error) {
	n := &callBlockNode{}
	if p.isOp("(") {
		params, _, err := p.parseParams()
		if err != nil {
			return nil, err
		}
		n.params = params
	}
	call, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	n.call = call
	if err := p.expectBlockEnd(); err != nil {
		return nil, err
	}
	if n.body, _, err = p.parseBody("endcall"); err != nil {
		return nil, err
	}
	p.next()
	return n, p.expectBlockEnd()
}

func (p *parser) parseFilterBlock() (node, error) {
	f, err := p.parseFilterTail(nil)
	if err != nil {
		return nil, err
	}
	n := &filterBlockNode{filter: f}
	if err := p.expectBlockEnd(); err != nil {
		return nil, err
	}
	if n.body, _, err = p.parseBody("endfilter"); err != nil {
		return nil, err
	}
	p.next()
	return n, p.expectBlockEnd()
}

// parseTuple parses an expression, or a bare tuple of them (a, b).
func (p *parser) parseTuple() (expr, error) {
	first, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if !p.isOp(",") {
		return first, nil
	}
	items := []expr{first}
	for p.isOp(",") {
		p.next()
		if p.peek().kind == tBlockEnd || p.peek().kind == tVarEnd {
			break
		}
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		items = append(items, e)
	}
	return &tupleExpr{items}, nil
}

func (p *parser) parseExpr() (expr, error) {
	e, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	for p.isName("if") {
		p.next()
		cond, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		var no expr
		if p.isName("else") {
			p.next()
			if no, err = p.parseExpr(); err != nil {
				return nil, err
			}
		}
		e = &condExpr{cond, e, no}
	}
	return e, nil
}

// parseCondless parses an expression without a trailing conditional, as a
// for loop's iterable and filter are.
func (p *parser) parseCondless() (expr, error) { return p.parseOr() }

func (p *parser) parseOr() (expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.isName("or") {
		line := p.next().line
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &binExpr{"or", left, right, line}
	}
	return left, nil
}

func (p *parser) parseAnd() (expr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.isName("and") {
		line := p.next().line
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &binExpr{"and", left, right, line}
	}
	return left, nil
}

func (p *parser) parseNot() (expr, error) {
	if p.isName("not") {
		line := p.next().line
		x, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &unaryExpr{"not", x, line}, nil
	}
	return p.parseCompare()
}

func (p *parser) parseCompare() (expr, error) {
	first, err := p.parseMath1()
	if err != nil {
		return nil, err
	}
	c := &compareExpr{first: first, line: p.peek().line}
	for {
		t := p.peek()
		var op string
		switch {
		case t.kind == tOp && (t.val == "==" || t.val == "!=" || t.val == "<" || t.val == ">" || t.val == "<=" || t.val == ">="):
			op = t.val
			p.next()
		case t.kind == tName && t.val == "in":
			op = "in"
			p.next()
		case t.kind == tName && t.val == "not" && p.toks[p.pos+1].kind == tName && p.toks[p.pos+1].val == "in":
			op = "notin"
			p.next()
			p.next()
		default:
			if len(c.ops) == 0 {
				return first, nil
			}
			return c, nil
		}
		right, err := p.parseMath1()
		if err != nil {
			return nil, err
		}
		c.ops = append(c.ops, op)
		c.rest = append(c.rest, right)
	}
}

func (p *parser) parseMath1() (expr, error) {
	left, err := p.parseConcat()
	if err != nil {
		return nil, err
	}
	for p.isOp("+") || p.isOp("-") {
		t := p.next()
		right, err := p.parseConcat()
		if err != nil {
			return nil, err
		}
		left = &binExpr{t.val, left, right, t.line}
	}
	return left, nil
}

func (p *parser) parseConcat() (expr, error) {
	first, err := p.parseMath2()
	if err != nil {
		return nil, err
	}
	if !p.isOp("~") {
		return first, nil
	}
	parts := []expr{first}
	for p.isOp("~") {
		p.next()
		e, err := p.parseMath2()
		if err != nil {
			return nil, err
		}
		parts = append(parts, e)
	}
	return &concatExpr{parts}, nil
}

func (p *parser) parseMath2() (expr, error) {
	left, err := p.parsePow()
	if err != nil {
		return nil, err
	}
	for p.isOp("*") || p.isOp("/") || p.isOp("//") || p.isOp("%") {
		t := p.next()
		right, err := p.parsePow()
		if err != nil {
			return nil, err
		}
		left = &binExpr{t.val, left, right, t.line}
	}
	return left, nil
}

func (p *parser) parsePow() (expr, error) {
	left, err := p.parseUnary(true)
	if err != nil {
		return nil, err
	}
	for p.isOp("**") {
		t := p.next()
		right, err := p.parseUnary(true)
		if err != nil {
			return nil, err
		}
		left = &binExpr{"**", left, right, t.line}
	}
	return left, nil
}

func (p *parser) parseUnary(withFilter bool) (expr, error) {
	var e expr
	var err error
	if p.isOp("-") || p.isOp("+") {
		t := p.next()
		x, err := p.parseUnary(false)
		if err != nil {
			return nil, err
		}
		e = &unaryExpr{t.val, x, t.line}
	} else if e, err = p.parsePrimary(); err != nil {
		return nil, err
	}
	if e, err = p.parsePostfix(e); err != nil {
		return nil, err
	}
	if withFilter {
		return p.parseFilterChain(e)
	}
	return e, nil
}

func (p *parser) parsePrimary() (expr, error) {
	t := p.next()
	switch t.kind {
	case tName:
		switch t.val {
		case "true", "True":
			return &literal{true}, nil
		case "false", "False":
			return &literal{false}, nil
		case "none", "None":
			return &literal{nil}, nil
		}
		return &nameExpr{t.val, t.line}, nil
	case tString:
		s := t.val
		// Adjacent strings concatenate.
		for p.peek().kind == tString {
			s += p.next().val
		}
		return &literal{s}, nil
	case tInt:
		v, err := strconv.ParseInt(t.val, 10, 64)
		if err != nil {
			return nil, errf(t.line, "integer out of range")
		}
		return &literal{v}, nil
	case tFloat:
		v, _ := strconv.ParseFloat(t.val, 64)
		return &literal{v}, nil
	case tOp:
		switch t.val {
		case "(":
			if p.isOp(")") {
				p.next()
				return &tupleExpr{}, nil
			}
			first, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if p.isOp(")") {
				p.next()
				return first, nil
			}
			items := []expr{first}
			for p.isOp(",") {
				p.next()
				if p.isOp(")") {
					break
				}
				e, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				items = append(items, e)
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return &tupleExpr{items}, nil
		case "[":
			var items []expr
			for !p.isOp("]") {
				e, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				items = append(items, e)
				if !p.isOp(",") {
					break
				}
				p.next()
			}
			if err := p.expectOp("]"); err != nil {
				return nil, err
			}
			return &listExpr{items}, nil
		case "{":
			d := &dictExpr{}
			for !p.isOp("}") {
				k, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				if err := p.expectOp(":"); err != nil {
					return nil, err
				}
				v, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				d.keys = append(d.keys, k)
				d.vals = append(d.vals, v)
				if !p.isOp(",") {
					break
				}
				p.next()
			}
			if err := p.expectOp("}"); err != nil {
				return nil, err
			}
			return d, nil
		}
	}
	return nil, errf(t.line, "unexpected %q", t.val)
}

func (p *parser) parsePostfix(e expr) (expr, error) {
	for {
		t := p.peek()
		if t.kind != tOp {
			return e, nil
		}
		switch t.val {
		case ".":
			p.next()
			nt := p.next()
			if nt.kind == tName {
				e = &attrExpr{e, nt.val, nt.line}
			} else if nt.kind == tInt {
				v, _ := strconv.ParseInt(nt.val, 10, 64)
				e = &itemExpr{e, &literal{v}, nt.line}
			} else {
				return nil, errf(nt.line, "expected an attribute name")
			}
		case "[":
			p.next()
			var parts [3]expr
			isSlice := false
			idx := 0
			for {
				if p.isOp("]") {
					break
				}
				if p.isOp(":") {
					p.next()
					isSlice = true
					idx++
					if idx > 2 {
						return nil, errf(t.line, "invalid slice")
					}
					continue
				}
				x, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				parts[idx] = x
			}
			p.next()
			if isSlice {
				e = &sliceExpr{e, parts[0], parts[1], parts[2]}
			} else {
				if parts[0] == nil {
					return nil, errf(t.line, "empty subscript")
				}
				e = &itemExpr{e, parts[0], t.line}
			}
		case "(":
			p.next()
			args, kwargs, err := p.parseArgs()
			if err != nil {
				return nil, err
			}
			e = &callExpr{e, args, kwargs, t.line}
		default:
			return e, nil
		}
	}
}

// parseArgs parses call arguments after "(" through ")".
func (p *parser) parseArgs() ([]expr, []kwarg, error) {
	var args []expr
	var kwargs []kwarg
	for !p.isOp(")") {
		if p.peek().kind == tName && p.toks[p.pos+1].kind == tOp && p.toks[p.pos+1].val == "=" {
			name := p.next().val
			p.next()
			v, err := p.parseExpr()
			if err != nil {
				return nil, nil, err
			}
			kwargs = append(kwargs, kwarg{name, v})
		} else {
			v, err := p.parseExpr()
			if err != nil {
				return nil, nil, err
			}
			args = append(args, v)
		}
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	return args, kwargs, p.expectOp(")")
}

func (p *parser) parseFilterTail(value expr) (*filterExpr, error) {
	t := p.peek()
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	for p.isOp(".") { // dotted filter names
		p.next()
		part, err := p.expectName()
		if err != nil {
			return nil, err
		}
		name += "." + part
	}
	f := &filterExpr{value: value, name: name, line: t.line}
	if p.isOp("(") {
		p.next()
		if f.args, f.kwargs, err = p.parseArgs(); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (p *parser) parseFilterChain(e expr) (expr, error) {
	for {
		if p.isOp("|") {
			p.next()
			f, err := p.parseFilterTail(e)
			if err != nil {
				return nil, err
			}
			e = f
			continue
		}
		if p.isName("is") {
			line := p.next().line
			negate := false
			if p.isName("not") {
				p.next()
				negate = true
			}
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			te := &testExpr{value: e, name: name, negate: negate, line: line}
			if p.isOp("(") {
				p.next()
				args, _, err := p.parseArgs()
				if err != nil {
					return nil, err
				}
				te.args = args
			} else if t := p.peek(); t.kind == tString || t.kind == tInt || t.kind == tFloat ||
				(t.kind == tName && !isKeyword(t.val)) || (t.kind == tOp && (t.val == "[" || t.val == "{" || t.val == "-")) {
				// A test takes one argument without parentheses: is divisibleby 3.
				arg, err := p.parseUnary(false)
				if err != nil {
					return nil, err
				}
				arg, err = p.parsePostfix(arg)
				if err != nil {
					return nil, err
				}
				te.args = []expr{arg}
			}
			e = te
			continue
		}
		return e, nil
	}
}

func isKeyword(s string) bool {
	switch s {
	case "and", "or", "not", "in", "is", "if", "else", "for", "recursive":
		return true
	}
	return false
}

// Template is a parsed template.
type Template struct {
	body []node
}

// Parse parses template source.
func Parse(src string) (*Template, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	body, _, err := p.parseBody()
	if err != nil {
		return nil, err
	}
	return &Template{body: body}, nil
}
