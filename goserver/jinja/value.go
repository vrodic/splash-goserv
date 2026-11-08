package jinja

import (
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"splash/goserver/ojson"
)

// Value is a template value: nil (None), bool, int64, float64, *big.Int,
// string, []Value (list), Tuple, *ojson.Object (dict), *Namespace, *Macro,
// Func, *loopObj, Undefined or *method.
type Value = any

// Tuple is a Python tuple.
type Tuple []Value

// Undefined is a name or attribute that does not exist.
type Undefined struct{ name string }

// Namespace is the object namespace() returns, whose attributes set may set.
type Namespace struct{ attrs *ojson.Object }

// Func is a callable global.
type Func func(args []Value, kwargs *ojson.Object) (Value, error)

// method is a value's bound method.
type method struct {
	recv Value
	name string
}

func isUndefined(v Value) bool {
	_, ok := v.(Undefined)
	return ok
}

// truthy is Python's bool(value).
func truthy(v Value) bool {
	switch x := v.(type) {
	case nil:
		return false
	case Undefined:
		return false
	case bool:
		return x
	case int64:
		return x != 0
	case int:
		return x != 0
	case float64:
		return x != 0
	case *big.Int:
		return x.Sign() != 0
	case string:
		return x != ""
	case []Value:
		return len(x) > 0
	case Tuple:
		return len(x) > 0
	case *ojson.Object:
		return x.Len() > 0
	}
	return true
}

// number returns v as a float or an int when it is one (bools are ints).
func number(v Value) (i int64, f float64, isInt, ok bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, 1, true, true
		}
		return 0, 0, true, true
	case int64:
		return x, float64(x), true, true
	case int:
		return int64(x), float64(x), true, true
	case float64:
		return 0, x, false, true
	case *big.Int:
		f, _ := new(big.Float).SetInt(x).Float64()
		if x.IsInt64() {
			return x.Int64(), f, true, true
		}
		return 0, f, false, true
	}
	return 0, 0, false, false
}

func isNumber(v Value) bool {
	_, _, _, ok := number(v)
	return ok
}

// equal is Python's ==.
func equal(a, b Value) bool {
	if ai, af, aInt, aok := number(a); aok {
		if bi, bf, bInt, bok := number(b); bok {
			if aInt && bInt {
				return ai == bi
			}
			return af == bf
		}
		return false
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case Undefined:
		return isUndefined(b)
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []Value:
		y, ok := b.([]Value)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case Tuple:
		y, ok := b.(Tuple)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case *ojson.Object:
		y, ok := b.(*ojson.Object)
		if !ok || x.Len() != y.Len() {
			return false
		}
		for _, k := range x.Keys() {
			yv, ok := y.Get(k)
			if !ok || !equal(x.Value(k), yv) {
				return false
			}
		}
		return true
	}
	return a == b
}

// compare is Python's ordering of numbers, strings and sequences.
func compare(a, b Value) (int, error) {
	if ai, af, aInt, aok := number(a); aok {
		if bi, bf, bInt, bok := number(b); bok {
			if aInt && bInt {
				switch {
				case ai < bi:
					return -1, nil
				case ai > bi:
					return 1, nil
				}
				return 0, nil
			}
			switch {
			case af < bf:
				return -1, nil
			case af > bf:
				return 1, nil
			}
			return 0, nil
		}
	}
	if x, ok := a.(string); ok {
		if y, ok := b.(string); ok {
			return strings.Compare(x, y), nil
		}
	}
	xs, xok := sequence(a)
	ys, yok := sequence(b)
	if xok && yok {
		for i := 0; i < len(xs) && i < len(ys); i++ {
			if !equal(xs[i], ys[i]) {
				return compare(xs[i], ys[i])
			}
		}
		return len(xs) - len(ys), nil
	}
	return 0, fmt.Errorf("'<' not supported between instances of '%s' and '%s'", typeName(a), typeName(b))
}

func sequence(v Value) ([]Value, bool) {
	switch x := v.(type) {
	case []Value:
		return x, true
	case Tuple:
		return x, true
	}
	return nil, false
}

func typeName(v Value) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int64, int, *big.Int:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []Value:
		return "list"
	case Tuple:
		return "tuple"
	case *ojson.Object:
		return "dict"
	case Undefined:
		return "Undefined"
	case *Namespace:
		return "Namespace"
	case *Macro:
		return "Macro"
	case *loopObj:
		return "LoopContext"
	}
	return "object"
}

// str is Python's str(value), as a template prints it.
func str(v Value) string {
	switch x := v.(type) {
	case string:
		return x
	case Undefined:
		return ""
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case *big.Int:
		return x.String()
	case float64:
		return ojson.FloatRepr(x)
	}
	return repr(v)
}

// repr is Python's repr(value).
func repr(v Value) string {
	switch x := v.(type) {
	case string:
		return reprString(x)
	case []Value:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = repr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case Tuple:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = repr(item)
		}
		if len(parts) == 1 {
			return "(" + parts[0] + ",)"
		}
		return "(" + strings.Join(parts, ", ") + ")"
	case *ojson.Object:
		parts := make([]string, 0, x.Len())
		for _, k := range x.Keys() {
			parts = append(parts, reprString(k)+": "+repr(x.Value(k)))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case *Namespace:
		return "<Namespace " + repr(x.attrs) + ">"
	case Undefined:
		return "Undefined"
	case *Macro:
		return "<Macro '" + x.name + "'>"
	}
	return str(v)
}

func reprString(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r < 0xa0:
			fmt.Fprintf(&b, `\x%02x`, r)
		case !unicode.IsPrint(r) && r != ' ':
			if r > 0xFFFF {
				fmt.Fprintf(&b, `\U%08x`, r)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// toJSONValue converts a template value for ojson: tuples become lists.
func toJSONValue(v Value) (any, error) {
	switch x := v.(type) {
	case nil, bool, int64, float64, string, *big.Int:
		return x, nil
	case int:
		return int64(x), nil
	case Tuple:
		return toJSONValue([]Value(x))
	case []Value:
		out := make([]any, len(x))
		for i, item := range x {
			c, err := toJSONValue(item)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case *ojson.Object:
		out := ojson.NewObject()
		for _, k := range x.Keys() {
			c, err := toJSONValue(x.Value(k))
			if err != nil {
				return nil, err
			}
			out.Set(k, c)
		}
		return out, nil
	}
	return nil, fmt.Errorf("Object of type %s is not JSON serializable", typeName(v))
}

// FromJSON converts an ojson value to a template value.
func FromJSON(v any) Value {
	switch x := v.(type) {
	case []any:
		out := make([]Value, len(x))
		for i, item := range x {
			out[i] = FromJSON(item)
		}
		return out
	case *ojson.Object:
		out := ojson.NewObject()
		for _, k := range x.Keys() {
			out.Set(k, FromJSON(x.Value(k)))
		}
		return out
	case int:
		return int64(x)
	}
	return v
}

// iterate is Python's iter(value) as a for loop reads it.
func iterate(v Value) ([]Value, error) {
	switch x := v.(type) {
	case []Value:
		return x, nil
	case Tuple:
		return x, nil
	case *ojson.Object:
		keys := x.Keys()
		out := make([]Value, len(keys))
		for i, k := range keys {
			out[i] = k
		}
		return out, nil
	case string:
		out := make([]Value, 0, len(x))
		for _, r := range x {
			out = append(out, string(r))
		}
		return out, nil
	case Undefined, nil:
		if x == nil {
			return nil, fmt.Errorf("'NoneType' object is not iterable")
		}
		return nil, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", typeName(v))
}

func length(v Value) (int, error) {
	switch x := v.(type) {
	case string:
		return utf8.RuneCountInString(x), nil
	case []Value:
		return len(x), nil
	case Tuple:
		return len(x), nil
	case *ojson.Object:
		return x.Len(), nil
	case Undefined:
		return 0, nil
	}
	return 0, fmt.Errorf("object of type '%s' has no len()", typeName(v))
}

func contains(container, item Value) (bool, error) {
	switch x := container.(type) {
	case string:
		s, ok := item.(string)
		if !ok {
			return false, fmt.Errorf("'in <string>' requires string as left operand, not %s", typeName(item))
		}
		return strings.Contains(x, s), nil
	case *ojson.Object:
		s, ok := item.(string)
		return ok && x.Has(s), nil
	case Undefined:
		return false, nil
	}
	items, err := iterate(container)
	if err != nil {
		return false, fmt.Errorf("argument of type '%s' is not iterable", typeName(container))
	}
	for _, it := range items {
		if equal(it, item) {
			return true, nil
		}
	}
	return false, nil
}

func pyIndex(i int64, n int) (int, bool) {
	if i < 0 {
		i += int64(n)
	}
	if i < 0 || i >= int64(n) {
		return 0, false
	}
	return int(i), true
}

func sliceBounds(n int, start, stop, step Value) (int, int, int, error) {
	st := 1
	if step != nil {
		s, ok := step.(int64)
		if !ok || s == 0 {
			return 0, 0, 0, fmt.Errorf("slice step must be a nonzero integer")
		}
		st = int(s)
	}
	clamp := func(v Value, def int) (int, error) {
		if v == nil {
			return def, nil
		}
		i, ok := v.(int64)
		if !ok {
			return 0, fmt.Errorf("slice indices must be integers")
		}
		x := int(i)
		if x < 0 {
			x += n
			if x < 0 {
				if st < 0 {
					x = -1
				} else {
					x = 0
				}
			}
		} else if x >= n {
			if st < 0 {
				x = n - 1
			} else {
				x = n
			}
		}
		return x, nil
	}
	var a, b int
	var err error
	if st > 0 {
		if a, err = clamp(start, 0); err != nil {
			return 0, 0, 0, err
		}
		if b, err = clamp(stop, n); err != nil {
			return 0, 0, 0, err
		}
	} else {
		if a, err = clamp(start, n-1); err != nil {
			return 0, 0, 0, err
		}
		if b, err = clamp(stop, -1); err != nil {
			return 0, 0, 0, err
		}
		if stop == nil {
			b = -1
		}
	}
	return a, b, st, nil
}

func sliceIndexes(n int, start, stop, step Value) ([]int, error) {
	a, b, st, err := sliceBounds(n, start, stop, step)
	if err != nil {
		return nil, err
	}
	var idx []int
	if st > 0 {
		for i := a; i < b; i += st {
			idx = append(idx, i)
		}
	} else {
		for i := a; i > b; i += st {
			idx = append(idx, i)
		}
	}
	return idx, nil
}

func doSlice(v Value, start, stop, step Value) (Value, error) {
	switch x := v.(type) {
	case string:
		runes := []rune(x)
		idx, err := sliceIndexes(len(runes), start, stop, step)
		if err != nil {
			return nil, err
		}
		out := make([]rune, len(idx))
		for i, j := range idx {
			out[i] = runes[j]
		}
		return string(out), nil
	case []Value, Tuple:
		items, _ := sequence(x)
		idx, err := sliceIndexes(len(items), start, stop, step)
		if err != nil {
			return nil, err
		}
		out := make([]Value, len(idx))
		for i, j := range idx {
			out[i] = items[j]
		}
		if _, ok := x.(Tuple); ok {
			return Tuple(out), nil
		}
		return out, nil
	case Undefined:
		return x, nil
	}
	return nil, fmt.Errorf("'%s' object is not subscriptable", typeName(v))
}

// sortValues sorts in place by Python's ordering.
func sortValues(items []Value, key func(Value) Value, reverse bool) error {
	var failure error
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if key != nil {
			a, b = key(a), key(b)
		}
		c, err := compare(a, b)
		if err != nil && failure == nil {
			failure = err
		}
		if reverse {
			return c > 0
		}
		return c < 0
	})
	return failure
}

func toInt(v Value) (int64, bool) {
	i, f, isInt, ok := number(v)
	if !ok {
		return 0, false
	}
	if isInt {
		return i, true
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return int64(f), true
}
