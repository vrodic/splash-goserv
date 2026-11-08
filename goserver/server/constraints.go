package server

import (
	"container/list"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"splash/goserver/llg"
	"splash/goserver/ojson"
	"splash/goserver/tokenizer"
	"splash/goserver/wire"
)

// ConstraintError is output the grammar rejects, or a grammar with no valid
// next token.
type ConstraintError struct{ Msg string }

func (e *ConstraintError) Error() string { return e.Msg }

// FindLLGuidance is the llguidance library of the Python environment beside
// the server: the checkout's .venv or the packaged python.
func FindLLGuidance(root string) string {
	if p := os.Getenv("SPLASH_LLGUIDANCE"); p != "" {
		return p
	}
	for _, pattern := range []string{".venv/lib/python3*/site-packages/llguidance/_lib.abi3.so",
		"python/lib/python3*/site-packages/llguidance/_lib.abi3.so"} {
		if matches, _ := filepath.Glob(filepath.Join(root, pattern)); len(matches) > 0 {
			return matches[len(matches)-1]
		}
	}
	return ""
}

// ConstraintFactory compiles grammars into matchers, cached by grammar text
// (constraints.ConstraintFactory).
type ConstraintFactory struct {
	tok     *llg.Tokenizer
	ready   *wire.Ready
	mu      sync.Mutex
	cache   map[string]*list.Element
	order   *list.List
	bytes   int
	pending map[string]*pendingGrammar
	hits    int
	misses  int
}

type cachedGrammar struct {
	grammar string
	matcher *llg.Matcher
}

type pendingGrammar struct {
	done    chan struct{}
	matcher *llg.Matcher
	err     error
}

const grammarCacheSize = 32
const grammarCacheBytes = 8 * 1024 * 1024

// NewConstraintFactory loads llguidance and its tokenizer for the engine's
// vocabulary and stop tokens.
func NewConstraintFactory(libPath string, tok *tokenizer.Tokenizer, ready *wire.Ready) (*ConstraintFactory, error) {
	if libPath == "" {
		return nil, errors.New("no llguidance library found (set SPLASH_LLGUIDANCE)")
	}
	if err := llg.Load(libPath); err != nil {
		return nil, err
	}
	// Each token's bytes, as llguidance's own adapters give them: an added
	// token, which grammars name by token as <tool_call> and </think>,
	// prefixed with 0xFF as a special one; rows past the tokenizer's
	// vocabulary are empty.
	tokens := make([][]byte, ready.Vocabulary)
	for id := range tokens {
		if id >= tok.VocabSize() {
			continue
		}
		b := tok.DecodeBytes([]int{id}, false, nil)
		if tok.IsAdded(id) {
			b = append([]byte{0xFF}, b...)
		}
		tokens[id] = b
	}
	t, err := llg.NewTokenizerFromBytes(tokens, ready.StopTokens, tok.Encode)
	tokens = nil
	if err != nil {
		return nil, err
	}
	patternValidator = func(g string) bool { return t.Validate("lark", g) == "" }
	return &ConstraintFactory{tok: t, ready: ready, cache: map[string]*list.Element{}, order: list.New(),
		pending: map[string]*pendingGrammar{}}, nil
}

func grammarKind(g string) string {
	if strings.HasPrefix(g, "{") {
		return "llguidance"
	}
	return "lark"
}

func grammarError(msg string) *APIError {
	if strings.HasPrefix(msg, "panic") {
		return apiErr(400, "tool or output schema is too large to compile")
	}
	first, _, _ := strings.Cut(msg, "\n")
	return apiErr(400, "unsupported output schema: "+first)
}

type prefixCheck struct {
	tokens  []uint32
	message string
}

// Create returns a constraint for grammar. prefixes, run when the grammar
// compiles, are token sequences the output must be able to begin with.
func (f *ConstraintFactory) Create(grammar string, prefixes func() []prefixCheck) (*TokenConstraint, error) {
	m, err := f.matcher(grammar, prefixes)
	if err != nil {
		return nil, err
	}
	words := (int(f.ready.Vocabulary) + 31) / 32
	return &TokenConstraint{matcher: m.Clone(), vocabulary: int(f.ready.Vocabulary), eos: f.ready.StopTokens,
		rows: int(f.ready.MaskRows), words: words}, nil
}

func (f *ConstraintFactory) matcher(grammar string, prefixes func() []prefixCheck) (*llg.Matcher, error) {
	f.mu.Lock()
	if e, ok := f.cache[grammar]; ok {
		f.order.MoveToFront(e)
		f.hits++
		f.mu.Unlock()
		return e.Value.(*cachedGrammar).matcher, nil
	}
	if p, ok := f.pending[grammar]; ok {
		f.mu.Unlock()
		<-p.done
		f.mu.Lock()
		f.hits++
		f.mu.Unlock()
		return p.matcher, p.err
	}
	p := &pendingGrammar{done: make(chan struct{})}
	f.pending[grammar] = p
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.pending, grammar)
		f.mu.Unlock()
		close(p.done)
	}()
	kind := grammarKind(grammar)
	if msg := f.tok.Validate(kind, grammar); msg != "" {
		p.err = grammarError(msg)
		return nil, p.err
	}
	m := f.tok.NewMatcher(kind, grammar)
	if m.IsError() {
		p.err = grammarError(m.Error())
		return nil, p.err
	}
	if prefixes != nil {
		for _, check := range prefixes() {
			c := m.Clone()
			ok := c.ConsumeTokens(check.tokens)
			c.Free()
			if !ok {
				p.err = apiErr(400, check.message)
				return nil, p.err
			}
		}
	}
	f.mu.Lock()
	f.misses++
	if len(grammar) <= grammarCacheBytes {
		f.cache[grammar] = f.order.PushFront(&cachedGrammar{grammar, m})
		f.bytes += len(grammar)
		for f.order.Len() > grammarCacheSize || f.bytes > grammarCacheBytes {
			last := f.order.Back()
			g := last.Value.(*cachedGrammar)
			f.order.Remove(last)
			delete(f.cache, g.grammar)
			f.bytes -= len(g.grammar)
		}
	}
	f.mu.Unlock()
	p.matcher = m
	return m, nil
}

// Stats is the grammar cache's state for /status.
func (f *ConstraintFactory) Stats() *ojson.Object {
	if f == nil {
		return ojson.O("available", false)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return ojson.O("entries", int64(f.order.Len()), "capacity", int64(grammarCacheSize), "source_bytes", int64(f.bytes),
		"source_budget_bytes", int64(grammarCacheBytes), "hits", int64(f.hits), "misses", int64(f.misses))
}

// TokenConstraint is one request's grammar state (constraints.TokenConstraint).
type TokenConstraint struct {
	mu         sync.Mutex
	matcher    *llg.Matcher
	vocabulary int
	eos        []uint32
	rows       int
	words      int
	committed  [][]uint32
}

// Commit queues generated tokens; the next mask or Finish consumes them.
func (c *TokenConstraint) Commit(tokens []uint32) error {
	for _, t := range tokens {
		if int(t) >= c.vocabulary {
			return &ConstraintError{"generated token is out of range"}
		}
	}
	c.mu.Lock()
	c.committed = append(c.committed, append([]uint32(nil), tokens...))
	c.mu.Unlock()
	return nil
}

func (c *TokenConstraint) consumeCommitted() error {
	batches := c.committed
	c.committed = nil
	m := c.matcher
	for _, tokens := range batches {
		// The bulk API rejects EOS after a stop that takes no extension, and
		// a speculative batch can carry the grammar's last tokens and the
		// EOS together: the tokens before an EOS are consumed in bulk, then
		// the EOS on its own.
		head := tokens
		var eos []uint32
		for i, t := range tokens {
			if containsU32(c.eos, t) {
				head, eos = tokens[:i], tokens[i:]
				break
			}
		}
		ok := len(head) == 0 || m.ConsumeTokens(head)
		if ok && len(eos) > 0 {
			if len(eos) != 1 {
				ok = false
			} else if m.IsStopped() && !m.IsError() {
				// The grammar has ended, and the EOS ends the output with
				// it; a matcher that stopped takes no further token.
				ok = true
			} else {
				ok = m.ConsumeTokens(eos)
			}
		}
		if !ok {
			msg := m.Error()
			first, _, _ := strings.Cut(msg, "\n")
			if first == "" {
				first = "invalid token"
			}
			return &ConstraintError{first}
		}
	}
	return nil
}

func containsU32(list []uint32, v uint32) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Finish checks the tokens generated after the last mask.
func (c *TokenConstraint) Finish() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.consumeCommitted()
}

// Masks returns the masks before the first simulated token and after each,
// as little-endian words.
func (c *TokenConstraint) Masks(simulation []uint32) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.consumeCommitted(); err != nil {
		return nil, err
	}
	if len(simulation) >= c.rows {
		return nil, &ConstraintError{"too many simulation tokens"}
	}
	inRange := len(simulation)
	for i, t := range simulation {
		if int(t) >= c.vocabulary {
			inRange = i
			break
		}
	}
	m := c.matcher
	valid := m.ValidateTokens(simulation[:inRange])
	rows := len(simulation) + 1
	mask := make([]uint32, rows*c.words)
	if !m.ComputeMaskInto(mask[:c.words]) {
		return nil, &ConstraintError{firstLine(m.Error(), "output grammar has no valid token")}
	}
	// The rows after each draft token are computed on a copy: a refused
	// token leaves a matcher in an error state rollback does not clear, and
	// a grammar that has ended takes no token, its EOS included.
	consumed := 0
	if valid > 0 {
		draft := m.Clone()
		for i := 0; i < valid; i++ {
			if draft.IsStopped() || !draft.ConsumeToken(simulation[i]) {
				break
			}
			if !draft.ComputeMaskInto(mask[(i+1)*c.words : (i+2)*c.words]) {
				break
			}
			consumed++
		}
		draft.Free()
	}
	validRows := consumed + 1
	for r := validRows; r < rows; r++ {
		copy(mask[r*c.words:(r+1)*c.words], mask[(validRows-1)*c.words:validRows*c.words])
	}
	for r := 0; r < validRows; r++ {
		any := false
		for _, w := range mask[r*c.words : (r+1)*c.words] {
			if w != 0 {
				any = true
				break
			}
		}
		if !any {
			return nil, &ConstraintError{"output grammar has no valid token"}
		}
	}
	out := make([]byte, 4*len(mask))
	for i, w := range mask {
		binary.LittleEndian.PutUint32(out[4*i:], w)
	}
	return out, nil
}

func firstLine(s, fallback string) string {
	first, _, _ := strings.Cut(s, "\n")
	if first == "" {
		return fallback
	}
	return first
}

// GrammarFor builds the grammar a Chat body's constraint would compile, for
// parity checks: "" for an unconstrained body.
func GrammarFor(body *ojson.Object, thinking bool, family string) (string, error) {
	cf := familyNamed(family).Format
	tools, policy, err := normalizeTools(body.Value("tools"), body.Value("tool_choice"), body.Value("parallel_tool_calls"))
	_ = tools
	if err != nil {
		return "", err
	}
	schema, err := normalizeResponseFormat(body.Value("response_format"))
	if err != nil {
		return "", err
	}
	if policy != nil && (policy.Constrained || schema != nil) {
		return toolGrammar(policy, thinking, schema, cf)
	}
	if schema != nil {
		return jsonGrammar(schema, thinking, cf), nil
	}
	return "", nil
}

// SetPatternValidator lets a parity check validate patterns.
func SetPatternValidator(f func(string) bool) { patternValidator = f }
