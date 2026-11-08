package jinja

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"splash/goserver/ojson"
)

// Macro is a template macro.
type Macro struct {
	name     string
	params   []string
	defaults []expr
	body     []node
	r        *renderer
	caller   []node // the body of the call block calling it
	callerFr *frame
}

type frame struct {
	vars   map[string]Value
	parent *frame
}

func (f *frame) lookup(name string) (Value, bool) {
	for fr := f; fr != nil; fr = fr.parent {
		if v, ok := fr.vars[name]; ok {
			return v, true
		}
	}
	return nil, false
}

type loopObj struct {
	index   int
	items   []Value
	depth   int
	recurse func(Value) (string, error)
}

type renderer struct {
	globals   *frame
	rootFrame *frame
	steps     int
}

// Errors that stop loops.
var errBreak = errors.New("break")
var errContinue = errors.New("continue")

// maxSteps bounds a render, as a request's template variables can make a
// template loop for long.
const maxSteps = 50_000_000

// Render renders the template with vars, a mapping of names to values.
func (t *Template) Render(vars map[string]Value) (out string, err error) {
	defer func() {
		if p := recover(); p != nil {
			if e, ok := p.(*Error); ok {
				err = e
				return
			}
			panic(p)
		}
	}()
	globals := &frame{vars: map[string]Value{}}
	r := &renderer{globals: globals}
	for k, v := range builtinGlobals() {
		globals.vars[k] = v
	}
	root := &frame{vars: map[string]Value{}, parent: globals}
	r.rootFrame = root
	for k, v := range vars {
		root.vars[k] = v
	}
	var b strings.Builder
	if err := r.run(t.body, root, &b); err != nil {
		if err == errBreak || err == errContinue {
			return "", &Error{Msg: "break or continue outside a loop"}
		}
		return "", err
	}
	return b.String(), nil
}

func (r *renderer) step() {
	r.steps++
	if r.steps > maxSteps {
		panic(&Error{Msg: "template render exceeded its step limit"})
	}
}

func (r *renderer) run(nodes []node, fr *frame, b *strings.Builder) error {
	for _, n := range nodes {
		r.step()
		switch n := n.(type) {
		case *textNode:
			b.WriteString(n.text)
		case *outputNode:
			v, err := r.eval(n.expr, fr)
			if err != nil {
				return err
			}
			b.WriteString(str(v))
		case *ifNode:
			done := false
			for i, c := range n.conds {
				v, err := r.eval(c, fr)
				if err != nil {
					return err
				}
				if truthy(v) {
					if err := r.run(n.bodies[i], fr, b); err != nil {
						return err
					}
					done = true
					break
				}
			}
			if !done && n.orelse != nil {
				if err := r.run(n.orelse, fr, b); err != nil {
					return err
				}
			}
		case *forNode:
			if err := r.runFor(n, fr, b, 0); err != nil {
				return err
			}
		case *setNode:
			if err := r.runSet(n, fr); err != nil {
				return err
			}
		case *macroNode:
			fr.vars[n.name] = &Macro{name: n.name, params: n.params, defaults: n.defaults, body: n.body, r: r}
		case *callBlockNode:
			call, ok := n.call.(*callExpr)
			if !ok {
				return &Error{Msg: "call block needs a macro call"}
			}
			fn, err := r.eval(call.fn, fr)
			if err != nil {
				return err
			}
			m, ok := fn.(*Macro)
			if !ok {
				return &Error{Msg: "call block needs a macro"}
			}
			mc := *m
			mc.caller, mc.callerFr = n.body, fr
			args, kwargs, err := r.evalArgs(call.args, call.kwargs, fr)
			if err != nil {
				return err
			}
			v, err := r.callMacro(&mc, args, kwargs)
			if err != nil {
				return err
			}
			b.WriteString(str(v))
		case *filterBlockNode:
			var inner strings.Builder
			if err := r.run(n.body, fr, &inner); err != nil {
				return err
			}
			args, kwargs, err := r.evalArgs(n.filter.args, n.filter.kwargs, fr)
			if err != nil {
				return err
			}
			v, err := r.applyFilter(n.filter.name, inner.String(), args, kwargs, n.filter.line)
			if err != nil {
				return err
			}
			b.WriteString(str(v))
		case *scopeNode:
			if err := r.run(n.body, &frame{vars: map[string]Value{}, parent: fr}, b); err != nil {
				return err
			}
		case *breakNode:
			return errBreak
		case *continueNode:
			return errContinue
		}
	}
	return nil
}

func (r *renderer) runFor(n *forNode, fr *frame, b *strings.Builder, depth int) error {
	iterable, err := r.eval(n.iter, fr)
	if err != nil {
		return err
	}
	return r.loopOver(n, iterable, fr, b, depth)
}

func (r *renderer) loopOver(n *forNode, iterable Value, fr *frame, b *strings.Builder, depth int) error {
	items, err := iterate(iterable)
	if err != nil {
		return &Error{Msg: err.Error(), Line: n.line}
	}
	bind := func(f *frame, item Value) error {
		if len(n.targets) == 1 {
			f.vars[n.targets[0]] = item
			return nil
		}
		parts, err := iterate(item)
		if err != nil || len(parts) != len(n.targets) {
			return &Error{Msg: fmt.Sprintf("cannot unpack %s into %d values", typeName(item), len(n.targets)), Line: n.line}
		}
		for i, t := range n.targets {
			f.vars[t] = parts[i]
		}
		return nil
	}
	if n.filter != nil {
		kept := items[:0:0]
		for _, item := range items {
			f := &frame{vars: map[string]Value{}, parent: fr}
			if err := bind(f, item); err != nil {
				return err
			}
			v, err := r.eval(n.filter, f)
			if err != nil {
				return err
			}
			if truthy(v) {
				kept = append(kept, item)
			}
		}
		items = kept
	}
	if len(items) == 0 {
		if n.orelse != nil {
			return r.run(n.orelse, fr, b)
		}
		return nil
	}
	loop := &loopObj{items: items, depth: depth}
	if n.recursive {
		loop.recurse = func(v Value) (string, error) {
			var inner strings.Builder
			if err := r.loopOver(n, v, fr, &inner, depth+1); err != nil {
				return "", err
			}
			return inner.String(), nil
		}
	}
	for i, item := range items {
		loop.index = i
		f := &frame{vars: map[string]Value{"loop": loop}, parent: fr}
		if err := bind(f, item); err != nil {
			return err
		}
		err := r.run(n.body, f, b)
		if err == errBreak {
			break
		}
		if err != nil && err != errContinue {
			return err
		}
	}
	return nil
}

func (r *renderer) runSet(n *setNode, fr *frame) error {
	var v Value
	var err error
	if n.body != nil {
		var inner strings.Builder
		if err := r.run(n.body, &frame{vars: map[string]Value{}, parent: fr}, &inner); err != nil {
			return err
		}
		v = inner.String()
	} else {
		if v, err = r.eval(n.value, fr); err != nil {
			return err
		}
	}
	if len(n.targets) == 0 { // {% do %}
		return nil
	}
	values := []Value{v}
	if len(n.targets) > 1 {
		if values, err = iterate(v); err != nil || len(values) != len(n.targets) {
			return &Error{Msg: "cannot unpack the value set", Line: n.line}
		}
	}
	for i, target := range n.targets {
		switch t := target.(type) {
		case *nameExpr:
			fr.vars[t.name] = values[i]
		case *attrExpr:
			obj, err := r.eval(t.obj, fr)
			if err != nil {
				return err
			}
			ns, ok := obj.(*Namespace)
			if !ok {
				return &Error{Msg: "cannot assign attribute on non-namespace object", Line: n.line}
			}
			ns.attrs.Set(t.attr, values[i])
		default:
			return &Error{Msg: "cannot assign to this target", Line: n.line}
		}
	}
	return nil
}

func (r *renderer) evalArgs(args []expr, kwargs []kwarg, fr *frame) ([]Value, *ojson.Object, error) {
	out := make([]Value, len(args))
	for i, a := range args {
		v, err := r.eval(a, fr)
		if err != nil {
			return nil, nil, err
		}
		out[i] = v
	}
	var kw *ojson.Object
	if len(kwargs) > 0 {
		kw = ojson.NewObject()
		for _, k := range kwargs {
			v, err := r.eval(k.val, fr)
			if err != nil {
				return nil, nil, err
			}
			kw.Set(k.name, v)
		}
	}
	return out, kw, nil
}

func undefinedError(v Undefined, line int) *Error {
	if v.name != "" {
		return &Error{Msg: v.name + " is undefined", Line: line}
	}
	return &Error{Msg: "undefined value", Line: line}
}

func (r *renderer) eval(e expr, fr *frame) (Value, error) {
	r.step()
	switch e := e.(type) {
	case *literal:
		return e.v, nil
	case *nameExpr:
		if v, ok := fr.lookup(e.name); ok {
			return v, nil
		}
		return Undefined{name: "'" + e.name + "'"}, nil
	case *listExpr:
		out := make([]Value, len(e.items))
		for i, item := range e.items {
			v, err := r.eval(item, fr)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case *tupleExpr:
		out := make(Tuple, len(e.items))
		for i, item := range e.items {
			v, err := r.eval(item, fr)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case *dictExpr:
		d := ojson.NewObject()
		for i := range e.keys {
			k, err := r.eval(e.keys[i], fr)
			if err != nil {
				return nil, err
			}
			v, err := r.eval(e.vals[i], fr)
			if err != nil {
				return nil, err
			}
			d.Set(str(k), v)
		}
		return d, nil
	case *attrExpr:
		obj, err := r.eval(e.obj, fr)
		if err != nil {
			return nil, err
		}
		return getAttr(obj, e.attr, e.line)
	case *itemExpr:
		obj, err := r.eval(e.obj, fr)
		if err != nil {
			return nil, err
		}
		key, err := r.eval(e.key, fr)
		if err != nil {
			return nil, err
		}
		return getItem(obj, key, e.line)
	case *sliceExpr:
		obj, err := r.eval(e.obj, fr)
		if err != nil {
			return nil, err
		}
		var parts [3]Value
		for i, p := range []expr{e.start, e.stop, e.step} {
			if p == nil {
				continue
			}
			v, err := r.eval(p, fr)
			if err != nil {
				return nil, err
			}
			parts[i] = v
		}
		v, err := doSlice(obj, parts[0], parts[1], parts[2])
		if err != nil {
			return nil, &Error{Msg: err.Error()}
		}
		return v, nil
	case *callExpr:
		fn, err := r.eval(e.fn, fr)
		if err != nil {
			return nil, err
		}
		args, kwargs, err := r.evalArgs(e.args, e.kwargs, fr)
		if err != nil {
			return nil, err
		}
		return r.call(fn, args, kwargs, e.line, fr)
	case *filterExpr:
		v, err := r.eval(e.value, fr)
		if err != nil {
			return nil, err
		}
		args, kwargs, err := r.evalArgs(e.args, e.kwargs, fr)
		if err != nil {
			return nil, err
		}
		return r.applyFilter(e.name, v, args, kwargs, e.line)
	case *testExpr:
		// "is defined" reads a missing attribute of an undefined value
		// without failing.
		v, err := r.eval(e.value, fr)
		if err != nil {
			var je *Error
			if (e.name == "defined" || e.name == "undefined") && errors.As(err, &je) && !je.Raised {
				v = Undefined{}
			} else {
				return nil, err
			}
		}
		args, _, err := r.evalArgs(e.args, nil, fr)
		if err != nil {
			return nil, err
		}
		ok, err := r.applyTest(e.name, v, args, e.line, fr)
		if err != nil {
			return nil, err
		}
		return ok != e.negate, nil
	case *unaryExpr:
		x, err := r.eval(e.x, fr)
		if err != nil {
			return nil, err
		}
		switch e.op {
		case "not":
			return !truthy(x), nil
		case "-":
			i, f, isInt, ok := number(x)
			if !ok {
				return nil, &Error{Msg: "bad operand type for unary -: '" + typeName(x) + "'", Line: e.line}
			}
			if isInt {
				return -i, nil
			}
			return -f, nil
		case "+":
			if !isNumber(x) {
				return nil, &Error{Msg: "bad operand type for unary +", Line: e.line}
			}
			return x, nil
		}
	case *condExpr:
		c, err := r.eval(e.cond, fr)
		if err != nil {
			return nil, err
		}
		if truthy(c) {
			return r.eval(e.yes, fr)
		}
		if e.no == nil {
			return Undefined{}, nil
		}
		return r.eval(e.no, fr)
	case *concatExpr:
		var b strings.Builder
		for _, p := range e.parts {
			v, err := r.eval(p, fr)
			if err != nil {
				return nil, err
			}
			b.WriteString(str(v))
		}
		return b.String(), nil
	case *compareExpr:
		left, err := r.eval(e.first, fr)
		if err != nil {
			return nil, err
		}
		for i, op := range e.ops {
			right, err := r.eval(e.rest[i], fr)
			if err != nil {
				return nil, err
			}
			ok, err := compareOp(op, left, right)
			if err != nil {
				return nil, &Error{Msg: err.Error(), Line: e.line}
			}
			if !ok {
				return false, nil
			}
			left = right
		}
		return true, nil
	case *binExpr:
		left, err := r.eval(e.left, fr)
		if err != nil {
			return nil, err
		}
		switch e.op {
		case "and":
			if !truthy(left) {
				return left, nil
			}
			return r.eval(e.right, fr)
		case "or":
			if truthy(left) {
				return left, nil
			}
			return r.eval(e.right, fr)
		}
		right, err := r.eval(e.right, fr)
		if err != nil {
			return nil, err
		}
		v, err := arith(e.op, left, right)
		if err != nil {
			return nil, &Error{Msg: err.Error(), Line: e.line}
		}
		return v, nil
	}
	return nil, &Error{Msg: fmt.Sprintf("cannot evaluate %T", e)}
}

func compareOp(op string, a, b Value) (bool, error) {
	switch op {
	case "==":
		return equal(a, b), nil
	case "!=":
		return !equal(a, b), nil
	case "in":
		return contains(b, a)
	case "notin":
		ok, err := contains(b, a)
		return !ok, err
	}
	c, err := compare(a, b)
	if err != nil {
		return false, err
	}
	switch op {
	case "<":
		return c < 0, nil
	case ">":
		return c > 0, nil
	case "<=":
		return c <= 0, nil
	case ">=":
		return c >= 0, nil
	}
	return false, fmt.Errorf("unknown operator %s", op)
}

func arith(op string, a, b Value) (Value, error) {
	if isUndefined(a) || isUndefined(b) {
		u := a
		if !isUndefined(a) {
			u = b
		}
		return nil, undefinedError(u.(Undefined), 0)
	}
	ai, af, aInt, aok := number(a)
	bi, bf, bInt, bok := number(b)
	if aok && bok {
		bothInt := aInt && bInt
		switch op {
		case "+":
			if bothInt {
				return ai + bi, nil
			}
			return af + bf, nil
		case "-":
			if bothInt {
				return ai - bi, nil
			}
			return af - bf, nil
		case "*":
			if bothInt {
				return ai * bi, nil
			}
			return af * bf, nil
		case "/":
			if bf == 0 {
				return nil, errors.New("division by zero")
			}
			return af / bf, nil
		case "//":
			if bf == 0 {
				return nil, errors.New("integer division or modulo by zero")
			}
			if bothInt {
				q := ai / bi
				if (ai%bi != 0) && ((ai < 0) != (bi < 0)) {
					q--
				}
				return q, nil
			}
			return math.Floor(af / bf), nil
		case "%":
			if bf == 0 {
				return nil, errors.New("integer division or modulo by zero")
			}
			if bothInt {
				m := ai % bi
				if m != 0 && ((m < 0) != (bi < 0)) {
					m += bi
				}
				return m, nil
			}
			m := math.Mod(af, bf)
			if m != 0 && ((m < 0) != (bf < 0)) {
				m += bf
			}
			return m, nil
		case "**":
			if bothInt && bi >= 0 {
				return new(big.Int).Exp(big.NewInt(ai), big.NewInt(bi), nil).Int64(), nil
			}
			return math.Pow(af, bf), nil
		}
	}
	switch x := a.(type) {
	case string:
		switch op {
		case "+":
			if y, ok := b.(string); ok {
				return x + y, nil
			}
		case "*":
			if n, ok := b.(int64); ok {
				if n < 0 {
					n = 0
				}
				return strings.Repeat(x, int(n)), nil
			}
		case "%":
			return percentFormat(x, b)
		}
	case []Value:
		if op == "+" {
			if y, ok := b.([]Value); ok {
				out := make([]Value, 0, len(x)+len(y))
				return append(append(out, x...), y...), nil
			}
		}
		if op == "*" {
			if n, ok := b.(int64); ok {
				var out []Value
				for i := int64(0); i < n; i++ {
					out = append(out, x...)
				}
				return out, nil
			}
		}
	case Tuple:
		if op == "+" {
			if y, ok := b.(Tuple); ok {
				out := make(Tuple, 0, len(x)+len(y))
				return append(append(out, x...), y...), nil
			}
		}
	}
	if op == "*" {
		if _, ok := b.(string); ok && aInt {
			return arith(op, b, a)
		}
	}
	return nil, fmt.Errorf("unsupported operand type(s) for %s: '%s' and '%s'", op, typeName(a), typeName(b))
}

// percentFormat is Python's printf-style "format % args" for %s, %d, %r, %f.
func percentFormat(format string, args Value) (Value, error) {
	list, ok := args.(Tuple)
	if !ok {
		list = Tuple{args}
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(format) {
			return nil, errors.New("incomplete format")
		}
		spec := ""
		for i < len(format) && strings.IndexByte("0123456789.-+ ", format[i]) >= 0 {
			spec += string(format[i])
			i++
		}
		if i >= len(format) {
			return nil, errors.New("incomplete format")
		}
		verb := format[i]
		if verb == '%' {
			b.WriteByte('%')
			continue
		}
		if n >= len(list) {
			return nil, errors.New("not enough arguments for format string")
		}
		arg := list[n]
		n++
		switch verb {
		case 's':
			b.WriteString(fmt.Sprintf("%"+spec+"s", str(arg)))
		case 'r':
			b.WriteString(fmt.Sprintf("%"+spec+"s", repr(arg)))
		case 'd', 'i':
			iv, ok := toInt(arg)
			if !ok {
				return nil, errors.New("%d format: a number is required")
			}
			b.WriteString(fmt.Sprintf("%"+spec+"d", iv))
		case 'f', 'e', 'g':
			_, f, _, ok := number(arg)
			if !ok {
				return nil, errors.New("must be real number")
			}
			if !strings.Contains(spec, ".") && verb == 'f' {
				spec += ".6"
			}
			b.WriteString(fmt.Sprintf("%"+spec+string(verb), f))
		default:
			return nil, fmt.Errorf("unsupported format character '%c'", verb)
		}
	}
	return b.String(), nil
}

func getAttr(obj Value, attr string, line int) (Value, error) {
	switch x := obj.(type) {
	case Undefined:
		return nil, undefinedError(x, line)
	case *ojson.Object:
		if hasMethod(obj, attr) {
			return &method{obj, attr}, nil
		}
		if v, ok := x.Get(attr); ok {
			return v, nil
		}
		return Undefined{name: "'dict object' has no attribute '" + attr + "'"}, nil
	case *Namespace:
		if v, ok := x.attrs.Get(attr); ok {
			return v, nil
		}
		return Undefined{name: "'Namespace' has no attribute '" + attr + "'"}, nil
	case *loopObj:
		return loopAttr(x, attr)
	}
	if hasMethod(obj, attr) {
		return &method{obj, attr}, nil
	}
	return Undefined{name: fmt.Sprintf("'%s object' has no attribute '%s'", typeName(obj), attr)}, nil
}

func getItem(obj, key Value, line int) (Value, error) {
	switch x := obj.(type) {
	case Undefined:
		return nil, undefinedError(x, line)
	case *ojson.Object:
		if k, ok := key.(string); ok {
			if v, ok := x.Get(k); ok {
				return v, nil
			}
			if hasMethod(obj, k) {
				return &method{obj, k}, nil
			}
		}
		return Undefined{name: "'dict object' has no attribute " + repr(key)}, nil
	case []Value, Tuple:
		items, _ := sequence(x)
		i, ok := key.(int64)
		if !ok {
			if b, isBool := key.(bool); isBool {
				i = map[bool]int64{true: 1, false: 0}[b]
			} else {
				return Undefined{name: "list index must be an integer"}, nil
			}
		}
		if idx, ok := pyIndex(i, len(items)); ok {
			return items[idx], nil
		}
		return Undefined{name: "list index out of range"}, nil
	case string:
		i, ok := key.(int64)
		if !ok {
			if k, ok := key.(string); ok && hasMethod(obj, k) {
				return &method{obj, k}, nil
			}
			return Undefined{}, nil
		}
		runes := []rune(x)
		if idx, ok := pyIndex(i, len(runes)); ok {
			return string(runes[idx]), nil
		}
		return Undefined{name: "string index out of range"}, nil
	case *Namespace:
		if k, ok := key.(string); ok {
			return getAttr(x, k, line)
		}
	case *loopObj:
		if k, ok := key.(string); ok {
			return loopAttr(x, k)
		}
	}
	return Undefined{}, nil
}

func loopAttr(l *loopObj, attr string) (Value, error) {
	n := len(l.items)
	switch attr {
	case "index":
		return int64(l.index + 1), nil
	case "index0":
		return int64(l.index), nil
	case "revindex":
		return int64(n - l.index), nil
	case "revindex0":
		return int64(n - l.index - 1), nil
	case "first":
		return l.index == 0, nil
	case "last":
		return l.index == n-1, nil
	case "length":
		return int64(n), nil
	case "depth":
		return int64(l.depth + 1), nil
	case "depth0":
		return int64(l.depth), nil
	case "previtem":
		if l.index == 0 {
			return Undefined{name: "there is no previous item"}, nil
		}
		return l.items[l.index-1], nil
	case "nextitem":
		if l.index == n-1 {
			return Undefined{name: "there is no next item"}, nil
		}
		return l.items[l.index+1], nil
	case "cycle":
		return Func(func(args []Value, _ *ojson.Object) (Value, error) {
			if len(args) == 0 {
				return nil, errors.New("no items for cycling given")
			}
			return args[l.index%len(args)], nil
		}), nil
	}
	return Undefined{name: "'loop' has no attribute '" + attr + "'"}, nil
}

func (r *renderer) call(fn Value, args []Value, kwargs *ojson.Object, line int, fr *frame) (Value, error) {
	switch f := fn.(type) {
	case Func:
		v, err := f(args, kwargs)
		if err != nil {
			var je *Error
			if errors.As(err, &je) {
				if je.Line == 0 && !je.Raised {
					je.Line = line
				}
				return nil, je
			}
			return nil, &Error{Msg: err.Error(), Line: line}
		}
		return v, nil
	case *Macro:
		return r.callMacro(f, args, kwargs)
	case *method:
		v, err := callMethod(f.recv, f.name, args, kwargs)
		if err != nil {
			return nil, &Error{Msg: err.Error(), Line: line}
		}
		return v, nil
	case *loopObj:
		if f.recurse == nil || len(args) != 1 {
			return nil, &Error{Msg: "loop is not recursive", Line: line}
		}
		return f.recurse(args[0])
	case Undefined:
		return nil, undefinedError(f, line)
	}
	return nil, &Error{Msg: fmt.Sprintf("'%s' object is not callable", typeName(fn)), Line: line}
}

func (r *renderer) callMacro(m *Macro, args []Value, kwargs *ojson.Object) (Value, error) {
	fr := &frame{vars: map[string]Value{}, parent: r.globalsChild()}
	var extra []Value
	for i, a := range args {
		if i < len(m.params) {
			fr.vars[m.params[i]] = a
		} else {
			extra = append(extra, a)
		}
	}
	extraKw := ojson.NewObject()
	if kwargs != nil {
		for _, k := range kwargs.Keys() {
			known := false
			for _, p := range m.params {
				if p == k {
					known = true
				}
			}
			if known {
				fr.vars[k] = kwargs.Value(k)
			} else {
				extraKw.Set(k, kwargs.Value(k))
			}
		}
	}
	for i, p := range m.params {
		if _, ok := fr.vars[p]; ok {
			continue
		}
		if m.defaults[i] != nil {
			v, err := r.eval(m.defaults[i], fr)
			if err != nil {
				return nil, err
			}
			fr.vars[p] = v
		} else {
			fr.vars[p] = Undefined{name: "'" + p + "'"}
		}
	}
	fr.vars["varargs"] = Tuple(extra)
	fr.vars["kwargs"] = extraKw
	if m.caller != nil {
		body, cfr := m.caller, m.callerFr
		fr.vars["caller"] = Func(func(_ []Value, _ *ojson.Object) (Value, error) {
			var b strings.Builder
			if err := r.run(body, &frame{vars: map[string]Value{}, parent: cfr}, &b); err != nil {
				return nil, err
			}
			return b.String(), nil
		})
	}
	var b strings.Builder
	if err := r.run(m.body, fr, &b); err != nil {
		return nil, err
	}
	return b.String(), nil
}

// globalsChild is the frame a macro's own frame descends from: the template's
// top level, whose names it reads as they are when it is called.
func (r *renderer) globalsChild() *frame { return r.root() }

func (r *renderer) root() *frame {
	return r.rootFrame
}

func builtinGlobals() map[string]Value {
	return map[string]Value{
		"raise_exception": Func(func(args []Value, _ *ojson.Object) (Value, error) {
			msg := ""
			if len(args) > 0 {
				msg = str(args[0])
			}
			return nil, &Error{Msg: msg, Raised: true}
		}),
		"strftime_now": Func(func(args []Value, _ *ojson.Object) (Value, error) {
			if len(args) == 0 {
				return nil, errors.New("strftime_now needs a format")
			}
			return strftime(time.Now(), str(args[0])), nil
		}),
		"range": Func(func(args []Value, _ *ojson.Object) (Value, error) {
			var start, stop, step int64 = 0, 0, 1
			ints := make([]int64, len(args))
			for i, a := range args {
				v, ok := a.(int64)
				if !ok {
					return nil, errors.New("range() arguments must be integers")
				}
				ints[i] = v
			}
			switch len(ints) {
			case 1:
				stop = ints[0]
			case 2:
				start, stop = ints[0], ints[1]
			case 3:
				start, stop, step = ints[0], ints[1], ints[2]
			default:
				return nil, errors.New("range expected 1 to 3 arguments")
			}
			if step == 0 {
				return nil, errors.New("range() arg 3 must not be zero")
			}
			var out []Value
			for i := start; (step > 0 && i < stop) || (step < 0 && i > stop); i += step {
				out = append(out, i)
				if len(out) > 1_000_000 {
					return nil, errors.New("range too large")
				}
			}
			return out, nil
		}),
		"namespace": Func(func(args []Value, kwargs *ojson.Object) (Value, error) {
			ns := &Namespace{attrs: ojson.NewObject()}
			for _, a := range args {
				if d, ok := a.(*ojson.Object); ok {
					for _, k := range d.Keys() {
						ns.attrs.Set(k, d.Value(k))
					}
				}
			}
			if kwargs != nil {
				for _, k := range kwargs.Keys() {
					ns.attrs.Set(k, kwargs.Value(k))
				}
			}
			return ns, nil
		}),
		"dict": Func(func(args []Value, kwargs *ojson.Object) (Value, error) {
			d := ojson.NewObject()
			for _, a := range args {
				if src, ok := a.(*ojson.Object); ok {
					for _, k := range src.Keys() {
						d.Set(k, src.Value(k))
					}
				}
			}
			if kwargs != nil {
				for _, k := range kwargs.Keys() {
					d.Set(k, kwargs.Value(k))
				}
			}
			return d, nil
		}),
	}
}

func strftime(t time.Time, format string) string {
	repl := map[byte]string{
		'Y': t.Format("2006"), 'm': t.Format("01"), 'd': t.Format("02"),
		'H': t.Format("15"), 'M': t.Format("04"), 'S': t.Format("05"),
		'y': t.Format("06"), 'b': t.Format("Jan"), 'B': t.Format("January"),
		'a': t.Format("Mon"), 'A': t.Format("Monday"), 'p': t.Format("PM"),
		'I': t.Format("03"), 'j': fmt.Sprintf("%03d", t.YearDay()), 'Z': t.Format("MST"),
		'z': t.Format("-0700"), 'e': fmt.Sprintf("%2d", t.Day()), '%': "%",
	}
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] == '%' && i+1 < len(format) {
			if s, ok := repl[format[i+1]]; ok {
				b.WriteString(s)
				i++
				continue
			}
			if format[i+1] == '-' && i+2 < len(format) {
				if s, ok := repl[format[i+2]]; ok {
					b.WriteString(strings.TrimLeft(s, "0 "))
					i += 2
					continue
				}
			}
		}
		b.WriteByte(format[i])
	}
	return b.String()
}
