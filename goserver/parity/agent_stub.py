import json, os, sys, pathlib
name = pathlib.Path(sys.argv[0]).name
if sys.argv[1:2] == ["--version"]:
    print("2.1.0"); sys.exit(0)
if name == "hermes" and sys.argv[1:3] == ["profile", "create"]:
    home = pathlib.Path(os.environ.get("HERMES_HOME", pathlib.Path.home() / ".hermes")) / "profiles" / sys.argv[3]
    home.mkdir(parents=True, exist_ok=True)
    (home / "config.yaml").write_text("model:\n  default: other\n  extra: keep\ntoolsets:\n- web\n")
    sys.exit(0)
keys = sorted(k for k in os.environ if k.startswith(("ANTHROPIC", "CLAUDE_CODE", "OPENCODE", "SPLASH", "HERMES", "CUSTOM", "OPENAI", "NO_PROXY", "no_proxy")))
out = {"argv": [pathlib.Path(sys.argv[0]).name] + sys.argv[1:], "env": {k: os.environ[k] for k in keys}}
if "OPENCODE_CONFIG_CONTENT" in out["env"]:
    out["env"]["OPENCODE_CONFIG_CONTENT"] = json.loads(out["env"]["OPENCODE_CONFIG_CONTENT"])
for f in [os.environ.get("PI_CODING_AGENT_DIR", "") + "/models.json"]:
    if os.path.exists(f): out["pi_models"] = json.load(open(f))
hh = os.environ.get("HERMES_HOME")
if hh and os.path.exists(hh + "/config.yaml"):
    import yaml; out["hermes_config"] = yaml.safe_load(open(hh + "/config.yaml"))
print(json.dumps(out, sort_keys=True))
