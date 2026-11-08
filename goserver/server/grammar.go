package server

import (
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"

	"splash/goserver/ojson"
)

// The grammar builders of server/tool_schema.py, which write the same
// llguidance grammars byte for byte (goserver/parity/grammar_parity.py).

const (
	maxSchemaDepth       = 64
	maxFramedSchemaBytes = 16 * 1024 * 1024
	maxGrammarBound      = 64
	whitespacePattern    = `[ \t\n\r]{0,64}`
	whitespaceRule       = "WS: /" + whitespacePattern + "/"
	rawValue             = "/(?s:.*)/"
)

var nameRegex = larkRegex(`[^<> \t\n\r]([^<>\n\r]{0,254}[^<> \t\n\r])?`)

var schemaAnnotations = setOf("$comment", "title", "description", "default", "examples", "deprecated", "readOnly", "writeOnly")
var schemaIdentifiers = setOf("$schema", "$id", "$anchor", "$dynamicAnchor", "$defs", "definitions")
var inPlaceApplicators = setOf("allOf", "not", "if", "then", "else", "dependentSchemas", "dependencies", "extends", "$dynamicRef", "$recursiveRef")
var moreProperties = setOf("additionalProperties", "unevaluatedProperties", "patternProperties")
var schemaMapKeywords = setOf("properties", "patternProperties", "$defs", "definitions", "dependentSchemas")
var subschemaKeywords = setOf("items", "prefixItems", "additionalItems", "contains", "additionalProperties",
	"unevaluatedItems", "unevaluatedProperties", "propertyNames", "allOf", "anyOf", "oneOf", "not", "if", "then",
	"else", "contentSchema", "extends")

func setOf(items ...string) map[string]bool {
	m := map[string]bool{}
	for _, i := range items {
		m[i] = true
	}
	return m
}

// pyDumps is json.dumps(v) with its default ASCII escaping; compact uses
// separators (",", ":").
func pyDumps(v any, compact bool) string {
	opt := ojson.Options{EnsureASCII: true, ItemSep: ", ", KeySep: ": "}
	if compact {
		opt.ItemSep, opt.KeySep = ",", ":"
	}
	s, _ := ojson.Dumps(v, opt)
	return s
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case *ojson.Object:
		o := ojson.NewObject()
		for _, k := range x.Keys() {
			o.Set(k, deepCopy(x.Value(k)))
		}
		return o
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = deepCopy(item)
		}
		return out
	}
	return v
}

// subschemas is schema and, depth first, every schema nested under it.
func subschemas(schema any) []any {
	var out []any
	var walk func(any)
	walk = func(s any) {
		out = append(out, s)
		obj, ok := s.(*ojson.Object)
		if !ok {
			return
		}
		for _, key := range obj.Keys() {
			item := obj.Value(key)
			var children []any
			switch {
			case schemaMapKeywords[key]:
				if m, ok := item.(*ojson.Object); ok {
					for _, k := range m.Keys() {
						children = append(children, m.Value(k))
					}
				}
			case key == "dependencies":
				if m, ok := item.(*ojson.Object); ok {
					for _, k := range m.Keys() {
						if _, isList := m.Value(k).([]any); !isList {
							children = append(children, m.Value(k))
						}
					}
				}
			case subschemaKeywords[key]:
				if list, ok := item.([]any); ok {
					children = list
				} else {
					children = []any{item}
				}
			case key == "type" || key == "disallow":
				if list, ok := item.([]any); ok {
					for _, c := range list {
						if _, isObj := c.(*ojson.Object); isObj {
							children = append(children, c)
						}
					}
				}
			}
			for _, c := range children {
				walk(c)
			}
		}
	}
	walk(schema)
	return out
}

func jsonObjects(v any) []*ojson.Object {
	var out []*ojson.Object
	pending := []any{v}
	for len(pending) > 0 {
		x := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		switch y := x.(type) {
		case *ojson.Object:
			out = append(out, y)
			for _, k := range y.Keys() {
				pending = append(pending, y.Value(k))
			}
		case []any:
			pending = append(pending, y...)
		}
	}
	return out
}

func checkSchemaDepth(schema any, invalid string) error {
	if !checkDepth(schema, 1) {
		return apiErr(400, fmt.Sprintf("%s: nested more than %d levels deep", invalid, maxSchemaDepth))
	}
	return nil
}

// remoteRef is the first reference of schema to another document.
func remoteRef(schema any) (string, error) {
	for _, node := range subschemas(schema) {
		obj, ok := node.(*ojson.Object)
		if !ok {
			continue
		}
		if obj.Has("$schema") {
			if _, ok := obj.Value("$schema").(string); !ok {
				return "", apiErr(400, "$schema must be a string")
			}
		}
		for _, key := range []string{"$ref", "$dynamicRef", "$recursiveRef"} {
			if !obj.Has(key) {
				continue
			}
			ref, ok := obj.Value(key).(string)
			if !ok {
				return "", apiErr(400, key+" must be a string")
			}
			if !strings.HasPrefix(ref, "#") {
				return ref, nil
			}
		}
	}
	return "", nil
}

var indexRE = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// lookupRef is tool_schema._lookup_tool_reference.
func lookupRef(ref any, root any) (any, error) {
	r, ok := ref.(string)
	if !ok || !strings.HasPrefix(r, "#") {
		return nil, apiErr(400, "unsupported tool parameter reference")
	}
	fragment, err := url.PathUnescape(r[1:])
	if err != nil {
		fragment = r[1:]
	}
	if fragment == "" {
		return root, nil
	}
	if strings.HasPrefix(fragment, "/") {
		current := root
		for _, part := range strings.Split(fragment[1:], "/") {
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			switch c := current.(type) {
			case []any:
				if indexRE.MatchString(part) {
					var i int
					fmt.Sscan(part, &i)
					if i < len(c) {
						current = c[i]
						continue
					}
				}
			case *ojson.Object:
				if c.Has(part) {
					current = c.Value(part)
					continue
				}
			}
			return nil, apiErr(400, "unresolved tool parameter reference: "+r)
		}
		return current, nil
	}
	for _, node := range subschemas(root) {
		if obj, ok := node.(*ojson.Object); ok && (obj.Value("$anchor") == fragment || obj.Value("$dynamicAnchor") == fragment) {
			return obj, nil
		}
	}
	return nil, apiErr(400, "unresolved tool parameter reference: "+r)
}

func referenced(node *ojson.Object, root any) any {
	v, err := lookupRef(node.Value("$ref"), root)
	if err != nil {
		return nil
	}
	return v
}

func enumeration(node *ojson.Object) ([]any, bool) {
	if node.Has("const") {
		return []any{node.Value("const")}, true
	}
	if list, ok := node.Value("enum").([]any); ok {
		return list, true
	}
	return nil, false
}

func spelledTypes(kind any) any {
	switch k := kind.(type) {
	case []any:
		out := make([]any, len(k))
		for i, item := range k {
			if _, ok := item.(string); ok {
				out[i] = spelledTypes(item)
			} else {
				out[i] = item
			}
		}
		return out
	case string:
		if t, ok := typeNames[strings.ToLower(strings.TrimSpace(k))]; ok {
			return t
		}
	}
	return kind
}

// jsonTypeNames is schema with its type names spelled as JSON types.
func jsonTypeNames(schema any) any {
	changed := false
	for _, node := range subschemas(schema) {
		if obj, ok := node.(*ojson.Object); ok && obj.Has("type") {
			if !equalJSON(spelledTypes(obj.Value("type")), obj.Value("type")) {
				changed = true
			}
		}
	}
	if !changed {
		return schema
	}
	out := deepCopy(schema)
	for _, node := range subschemas(out) {
		if obj, ok := node.(*ojson.Object); ok && obj.Has("type") {
			obj.Set("type", spelledTypes(obj.Value("type")))
		}
	}
	return out
}

func equalJSON(a, b any) bool { return ojson.MarshalString(a) == ojson.MarshalString(b) }

func isJSONTypeName(v any) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	switch s {
	case "null", "boolean", "integer", "number", "string", "array", "object":
		return s, true
	}
	return "", false
}

func keysExcept(o *ojson.Object, drop ...map[string]bool) map[string]bool {
	out := map[string]bool{}
outer:
	for _, k := range o.Keys() {
		for _, d := range drop {
			if d[k] {
				continue outer
			}
		}
		out[k] = true
	}
	return out
}

func extended(node *ojson.Object) bool {
	keys := keysExcept(node, schemaAnnotations, schemaIdentifiers)
	for k := range keys {
		if inPlaceApplicators[k] {
			return true
		}
	}
	if keys["$ref"] && len(keys) != 1 {
		return true
	}
	if keys["anyOf"] || keys["oneOf"] {
		for k := range keys {
			if k != "anyOf" && k != "oneOf" && k != "type" {
				return true
			}
		}
	}
	return false
}

// strictSchema closes objects and arrays as strict tools generate them.
func strictSchema(schema any) any {
	for _, node := range subschemas(schema) {
		if obj, ok := node.(*ojson.Object); ok && extended(obj) {
			return schema
		}
	}
	out := deepCopy(schema)
	for _, node := range subschemas(out) {
		obj, ok := node.(*ojson.Object)
		if !ok || obj.Has("anyOf") || obj.Has("oneOf") || obj.Has("$ref") {
			continue
		}
		kind := obj.Value("type")
		kinds := []any{kind}
		if list, ok := kind.([]any); ok {
			kinds = list
		}
		has := func(name string) bool {
			for _, k := range kinds {
				if k == name {
					return true
				}
			}
			return false
		}
		if has("object") || (kind == nil && obj.Has("properties")) {
			if !(obj.Has("additionalProperties") || obj.Has("unevaluatedProperties") || obj.Has("patternProperties")) {
				obj.Set("additionalProperties", false)
			}
		}
		if has("array") || (kind == nil && obj.Has("prefixItems")) {
			if _, isList := obj.Value("items").([]any); isList {
				if !obj.Has("additionalItems") && !obj.Has("unevaluatedItems") {
					obj.Set("additionalItems", false)
				}
			} else if !obj.Has("items") && !obj.Has("unevaluatedItems") {
				obj.Set("items", false)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- framing

type argShape struct {
	props      *ojson.Object
	required   []string
	additional any
}

func schemaCombination(keyword string, values []any) any {
	identity := keyword == "allOf"
	var kept []any
	seen := map[*ojson.Object]bool{}
	seenScalar := map[string]bool{}
	for _, v := range values {
		if b, ok := v.(bool); ok && b == identity {
			continue
		}
		if o, ok := v.(*ojson.Object); ok {
			if seen[o] {
				continue
			}
			seen[o] = true
		} else {
			key := fmt.Sprintf("%T:%v", v, v)
			if seenScalar[key] {
				continue
			}
			seenScalar[key] = true
		}
		kept = append(kept, v)
	}
	if len(kept) == 0 {
		return identity
	}
	for _, v := range kept {
		if _, ok := v.(bool); ok {
			return !identity
		}
	}
	if len(kept) == 1 {
		return kept[0]
	}
	return ojson.O(keyword, kept)
}

func jsonSize(value any, limit int) int {
	size := 0
	pending := []any{value}
	for len(pending) > 0 && size <= limit {
		v := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		switch x := v.(type) {
		case *ojson.Object:
			for _, k := range x.Keys() {
				pending = append(pending, k)
			}
			for _, k := range x.Keys() {
				pending = append(pending, x.Value(k))
			}
			size++
		case []any:
			pending = append(pending, x...)
			size++
		case string:
			size += len([]rune(x))
		default:
			size++
		}
	}
	return size
}

func charge(budget *int, size int) error {
	*budget -= size
	if *budget < 0 {
		return apiErr(400, "tool parameter schemas are too complex")
	}
	return nil
}

func keyword(node *ojson.Object, key string) any { return node.Value(key) }

// argumentShape is tool_schema._argument_shape.
func argumentShape(root any, budget *int) (*argShape, error) {
	var combine func(shapes []*argShape, union bool) (*argShape, error)
	combine = func(shapes []*argShape, union bool) (*argShape, error) {
		if union {
			var kept []*argShape
			for _, s := range shapes {
				if s != nil {
					kept = append(kept, s)
				}
			}
			if len(kept) == 0 {
				return nil, nil
			}
			shapes = kept
		} else {
			for _, s := range shapes {
				if s == nil {
					return nil, nil
				}
			}
		}
		if len(shapes) == 0 {
			return &argShape{props: ojson.NewObject(), required: []string{}, additional: true}, nil
		}
		names := ojson.NewObject()
		for _, s := range shapes {
			for _, k := range s.props.Keys() {
				names.Set(k, true)
			}
		}
		if err := charge(budget, names.Len()*len(shapes)); err != nil {
			return nil, err
		}
		required := map[string]bool{}
		for _, r := range shapes[0].required {
			required[r] = true
		}
		for _, s := range shapes[1:] {
			if union {
				next := map[string]bool{}
				for _, r := range s.required {
					if required[r] {
						next[r] = true
					}
				}
				required = next
			} else {
				for _, r := range s.required {
					required[r] = true
				}
			}
		}
		kw := "allOf"
		if union {
			kw = "anyOf"
		}
		props := ojson.NewObject()
		for _, name := range names.Keys() {
			var vals []any
			for _, s := range shapes {
				if v, ok := s.props.Get(name); ok {
					vals = append(vals, v)
				} else {
					vals = append(vals, s.additional)
				}
			}
			props.Set(name, schemaCombination(kw, vals))
		}
		var adds []any
		for _, s := range shapes {
			adds = append(adds, s.additional)
		}
		req := make([]string, 0, len(required))
		for r := range required {
			req = append(req, r)
		}
		sort.Strings(req)
		return &argShape{props: props, required: req, additional: schemaCombination(kw, adds)}, nil
	}
	references := map[string]*argShape{}
	refDone := map[string]bool{}
	var project func(node any, visiting map[string]bool) (*argShape, error)
	project = func(n any, visiting map[string]bool) (*argShape, error) {
		if b, ok := n.(bool); ok && !b {
			return nil, nil
		}
		node, ok := n.(*ojson.Object)
		if !ok {
			node = ojson.NewObject()
		}
		var kind any = "object"
		switch k := node.Value("type").(type) {
		case string, []any:
			kind = k
		}
		names := []any{kind}
		if list, ok := kind.([]any); ok {
			names = list
		}
		allNonObject := true
		for _, name := range names {
			t, ok := isJSONTypeName(name)
			if !ok || t == "object" {
				allNonObject = false
			}
		}
		if allNonObject {
			return nil, nil
		}
		props := ojson.NewObject()
		if p, ok := node.Value("properties").(*ojson.Object); ok {
			props = p.Copy()
		}
		var additional any = true
		if node.Has("additionalProperties") {
			additional = node.Value("additionalProperties")
		}
		if pp, ok := node.Value("patternProperties").(*ojson.Object); ok && pp.Len() > 0 {
			vals := []any{additional}
			for _, k := range pp.Keys() {
				vals = append(vals, pp.Value(k))
			}
			additional = schemaCombination("anyOf", vals)
		}
		var required []string
		if list, ok := node.Value("required").([]any); ok {
			for _, r := range list {
				if s, ok := r.(string); ok {
					required = append(required, s)
				}
			}
		}
		for _, r := range required {
			if !props.Has(r) {
				props.Set(r, additional)
			}
		}
		if required == nil {
			required = []string{}
		}
		shapes := []*argShape{{props: props, required: required, additional: additional}}
		if ref, ok := node.Value("$ref").(string); ok {
			if visiting[ref] {
				return nil, apiErr(400, "cyclic direct tool argument reference")
			}
			if !refDone[ref] {
				next := map[string]bool{ref: true}
				for k := range visiting {
					next[k] = true
				}
				s, err := project(referenced(node, root), next)
				if err != nil {
					return nil, err
				}
				references[ref], refDone[ref] = s, true
			}
			shapes = append(shapes, references[ref])
		}
		if list, ok := node.Value("allOf").([]any); ok {
			for _, c := range list {
				s, err := project(c, visiting)
				if err != nil {
					return nil, err
				}
				shapes = append(shapes, s)
			}
		}
		for _, kw := range []string{"anyOf", "oneOf"} {
			if list, ok := node.Value(kw).([]any); ok {
				var members []*argShape
				for _, c := range list {
					s, err := project(c, visiting)
					if err != nil {
						return nil, err
					}
					members = append(members, s)
				}
				s, err := combine(members, true)
				if err != nil {
					return nil, err
				}
				shapes = append(shapes, s)
			}
		}
		if node.Has("if") {
			then := any(ojson.NewObject())
			if node.Has("then") {
				then = node.Value("then")
			}
			els := any(ojson.NewObject())
			if node.Has("else") {
				els = node.Value("else")
			}
			a, err := project(then, visiting)
			if err != nil {
				return nil, err
			}
			b, err := project(els, visiting)
			if err != nil {
				return nil, err
			}
			s, err := combine([]*argShape{a, b}, true)
			if err != nil {
				return nil, err
			}
			shapes = append(shapes, s)
		}
		if ds, ok := node.Value("dependentSchemas").(*ojson.Object); ok {
			for _, k := range ds.Keys() {
				a, err := project(ds.Value(k), visiting)
				if err != nil {
					return nil, err
				}
				b, _ := project(ojson.NewObject(), visiting)
				s, err := combine([]*argShape{a, b}, true)
				if err != nil {
					return nil, err
				}
				shapes = append(shapes, s)
			}
		}
		if choices, ok := enumeration(node); ok {
			var objects []*argShape
			for _, v := range choices {
				obj, isObj := v.(*ojson.Object)
				if !isObj {
					continue
				}
				p := ojson.NewObject()
				for _, k := range obj.Keys() {
					p.Set(k, ojson.O("const", obj.Value(k)))
				}
				req := append([]string{}, obj.Keys()...)
				objects = append(objects, &argShape{props: p, required: req, additional: false})
			}
			if len(objects) == 0 {
				return nil, nil
			}
			s, err := combine(objects, true)
			if err != nil {
				return nil, err
			}
			shapes = append(shapes, s)
		}
		return combine(shapes, false)
	}
	shape, err := project(root, map[string]bool{})
	if err != nil {
		return nil, err
	}
	if shape == nil {
		return nil, apiErr(400, "tool parameters must allow a top-level JSON object")
	}
	return shape, nil
}

func definitions(schema any) *ojson.Object {
	if o, ok := schema.(*ojson.Object); ok {
		if d, ok := o.Value("$defs").(*ojson.Object); ok {
			return d
		}
	}
	return ojson.NewObject()
}

func localRefs(value any) []*ojson.Object {
	var out []*ojson.Object
	for _, node := range subschemas(value) {
		if o, ok := node.(*ojson.Object); ok {
			if ref, ok := o.Value("$ref").(string); ok && strings.HasPrefix(ref, "#") {
				out = append(out, o)
			}
		}
	}
	return out
}

func schemaWithRoot(schema, root any) (any, bool) {
	if len(localRefs(schema)) == 0 {
		return schema, false
	}
	rootDefs, schemaDefs := definitions(root), definitions(schema)
	name := "__splash_root"
	for rootDefs.Has(name) || schemaDefs.Has(name) {
		name += "_"
	}
	prefix := "#/$defs/" + name
	rebase := func(v any) any {
		out := deepCopy(v)
		for _, node := range localRefs(out) {
			ref := node.Value("$ref").(string)
			if ref == "#" || strings.HasPrefix(ref, "#/") {
				node.Set("$ref", prefix+ref[1:])
			}
		}
		return out
	}
	out := rebase(schema)
	obj, ok := out.(*ojson.Object)
	if !ok {
		return out, true
	}
	defs := definitions(obj).Copy()
	defs.Set(name, rebase(root))
	obj.Set("$defs", defs)
	return obj, true
}

// toolArgumentSchema is a strict tool's arguments as its grammar generates
// them (tool_schema.tool_argument_schema).
func toolArgumentSchema(root any, budget *int) (*argShape, error) {
	framed := func(v any) (any, error) {
		if err := charge(budget, jsonSize(v, *budget)); err != nil {
			return nil, err
		}
		out, changed := schemaWithRoot(v, root)
		if changed {
			if err := charge(budget, jsonSize(root, *budget)); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	shape, err := argumentShape(root, budget)
	if err != nil {
		return nil, err
	}
	props := ojson.NewObject()
	for _, k := range shape.props.Keys() {
		v, err := framed(shape.props.Value(k))
		if err != nil {
			return nil, err
		}
		props.Set(k, v)
	}
	shape.props = props
	if shape.additional, err = framed(shape.additional); err != nil {
		return nil, err
	}
	return shape, nil
}

// ArgumentSchemas frames each strict tool's arguments, with one budget.
func (p *ToolPolicy) ArgumentSchemas() (map[string]*argShape, error) {
	p.argOnce.Do(func() {
		budget := maxFramedSchemaBytes
		p.args = map[string]*argShape{}
		for _, name := range p.Schemas.Keys() {
			if !p.Strict[name] {
				continue
			}
			s, err := toolArgumentSchema(strictSchema(jsonTypeNames(p.Schemas.Value(name))), &budget)
			if err != nil {
				p.argErr = err
				return
			}
			p.args[name] = s
		}
	})
	return p.args, p.argErr
}

// ---------------------------------------------------------------- grammar text

func larkRegex(pattern string) string { return "/" + strings.ReplaceAll(pattern, "/", `\/`) + "/" }

// reEscape is Python's re.escape.
func reEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("()[]{}?*+-|^$\\.&~# \t\n\r\v\f", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func textRule(tags ...string) string {
	parts := make([]string, len(tags))
	for i, t := range tags {
		parts[i] = reEscape(t)
	}
	return "TEXT: /(?s:.*)/ & ~" + larkRegex("(?s:.*)("+strings.Join(parts, "|")+")(?s:.*)")
}

func tags(s ToolCallSyntax, ts ...string) string {
	if s.TagsAreTokens {
		var kept []string
		for _, t := range ts {
			if t != "" {
				kept = append(kept, t)
			}
		}
		return strings.Join(kept, " ")
	}
	return pyDumps(strings.Join(ts, ""), false)
}

func grammarFunctionOpening(s ToolCallSyntax, name string) string {
	if s.TagsAreTokens {
		var parts []string
		for _, p := range []string{s.NameOpen, pyDumps(name, false), s.NameClose} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, " ")
	}
	return pyDumps(s.functionOpening(name), false)
}

func keyTags(s ToolCallSyntax) (string, string) {
	return tags(s, s.KeyOpen), tags(s, s.KeyClose, s.ValueOpen)
}

func rawValueRules(s ToolCallSyntax, rule string) []string {
	if s.TagsAreTokens {
		return []string{rule + ": RAW_VALUE " + s.ValueClose}
	}
	return []string{rule + "[suffix=" + pyDumps(s.ValueClose, false) + "]: " + rawValue}
}

func valueTerminals(s ToolCallSyntax) []string {
	if !s.TagsAreTokens {
		return nil
	}
	return []string{"RAW_VALUE: " + rawValue + " & ~" + larkRegex("(?s:.*)"+reEscape(s.ValueClose)+"(?s:.*)")}
}

func freeArguments(s ToolCallSyntax) string {
	open, close := keyTags(s)
	rules := []string{"%llguidance {}", "start: parameter*", "parameter: " + open + " NAME " + close + " value"}
	rules = append(rules, rawValueRules(s, "value")...)
	rules = append(rules, "NAME: "+nameRegex)
	rules = append(rules, valueTerminals(s)...)
	return strings.Join(rules, "\n") + "\n"
}

func unionMembers(schema any) []any {
	if o, ok := schema.(*ojson.Object); ok {
		members := o.Value("anyOf")
		if !o.Has("anyOf") {
			members = o.Value("oneOf")
		}
		others := keysExcept(o, schemaAnnotations, schemaIdentifiers)
		onlyUnions := true
		for k := range others {
			if k != "anyOf" && k != "oneOf" {
				onlyUnions = false
			}
		}
		if list, ok := members.([]any); ok && len(list) > 0 && onlyUnions {
			return list
		}
	}
	return []any{schema}
}

// literalTexts is tool_schema._literal_texts: nil where the schema does not
// list its values.
func literalTexts(schema, root any) []string {
	type entry struct {
		texts []string
		done  bool
	}
	memo := map[*ojson.Object]*entry{}
	var read func(any) []string
	read = func(n any) []string {
		node, ok := n.(*ojson.Object)
		if !ok {
			return nil
		}
		if e, ok := memo[node]; ok {
			return e.texts
		}
		memo[node] = &entry{}
		var result []string
		isNone := true
		add := func(list []string, t string) []string {
			for _, x := range list {
				if x == t {
					return list
				}
			}
			return append(list, t)
		}
		kind := node.Value("type")
		if list, ok := kind.([]any); ok && len(list) == 1 {
			kind = list[0]
		}
		members := node.Value("anyOf")
		if !node.Has("anyOf") {
			members = node.Value("oneOf")
		}
		if values, ok := enumeration(node); ok {
			isNone, result = false, []string{}
			for _, v := range values {
				if s, isStr := v.(string); isStr {
					result = add(result, s)
				} else {
					result = add(result, pyDumps(v, true))
				}
			}
		} else if kind == "null" {
			isNone, result = false, []string{"null"}
		} else if kind == "boolean" {
			isNone, result = false, []string{"true", "false"}
		} else if list, ok := members.([]any); ok && len(list) > 0 {
			isNone, result = false, []string{}
			for _, m := range list {
				mt := read(m)
				if mt == nil {
					isNone, result = true, nil
					break
				}
				for _, t := range mt {
					result = add(result, t)
				}
			}
		} else if node.Has("$ref") {
			if target := referenced(node, root); target != nil {
				if r := read(target); r != nil {
					isNone, result = false, r
				}
			}
		}
		if all, ok := node.Value("allOf").([]any); ok {
			for _, m := range all {
				mt := read(m)
				if mt == nil {
					continue
				}
				if isNone {
					isNone, result = false, mt
				} else {
					var kept []string
					for _, t := range result {
						for _, x := range mt {
							if x == t {
								kept = append(kept, t)
								break
							}
						}
					}
					if kept == nil {
						kept = []string{}
					}
					result = kept
				}
			}
		}
		if isNone {
			result = nil
		}
		memo[node].texts = result
		return result
	}
	return read(schema)
}

// schemaTypes is tool_schema.schema_types.
func schemaTypes(schema, root any) typeSet {
	return newTypeReader(root).read(schema)
}

func parameterRules(rule, prefix string, valueSchema any, s ToolCallSyntax) ([]string, error) {
	closing := tags(s, s.ValueClose)
	if b, ok := valueSchema.(bool); ok && !b {
		return nil, nil
	}
	var rules, choices []string
	var structured []any
	for _, member := range unionMembers(valueSchema) {
		texts := literalTexts(member, valueSchema)
		if texts == nil {
			types := schemaTypes(member, valueSchema)
			if types == nil || types["string"] {
				return append([]string{rule + ": " + prefix + " " + rule + "_value"}, rawValueRules(s, rule+"_value")...), nil
			}
			structured = append(structured, member)
			continue
		}
		for _, text := range texts {
			if strings.Contains(text, s.ValueClose) {
				return nil, apiErr(400, "tool parameter enum contains XML framing")
			}
			if text != "" {
				choices = append(choices, pyDumps(text, false))
			} else {
				rules = append(rules, fmt.Sprintf("%s_empty_%d:", rule, len(rules)))
				choices = append(choices, fmt.Sprintf("%s_empty_%d", rule, len(rules)-1))
			}
		}
	}
	if len(structured) > 0 {
		schema := valueSchema
		if o, isObj := valueSchema.(*ojson.Object); isObj && !(len(structured) > 0 && structured[0] == any(o)) {
			n := ojson.NewObject()
			for _, k := range o.Keys() {
				if k != "anyOf" && k != "oneOf" {
					n.Set(k, o.Value(k))
				}
			}
			n.Set("anyOf", structured)
			schema = n
		}
		choices = append(choices, "%json "+pyDumps(grammarCompatibleSchema(schema), true))
	}
	if len(choices) == 0 {
		return nil, nil
	}
	var unique []string
	seen := map[string]bool{}
	for _, c := range choices {
		if !seen[c] {
			seen[c] = true
			unique = append(unique, c)
		}
	}
	return append(rules, rule+": "+prefix+" ("+strings.Join(unique, " | ")+") "+closing), nil
}

func argumentGrammar(shape *argShape, s ToolCallSyntax) (string, error) {
	open, close := keyTags(s)
	required := map[string]bool{}
	for _, r := range shape.required {
		required[r] = true
	}
	var rules, sequence []string
	for index, name := range shape.props.Keys() {
		value := shape.props.Value(name)
		rule := fmt.Sprintf("parameter_%d", index)
		prefix := open + " " + pyDumps(name, false) + " " + close
		if b, ok := value.(bool); !(ok && !b) && !isTagName(name) {
			return "", apiErr(400, "invalid tool parameter name")
		}
		pr, err := parameterRules(rule, prefix, value, s)
		if err != nil {
			return "", err
		}
		if pr == nil {
			if required[name] {
				return "", apiErr(400, "required tool parameter cannot have a value: "+name)
			}
			continue
		}
		rules = append(rules, pr...)
		if required[name] {
			sequence = append(sequence, rule)
		} else {
			sequence = append(sequence, rule+"?")
		}
	}
	extra, err := parameterRules("extra", open+" EXTRA_NAME "+close, shape.additional, s)
	if err != nil {
		return "", err
	}
	if extra != nil {
		var declared []string
		for _, name := range shape.props.Keys() {
			declared = append(declared, pyDumps(name, false))
		}
		line := "EXTRA_NAME: " + nameRegex
		if len(declared) > 0 {
			line += " & ~(" + strings.Join(declared, " | ") + ")"
		}
		rules = append(rules, line)
		rules = append(rules, extra...)
		sequence = append(sequence, "extra*")
	}
	rules = append(rules, valueTerminals(s)...)
	var b strings.Builder
	b.WriteString("%llguidance {}\nstart:")
	for _, item := range sequence {
		b.WriteString(" " + item)
	}
	b.WriteString("\n" + strings.Join(rules, "\n") + "\n")
	return b.String(), nil
}

func reasoningRule(thinking bool, cf *ChatFormat) (string, string) {
	close := fmt.Sprintf("<[%d]>", cf.ThinkEndID)
	if thinking {
		return "think ", "think: TEXT " + close
	}
	if cf.ThinkStartID >= 0 {
		return "think? ", fmt.Sprintf("think: <[%d]> TEXT %s", cf.ThinkStartID, close)
	}
	return "", ""
}

// ---------------------------------------------------------------- patterns

var patternChecks = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

// patternValidator checks a grammar; set once llguidance is loaded.
var patternValidator func(grammar string) bool

func grammarTakesPattern(pattern string) bool {
	patternChecks.Lock()
	ok, found := patternChecks.m[pattern]
	patternChecks.Unlock()
	if found {
		return ok
	}
	grammar := "%llguidance {}\nstart: %json " + pyDumps(ojson.O("type", "string", "pattern", pattern), false) + "\n"
	ok = patternValidator != nil && patternValidator(grammar)
	patternChecks.Lock()
	if len(patternChecks.m) >= 1024 {
		patternChecks.m = map[string]bool{}
	}
	patternChecks.m[pattern] = ok
	patternChecks.Unlock()
	return ok
}

var grammarBoundKeywords = []string{"minItems", "maxItems", "multipleOf"}

// grammarCompatibleSchema keeps the constraints the grammar compiler takes.
func grammarCompatibleSchema(schema any) any {
	out := deepCopy(schema)
	for _, node := range subschemas(out) {
		obj, ok := node.(*ojson.Object)
		if !ok {
			continue
		}
		obj.Delete("propertyNames")
		pattern, has := obj.Get("pattern")
		obj.Delete("pattern")
		if p, isStr := pattern.(string); has && isStr && grammarTakesPattern(p) {
			obj.Set("pattern", p)
		}
	}
	for _, obj := range jsonObjects(out) {
		for _, key := range grammarBoundKeywords {
			bound := obj.Value(key)
			var over bool
			switch b := bound.(type) {
			case int64:
				over = b > maxGrammarBound
			case float64:
				over = b > maxGrammarBound
			case *big.Int:
				over = b.Sign() > 0
			}
			if over {
				obj.Delete(key)
			}
		}
	}
	if b, ok := out.(bool); ok && b {
		out = ojson.NewObject()
	}
	if obj, ok := out.(*ojson.Object); ok {
		obj.Set("x-guidance", ojson.O("lenient", true, "whitespace_pattern", whitespacePattern))
	}
	return out
}

// ---------------------------------------------------------------- grammars

// jsonGrammar is a response schema's grammar without tools.
func jsonGrammar(schema any, thinking bool, cf *ChatFormat) string {
	think, thinkRule := reasoningRule(thinking, cf)
	g := []string{"%llguidance {}", "start: " + think + "WS %json " + pyDumps(grammarCompatibleSchema(schema), true)}
	if thinkRule != "" {
		g = append(g, thinkRule, textRule(thinkEnd))
	}
	g = append(g, whitespaceRule)
	return strings.Join(g, "\n") + "\n"
}

// toolGrammar is a constrained tool request's grammar.
func toolGrammar(p *ToolPolicy, thinking bool, responseSchema any, cf *ChatFormat) (string, error) {
	s := cf.Syntax
	args, err := p.ArgumentSchemas()
	if err != nil {
		return "", err
	}
	strictArgs := map[string]string{}
	for name := range p.Strict {
		g, err := argumentGrammar(args[name], s)
		if err != nil {
			return "", err
		}
		strictArgs[name] = g
	}
	var side []any
	if len(strictArgs) < p.Schemas.Len() {
		side = append(side, ojson.O("name", "free_arguments", "lark_grammar", freeArguments(s)))
	}
	closing := ""
	if s.ArgumentsClose != "" {
		closing = " " + tags(s, s.ArgumentsClose)
	}
	var calls []string
	for index, name := range p.Schemas.Keys() {
		arguments := "free_arguments"
		if g, ok := strictArgs[name]; ok {
			arguments = fmt.Sprintf("arguments_%d", index)
			side = append(side, ojson.O("name", arguments, "lark_grammar", g))
		}
		calls = append(calls, fmt.Sprintf("call_%d: %s %s @%s%s %s", index, toolCallOpen, grammarFunctionOpening(s, name), arguments, closing, toolCallClose))
	}
	names := make([]string, len(calls))
	for i := range calls {
		names[i] = fmt.Sprintf("call_%d", i)
	}
	call := "(" + strings.Join(names, " | ") + ")"
	var body string
	switch {
	case len(calls) == 0:
		body = "TEXT"
		if responseSchema != nil {
			body = "answer"
		}
	case p.Required || responseSchema != nil:
		if p.Parallel {
			body = "WS " + call + " (TEXT " + call + ")* TEXT"
		} else {
			body = "WS " + call + " WS"
		}
		if !p.Required {
			body = "((" + body + ") | answer)"
		}
	default:
		if p.Parallel {
			body = "(TEXT " + call + ")* TEXT"
		} else {
			body = "TEXT (" + call + " WS)?"
		}
	}
	think, thinkRule := reasoningRule(thinking, cf)
	main := []string{"%llguidance {}", "start: " + think + body}
	if responseSchema != nil && !p.Required {
		main = append(main, "answer: WS %json "+pyDumps(grammarCompatibleSchema(responseSchema), true)+" WS")
	}
	if thinkRule != "" {
		main = append(main, thinkRule)
	}
	main = append(main, calls...)
	main = append(main, whitespaceRule, textRule(toolCallOpen, thinkEnd))
	side = append([]any{ojson.O("name", "tool_output", "lark_grammar", strings.Join(main, "\n")+"\n")}, side...)
	return pyDumps(ojson.O("grammars", side), true), nil
}

// normalizeResponseFormat reads response_format: its schema, or nil.
func normalizeResponseFormat(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	obj, ok := value.(*ojson.Object)
	if !ok {
		return nil, apiErr(400, "response_format must be an object")
	}
	if obj.Len() == 1 && obj.Value("type") == "text" {
		return nil, nil
	}
	var schema any
	switch obj.Value("type") {
	case "json_object":
		schema = ojson.O("type", "object")
	case "json_schema":
		wrapper, _ := obj.Value("json_schema").(*ojson.Object)
		schema = wrapper.Value("schema")
		switch schema.(type) {
		case *ojson.Object, bool:
		default:
			return nil, apiErr(400, "response_format.json_schema.schema is required")
		}
	default:
		return nil, apiErr(400, "unsupported response_format")
	}
	if err := checkSchemaDepth(schema, "invalid response schema"); err != nil {
		return nil, err
	}
	ref, err := remoteRef(schema)
	if err != nil {
		return nil, err
	}
	if ref != "" {
		return nil, apiErr(400, "remote schema reference is not allowed: "+ref)
	}
	if msg := typeMetaError(schema); msg != "" {
		return nil, apiErr(400, "invalid response schema: "+msg)
	}
	return schema, nil
}

// checkStrictTool refuses a strict tool's schema its grammar cannot be built
// from.
func checkStrictTool(name string, schema any) error {
	if err := checkSchemaDepth(schema, "invalid tool schema for "+name); err != nil {
		return err
	}
	ref, err := remoteRef(schema)
	if err != nil {
		return err
	}
	if ref != "" {
		return apiErr(400, "remote tool schema reference is not allowed: "+ref)
	}
	if msg := typeMetaError(schema); msg != "" {
		return apiErr(400, "invalid tool schema for "+name+": "+msg)
	}
	return nil
}

// typeMetaError is the message jsonschema's check_schema gives a schema whose
// type keyword names no JSON type, as its best_match reports it: a nested
// schema's before the root's, the first in document order. "" where none.
func typeMetaError(schema any) string {
	root, nested := "", ""
	var walk func(any, int)
	walk = func(s any, depth int) {
		obj, ok := s.(*ojson.Object)
		if !ok {
			return
		}
		if obj.Has("type") && !validTypeKeyword(obj.Value("type")) {
			msg := pyRepr(obj.Value("type")) + " is not valid under any of the given schemas"
			if depth == 0 {
				root = msg
			} else if nested == "" {
				nested = msg
			}
		}
		for _, key := range obj.Keys() {
			item := obj.Value(key)
			switch {
			case schemaMapKeywords[key]:
				if m, ok := item.(*ojson.Object); ok {
					for _, k := range m.Keys() {
						walk(m.Value(k), depth+2)
					}
				}
			case subschemaKeywords[key]:
				if list, ok := item.([]any); ok {
					for _, c := range list {
						walk(c, depth+2)
					}
				} else {
					walk(item, depth+1)
				}
			}
		}
	}
	walk(schema, 0)
	if nested != "" {
		return nested
	}
	return root
}

func validTypeKeyword(v any) bool {
	if _, ok := isJSONTypeName(v); ok {
		return true
	}
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, item := range list {
		name, ok := isJSONTypeName(item)
		if !ok || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}

// pyRepr is Python's repr of a JSON value.
func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		q := "'"
		if strings.Contains(x, "'") && !strings.Contains(x, `"`) {
			q = `"`
		}
		r := strings.ReplaceAll(x, `\`, `\\`)
		if q == "'" {
			r = strings.ReplaceAll(r, "'", `\'`)
		}
		r = strings.NewReplacer("\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(r)
		return q + r + q
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = pyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *ojson.Object:
		parts := make([]string, 0, x.Len())
		for _, k := range x.Keys() {
			parts = append(parts, pyRepr(k)+": "+pyRepr(x.Value(k)))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case float64:
		return ojson.FloatRepr(x)
	}
	return fmt.Sprint(v)
}
