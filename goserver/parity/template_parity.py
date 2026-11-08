import json, sys, subprocess, random, copy
from transformers import AutoTokenizer
from server import chat_templates as ct
tdir = sys.argv[1]
tok = AutoTokenizer.from_pretrained(tdir)
source = tok.chat_template
templates = [source] + sys.argv[2:]  # extra template files
convs = [list(c) for pair in ct._CANARIES for c in pair] + [list(c) for c in ct._ORDINARY]
rng = random.Random(3)
weird = ["  padded  ", "multi\nline\n", "unicode ✓ 日本 😀", "'quotes' and \"dq\"", "<tool_response>x</tool_response>", "", "tab\there"]
tool2 = {"type":"function","function":{"name":"run","description":"Run ü","parameters":{"type":"object","properties":{"cmd":{"type":"string"},"n":{"type":"integer","default":1.0},"flags":{"type":"array","items":{"type":"string"}}},"required":["cmd"]}}}
for _ in range(150):
    msgs=[]
    if rng.random()<0.5: msgs.append({"role":"system","content":rng.choice(weird)+"sys"})
    for t in range(rng.randint(1,4)):
        msgs.append({"role":"user","content":rng.choice(weird)+"q"})
        if rng.random()<0.7:
            a={"role":"assistant","content":rng.choice(weird)}
            if rng.random()<0.5: a["reasoning_content"]=rng.choice(weird)
            if rng.random()<0.4:
                a["tool_calls"]=[{"id":"c1","type":"function","function":{"name":"run","arguments":{"cmd":rng.choice(weird),"n":rng.choice([1,2.5,True,None,[1,"a"],{"k":"v"}])}}} for _ in range(rng.randint(1,2))]
                msgs.append(a)
                for _ in a["tool_calls"]: msgs.append({"role":"tool","tool_call_id":"c1","content":rng.choice(weird)})
                if rng.random()<0.5: msgs.append({"role":"user","content":"next"})
                else: break
            else: msgs.append(a)
    if msgs[-1]["role"]=="assistant" and rng.random()<0.5: msgs.append({"role":"user","content":"end"})
    convs.append(msgs)
convs.append([{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":" here"}]}])
options = list(ct._OPTIONS) + [{"add_generation_prompt":True,"tools":[ct._TOOLS[0]["function"] and ct._TOOLS[0], tool2]}]
cases=[]
for src in templates:
    if src != source: src=open(src).read()
    for m in convs:
        for o in options:
            cases.append((src,m,o))
special = tok.special_tokens_map
bad=0; n=0
by_src={}
for src,m,o in cases: by_src.setdefault(src,[]).append((m,o))
for src, items in by_src.items():
    path=__import__('tempfile').mktemp(suffix='.jinja')
    open(path,'w').write(src)
    lines=[]
    for m,o in items:
        v={**special, "messages":m, "tools":o.get("tools"), "documents":None, "add_generation_prompt":o.get("add_generation_prompt",False)}
        v.update({k:val for k,val in o.items() if k not in("tools","add_generation_prompt")})
        lines.append(json.dumps(v, ensure_ascii=False))
    res=subprocess.run(["build/tplcheck",path],input=("\n".join(lines)+"\n").encode(),capture_output=True)
    if res.returncode: print(res.stderr.decode()); sys.exit(1)
    outs=[json.loads(l) for l in res.stdout.decode().splitlines()]
    for (m,o),g in zip(items,outs):
        n+=1
        try: want={"out":tok.apply_chat_template(copy.deepcopy(m), tokenize=False, chat_template=src, **o)}
        except Exception as e: want={"error":str(e)}
        if ("out" in want) != ("out" in g) or ("out" in want and want["out"]!=g["out"]):
            bad+=1
            if bad<=4:
                print("MISMATCH", json.dumps(o)[:150], json.dumps(m)[:200])
                w=want.get("out",want.get("error")); gg=g.get("out",g.get("error"))
                i=next((i for i,(a,b) in enumerate(zip(w,gg)) if a!=b), min(len(w),len(gg)))
                print("  py:", repr(w[max(0,i-80):i+120])); print("  go:", repr(gg[max(0,i-80):i+120]))
print(f"{n} renders, {bad} mismatches")
