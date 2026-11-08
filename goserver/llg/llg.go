// Package llg drives llguidance, the grammar engine the Python server
// constrains output with, through its C API. The library is the one the
// Python environment's llguidance wheel ships (llguidance/_lib.abi3.so),
// loaded at run time; the CPython symbols it imports for its Python module
// are stand-ins (pystubs.c) that nothing calls.
package llg

/*
#cgo CFLAGS: -O2
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "llguidance.h"

#define FN(ret, name, args) static ret (*p_##name) args;
FN(LlgTokenizer*, llg_new_tokenizer_v2, (const LlgTokenizerInitV2*, char*, size_t))
FN(void, llg_free_tokenizer, (LlgTokenizer*))
FN(void, llg_constraint_init_set_defaults, (LlgConstraintInit*, const LlgTokenizer*))
FN(LlgMatcher*, llg_new_matcher, (const LlgConstraintInit*, const char*, const char*))
FN(int32_t, llg_validate_grammar, (const LlgConstraintInit*, const char*, const char*, char*, size_t))
FN(const char*, llg_matcher_get_error, (LlgMatcher*))
FN(bool, llg_matcher_is_error, (const LlgMatcher*))
FN(int32_t, llg_matcher_consume_token, (LlgMatcher*, uint32_t))
FN(int32_t, llg_matcher_consume_tokens, (LlgMatcher*, const uint32_t*, size_t))
FN(int32_t, llg_matcher_validate_tokens, (LlgMatcher*, const uint32_t*, size_t))
FN(int32_t, llg_matcher_compute_mask_into, (LlgMatcher*, uint32_t*, size_t))
FN(size_t, llg_matcher_get_mask_byte_size, (const LlgMatcher*))
FN(int32_t, llg_matcher_rollback, (LlgMatcher*, size_t))
FN(bool, llg_matcher_is_stopped, (const LlgMatcher*))
FN(bool, llg_matcher_is_accepting, (LlgMatcher*))
FN(LlgMatcher*, llg_clone_matcher, (const LlgMatcher*))
FN(void, llg_free_matcher, (LlgMatcher*))
FN(const char*, llg_get_version, (void))

#define LOAD(name) p_##name = dlsym(h, #name); if (!p_##name) { snprintf(err, n, "missing %s", #name); return -1; }

static int go_llg_load(const char *path, char *err, size_t n) {
	void *h = dlopen(path, RTLD_NOW | RTLD_LOCAL);
	if (!h) { snprintf(err, n, "%s", dlerror()); return -1; }
	LOAD(llg_new_tokenizer_v2) LOAD(llg_free_tokenizer) LOAD(llg_constraint_init_set_defaults)
	LOAD(llg_new_matcher) LOAD(llg_validate_grammar) LOAD(llg_matcher_get_error) LOAD(llg_matcher_is_error)
	LOAD(llg_matcher_consume_token) LOAD(llg_matcher_consume_tokens) LOAD(llg_matcher_validate_tokens)
	LOAD(llg_matcher_compute_mask_into) LOAD(llg_matcher_get_mask_byte_size) LOAD(llg_matcher_rollback)
	LOAD(llg_matcher_is_stopped) LOAD(llg_matcher_is_accepting) LOAD(llg_clone_matcher)
	LOAD(llg_free_matcher) LOAD(llg_get_version)
	return 0;
}

static const char *json_slices[] = {
	"[\\x20\\x0A\\x0D\\x09]+",
	"[^\"\\\\\\x00-\\x1F\\x7F]{1,10}",
	"[^\"\\\\\\x00-\\x1F\\x7F]{1,30}",
	"[^\"\\\\\\x00-\\x1F\\x7F]+",
	NULL,
};

extern size_t goTokenize(void *user, uint8_t *bytes, size_t n, uint32_t *out, size_t out_len);

static size_t go_tokenize_trampoline(const void *user, const uint8_t *bytes, size_t n, uint32_t *out, size_t out_len) {
	return goTokenize((void *)user, (uint8_t *)bytes, n, out, out_len);
}

static LlgTokenizer *go_new_tokenizer(uint32_t vocab, const char *json, const uint32_t *eos, uint32_t neos, char *err, size_t n) {
	LlgTokenizerInitV2 init;
	memset(&init, 0, sizeof init);
	init.struct_size = sizeof init;
	init.vocab_size = vocab;
	init.tok_eos = eos[0];
	init.tokenizer_json = json;
	init.tokenize_fn = go_tokenize_trampoline;
	init.tokenize_assumes_string = true;
	init.slices = json_slices;
	init.tok_eos_extra = neos > 1 ? eos + 1 : NULL;
	init.tok_eos_extra_count = neos > 1 ? neos - 1 : 0;
	return p_llg_new_tokenizer_v2(&init, err, n);
}

static LlgTokenizer *go_new_tokenizer_bytes(uint32_t vocab, const uint32_t *lens, const uint8_t *bytes, const uint32_t *eos, uint32_t neos, char *err, size_t n) {
	LlgTokenizerInitV2 init;
	memset(&init, 0, sizeof init);
	init.struct_size = sizeof init;
	init.vocab_size = vocab;
	init.tok_eos = eos[0];
	init.token_lens = lens;
	init.token_bytes = bytes;
	init.tokenize_fn = go_tokenize_trampoline;
	init.tokenize_assumes_string = true;
	init.slices = json_slices;
	init.tok_eos_extra = neos > 1 ? eos + 1 : NULL;
	init.tok_eos_extra_count = neos > 1 ? neos - 1 : 0;
	return p_llg_new_tokenizer_v2(&init, err, n);
}

static void go_init(LlgConstraintInit *init, const LlgTokenizer *tok) {
	memset(init, 0, sizeof *init);
	p_llg_constraint_init_set_defaults(init, tok);
	init->log_buffer_level = 0;
	init->log_stderr_level = 0;
}

static LlgMatcher *go_new_matcher(const LlgTokenizer *tok, const char *kind, const char *data) {
	LlgConstraintInit init;
	go_init(&init, tok);
	return p_llg_new_matcher(&init, kind, data);
}

static int32_t go_validate(const LlgTokenizer *tok, const char *kind, const char *data, char *msg, size_t n) {
	LlgConstraintInit init;
	go_init(&init, tok);
	return p_llg_validate_grammar(&init, kind, data, msg, n);
}

static const char *go_get_error(LlgMatcher *m) { return p_llg_matcher_get_error(m); }
static bool go_is_error(LlgMatcher *m) { return p_llg_matcher_is_error(m); }
static int32_t go_consume_token(LlgMatcher *m, uint32_t t) { return p_llg_matcher_consume_token(m, t); }
static int32_t go_consume_tokens(LlgMatcher *m, const uint32_t *t, size_t n) { return p_llg_matcher_consume_tokens(m, t, n); }
static int32_t go_validate_tokens(LlgMatcher *m, const uint32_t *t, size_t n) { return p_llg_matcher_validate_tokens(m, t, n); }
static int32_t go_compute_mask_into(LlgMatcher *m, uint32_t *d, size_t n) { return p_llg_matcher_compute_mask_into(m, d, n); }
static size_t go_mask_byte_size(LlgMatcher *m) { return p_llg_matcher_get_mask_byte_size(m); }
static int32_t go_rollback(LlgMatcher *m, size_t n) { return p_llg_matcher_rollback(m, n); }
static bool go_is_stopped(LlgMatcher *m) { return p_llg_matcher_is_stopped(m); }
static bool go_is_accepting(LlgMatcher *m) { return p_llg_matcher_is_accepting(m); }
static LlgMatcher *go_clone(LlgMatcher *m) { return p_llg_clone_matcher(m); }
static void go_free(LlgMatcher *m) { p_llg_free_matcher(m); }
static const char *go_version(void) { return p_llg_get_version(); }
*/
import "C"

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"unsafe"
)

var (
	loadOnce sync.Once
	loadErr  error
)

// Load loads the llguidance library at path, once.
func Load(path string) error {
	loadOnce.Do(func() {
		cpath := C.CString(path)
		defer C.free(unsafe.Pointer(cpath))
		buf := make([]byte, 1024)
		if C.go_llg_load(cpath, (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf))) != 0 {
			loadErr = errors.New("cannot load llguidance from " + path + ": " + cstr(buf))
		}
	})
	return loadErr
}

// Version is llguidance's version string.
func Version() string { return C.GoString(C.go_version()) }

func cstr(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// Tokenizer is an llguidance tokenizer.
type Tokenizer struct{ p *C.LlgTokenizer }

// Encoder tokenizes text as the server does; llguidance calls it to
// tokenize the grammar's literals.
var encoder func(string) []int

//export goTokenize
func goTokenize(_ unsafe.Pointer, bytes *C.uint8_t, n C.size_t, out *C.uint32_t, outLen C.size_t) C.size_t {
	ids := encoder(string(unsafe.Slice((*byte)(unsafe.Pointer(bytes)), int(n))))
	dst := unsafe.Slice((*uint32)(unsafe.Pointer(out)), int(outLen))
	for i, id := range ids {
		if i >= len(dst) {
			break
		}
		dst[i] = uint32(id)
	}
	return C.size_t(len(ids))
}

// NewTokenizer builds a tokenizer from tokenizer.json text for a vocabulary
// of vocab rows, ending at the eos tokens; encode tokenizes text.
func NewTokenizer(tokenizerJSON string, vocab int, eos []uint32, encode func(string) []int) (*Tokenizer, error) {
	encoder = encode
	if len(eos) == 0 {
		return nil, errors.New("llguidance needs an EOS token")
	}
	cjson := C.CString(tokenizerJSON)
	defer C.free(unsafe.Pointer(cjson))
	ceos := (*C.uint32_t)(C.malloc(C.size_t(4 * len(eos))))
	defer C.free(unsafe.Pointer(ceos))
	copy(unsafe.Slice((*uint32)(unsafe.Pointer(ceos)), len(eos)), eos)
	buf := make([]byte, 4096)
	p := C.go_new_tokenizer(C.uint32_t(vocab), cjson, ceos, C.uint32_t(len(eos)), (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)))
	if p == nil {
		return nil, errors.New("llguidance tokenizer: " + cstr(buf))
	}
	return &Tokenizer{p}, nil
}

// Validate checks a grammar; it returns the error, or "" for none.
func (t *Tokenizer) Validate(kind, data string) string {
	ckind, cdata := C.CString(kind), C.CString(data)
	defer C.free(unsafe.Pointer(ckind))
	defer C.free(unsafe.Pointer(cdata))
	buf := make([]byte, 8192)
	if C.go_validate(t.p, ckind, cdata, (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf))) < 0 {
		return cstr(buf)
	}
	return ""
}

// Matcher is a grammar's state over generated tokens. It is not safe for
// concurrent use.
type Matcher struct{ p *C.LlgMatcher }

func wrap(p *C.LlgMatcher) *Matcher {
	m := &Matcher{p}
	runtime.SetFinalizer(m, func(m *Matcher) { m.Free() })
	return m
}

// NewMatcher compiles a grammar ("lark", "llguidance", "json"...).
func (t *Tokenizer) NewMatcher(kind, data string) *Matcher {
	ckind, cdata := C.CString(kind), C.CString(data)
	defer C.free(unsafe.Pointer(ckind))
	defer C.free(unsafe.Pointer(cdata))
	return wrap(C.go_new_matcher(t.p, ckind, cdata))
}

// Free releases the matcher.
func (m *Matcher) Free() {
	if m.p != nil {
		C.go_free(m.p)
		m.p = nil
	}
}

func (m *Matcher) Clone() *Matcher { return wrap(C.go_clone(m.p)) }
func (m *Matcher) IsError() bool   { return bool(C.go_is_error(m.p)) }
func (m *Matcher) Error() string {
	if s := C.go_get_error(m.p); s != nil {
		return C.GoString(s)
	}
	return ""
}
func (m *Matcher) IsStopped() bool   { return bool(C.go_is_stopped(m.p)) }
func (m *Matcher) IsAccepting() bool { return bool(C.go_is_accepting(m.p)) }
func (m *Matcher) MaskBytes() int    { return int(C.go_mask_byte_size(m.p)) }

func tokensPtr(tokens []uint32) *C.uint32_t {
	if len(tokens) == 0 {
		return nil
	}
	return (*C.uint32_t)(unsafe.Pointer(&tokens[0]))
}

// ConsumeToken advances by one token; false where the grammar refuses it.
func (m *Matcher) ConsumeToken(t uint32) bool { return C.go_consume_token(m.p, C.uint32_t(t)) == 0 }

// ConsumeTokens advances by tokens; false where the grammar refuses them.
func (m *Matcher) ConsumeTokens(tokens []uint32) bool {
	return C.go_consume_tokens(m.p, tokensPtr(tokens), C.size_t(len(tokens))) == 0
}

// ValidateTokens is how many of tokens the grammar accepts in a row.
func (m *Matcher) ValidateTokens(tokens []uint32) int {
	n := int(C.go_validate_tokens(m.p, tokensPtr(tokens), C.size_t(len(tokens))))
	if n < 0 {
		return 0
	}
	return n
}

// ComputeMaskInto writes the allowed-token bitmask into dst, whose length
// is MaskBytes()/4 words.
func (m *Matcher) ComputeMaskInto(dst []uint32) bool {
	return C.go_compute_mask_into(m.p, (*C.uint32_t)(unsafe.Pointer(&dst[0])), C.size_t(4*len(dst))) == 0
}

// Rollback undoes the last n tokens.
func (m *Matcher) Rollback(n int) bool { return C.go_rollback(m.p, C.size_t(n)) == 0 }

// NewTokenizerFromBytes builds a tokenizer from each token's bytes, a
// special token's prefixed with 0xFF, for a vocabulary of len(tokens) rows
// ending at the eos tokens; encode tokenizes text.
func NewTokenizerFromBytes(tokens [][]byte, eos []uint32, encode func(string) []int) (*Tokenizer, error) {
	if len(eos) == 0 {
		return nil, errors.New("llguidance needs an EOS token")
	}
	encoder = encode
	total := 0
	for _, t := range tokens {
		total += len(t)
	}
	lens := (*C.uint32_t)(C.malloc(C.size_t(4 * len(tokens))))
	defer C.free(unsafe.Pointer(lens))
	bytes := (*C.uint8_t)(C.malloc(C.size_t(total + 1)))
	defer C.free(unsafe.Pointer(bytes))
	ls := unsafe.Slice((*uint32)(unsafe.Pointer(lens)), len(tokens))
	bs := unsafe.Slice((*byte)(unsafe.Pointer(bytes)), total+1)
	off := 0
	for i, t := range tokens {
		ls[i] = uint32(len(t))
		off += copy(bs[off:], t)
	}
	ceos := (*C.uint32_t)(C.malloc(C.size_t(4 * len(eos))))
	defer C.free(unsafe.Pointer(ceos))
	copy(unsafe.Slice((*uint32)(unsafe.Pointer(ceos)), len(eos)), eos)
	buf := make([]byte, 4096)
	p := C.go_new_tokenizer_bytes(C.uint32_t(len(tokens)), lens, bytes, ceos, C.uint32_t(len(eos)), (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)))
	if p == nil {
		return nil, errors.New("llguidance tokenizer: " + cstr(buf))
	}
	return &Tokenizer{p}, nil
}
