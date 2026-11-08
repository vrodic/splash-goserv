package server

import (
	"math"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"splash/goserver/ojson"
)

// ToolCallSyntax is how a family's template lays out a call
// (chat_format.ToolCallSyntax).
type ToolCallSyntax struct {
	NameOpen, NameClose, KeyOpen, KeyClose, ValueOpen, ValueClose, ArgumentsClose string
	TagsAreTokens                                                                 bool
}

func (s ToolCallSyntax) callOpen() string { return toolCallOpen + s.NameOpen }

func (s ToolCallSyntax) functionOpening(name string) string {
	return s.NameOpen + name + s.NameClose
}

func (s ToolCallSyntax) nameEnd() byte {
	if s.NameClose != "" {
		return s.NameClose[0]
	}
	return s.KeyOpen[0]
}

func (s ToolCallSyntax) argumentTags() []string {
	var tags []string
	for _, t := range []string{s.KeyOpen, strings.TrimSpace(s.ArgumentsClose), toolCallClose} {
		if t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

func (s ToolCallSyntax) valueEnds() []string {
	tags := s.argumentTags()
	out := make([]string, len(tags))
	for i, t := range tags {
		out[i] = s.ValueClose + t
	}
	return out
}

// ChatFormat is what the server reads and checks in a family's tokens
// (chat_format.ChatFormat). ThinkStartID is -1 where the generation prompt
// opens the reasoning.
type ChatFormat struct {
	StopTokens      []string
	ThinkEndID      int
	ThinkStartID    int
	CallOpenID      int
	Syntax          ToolCallSyntax
	MessageBoundary string
	ImagePad        string
}

func (c *ChatFormat) reasoningSwitch() bool   { return c.ThinkStartID < 0 }
func (c *ChatFormat) callsEndReasoning() bool { return c.ThinkStartID < 0 }

var qwenXML = ToolCallSyntax{NameOpen: "\n<function=", NameClose: ">\n", KeyOpen: "<parameter=", KeyClose: ">",
	ValueOpen: "\n", ValueClose: "\n</parameter>\n", ArgumentsClose: "</function>\n"}
var glmArgs = ToolCallSyntax{KeyOpen: "<arg_key>", KeyClose: "</arg_key>", ValueOpen: "<arg_value>",
	ValueClose: "</arg_value>", TagsAreTokens: true}

var qwenFormat = ChatFormat{StopTokens: []string{"<|endoftext|>", "<|im_end|>"}, ThinkEndID: 248069, ThinkStartID: -1,
	CallOpenID: 248058, Syntax: qwenXML, MessageBoundary: "<|im_end|>", ImagePad: "<|image_pad|>"}
var flintFormat = ChatFormat{StopTokens: []string{"<|end_of_text|>", "<|user|>", "<|observation|>", "<|developer|>"},
	ThinkEndID: 150007, ThinkStartID: 150006, CallOpenID: 150010, Syntax: glmArgs, MessageBoundary: "<|assistant|>"}

// Family is a model family the release serves (install/families.py).
type Family struct {
	Name     string
	Format   *ChatFormat
	Sampling map[string]any
}

var qwenSampling = map[string]any{"temperature": 1.0, "top_p": 0.95, "top_k": int64(20)}

var families = []Family{
	{"Qwen3.8-27B", &qwenFormat, qwenSampling},
	{"Qwen3.6-35B-A3B", &qwenFormat, qwenSampling},
	{"Qwen3.5-122B-A10B", &qwenFormat, qwenSampling},
	{"Qwen3.8-Flash-Next", &qwenFormat, qwenSampling},
	{"Flint", &flintFormat, qwenSampling},
}

func familyNamed(name string) *Family {
	for i := range families {
		if families[i].Name == name {
			return &families[i]
		}
	}
	return nil
}

// typeSet is a set of JSON type names; nil constrains no type.
type typeSet map[string]bool

func (t typeSet) only(name string) bool { return len(t) == 1 && t[name] }

// ToolPolicy is how a request's tool calls are read.
type ToolPolicy struct {
	Schemas     *ojson.Object // name -> schema (nil value for unreadable)
	Required    bool
	Parallel    bool
	Strict      map[string]bool
	Constrained bool
}

// MayCall reports whether a call may be made.
func (p *ToolPolicy) MayCall() bool { return p != nil && p.Schemas.Len() > 0 }

var typeNames = map[string]string{"null": "null", "boolean": "boolean", "integer": "integer", "number": "number",
	"string": "string", "array": "array", "object": "object", "str": "string", "text": "string", "int": "integer",
	"float": "number", "bool": "boolean", "dict": "object", "list": "array"}

func jsonType(name string) string {
	if t, ok := typeNames[strings.ToLower(strings.TrimSpace(name))]; ok {
		return t
	}
	return ""
}

// ParameterTypes gives the JSON types each tool's parameters convert by.
type ParameterTypes struct {
	policy *ToolPolicy
	cache  map[string][2]any
}

func NewParameterTypes(p *ToolPolicy) *ParameterTypes {
	return &ParameterTypes{policy: p, cache: map[string][2]any{}}
}

// Of returns the types of tool name's declared parameters and of any other.
func (pt *ParameterTypes) Of(name string) (map[string]typeSet, typeSet) {
	if c, ok := pt.cache[name]; ok {
		return c[0].(map[string]typeSet), nil
	}
	params := map[string]typeSet{}
	schema, _ := pt.policy.Schemas.Value(name).(*ojson.Object)
	if schema != nil {
		reader := newTypeReader(schema)
		if props, ok := schema.Value("properties").(*ojson.Object); ok {
			for _, k := range props.Keys() {
				params[k] = reader.read(props.Value(k))
			}
		}
	}
	pt.cache[name] = [2]any{params, nil}
	return params, nil
}

type typeReader struct {
	root *ojson.Object
	seen map[*ojson.Object]bool
}

func newTypeReader(root *ojson.Object) *typeReader {
	return &typeReader{root: root, seen: map[*ojson.Object]bool{}}
}

func narrowed(a, b typeSet) typeSet {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := typeSet{}
	for k := range a {
		if b[k] {
			out[k] = true
		}
	}
	if (a["number"] && b["integer"]) || (a["integer"] && b["number"]) {
		out["integer"] = true
	}
	return out
}

func valueType(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case int64, *big.Int:
		return "integer"
	case float64:
		_ = x
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case *ojson.Object:
		return "object"
	}
	return ""
}

// read is tool_schema.schema_types for one node.
func (r *typeReader) read(node any) typeSet {
	obj, ok := node.(*ojson.Object)
	if !ok {
		if b, isBool := node.(bool); isBool && !b {
			return typeSet{}
		}
		return nil
	}
	if r.seen[obj] {
		return nil
	}
	r.seen[obj] = true
	defer delete(r.seen, obj)
	var result typeSet
	kind := obj.Value("type")
	switch k := kind.(type) {
	case string, []any:
		kinds := []any{k}
		if list, ok := k.([]any); ok {
			kinds = list
		}
		set := typeSet{}
		ok := true
		for _, item := range kinds {
			s, isStr := item.(string)
			t := ""
			if isStr {
				t = jsonType(s)
			}
			if t == "" {
				ok = false
				break
			}
			set[t] = true
		}
		if ok {
			result = set
		}
	default:
		var values []any
		if c, has := obj.Get("const"); has {
			values = []any{c}
		} else if e, isList := obj.Value("enum").([]any); isList {
			values = e
		}
		if values != nil {
			result = typeSet{}
			for _, v := range values {
				if t := valueType(v); t != "" {
					result[t] = true
				}
			}
		} else {
			members, _ := obj.Value("anyOf").([]any)
			if members == nil {
				members, _ = obj.Value("oneOf").([]any)
			}
			if len(members) > 0 {
				var union typeSet
				for _, m := range members {
					if t := r.read(m); t != nil {
						if union == nil {
							union = typeSet{}
						}
						for k := range t {
							union[k] = true
						}
					}
				}
				result = union
			}
		}
	}
	all, _ := obj.Value("allOf").([]any)
	if ref, ok := obj.Value("$ref").(string); ok {
		if target := r.resolve(ref); target != nil {
			all = append(append([]any(nil), all...), target)
		}
	}
	for _, m := range all {
		result = narrowed(result, r.read(m))
	}
	return result
}

func (r *typeReader) resolve(ref string) any {
	if !strings.HasPrefix(ref, "#") {
		return nil
	}
	var node any = r.root
	path := strings.TrimPrefix(ref, "#")
	if path == "" {
		return node
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		obj, ok := node.(*ojson.Object)
		if !ok {
			return nil
		}
		node = obj.Value(part)
	}
	return node
}

var pythonLiterals = map[string]any{"True": true, "False": false, "None": nil}

// convertValue reads a parameter's text as its declared JSON types do
// (output.convert_value).
func convertValue(text string, types typeSet) any {
	value, err := ojson.DecodeString(text)
	isJSON := err == nil && fitsArguments(value, 2)
	if !isJSON {
		lit, ok := pythonLiterals[strings.TrimSpace(text)]
		if ok && !types["string"] && types[valueType(lit)] {
			return lit
		}
		return text
	}
	if types["string"] {
		kind := valueType(value)
		declared := types[kind] || (kind == "integer" && types["number"])
		if declared && kind != "string" {
			return value
		}
		return text
	}
	return value
}

func fitsArguments(v any, depth int) bool {
	switch x := v.(type) {
	case float64:
		return !math.IsInf(x, 0) && !math.IsNaN(x)
	case []any:
		if depth > ojson.MaxDepth-5 {
			return false
		}
		for _, item := range x {
			if !fitsArguments(item, depth+1) {
				return false
			}
		}
	case *ojson.Object:
		if depth > ojson.MaxDepth-5 {
			return false
		}
		for _, k := range x.Keys() {
			if !fitsArguments(x.Value(k), depth+1) {
				return false
			}
		}
	}
	return true
}

func isTagName(v any) bool {
	name, ok := v.(string)
	if !ok || name == "" || utf8.RuneCountInString(name) > maxNameLength || name != strings.Trim(name, nameSpace) ||
		strings.ContainsAny(name, "<>\n\r") {
		return false
	}
	return norm.NFC.IsNormalString(name)
}

const tagNameRule = "1 to 256 characters in NFC, without <, >, line breaks, lone surrogates or surrounding whitespace"

func checkDepth(v any, depth int) bool {
	switch x := v.(type) {
	case []any:
		if depth > 64 {
			return false
		}
		for _, item := range x {
			if !checkDepth(item, depth+1) {
				return false
			}
		}
	case *ojson.Object:
		if depth > 64 {
			return false
		}
		for _, k := range x.Keys() {
			if !checkDepth(x.Value(k), depth+1) {
				return false
			}
		}
	}
	return true
}

// normalizeTools checks a request's tools and returns them with its policy
// (tool_schema.normalize_tools).
func normalizeTools(tools, toolChoice, parallel any) ([]any, *ToolPolicy, error) {
	if parallel == nil {
		parallel = true
	}
	par, ok := parallel.(bool)
	if !ok {
		return nil, nil, apiErr(400, "parallel_tool_calls must be a boolean")
	}
	if tools == nil {
		switch toolChoice {
		case nil, "none", "auto":
			return nil, nil, nil
		}
		return nil, nil, apiErr(400, "tool_choice requires tools")
	}
	list, ok := tools.([]any)
	if !ok {
		return nil, nil, apiErr(400, "tools must be an array")
	}
	schemas := ojson.NewObject()
	marked := map[string]bool{}
	var offered []any
	for _, t := range list {
		tool, ok := t.(*ojson.Object)
		if !ok {
			return nil, nil, apiErr(400, "only function tools are supported")
		}
		if typ, has := tool.Get("type"); has && typ != "function" && typ != nil {
			return nil, nil, apiErr(400, "only function tools are supported")
		}
		if v, _ := tool.Get("type"); v == nil {
			fixed := ojson.O("type", "function")
			for _, k := range tool.Keys() {
				if k != "type" {
					fixed.Set(k, tool.Value(k))
				}
			}
			tool = fixed
		}
		offered = append(offered, tool)
		fn, ok := tool.Value("function").(*ojson.Object)
		if !ok {
			return nil, nil, apiErr(400, "a function tool needs a function object")
		}
		name := fn.Value("name")
		if !isTagName(name) {
			return nil, nil, apiErr(400, "tool name must be "+tagNameRule)
		}
		n := name.(string)
		if schemas.Has(n) {
			return nil, nil, apiErr(400, "duplicate tool name: "+n)
		}
		schema := fn.Value("parameters")
		if schema == nil {
			schema = ojson.NewObject()
		}
		switch schema.(type) {
		case *ojson.Object, bool:
		default:
			return nil, nil, apiErr(400, "invalid tool schema for "+n)
		}
		strict := fn.Value("strict")
		if strict != nil {
			if _, ok := strict.(bool); !ok {
				return nil, nil, apiErr(400, "strict must be a boolean for tool "+n)
			}
		}
		if !checkDepth(schema, 1) {
			schema = nil
		}
		schemas.Set(n, schema)
		if strict == true {
			marked[n] = true
		}
	}
	choice := toolChoice
	if choice == nil {
		choice = "auto"
	}
	if len(list) == 0 {
		if choice != "auto" && choice != "none" {
			return nil, nil, apiErr(400, "tool_choice requires at least one tool")
		}
		return nil, nil, nil
	}
	if choice == "none" {
		return offered, &ToolPolicy{Schemas: ojson.NewObject(), Parallel: par, Strict: map[string]bool{}, Constrained: true}, nil
	}
	if obj, ok := choice.(*ojson.Object); ok {
		fn, _ := obj.Value("function").(*ojson.Object)
		name, _ := fn.Value("name").(string)
		if (obj.Value("type") != nil && obj.Value("type") != "function") || name == "" || !schemas.Has(name) {
			return nil, nil, apiErr(400, "invalid named tool_choice")
		}
		schemas = ojson.O(name, schemas.Value(name))
		par = false
	} else if choice != "auto" && choice != "required" {
		return nil, nil, apiErr(400, "invalid tool_choice")
	}
	strict := map[string]bool{}
	for k := range marked {
		if schemas.Has(k) {
			strict[k] = true
		}
	}
	return offered, &ToolPolicy{Schemas: schemas, Required: choice != "auto", Parallel: par, Strict: strict,
		Constrained: choice != "auto" || !par || len(strict) > 0}, nil
}

// toolsSignature is the console's (count, short hash) of the tool list.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var surrogateRE = regexp.MustCompile("�")
