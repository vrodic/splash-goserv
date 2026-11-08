// Package tokenizer is a byte-level BPE tokenizer that reads a Hugging Face
// tokenizer.json of the shape Splash's model families ship: an NFC
// normalizer, a Split pre-tokenizer with the Qwen/GPT-4-style regex followed
// by ByteLevel, a BPE model without dropout, and a ByteLevel decoder.
//
// It replaces the Python server's transformers AutoTokenizer, which keeps the
// Rust tokenizer plus Python objects for every vocabulary entry.
package tokenizer

import (
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// AddedToken is an entry of tokenizer.json's added_tokens.
type AddedToken struct {
	ID         int    `json:"id"`
	Content    string `json:"content"`
	Special    bool   `json:"special"`
	Normalized bool   `json:"normalized"`
	LStrip     bool   `json:"lstrip"`
	RStrip     bool   `json:"rstrip"`
	SingleWord bool   `json:"single_word"`
}

type mergeEntry struct {
	rank int32
	id   int32
}

// Tokenizer encodes and decodes text. It is safe for concurrent use.
type Tokenizer struct {
	vocab   map[string]int32
	tokens  []string // id -> token text (byte-level encoded for model tokens)
	special []bool   // id -> added special token
	added   []bool   // id -> added token
	merges  map[uint64]mergeEntry
	// Added tokens, longest first, for splitting the input.
	addedTokens []AddedToken
	addedByText map[string]int32
	addedFirst  map[byte][]int // first byte -> indexes into addedTokens
	byteToken   [256]int32     // the token id of each single byte
	nfc         bool

	cacheMu sync.Mutex
	cache   map[string][]int32
}

const cacheLimit = 1 << 16

type tokenizerFile struct {
	AddedTokens []AddedToken    `json:"added_tokens"`
	Normalizer  json.RawMessage `json:"normalizer"`
	PreTok      json.RawMessage `json:"pre_tokenizer"`
	Decoder     json.RawMessage `json:"decoder"`
	Model       struct {
		Type         string           `json:"type"`
		Dropout      *float64         `json:"dropout"`
		Vocab        map[string]int32 `json:"vocab"`
		Merges       json.RawMessage  `json:"merges"`
		ByteFallback bool             `json:"byte_fallback"`
	} `json:"model"`
}

// expectedRegex is the pre-tokenizer pattern this package implements by hand.
const expectedRegex = `(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?[\p{L}\p{M}]+|\p{N}| ?[^\s\p{L}\p{M}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+`

// Load reads tokenizer.json at path.
func Load(path string) (*Tokenizer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file tokenizerFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("tokenizer.json: %w", err)
	}
	data = nil
	if file.Model.Type != "BPE" || (file.Model.Dropout != nil && *file.Model.Dropout != 0) || file.Model.ByteFallback {
		return nil, errors.New("tokenizer.json: only a byte-level BPE model without dropout is supported")
	}
	if err := checkPipeline(&file); err != nil {
		return nil, err
	}
	t := &Tokenizer{vocab: file.Model.Vocab, addedByText: map[string]int32{}, cache: map[string][]int32{}}
	var normalizer struct{ Type string }
	if len(file.Normalizer) > 0 && string(file.Normalizer) != "null" {
		_ = json.Unmarshal(file.Normalizer, &normalizer)
		if normalizer.Type != "NFC" {
			return nil, fmt.Errorf("tokenizer.json: unsupported normalizer %q", normalizer.Type)
		}
		t.nfc = true
	}
	size := 0
	for _, id := range t.vocab {
		if int(id)+1 > size {
			size = int(id) + 1
		}
	}
	for _, a := range file.AddedTokens {
		if a.ID+1 > size {
			size = a.ID + 1
		}
	}
	t.tokens = make([]string, size)
	t.special = make([]bool, size)
	t.added = make([]bool, size)
	for text, id := range t.vocab {
		t.tokens[id] = text
	}
	for _, a := range file.AddedTokens {
		if a.Normalized || a.LStrip || a.RStrip || a.SingleWord {
			return nil, fmt.Errorf("tokenizer.json: added token %q uses unsupported options", a.Content)
		}
		t.tokens[a.ID] = a.Content
		t.special[a.ID] = a.Special
		t.added[a.ID] = true
		t.addedByText[a.Content] = int32(a.ID)
		t.addedTokens = append(t.addedTokens, a)
	}
	sort.SliceStable(t.addedTokens, func(i, j int) bool {
		return len(t.addedTokens[i].Content) > len(t.addedTokens[j].Content)
	})
	t.addedFirst = map[byte][]int{}
	for i, a := range t.addedTokens {
		if a.Content == "" {
			continue
		}
		t.addedFirst[a.Content[0]] = append(t.addedFirst[a.Content[0]], i)
	}
	for b := 0; b < 256; b++ {
		id, ok := t.vocab[string(byteToRune[b])]
		if !ok {
			return nil, fmt.Errorf("tokenizer.json: byte %d has no token", b)
		}
		t.byteToken[b] = id
	}
	if err := t.loadMerges(file.Model.Merges); err != nil {
		return nil, err
	}
	return t, nil
}

func checkPipeline(file *tokenizerFile) error {
	var pre struct {
		Type          string `json:"type"`
		Pretokenizers []struct {
			Type    string `json:"type"`
			Pattern struct {
				Regex string `json:"Regex"`
			} `json:"pattern"`
			Behavior       string `json:"behavior"`
			Invert         bool   `json:"invert"`
			AddPrefixSpace bool   `json:"add_prefix_space"`
			UseRegex       bool   `json:"use_regex"`
		} `json:"pretokenizers"`
	}
	if err := json.Unmarshal(file.PreTok, &pre); err != nil {
		return fmt.Errorf("tokenizer.json: pre_tokenizer: %w", err)
	}
	p := pre.Pretokenizers
	if pre.Type != "Sequence" || len(p) != 2 || p[0].Type != "Split" || p[1].Type != "ByteLevel" ||
		p[0].Pattern.Regex != expectedRegex || p[0].Behavior != "Isolated" || p[0].Invert ||
		p[1].AddPrefixSpace || p[1].UseRegex {
		return errors.New("tokenizer.json: unsupported pre-tokenizer (the Go server implements the Qwen byte-level pipeline)")
	}
	var dec struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(file.Decoder, &dec)
	if dec.Type != "ByteLevel" {
		return fmt.Errorf("tokenizer.json: unsupported decoder %q", dec.Type)
	}
	return nil
}

func (t *Tokenizer) loadMerges(raw json.RawMessage) error {
	// Merges are either "a b" strings or [a, b] pairs.
	var asStrings []string
	var pairs [][2]string
	if err := json.Unmarshal(raw, &asStrings); err != nil {
		if err := json.Unmarshal(raw, &pairs); err != nil {
			return fmt.Errorf("tokenizer.json: merges: %w", err)
		}
	} else {
		pairs = make([][2]string, len(asStrings))
		for i, m := range asStrings {
			a, b, ok := strings.Cut(m, " ")
			if !ok {
				return fmt.Errorf("tokenizer.json: merge %q", m)
			}
			pairs[i] = [2]string{a, b}
		}
		asStrings = nil
	}
	t.merges = make(map[uint64]mergeEntry, len(pairs))
	for rank, pair := range pairs {
		a, okA := t.vocab[pair[0]]
		b, okB := t.vocab[pair[1]]
		c, okC := t.vocab[pair[0]+pair[1]]
		if !okA || !okB || !okC {
			return fmt.Errorf("tokenizer.json: merge %q %q names unknown tokens", pair[0], pair[1])
		}
		key := uint64(uint32(a))<<32 | uint64(uint32(b))
		if _, seen := t.merges[key]; !seen {
			t.merges[key] = mergeEntry{rank: int32(rank), id: c}
		}
	}
	return nil
}

// VocabSize is the number of token ids, added tokens included (len(tokenizer)).
func (t *Tokenizer) VocabSize() int { return len(t.tokens) }

// TokenToID returns the id of a token, an added token or a vocabulary entry.
func (t *Tokenizer) TokenToID(token string) (int, bool) {
	if id, ok := t.addedByText[token]; ok {
		return int(id), true
	}
	id, ok := t.vocab[token]
	return int(id), ok
}

// IsAdded reports whether id is one of the tokenizer's added tokens.
func (t *Tokenizer) IsAdded(id int) bool { return id >= 0 && id < len(t.added) && t.added[id] }

// IsSpecial reports whether id is an added special token.
func (t *Tokenizer) IsSpecial(id int) bool {
	return id >= 0 && id < len(t.special) && t.special[id]
}

// Encode tokenizes text as tokenizer(text, add_special_tokens=False) does:
// added tokens are matched in the raw text first, the rest normalized,
// pre-tokenized and encoded with BPE. Invalid UTF-8 reads as U+FFFD.
func (t *Tokenizer) Encode(text string) []int {
	out := make([]int, 0, len(text)/3+4)
	start := 0
	for i := 0; i < len(text); {
		if cands, ok := t.addedFirst[text[i]]; ok {
			matched := -1
			for _, c := range cands {
				content := t.addedTokens[c].Content
				if strings.HasPrefix(text[i:], content) {
					matched = c
					break // longest first
				}
			}
			if matched >= 0 {
				out = t.encodeSegment(text[start:i], out)
				out = append(out, t.addedTokens[matched].ID)
				i += len(t.addedTokens[matched].Content)
				start = i
				continue
			}
		}
		i++
	}
	return t.encodeSegment(text[start:], out)
}

func (t *Tokenizer) encodeSegment(text string, out []int) []int {
	if text == "" {
		return out
	}
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "�")
	}
	if t.nfc {
		text = norm.NFC.String(text)
	}
	var buf []byte
	for _, piece := range preTokenize(text) {
		out = t.bpe(piece, out, &buf)
	}
	return out
}

func (t *Tokenizer) bpe(piece string, out []int, buf *[]byte) []int {
	// The whole piece as one token is the common case.
	*buf = (*buf)[:0]
	for i := 0; i < len(piece); i++ {
		*buf = utf8.AppendRune(*buf, byteToRune[piece[i]])
	}
	if id, ok := t.vocab[string(*buf)]; ok {
		return append(out, int(id))
	}
	t.cacheMu.Lock()
	cached, ok := t.cache[piece]
	t.cacheMu.Unlock()
	if ok {
		for _, id := range cached {
			out = append(out, int(id))
		}
		return out
	}
	symbols := t.mergeWord(piece)
	t.cacheMu.Lock()
	if len(t.cache) >= cacheLimit {
		t.cache = map[string][]int32{}
	}
	t.cache[strings.Clone(piece)] = symbols
	t.cacheMu.Unlock()
	for _, id := range symbols {
		out = append(out, int(id))
	}
	return out
}

// DecodeBytes returns the raw bytes ids stand for: byte-level tokens decoded
// to their bytes, added tokens as their content. Special tokens are skipped
// when skipSpecial is set.
func (t *Tokenizer) DecodeBytes(ids []int, skipSpecial bool, dst []byte) []byte {
	for _, id := range ids {
		if id < 0 || id >= len(t.tokens) {
			continue
		}
		if t.added[id] {
			if skipSpecial && t.special[id] {
				continue
			}
			dst = append(dst, t.tokens[id]...)
			continue
		}
		for _, r := range t.tokens[id] {
			if b, ok := runeToByte[r]; ok {
				dst = append(dst, b)
			} else {
				dst = utf8.AppendRune(dst, r)
			}
		}
	}
	return dst
}

// Decode returns the text of ids, invalid UTF-8 as U+FFFD, as
// tokenizer.decode(ids, skip_special_tokens=skipSpecial,
// clean_up_tokenization_spaces=False) does.
func (t *Tokenizer) Decode(ids []int, skipSpecial bool) string {
	return lossy(t.DecodeBytes(ids, skipSpecial, nil))
}

// lossy is String::from_utf8_lossy: each maximal invalid sequence one U+FFFD.
func lossy(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size <= 1 {
			n := invalidLength(b)
			sb.WriteRune(utf8.RuneError)
			b = b[n:]
			continue
		}
		sb.WriteRune(r)
		b = b[size:]
	}
	return sb.String()
}

// invalidLength is how many bytes Rust's from_utf8_lossy replaces with one
// U+FFFD at the start of b: the longest prefix of a valid sequence, at least 1.
func invalidLength(b []byte) int {
	c := b[0]
	var need int
	var lo, hi byte = 0x80, 0xBF
	switch {
	case c >= 0xC2 && c <= 0xDF:
		need = 1
	case c == 0xE0:
		need, lo = 2, 0xA0
	case c >= 0xE1 && c <= 0xEC, c == 0xEE, c == 0xEF:
		need = 2
	case c == 0xED:
		need, hi = 2, 0x9F
	case c == 0xF0:
		need, lo = 3, 0x90
	case c >= 0xF1 && c <= 0xF3:
		need = 3
	case c == 0xF4:
		need, hi = 3, 0x8F
	default:
		return 1
	}
	n := 1
	for i := 0; i < need && n < len(b); i++ {
		x := b[n]
		if i == 0 {
			if x < lo || x > hi {
				return n
			}
		} else if x < 0x80 || x > 0xBF {
			return n
		}
		n++
	}
	return n
}

// The GPT-2 byte-to-unicode table.
var byteToRune [256]rune
var runeToByte = map[rune]byte{}

func init() {
	n := 0
	for b := 0; b < 256; b++ {
		if (b >= '!' && b <= '~') || (b >= 0xA1 && b <= 0xAC) || (b >= 0xAE && b <= 0xFF) {
			byteToRune[b] = rune(b)
		} else {
			byteToRune[b] = rune(256 + n)
			n++
		}
		runeToByte[byteToRune[b]] = byte(b)
	}
}

// DecodeStream decodes generated tokens incrementally, as tokenizers'
// DecodeStream(skip_special_tokens=True) does: text is released once the
// bytes so far no longer end inside a character.
type DecodeStream struct {
	t       *Tokenizer
	pending []byte
}

// NewDecodeStream returns an incremental decoder.
func (t *Tokenizer) NewDecodeStream() *DecodeStream { return &DecodeStream{t: t} }

// Step adds one token and returns the text it completes, "" when none.
func (d *DecodeStream) Step(id int) string {
	d.pending = d.t.DecodeBytes([]int{id}, true, d.pending)
	if len(d.pending) == 0 {
		return ""
	}
	text := lossy(d.pending)
	if strings.HasSuffix(text, "�") {
		return ""
	}
	d.pending = d.pending[:0]
	return text
}

type symbol struct {
	id         int32
	prev, next int32
	alive      bool
}

type candidate struct {
	rank int32
	pos  int32
	id   int32
	// The ids the pair held when pushed; a stale entry no longer matches.
	left, right int32
}

type candidateHeap []candidate

func (h candidateHeap) Len() int { return len(h) }
func (h candidateHeap) Less(i, j int) bool {
	if h[i].rank != h[j].rank {
		return h[i].rank < h[j].rank
	}
	return h[i].pos < h[j].pos
}
func (h candidateHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *candidateHeap) Push(x interface{}) { *h = append(*h, x.(candidate)) }
func (h *candidateHeap) Pop() interface{} {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// mergeWord applies the BPE merges to one pre-tokenized piece, lowest rank
// first and leftmost among equal ranks, as tokenizers' Word::merge_all does.
func (t *Tokenizer) mergeWord(piece string) []int32 {
	n := len(piece)
	syms := make([]symbol, n)
	for i := 0; i < n; i++ {
		syms[i] = symbol{id: t.byteToken[piece[i]], prev: int32(i - 1), next: int32(i + 1), alive: true}
	}
	syms[n-1].next = -1
	h := candidateHeap{}
	push := func(i int32) {
		j := syms[i].next
		if j < 0 {
			return
		}
		key := uint64(uint32(syms[i].id))<<32 | uint64(uint32(syms[j].id))
		if m, ok := t.merges[key]; ok {
			heap.Push(&h, candidate{rank: m.rank, pos: i, id: m.id, left: syms[i].id, right: syms[j].id})
		}
	}
	for i := int32(0); i < int32(n); i++ {
		push(i)
	}
	for h.Len() > 0 {
		c := heap.Pop(&h).(candidate)
		i := c.pos
		if !syms[i].alive || syms[i].id != c.left {
			continue
		}
		j := syms[i].next
		if j < 0 || syms[j].id != c.right {
			continue
		}
		syms[i].id = c.id
		syms[j].alive = false
		syms[i].next = syms[j].next
		if syms[j].next >= 0 {
			syms[syms[j].next].prev = i
		}
		if p := syms[i].prev; p >= 0 {
			push(p)
		}
		push(i)
	}
	out := make([]int32, 0, n)
	for i := int32(0); i >= 0 && int(i) < n; i = syms[i].next {
		out = append(out, syms[i].id)
	}
	return out
}
