package jinja

import (
	"errors"
	"fmt"
	"html"
	"math"
	"strconv"
	"strings"
	"unicode"

	"splash/goserver/ojson"
)

func kw(kwargs *ojson.Object, name string, args []Value, pos int) (Value, bool) {
	if kwargs != nil {
		if v, ok := kwargs.Get(name); ok {
			return v, true
		}
	}
	if pos >= 0 && pos < len(args) {
		return args[pos], true
	}
	return nil, false
}

var stringMethods = map[string]bool{
	"startswith": true, "endswith": true, "strip": true, "lstrip": true, "rstrip": true,
	"split": true, "rsplit": true, "upper": true, "lower": true, "title": true,
	"capitalize": true, "replace": true, "find": true, "rfind": true, "index": true,
	"count": true, "join": true, "splitlines": true, "isdigit": true, "isalpha": true,
	"isspace": true, "isalnum": true, "isupper": true, "islower": true, "format": true,
	"removeprefix": true, "removesuffix": true, "zfill": true, "casefold": true, "swapcase": true,
}
var dictMethods = map[string]bool{"items": true, "keys": true, "values": true, "get": true, "copy": true}
var listMethods = map[string]bool{"index": true, "count": true, "copy": true}

func hasMethod(v Value, name string) bool {
	switch v.(type) {
	case string:
		return stringMethods[name]
	case *ojson.Object:
		return dictMethods[name]
	case []Value, Tuple:
		return listMethods[name] || name == "append" || name == "pop" || name == "extend" || name == "insert"
	}
	return false
}

func stripSet(args []Value) (string, bool) {
	if len(args) == 0 || args[0] == nil {
		return "", false
	}
	return str(args[0]), true
}

func pyStrip(s string, args []Value, left, right bool) string {
	chars, custom := stripSet(args)
	if !custom {
		if left {
			s = strings.TrimLeftFunc(s, unicode.IsSpace)
		}
		if right {
			s = strings.TrimRightFunc(s, unicode.IsSpace)
		}
		return s
	}
	if left {
		s = strings.TrimLeft(s, chars)
	}
	if right {
		s = strings.TrimRight(s, chars)
	}
	return s
}

func pySplit(s string, sep Value, maxsplit int64, fromRight bool) []Value {
	var parts []string
	if sep == nil {
		fields := strings.FieldsFunc(s, unicode.IsSpace)
		if maxsplit >= 0 && int64(len(fields)) > maxsplit+1 {
			// Re-split keeping the remainder whole.
			if fromRight {
				rest := strings.TrimRightFunc(s, unicode.IsSpace)
				var tail []string
				for i := int64(0); i < maxsplit; i++ {
					j := strings.LastIndexFunc(rest, unicode.IsSpace)
					tail = append([]string{rest[j+1:]}, tail...)
					rest = strings.TrimRightFunc(rest[:j+1], unicode.IsSpace)
				}
				fields = append([]string{rest}, tail...)
			} else {
				rest := strings.TrimLeftFunc(s, unicode.IsSpace)
				var head []string
				for i := int64(0); i < maxsplit; i++ {
					j := strings.IndexFunc(rest, unicode.IsSpace)
					head = append(head, rest[:j])
					rest = strings.TrimLeftFunc(rest[j:], unicode.IsSpace)
				}
				fields = append(head, rest)
			}
		}
		parts = fields
	} else {
		sp := str(sep)
		n := -1
		if maxsplit >= 0 {
			n = int(maxsplit) + 1
		}
		if fromRight && n > 0 {
			all := strings.Split(s, sp)
			if len(all) > n {
				head := strings.Join(all[:len(all)-n+1], sp)
				all = append([]string{head}, all[len(all)-n+1:]...)
			}
			parts = all
		} else {
			parts = strings.SplitN(s, sp, n)
		}
	}
	out := make([]Value, len(parts))
	for i, p := range parts {
		out[i] = p
	}
	return out
}

func callMethod(recv Value, name string, args []Value, kwargs *ojson.Object) (Value, error) {
	switch x := recv.(type) {
	case string:
		switch name {
		case "startswith", "endswith":
			if len(args) == 0 {
				return nil, fmt.Errorf("%s needs an argument", name)
			}
			prefixes := []Value{args[0]}
			if t, ok := args[0].(Tuple); ok {
				prefixes = t
			}
			for _, p := range prefixes {
				ps, ok := p.(string)
				if !ok {
					return nil, fmt.Errorf("%s first arg must be str or a tuple of str", name)
				}
				if (name == "startswith" && strings.HasPrefix(x, ps)) || (name == "endswith" && strings.HasSuffix(x, ps)) {
					return true, nil
				}
			}
			return false, nil
		case "strip":
			return pyStrip(x, args, true, true), nil
		case "lstrip":
			return pyStrip(x, args, true, false), nil
		case "rstrip":
			return pyStrip(x, args, false, true), nil
		case "split", "rsplit":
			sep, _ := kw(kwargs, "sep", args, 0)
			max := int64(-1)
			if m, ok := kw(kwargs, "maxsplit", args, 1); ok {
				max, _ = toInt(m)
			}
			return pySplit(x, sep, max, name == "rsplit"), nil
		case "splitlines":
			lines := strings.Split(strings.ReplaceAll(x, "\r\n", "\n"), "\n")
			if len(lines) > 0 && lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
			out := make([]Value, len(lines))
			for i, l := range lines {
				out[i] = l
			}
			return out, nil
		case "upper":
			return strings.ToUpper(x), nil
		case "lower", "casefold":
			return strings.ToLower(x), nil
		case "swapcase":
			return strings.Map(func(r rune) rune {
				if unicode.IsUpper(r) {
					return unicode.ToLower(r)
				}
				return unicode.ToUpper(r)
			}, x), nil
		case "title":
			return title(x), nil
		case "capitalize":
			return capitalize(x), nil
		case "replace":
			if len(args) < 2 {
				return nil, errors.New("replace needs two arguments")
			}
			n := -1
			if len(args) > 2 {
				c, _ := toInt(args[2])
				n = int(c)
			}
			return strings.Replace(x, str(args[0]), str(args[1]), n), nil
		case "find", "rfind", "index":
			if len(args) == 0 {
				return nil, fmt.Errorf("%s needs an argument", name)
			}
			var i int
			if name == "rfind" {
				i = strings.LastIndex(x, str(args[0]))
			} else {
				i = strings.Index(x, str(args[0]))
			}
			if i >= 0 {
				i = len([]rune(x[:i]))
			} else if name == "index" {
				return nil, errors.New("substring not found")
			}
			return int64(i), nil
		case "count":
			if len(args) == 0 {
				return nil, errors.New("count needs an argument")
			}
			return int64(strings.Count(x, str(args[0]))), nil
		case "join":
			if len(args) == 0 {
				return nil, errors.New("join needs an argument")
			}
			items, err := iterate(args[0])
			if err != nil {
				return nil, err
			}
			parts := make([]string, len(items))
			for i, it := range items {
				s, ok := it.(string)
				if !ok {
					return nil, fmt.Errorf("sequence item %d: expected str instance, %s found", i, typeName(it))
				}
				parts[i] = s
			}
			return strings.Join(parts, x), nil
		case "isdigit", "isalpha", "isspace", "isalnum":
			if x == "" {
				return false, nil
			}
			for _, r := range x {
				ok := map[string]bool{
					"isdigit": unicode.IsDigit(r), "isalpha": unicode.IsLetter(r),
					"isspace": unicode.IsSpace(r), "isalnum": unicode.IsLetter(r) || unicode.IsNumber(r),
				}[name]
				if !ok {
					return false, nil
				}
			}
			return true, nil
		case "isupper", "islower":
			cased := false
			for _, r := range x {
				if unicode.IsUpper(r) || unicode.IsLower(r) {
					cased = true
					if (name == "isupper" && unicode.IsLower(r)) || (name == "islower" && unicode.IsUpper(r)) {
						return false, nil
					}
				}
			}
			return cased, nil
		case "removeprefix":
			if len(args) == 0 {
				return x, nil
			}
			return strings.TrimPrefix(x, str(args[0])), nil
		case "removesuffix":
			if len(args) == 0 {
				return x, nil
			}
			return strings.TrimSuffix(x, str(args[0])), nil
		case "zfill":
			w, _ := toInt(args[0])
			for int64(len([]rune(x))) < w {
				x = "0" + x
			}
			return x, nil
		case "format":
			return strFormat(x, args, kwargs)
		}
	case *ojson.Object:
		switch name {
		case "items":
			out := make([]Value, 0, x.Len())
			for _, k := range x.Keys() {
				out = append(out, Tuple{k, x.Value(k)})
			}
			return out, nil
		case "keys":
			out := make([]Value, 0, x.Len())
			for _, k := range x.Keys() {
				out = append(out, k)
			}
			return out, nil
		case "values":
			out := make([]Value, 0, x.Len())
			for _, k := range x.Keys() {
				out = append(out, x.Value(k))
			}
			return out, nil
		case "get":
			if len(args) == 0 {
				return nil, errors.New("get needs a key")
			}
			if k, ok := args[0].(string); ok {
				if v, ok := x.Get(k); ok {
					return v, nil
				}
			}
			if len(args) > 1 {
				return args[1], nil
			}
			return nil, nil
		case "copy":
			return x.Copy(), nil
		}
	case []Value, Tuple:
		items, _ := sequence(x)
		switch name {
		case "index":
			for i, it := range items {
				if len(args) > 0 && equal(it, args[0]) {
					return int64(i), nil
				}
			}
			return nil, errors.New("value is not in list")
		case "count":
			n := 0
			for _, it := range items {
				if len(args) > 0 && equal(it, args[0]) {
					n++
				}
			}
			return int64(n), nil
		case "copy":
			return append([]Value(nil), items...), nil
		default:
			// The immutable sandbox transformers renders in.
			return nil, fmt.Errorf("access to attribute '%s' of '%s' object is unsafe", name, typeName(recv))
		}
	}
	return nil, fmt.Errorf("'%s' object has no attribute '%s'", typeName(recv), name)
}

func strFormat(format string, args []Value, kwargs *ojson.Object) (Value, error) {
	var b strings.Builder
	auto := 0
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c == '{' {
			if i+1 < len(format) && format[i+1] == '{' {
				b.WriteByte('{')
				i++
				continue
			}
			end := strings.IndexByte(format[i:], '}')
			if end < 0 {
				return nil, errors.New("single '{' encountered in format string")
			}
			field := format[i+1 : i+end]
			field, _, _ = strings.Cut(field, ":")
			var v Value
			if field == "" {
				if auto >= len(args) {
					return nil, errors.New("replacement index out of range")
				}
				v = args[auto]
				auto++
			} else if n, err := strconv.Atoi(field); err == nil {
				if n >= len(args) {
					return nil, errors.New("replacement index out of range")
				}
				v = args[n]
			} else if kwargs != nil && kwargs.Has(field) {
				v = kwargs.Value(field)
			} else {
				return nil, fmt.Errorf("KeyError: '%s'", field)
			}
			b.WriteString(str(v))
			i += end
			continue
		}
		if c == '}' && i+1 < len(format) && format[i+1] == '}' {
			i++
		}
		b.WriteByte(c)
	}
	return b.String(), nil
}

func title(s string) string {
	var b strings.Builder
	prevCased := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			if prevCased {
				b.WriteRune(unicode.ToLower(r))
			} else {
				b.WriteRune(unicode.ToTitle(r))
			}
			prevCased = true
		} else {
			b.WriteRune(r)
			prevCased = false
		}
	}
	return b.String()
}

func capitalize(s string) string {
	runes := []rune(strings.ToLower(s))
	if len(runes) > 0 {
		runes[0] = unicode.ToTitle(runes[0])
	}
	return string(runes)
}

func attrPath(v Value, path string) Value {
	for _, part := range strings.Split(path, ".") {
		var next Value
		if n, err := strconv.Atoi(part); err == nil {
			next, _ = getItem(v, int64(n), 0)
		} else {
			next, _ = getItem(v, part, 0)
		}
		v = next
	}
	return v
}

func (r *renderer) applyFilter(name string, v Value, args []Value, kwargs *ojson.Object, line int) (Value, error) {
	fail := func(format string, a ...any) (Value, error) {
		return nil, &Error{Msg: fmt.Sprintf(format, a...), Line: line}
	}
	switch name {
	case "safe", "e", "escape", "forceescape":
		if name == "safe" {
			return v, nil
		}
		return html.EscapeString(str(v)), nil
	case "string":
		return str(v), nil
	case "trim":
		if s, ok := v.(string); ok {
			return pyStrip(s, args, true, true), nil
		}
		return pyStrip(str(v), args, true, true), nil
	case "upper":
		return strings.ToUpper(str(v)), nil
	case "lower":
		return strings.ToLower(str(v)), nil
	case "title":
		return title(str(v)), nil
	case "capitalize":
		return capitalize(str(v)), nil
	case "length", "count":
		n, err := length(v)
		if err != nil {
			return fail("%s", err)
		}
		return int64(n), nil
	case "default", "d":
		def, _ := kw(kwargs, "default_value", args, 0)
		if def == nil && len(args) == 0 && (kwargs == nil || !kwargs.Has("default_value")) {
			def = ""
		}
		boolean, _ := kw(kwargs, "boolean", args, 1)
		if isUndefined(v) || (truthy(boolean) && !truthy(v)) {
			return def, nil
		}
		return v, nil
	case "tojson":
		opt := ojson.Options{}
		if ind, ok := kw(kwargs, "indent", args, 1); ok && ind != nil {
			n, _ := toInt(ind)
			k := int(n)
			opt.Indent = &k
		}
		if ea, ok := kw(kwargs, "ensure_ascii", args, 0); ok {
			opt.EnsureASCII = truthy(ea)
		}
		if sep, ok := kw(kwargs, "separators", args, 2); ok && sep != nil {
			parts, err := iterate(sep)
			if err != nil || len(parts) != 2 {
				return fail("separators must be a pair")
			}
			opt.ItemSep, opt.KeySep = str(parts[0]), str(parts[1])
		}
		if sk, ok := kw(kwargs, "sort_keys", args, 3); ok {
			opt.SortKeys = truthy(sk)
		}
		jv, err := toJSONValue(v)
		if err != nil {
			return fail("%s", err)
		}
		s, err := ojson.Dumps(jv, opt)
		if err != nil {
			return fail("%s", err)
		}
		return s, nil
	case "items":
		if isUndefined(v) {
			return []Value{}, nil
		}
		d, ok := v.(*ojson.Object)
		if !ok {
			return fail("items filter needs a mapping, got %s", typeName(v))
		}
		return callMethod(d, "items", nil, nil)
	case "list":
		items, err := iterate(v)
		if err != nil {
			return fail("%s", err)
		}
		return append([]Value{}, items...), nil
	case "first", "last":
		items, err := iterate(v)
		if err != nil {
			return fail("%s", err)
		}
		if len(items) == 0 {
			return Undefined{name: "no first item"}, nil
		}
		if name == "first" {
			return items[0], nil
		}
		return items[len(items)-1], nil
	case "reverse":
		if s, ok := v.(string); ok {
			runes := []rune(s)
			for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
				runes[i], runes[j] = runes[j], runes[i]
			}
			return string(runes), nil
		}
		items, err := iterate(v)
		if err != nil {
			return fail("%s", err)
		}
		out := make([]Value, len(items))
		for i, it := range items {
			out[len(items)-1-i] = it
		}
		return out, nil
	case "join":
		sep := ""
		if s, ok := kw(kwargs, "d", args, 0); ok {
			sep = str(s)
		}
		items, err := iterate(v)
		if err != nil {
			return fail("%s", err)
		}
		attr, hasAttr := kw(kwargs, "attribute", args, 1)
		parts := make([]string, len(items))
		for i, it := range items {
			if hasAttr {
				it = attrPath(it, str(attr))
			}
			parts[i] = str(it)
		}
		return strings.Join(parts, sep), nil
	case "replace":
		if len(args) < 2 {
			return fail("replace needs two arguments")
		}
		n := -1
		if c, ok := kw(kwargs, "count", args, 2); ok && c != nil {
			k, _ := toInt(c)
			n = int(k)
		}
		return strings.Replace(str(v), str(args[0]), str(args[1]), n), nil
	case "int":
		def := Value(int64(0))
		if d, ok := kw(kwargs, "default", args, 0); ok {
			def = d
		}
		switch x := v.(type) {
		case string:
			s := strings.TrimSpace(x)
			if i, err := strconv.ParseInt(s, 10, 64); err == nil {
				return i, nil
			}
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return int64(f), nil
			}
			return def, nil
		}
		if i, ok := toInt(v); ok {
			return i, nil
		}
		return def, nil
	case "float":
		def := Value(0.0)
		if d, ok := kw(kwargs, "default", args, 0); ok {
			def = d
		}
		if s, ok := v.(string); ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return f, nil
			}
			return def, nil
		}
		if _, f, _, ok := number(v); ok {
			return f, nil
		}
		return def, nil
	case "abs":
		i, f, isInt, ok := number(v)
		if !ok {
			return fail("bad operand type for abs()")
		}
		if isInt {
			if i < 0 {
				return -i, nil
			}
			return i, nil
		}
		return math.Abs(f), nil
	case "round":
		_, f, _, ok := number(v)
		if !ok {
			return fail("round needs a number")
		}
		prec := int64(0)
		if p, ok := kw(kwargs, "precision", args, 0); ok {
			prec, _ = toInt(p)
		}
		method := "common"
		if m, ok := kw(kwargs, "method", args, 1); ok {
			method = str(m)
		}
		scale := math.Pow(10, float64(prec))
		switch method {
		case "ceil":
			return math.Ceil(f*scale) / scale, nil
		case "floor":
			return math.Floor(f*scale) / scale, nil
		}
		return math.Round(f*scale) / scale, nil
	case "indent":
		width := "    "
		if w, ok := kw(kwargs, "width", args, 0); ok {
			if s, isStr := w.(string); isStr {
				width = s
			} else {
				n, _ := toInt(w)
				width = strings.Repeat(" ", int(n))
			}
		}
		first, _ := kw(kwargs, "first", args, 1)
		blank, _ := kw(kwargs, "blank", args, 2)
		s := str(v)
		lines := strings.Split(s, "\n")
		for i := range lines {
			if i == 0 && !truthy(first) {
				continue
			}
			if lines[i] == "" && !truthy(blank) {
				continue
			}
			lines[i] = width + lines[i]
		}
		return strings.Join(lines, "\n"), nil
	case "center":
		w := int64(80)
		if x, ok := kw(kwargs, "width", args, 0); ok {
			w, _ = toInt(x)
		}
		s := str(v)
		n := len([]rune(s))
		if int64(n) >= w {
			return s, nil
		}
		left := (int(w) - n) / 2
		return strings.Repeat(" ", left) + s + strings.Repeat(" ", int(w)-n-left), nil
	case "wordcount":
		return int64(len(strings.Fields(str(v)))), nil
	case "unique":
		items, err := iterate(v)
		if err != nil {
			return fail("%s", err)
		}
		var out []Value
		for _, it := range items {
			dup := false
			for _, o := range out {
				if equal(o, it) {
					dup = true
					break
				}
			}
			if !dup {
				out = append(out, it)
			}
		}
		return out, nil
	case "sum":
		items, err := iterate(v)
		if err != nil {
			return fail("%s", err)
		}
		attr, hasAttr := kw(kwargs, "attribute", args, 0)
		var total Value = int64(0)
		if s, ok := kw(kwargs, "start", args, 1); ok {
			total = s
		}
		for _, it := range items {
			if hasAttr {
				it = attrPath(it, str(attr))
			}
			if total, err = arith("+", total, it); err != nil {
				return fail("%s", err)
			}
		}
		return total, nil
	case "min", "max":
		items, err := iterate(v)
		if err != nil {
			return fail("%s", err)
		}
		if len(items) == 0 {
			return Undefined{}, nil
		}
		best := items[0]
		for _, it := range items[1:] {
			c, err := compare(it, best)
			if err != nil {
				return fail("%s", err)
			}
			if (name == "min" && c < 0) || (name == "max" && c > 0) {
				best = it
			}
		}
		return best, nil
	case "sort":
		items, err := iterate(v)
		if err != nil {
			return fail("%s", err)
		}
		out := append([]Value{}, items...)
		reverse, _ := kw(kwargs, "reverse", args, 0)
		caseSensitive, _ := kw(kwargs, "case_sensitive", args, 1)
		attr, hasAttr := kw(kwargs, "attribute", args, 2)
		key := func(x Value) Value {
			if hasAttr {
				x = attrPath(x, str(attr))
			}
			if s, ok := x.(string); ok && !truthy(caseSensitive) {
				return strings.ToLower(s)
			}
			return x
		}
		if err := sortValues(out, key, truthy(reverse)); err != nil {
			return fail("%s", err)
		}
		return out, nil
	case "dictsort":
		d, ok := v.(*ojson.Object)
		if !ok {
			return fail("dictsort needs a mapping")
		}
		items, _ := callMethod(d, "items", nil, nil)
		list := items.([]Value)
		by := "key"
		if b, ok := kw(kwargs, "by", args, 1); ok {
			by = str(b)
		}
		reverse, _ := kw(kwargs, "reverse", args, 2)
		idx := 0
		if by == "value" {
			idx = 1
		}
		if err := sortValues(list, func(x Value) Value {
			k := x.(Tuple)[idx]
			if s, ok := k.(string); ok {
				return strings.ToLower(s)
			}
			return k
		}, truthy(reverse)); err != nil {
			return fail("%s", err)
		}
		return list, nil
	case "map", "select", "reject", "selectattr", "rejectattr":
		items, err := iterate(v)
		if err != nil {
			return fail("%s", err)
		}
		var out []Value
		for _, it := range items {
			switch name {
			case "map":
				if a, ok := kw(kwargs, "attribute", nil, -1); ok {
					x := attrPath(it, str(a))
					if isUndefined(x) {
						if d, ok := kw(kwargs, "default", nil, -1); ok {
							x = d
						}
					}
					out = append(out, x)
				} else if len(args) > 0 {
					x, err := r.applyFilter(str(args[0]), it, args[1:], nil, line)
					if err != nil {
						return nil, err
					}
					out = append(out, x)
				} else {
					out = append(out, it)
				}
			default:
				subject := it
				rest := args
				if name == "selectattr" || name == "rejectattr" {
					if len(args) == 0 {
						return fail("%s needs an attribute", name)
					}
					subject = attrPath(it, str(args[0]))
					rest = args[1:]
				}
				ok := truthy(subject)
				if len(rest) > 0 {
					if ok, err = r.applyTest(str(rest[0]), subject, rest[1:], line, nil); err != nil {
						return nil, err
					}
				}
				if ok == (name == "select" || name == "selectattr") {
					out = append(out, it)
				}
			}
		}
		if out == nil {
			out = []Value{}
		}
		return out, nil
	case "attr":
		if len(args) == 0 {
			return fail("attr needs a name")
		}
		return getAttr(v, str(args[0]), line)
	case "batch":
		items, err := iterate(v)
		if err != nil {
			return fail("%s", err)
		}
		n, _ := toInt(args[0])
		if n <= 0 {
			return fail("batch size must be positive")
		}
		var out []Value
		for i := 0; i < len(items); i += int(n) {
			end := i + int(n)
			if end > len(items) {
				end = len(items)
			}
			out = append(out, append([]Value{}, items[i:end]...))
		}
		return out, nil
	case "truncate":
		s := []rune(str(v))
		n := int64(255)
		if x, ok := kw(kwargs, "length", args, 0); ok {
			n, _ = toInt(x)
		}
		if int64(len(s)) <= n {
			return string(s), nil
		}
		end := "..."
		if x, ok := kw(kwargs, "end", args, 2); ok {
			end = str(x)
		}
		return string(s[:int(n)-len([]rune(end))]) + end, nil
	case "pprint":
		return repr(v), nil
	case "format":
		return percentFormat(str(v), Tuple(args))
	}
	return fail("no filter named '%s'", name)
}

func (r *renderer) applyTest(name string, v Value, args []Value, line int, fr *frame) (bool, error) {
	arg := func() (Value, error) {
		if len(args) == 0 {
			return nil, &Error{Msg: fmt.Sprintf("test %s needs an argument", name), Line: line}
		}
		return args[0], nil
	}
	switch name {
	case "defined":
		return !isUndefined(v), nil
	case "undefined":
		return isUndefined(v), nil
	case "none":
		return v == nil, nil
	case "string":
		_, ok := v.(string)
		return ok, nil
	case "number":
		return isNumber(v), nil
	case "integer":
		switch v.(type) {
		case int64, int:
			return true, nil
		}
		return false, nil
	case "float":
		_, ok := v.(float64)
		return ok, nil
	case "boolean":
		_, ok := v.(bool)
		return ok, nil
	case "true":
		b, ok := v.(bool)
		return ok && b, nil
	case "false":
		b, ok := v.(bool)
		return ok && !b, nil
	case "mapping":
		_, ok := v.(*ojson.Object)
		return ok, nil
	case "iterable":
		switch v.(type) {
		case string, []Value, Tuple, *ojson.Object, Undefined:
			return true, nil
		}
		return false, nil
	case "sequence":
		switch v.(type) {
		case string, []Value, Tuple, *ojson.Object, Undefined:
			return true, nil
		}
		return false, nil
	case "callable":
		switch v.(type) {
		case Func, *Macro, *method:
			return true, nil
		}
		return false, nil
	case "odd", "even":
		i, ok := toInt(v)
		if !ok {
			return false, &Error{Msg: "odd/even needs an integer", Line: line}
		}
		return (i%2 != 0) == (name == "odd"), nil
	case "divisibleby":
		a, err := arg()
		if err != nil {
			return false, err
		}
		i, _ := toInt(v)
		d, _ := toInt(a)
		return d != 0 && i%d == 0, nil
	case "eq", "equalto", "==", "sameas":
		a, err := arg()
		if err != nil {
			return false, err
		}
		return equal(v, a), nil
	case "ne", "!=":
		a, err := arg()
		if err != nil {
			return false, err
		}
		return !equal(v, a), nil
	case "lt", "<", "le", "<=", "gt", ">", "ge", ">=":
		a, err := arg()
		if err != nil {
			return false, err
		}
		op := map[string]string{"lt": "<", "le": "<=", "gt": ">", "ge": ">="}[name]
		if op == "" {
			op = name
		}
		ok, err := compareOp(op, v, a)
		if err != nil {
			return false, &Error{Msg: err.Error(), Line: line}
		}
		return ok, nil
	case "in":
		a, err := arg()
		if err != nil {
			return false, err
		}
		ok, err := contains(a, v)
		if err != nil {
			return false, &Error{Msg: err.Error(), Line: line}
		}
		return ok, nil
	case "lower":
		s := str(v)
		return s == strings.ToLower(s), nil
	case "upper":
		s := str(v)
		return s == strings.ToUpper(s), nil
	case "escaped":
		return false, nil
	}
	return false, &Error{Msg: fmt.Sprintf("no test named '%s'", name), Line: line}
}
