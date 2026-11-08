import json, sys, random, glob, subprocess, unicodedata
from transformers import AutoTokenizer
tdir = sys.argv[1]
from tokenizers import Tokenizer
raw = Tokenizer.from_file(tdir + "/tokenizer.json")
class T:
    def __call__(self, s, add_special_tokens=False): return {"input_ids": raw.encode(s, add_special_tokens=False).ids}
    def decode(self, ids, skip_special_tokens=True, clean_up_tokenization_spaces=False): return raw.decode(ids, skip_special_tokens=True)
tok = T()
corpus = []
for path in sorted(glob.glob('server/**/*.py', recursive=True))[:40] + glob.glob('docs/*.md') + ['runtime/main.mm']:
    text = open(path, encoding='utf-8', errors='replace').read()
    corpus.append(text)
    corpus += [text[i:i+300] for i in range(0, len(text), 777)]
rng = random.Random(7)
pools = ["hello", " world", "'s", "'LL", "'Re", "\n", "\r\n", "  ", "\t", "  \n ", "123", "٣٤", "é", "é", "日本語", "한국어", "😀", "👨‍👩‍👧", " ", "　", "--", "...", "<think>", "</think>", "<|im_end|>", "<tool_call>", "<|im_start|>assistant\n", "ſ", "Ⅻ", "x²", "​", "ﬁ", "Å", "ά", "́́", "$$", "{\"a\": 1}", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", " \n\n  \n", "def f(x):\n    return x\n"]
for _ in range(3000):
    corpus.append("".join(rng.choice(pools) for _ in range(rng.randint(1, 12))))
for _ in range(500):
    corpus.append("".join(chr(rng.choice([rng.randint(32,126), rng.randint(0xa0,0x2fff), rng.randint(0x3000,0x9fff), rng.randint(0x1f300,0x1faff)])) for _ in range(rng.randint(1,40))))
corpus.append("A"*20000 + "==")
lines = "\n".join(json.dumps(s) for s in corpus) + "\n"
res = subprocess.run(["build/tokcheck", tdir + "/tokenizer.json"], input=lines.encode(), capture_output=True)
if res.returncode: print(res.stderr.decode()); sys.exit(1)
outs = [json.loads(l) for l in res.stdout.decode().splitlines()]
bad = 0
for s, o in zip(corpus, outs):
    want = tok(s, add_special_tokens=False)["input_ids"]
    if o["ids"] != want:
        bad += 1
        if bad <= 5:
            print("ENCODE MISMATCH", repr(s[:120])); print(" go", o["ids"][:40]); print(" py", want[:40])
    dec = tok.decode(want, skip_special_tokens=True, clean_up_tokenization_spaces=False)
    if o["text"] != dec:
        bad += 1
        if bad <= 5: print("DECODE MISMATCH", repr(s[:80]), repr(o["text"][:80]), repr(dec[:80]))
print(f"{len(corpus)} strings, {bad} mismatches")
