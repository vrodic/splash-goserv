"""Compare the Go server's constraint grammars with server/tool_schema.py's
for a corpus of tools, tool choices and response formats.

PYTHONPATH=. .venv/bin/python goserver/parity/grammar_parity.py MODEL_DIR
"""
import json, subprocess, sys, glob
from server import tool_schema as ts
from server.chat_format import QWEN, FLINT
from server.errors import APIError

tdir = sys.argv[1]
lib = glob.glob(".venv/lib/python3*/site-packages/llguidance/_lib.abi3.so")[0]

def tool(name, params, strict=None, desc="d"):
    f = {"name": name, "description": desc, "parameters": params}
    if strict is not None:
        f["strict"] = strict
    return {"type": "function", "function": f}

schemas = [
    {"type": "object", "properties": {"city": {"type": "string"}, "days": {"type": "integer"}}, "required": ["city"]},
    {"type": "object", "properties": {"mode": {"enum": ["fast", "slow", ""]}, "n": {"type": ["integer", "null"]},
                                      "flag": {"type": "boolean"}, "tags": {"type": "array", "items": {"type": "string"}, "maxItems": 100}},
     "required": ["mode", "flag"]},
    {"type": "object", "properties": {"q": {"type": "string", "pattern": "^[a-z]+$"}, "x": {"type": "number", "multipleOf": 0.5},
                                      "obj": {"type": "object", "properties": {"a": {"$ref": "#/$defs/A"}}}},
     "$defs": {"A": {"type": "string", "enum": ["p", "q"]}}},
    {"type": "object", "properties": {"u": {"anyOf": [{"type": "integer"}, {"const": "auto"}]}, "v": {"oneOf": [{"type": "string"}, {"type": "null"}]}},
     "additionalProperties": {"type": "integer"}},
    {"type": "object", "allOf": [{"properties": {"a": {"type": "string"}}}, {"properties": {"b": {"type": "integer"}}, "required": ["b"]}]},
    {"type": "object", "properties": {"look": {"type": "string", "pattern": "(?<=a)b"}, "Ünï": {"type": "string", "description": "ünicode ✓"}},
     "patternProperties": {"^x-": {"type": "boolean"}}},
    {"type": "Object", "properties": {"s": {"type": "STR"}, "l": {"type": "list", "items": {"type": "int"}}}},
    {"type": "object", "properties": {"node": {"$ref": "#"}}, "required": []},
    {},
    {"type": "object", "properties": {"e": {"enum": [1, 2.5, None, True, {"k": "v"}]}}},
    {"type": "object", "if": {"properties": {"a": {"const": 1}}}, "then": {"properties": {"b": {"type": "string"}}}, "else": {"properties": {"c": {"type": "number"}}}},
]
response_formats = [
    None,
    {"type": "json_object"},
    {"type": "json_schema", "json_schema": {"name": "r", "schema": {"type": "object", "properties": {"answer": {"type": "string"}, "score": {"type": "integer", "minimum": 0}}, "required": ["answer"]}}},
    {"type": "json_schema", "json_schema": {"schema": True}},
    {"type": "json_schema", "json_schema": {"schema": {"type": "array", "items": {"type": "string", "pattern": "\\bword\\b"}, "minItems": 2, "maxItems": 200}}},
]
cases = []
for fam, cf in (("Qwen3.8-27B", QWEN), ("Flint", FLINT)):
    for thinking in (False, True):
        for rf in response_formats:
            if rf is not None:
                cases.append((fam, cf, thinking, {"response_format": rf}))
            for i, s in enumerate(schemas):
                for strict in (None, True):
                    tools = [tool("get_weather", schemas[0]), tool(f"t{i}", s, strict)]
                    for choice, par in ((None, None), ("required", None), ({"type": "function", "function": {"name": f"t{i}"}}, None), (None, False), ("none", None)):
                        body = {"tools": tools}
                        if choice is not None: body["tool_choice"] = choice
                        if par is not None: body["parallel_tool_calls"] = par
                        if rf is not None: body["response_format"] = rf
                        cases.append((fam, cf, thinking, body))

def python_grammar(body, thinking, cf):
    try:
        tools, policy = ts.normalize_tools(body.get("tools"), body.get("tool_choice"), body.get("parallel_tool_calls", True))
        schema, _ = ts.normalize_response_format(body.get("response_format"))
        if policy is not None and (policy.constrained or schema is not None):
            return {"grammar": ts.tool_grammar(policy, thinking, schema, chat_format=cf)}
        if schema is not None:
            return {"grammar": ts.json_grammar(schema, thinking, chat_format=cf)}
        return {"grammar": ""}
    except APIError as e:
        return {"error": e.message}

lines = "\n".join(json.dumps({"body": b, "thinking": t, "family": f}) for f, _, t, b in cases) + "\n"
res = subprocess.run(["build/grammarcheck", lib, tdir + "/tokenizer.json"], input=lines.encode(), capture_output=True)
if res.returncode:
    print(res.stderr.decode()); sys.exit(1)
outs = [json.loads(l) for l in res.stdout.decode().splitlines()]
bad = 0
for (f, cf, t, b), g in zip(cases, outs):
    want = python_grammar(b, t, cf)
    if want != g:
        bad += 1
        if bad <= 4:
            print("MISMATCH", f, t, json.dumps(b)[:300])
            w, gg = json.dumps(want), json.dumps(g)
            i = next((i for i, (x, y) in enumerate(zip(w, gg)) if x != y), min(len(w), len(gg)))
            print("  py:", w[max(0, i - 150):i + 150]); print("  go:", gg[max(0, i - 150):i + 150])
print(f"{len(cases)} grammars, {bad} mismatches")
