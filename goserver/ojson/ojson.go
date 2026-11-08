// Package ojson decodes JSON into ordered values and encodes them as
// Python's json module does, so that request bodies keep the key order and
// number spelling the Python server's templates and answers saw.
//
// Values are nil, bool, int64, *big.Int (integers past int64), float64,
// string, []any and *Object.
package ojson

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxDepth is the deepest arrays and objects nest (json_codec.MAX_DEPTH).
const MaxDepth = 128

// Object is a JSON object that keeps its keys in insertion order. A repeated
// key keeps its first position and its last value, as Python's dict does.
type Object struct {
	keys []string
	vals map[string]any
}

// NewObject returns an empty object.
func NewObject() *Object { return &Object{vals: map[string]any{}} }

// Len is the number of keys.
func (o *Object) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// Keys returns the keys in order.
func (o *Object) Keys() []string {
	if o == nil {
		return nil
	}
	return o.keys
}

// Get returns a key's value.
func (o *Object) Get(key string) (any, bool) {
	if o == nil {
		return nil, false
	}
	v, ok := o.vals[key]
	return v, ok
}

// Value returns a key's value, nil when absent.
func (o *Object) Value(key string) any {
	v, _ := o.Get(key)
	return v
}

// Has reports whether key is present.
func (o *Object) Has(key string) bool {
	_, ok := o.Get(key)
	return ok
}

// Set sets key, appending it when new.
func (o *Object) Set(key string, v any) *Object {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
	return o
}

// Delete removes key.
func (o *Object) Delete(key string) {
	if _, ok := o.vals[key]; !ok {
		return
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			break
		}
	}
}

// Copy returns a shallow copy.
func (o *Object) Copy() *Object {
	c := &Object{keys: append([]string(nil), o.keys...), vals: make(map[string]any, len(o.vals))}
	for k, v := range o.vals {
		c.vals[k] = v
	}
	return c
}

// O builds an object from key, value pairs.
func O(pairs ...any) *Object {
	o := NewObject()
	for i := 0; i+1 < len(pairs); i += 2 {
		o.Set(pairs[i].(string), pairs[i+1])
	}
	return o
}

// Decode parses strict JSON: no NaN or Infinity, at most MaxDepth levels.
func Decode(data []byte) (any, error) {
	d := decoder{data: data}
	d.skip()
	v, err := d.value(0)
	if err != nil {
		return nil, err
	}
	d.skip()
	if d.pos != len(d.data) {
		return nil, d.errorf("extra data")
	}
	return v, nil
}

// DecodeString parses JSON text.
func DecodeString(s string) (any, error) { return Decode([]byte(s)) }

type decoder struct {
	data []byte
	pos  int
}

func (d *decoder) errorf(format string, args ...any) error {
	return fmt.Errorf("invalid JSON at offset %d: %s", d.pos, fmt.Sprintf(format, args...))
}

func (d *decoder) skip() {
	for d.pos < len(d.data) {
		switch d.data[d.pos] {
		case ' ', '\t', '\n', '\r':
			d.pos++
		default:
			return
		}
	}
}

func (d *decoder) value(depth int) (any, error) {
	if d.pos >= len(d.data) {
		return nil, d.errorf("unexpected end")
	}
	switch c := d.data[d.pos]; {
	case c == '{':
		if depth >= MaxDepth {
			return nil, fmt.Errorf("JSON nests deeper than %d levels", MaxDepth)
		}
		d.pos++
		o := NewObject()
		d.skip()
		if d.pos < len(d.data) && d.data[d.pos] == '}' {
			d.pos++
			return o, nil
		}
		for {
			d.skip()
			if d.pos >= len(d.data) || d.data[d.pos] != '"' {
				return nil, d.errorf("expected a key")
			}
			key, err := d.str()
			if err != nil {
				return nil, err
			}
			d.skip()
			if d.pos >= len(d.data) || d.data[d.pos] != ':' {
				return nil, d.errorf("expected ':'")
			}
			d.pos++
			d.skip()
			v, err := d.value(depth + 1)
			if err != nil {
				return nil, err
			}
			o.Set(key, v)
			d.skip()
			if d.pos >= len(d.data) {
				return nil, d.errorf("unexpected end")
			}
			if d.data[d.pos] == ',' {
				d.pos++
				continue
			}
			if d.data[d.pos] == '}' {
				d.pos++
				return o, nil
			}
			return nil, d.errorf("expected ',' or '}'")
		}
	case c == '[':
		if depth >= MaxDepth {
			return nil, fmt.Errorf("JSON nests deeper than %d levels", MaxDepth)
		}
		d.pos++
		list := []any{}
		d.skip()
		if d.pos < len(d.data) && d.data[d.pos] == ']' {
			d.pos++
			return list, nil
		}
		for {
			d.skip()
			v, err := d.value(depth + 1)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
			d.skip()
			if d.pos >= len(d.data) {
				return nil, d.errorf("unexpected end")
			}
			if d.data[d.pos] == ',' {
				d.pos++
				continue
			}
			if d.data[d.pos] == ']' {
				d.pos++
				return list, nil
			}
			return nil, d.errorf("expected ',' or ']'")
		}
	case c == '"':
		return d.str()
	case c == 't':
		return d.literal("true", true)
	case c == 'f':
		return d.literal("false", false)
	case c == 'n':
		return d.literal("null", nil)
	case c == '-' || (c >= '0' && c <= '9'):
		return d.number()
	}
	return nil, d.errorf("unexpected character %q", d.data[d.pos])
}

func (d *decoder) literal(word string, v any) (any, error) {
	if !bytes.HasPrefix(d.data[d.pos:], []byte(word)) {
		return nil, d.errorf("invalid literal")
	}
	d.pos += len(word)
	return v, nil
}

func (d *decoder) number() (any, error) {
	start := d.pos
	if d.data[d.pos] == '-' {
		d.pos++
	}
	digits := func() int {
		n := 0
		for d.pos < len(d.data) && d.data[d.pos] >= '0' && d.data[d.pos] <= '9' {
			d.pos++
			n++
		}
		return n
	}
	if d.pos < len(d.data) && d.data[d.pos] == '0' {
		d.pos++
	} else if digits() == 0 {
		return nil, d.errorf("invalid number")
	}
	isFloat := false
	if d.pos < len(d.data) && d.data[d.pos] == '.' {
		isFloat = true
		d.pos++
		if digits() == 0 {
			return nil, d.errorf("invalid number")
		}
	}
	if d.pos < len(d.data) && (d.data[d.pos] == 'e' || d.data[d.pos] == 'E') {
		isFloat = true
		d.pos++
		if d.pos < len(d.data) && (d.data[d.pos] == '+' || d.data[d.pos] == '-') {
			d.pos++
		}
		if digits() == 0 {
			return nil, d.errorf("invalid number")
		}
	}
	text := string(d.data[start:d.pos])
	if isFloat {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsInf(f, 0) {
			return nil, errors.New("JSON number is outside the supported range")
		}
		return f, nil
	}
	if i, err := strconv.ParseInt(text, 10, 64); err == nil {
		return i, nil
	}
	b, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return nil, d.errorf("invalid number")
	}
	return b, nil
}

func (d *decoder) str() (string, error) {
	d.pos++ // opening quote
	start := d.pos
	// Fast path: no escapes.
	for d.pos < len(d.data) {
		c := d.data[d.pos]
		if c == '"' {
			s := d.data[start:d.pos]
			d.pos++
			if utf8.Valid(s) {
				return string(s), nil
			}
			return strings.ToValidUTF8(string(s), "�"), nil
		}
		if c == '\\' {
			break
		}
		if c < 0x20 {
			return "", d.errorf("control character in string")
		}
		d.pos++
	}
	var sb strings.Builder
	sb.Write(d.data[start:d.pos])
	for {
		if d.pos >= len(d.data) {
			return "", d.errorf("unterminated string")
		}
		c := d.data[d.pos]
		switch {
		case c == '"':
			d.pos++
			s := sb.String()
			if !utf8.ValidString(s) {
				s = strings.ToValidUTF8(s, "�")
			}
			return s, nil
		case c == '\\':
			d.pos++
			if d.pos >= len(d.data) {
				return "", d.errorf("unterminated string")
			}
			e := d.data[d.pos]
			d.pos++
			switch e {
			case '"', '\\', '/':
				sb.WriteByte(e)
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'u':
				r, err := d.hex4()
				if err != nil {
					return "", err
				}
				if utf16.IsSurrogate(r) {
					// A pair spells one character; a lone surrogate, which
					// Python keeps and the tokenizer reads as U+FFFD, is U+FFFD.
					if r < 0xDC00 && d.pos+6 <= len(d.data) && d.data[d.pos] == '\\' && d.data[d.pos+1] == 'u' {
						save := d.pos
						d.pos += 2
						r2, err := d.hex4()
						if err == nil && r2 >= 0xDC00 && r2 <= 0xDFFF {
							sb.WriteRune(utf16.DecodeRune(r, r2))
							continue
						}
						d.pos = save
					}
					sb.WriteRune(utf8.RuneError)
					continue
				}
				sb.WriteRune(r)
			default:
				return "", d.errorf("invalid escape")
			}
		case c < 0x20:
			return "", d.errorf("control character in string")
		default:
			sb.WriteByte(c)
			d.pos++
		}
	}
}

func (d *decoder) hex4() (rune, error) {
	if d.pos+4 > len(d.data) {
		return 0, d.errorf("invalid \\u escape")
	}
	v, err := strconv.ParseUint(string(d.data[d.pos:d.pos+4]), 16, 32)
	if err != nil {
		return 0, d.errorf("invalid \\u escape")
	}
	d.pos += 4
	return rune(v), nil
}

// Options of Dumps, as Python's json.dumps takes them.
type Options struct {
	EnsureASCII bool
	Indent      *int
	ItemSep     string
	KeySep      string
	SortKeys    bool
	// SSESafe escapes U+0085, U+2028 and U+2029 as json_codec does.
	SSESafe bool
}

// Compact is json_codec.dumps: separators (",", ":"), no ASCII escaping.
var Compact = Options{ItemSep: ",", KeySep: ":", SSESafe: true}

// Marshal encodes v compactly, as the server's answers are.
func Marshal(v any) []byte {
	var b bytes.Buffer
	encode(&b, v, Compact, 0)
	return b.Bytes()
}

// MarshalString encodes v compactly as a string.
func MarshalString(v any) string { return string(Marshal(v)) }

// Dumps encodes v as json.dumps does with these options.
func Dumps(v any, opt Options) (string, error) {
	var b bytes.Buffer
	if err := encode(&b, v, opt, 0); err != nil {
		return "", err
	}
	return b.String(), nil
}

// PyDumps is json.dumps(v, ensure_ascii=False): separators ", " and ": ".
func PyDumps(v any) string {
	s, _ := Dumps(v, Options{ItemSep: ", ", KeySep: ": "})
	return s
}

func encode(b *bytes.Buffer, v any, opt Options, level int) error {
	newline := func(l int) {
		if opt.Indent != nil {
			b.WriteByte('\n')
			b.WriteString(strings.Repeat(" ", *opt.Indent*l))
		}
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case uint64:
		b.WriteString(strconv.FormatUint(x, 10))
	case *big.Int:
		b.WriteString(x.String())
	case float32:
		return encode(b, float64(x), opt, level)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return errors.New("value is not JSON serializable")
		}
		b.WriteString(FloatRepr(x))
	case string:
		writeString(b, x, opt)
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return nil
		}
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteString(opt.itemSep())
			}
			newline(level + 1)
			if err := encode(b, item, opt, level+1); err != nil {
				return err
			}
		}
		newline(level)
		b.WriteByte(']')
	case []string:
		list := make([]any, len(x))
		for i, s := range x {
			list[i] = s
		}
		return encode(b, list, opt, level)
	case *Object:
		if x.Len() == 0 {
			b.WriteString("{}")
			return nil
		}
		keys := x.keys
		if opt.SortKeys {
			keys = append([]string(nil), keys...)
			sortStrings(keys)
		}
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(opt.itemSep())
			}
			newline(level + 1)
			writeString(b, k, opt)
			b.WriteString(opt.keySep())
			if err := encode(b, x.vals[k], opt, level+1); err != nil {
				return err
			}
		}
		newline(level)
		b.WriteByte('}')
	case Marshaler:
		return encode(b, x.JSONValue(), opt, level)
	default:
		return fmt.Errorf("value of type %T is not JSON serializable", v)
	}
	return nil
}

// Marshaler is a value that encodes as another.
type Marshaler interface{ JSONValue() any }

func (o Options) itemSep() string {
	if o.ItemSep != "" {
		return o.ItemSep
	}
	if o.Indent != nil {
		return ","
	}
	return ", "
}

func (o Options) keySep() string {
	if o.KeySep != "" {
		return o.KeySep
	}
	return ": "
}

func sortStrings(keys []string) {
	// Python sorts str by code point, which UTF-8 byte order matches.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
}

const hexDigits = "0123456789abcdef"

func writeString(b *bytes.Buffer, s string, opt Options) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				writeU(b, r)
			case opt.EnsureASCII && r > 0x7E:
				if r > 0xFFFF {
					r1, r2 := utf16.EncodeRune(r)
					writeU(b, r1)
					writeU(b, r2)
				} else {
					writeU(b, r)
				}
			case opt.SSESafe && (r == 0x85 || r == 0x2028 || r == 0x2029):
				writeU(b, r)
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

func writeU(b *bytes.Buffer, r rune) {
	b.WriteString(`\u`)
	b.WriteByte(hexDigits[(r>>12)&0xF])
	b.WriteByte(hexDigits[(r>>8)&0xF])
	b.WriteByte(hexDigits[(r>>4)&0xF])
	b.WriteByte(hexDigits[r&0xF])
}

// FloatRepr spells a float as Python's repr does: the shortest digits that
// read back, fixed notation from 1e-4 up to 1e16, else scientific.
func FloatRepr(f float64) string {
	if math.IsInf(f, 1) {
		return "inf"
	}
	if math.IsInf(f, -1) {
		return "-inf"
	}
	if math.IsNaN(f) {
		return "nan"
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde±XX
	sign := ""
	if e[0] == '-' {
		sign, e = "-", e[1:]
	}
	mant, expText, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expText)
	digits := strings.Replace(mant, ".", "", 1)
	if exp >= -5 && exp < 16 {
		// Python switches to scientific below 1e-4: repr(0.0001) is
		// '0.0001', repr(0.00001) is '1e-05'.
		if exp < -4 {
			goto scientific
		}
		if exp >= 0 {
			if len(digits) <= exp+1 {
				return sign + digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
			}
			return sign + digits[:exp+1] + "." + digits[exp+1:]
		}
		return sign + "0." + strings.Repeat("0", -exp-1) + digits
	}
scientific:
	m := digits[:1]
	if len(digits) > 1 {
		m += "." + digits[1:]
	}
	es := "+"
	if exp < 0 {
		es, exp = "-", -exp
	}
	return fmt.Sprintf("%s%se%s%02d", sign, m, es, exp)
}
