"""Same workload for either server: 8 concurrent streamed chats with long
prompts, plus Messages and tool-call requests; samples the server's RSS
and footprint (sys.argv[1] = server pid)."""
import json, subprocess, sys, threading, time, urllib.request, glob
pid = sys.argv[1]
M = "Jundot/Qwen3.8-27B-oQ4e-mtp"
src = "".join(open(p).read() for p in sorted(glob.glob("server/*.py"))[:12])[:60000]
def post(path, body):
    req = urllib.request.Request("http://127.0.0.1:8000" + path, json.dumps(body).encode(), {"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=600) as r:
        return r.read()
def chat(i):
    post("/v1/chat/completions", {"model": M, "stream": True, "max_tokens": 64, "reasoning_effort": "none",
        "messages": [{"role": "system", "content": f"Reviewer {i}."}, {"role": "user", "content": src[i*500:] + "\nSummarize in one line."}]})
def msgs(i):
    post("/v1/messages", {"model": M, "max_tokens": 64, "messages": [{"role": "user", "content": src[:20000] + f"\nOne word {i}."}],
        "tools": [{"name": "t", "input_schema": {"type": "object", "properties": {"a": {"type": "string"}}}}]})
peak = {"rss": 0, "fp": 0}
stop = False
def sample():
    while not stop:
        rss = int(subprocess.run(["ps", "-o", "rss=", "-p", pid], capture_output=True, text=True).stdout.strip() or 0) / 1024
        fp = subprocess.run(["footprint", pid], capture_output=True, text=True).stdout
        line = next((l for l in fp.splitlines() if "Footprint:" in l), "")
        v = line.split("Footprint:")[1].split("(")[0].strip() if line else "0 MB"
        num, unit = v.split()
        mb = float(num) / (1024 if unit == "KB" else 1) * (1024 if unit == "GB" else 1)
        peak["rss"] = max(peak["rss"], rss); peak["fp"] = max(peak["fp"], mb)
        time.sleep(0.5)
t = threading.Thread(target=sample); t.start()
start = time.time()
ths = [threading.Thread(target=chat, args=(i,)) for i in range(8)] + [threading.Thread(target=msgs, args=(i,)) for i in range(4)]
[x.start() for x in ths]; [x.join() for x in ths]
stop = True; t.join()
print(f"workload {time.time()-start:.1f}s  peak RSS {peak['rss']:.1f} MB  peak footprint {peak['fp']:.1f} MB")
