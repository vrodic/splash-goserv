import json, urllib.request
def post(path, body):
    req = urllib.request.Request("http://127.0.0.1:8000" + path, json.dumps(body).encode(), {"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=600) as r: return json.loads(r.read())
    except urllib.error.HTTPError as e: return json.loads(e.read())
def chat(label, **body):
    body.setdefault("reasoning_effort", "none"); body.setdefault("max_tokens", 300)
    d = post("/v1/chat/completions", body)
    c = d.get("choices", [{}])[0]
    print(label, "->", json.dumps(c.get("message", d), ensure_ascii=False)[:420], "|", c.get("finish_reason"))
    return c
U = lambda t: [{"role": "user", "content": t}]
schema = {"type": "object", "properties": {"fruit": {"type": "string", "enum": ["apple", "banana", "kiwi"]}, "calories": {"type": "integer"}}, "required": ["fruit", "calories"], "additionalProperties": False}
c = chat("1 json_schema", messages=U("Give me a fruit and its calories."), response_format={"type": "json_schema", "json_schema": {"name": "f", "schema": schema}})
v = json.loads(c["message"]["content"]); assert v["fruit"] in ("apple", "banana", "kiwi") and isinstance(v["calories"], int)
c = chat("2 json_object+thinking", messages=U("List two colors as JSON."), reasoning_effort="low", max_tokens=800, response_format={"type": "json_object"})
assert isinstance(json.loads(c["message"]["content"]), dict)
T = [{"type": "function", "function": {"name": "get_weather", "strict": True, "parameters": {"type": "object", "properties": {"city": {"type": "string"}, "unit": {"type": "string", "enum": ["c", "f"]}, "days": {"type": "integer"}}, "required": ["city", "unit"]}}},
     {"type": "function", "function": {"name": "search", "parameters": {"type": "object", "properties": {"q": {"type": "string"}}}}}]
c = chat("3 required", messages=U("hello there"), tools=T, tool_choice="required")
assert c["finish_reason"] == "tool_calls"
c = chat("4 named strict", messages=U("Weather in Oslo for a week in fahrenheit"), tools=T, tool_choice={"type": "function", "function": {"name": "get_weather"}})
a = json.loads(c["message"]["tool_calls"][0]["function"]["arguments"]); assert a["unit"] in ("c", "f") and "city" in a
c = chat("5 parallel false", messages=U("Weather in Oslo and in Rome, celsius? Call the tool for each."), tools=T, parallel_tool_calls=False)
assert len(c["message"].get("tool_calls", [])) <= 1
c = chat("6 none", messages=U("Weather in Oslo? Answer briefly."), tools=T, tool_choice="none", max_tokens=80)
assert not c["message"].get("tool_calls")
c = chat("7 required+thinking", messages=U("Find docs about llamas."), tools=T, tool_choice="required", reasoning_effort="low", max_tokens=1500)
assert c["finish_reason"] == "tool_calls"
d = post("/v1/messages", {"model": "Jundot/Qwen3.8-27B-oQ4e-mtp", "max_tokens": 300, "messages": U("Pick a fruit."), "output_config": {"format": {"type": "json_schema", "schema": schema}}})
print("8 messages output_format ->", json.dumps(d.get("content", d))[:300]); json.loads(d["content"][-1]["text"])
d = post("/v1/messages", {"model": "Jundot/Qwen3.8-27B-oQ4e-mtp", "max_tokens": 300, "messages": U("hi"), "tool_choice": {"type": "any"},
    "tools": [{"name": "lookup", "strict": True, "input_schema": {"type": "object", "properties": {"term": {"type": "string"}}, "required": ["term"]}}]})
print("9 messages tool_choice any ->", json.dumps(d.get("content", d))[:300], d.get("stop_reason")); assert d["stop_reason"] == "tool_use"
d = post("/v1/chat/completions", {"messages": U("x"), "response_format": {"type": "json_schema", "json_schema": {"schema": {"type": "STR"}}}})
print("10 bad schema ->", d)
print("ALL OK")
