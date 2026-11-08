package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"splash/goserver/ojson"
)

// The agent commands of install/clients.py.

var installURLs = map[string]string{
	"claude":   "https://code.claude.com/docs/en/overview",
	"opencode": "https://opencode.ai/docs/",
	"codex":    "https://developers.openai.com/codex/cli/",
	"hermes":   "https://hermes-agent.nousresearch.com/docs/getting-started/installation/",
	"pi":       "https://pi.dev/",
}

const clientResponseTokens = 32768

var opencodeAgents = []string{"build", "plan", "general", "explore", "title", "compaction"}
var opencodeEfforts = []string{"none", "low", "medium", "high", "xhigh"}

const opencodeConfigError = "OPENCODE_CONFIG_CONTENT must be a JSON object"

type agentServer struct {
	baseURL     string
	model       string
	context     int
	modalities  []string
	apiKey      string
	laterSystem bool
}

func (s agentServer) endpoint() string { return s.baseURL + "/v1" }

func (s agentServer) responseTokens() int {
	q := s.context / 4
	if q < 1 {
		q = 1
	}
	if q < clientResponseTokens {
		return q
	}
	return clientResponseTokens
}

func (s agentServer) name() string {
	u, _ := url.Parse(s.baseURL)
	if u != nil && u.Port() == strconv.Itoa(defaultPort) {
		return "splash"
	}
	if u != nil {
		return "splash-" + u.Port()
	}
	return "splash"
}

func (s agentServer) hasModality(m string) bool {
	for _, x := range s.modalities {
		if x == m {
			return true
		}
	}
	return false
}

// pyDumps is Python's json.dumps(v) with its defaults.
func pyDumps(v any) string {
	s, _ := ojson.Dumps(v, ojson.Options{EnsureASCII: true, ItemSep: ", ", KeySep: ": "})
	return s
}

// directClient reaches the server on loopback without any proxy, which
// would otherwise receive the server's key.
var directClient = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Second}

func requestJSON(port int, path string) (any, error) {
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if key := os.Getenv("SPLASH_API_KEY"); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := directClient.Do(req)
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return nil, errors.New("Splash authentication failed; set SPLASH_API_KEY to the server's key")
	}
	if resp.StatusCode != 200 {
		return nil, nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil
	}
	v, err := ojson.Decode(data)
	if err != nil {
		return nil, nil
	}
	return v, nil
}

// splitAgentArgs takes Splash's --port out of an agent command's
// arguments; the rest, and everything after --, go to the agent.
func splitAgentArgs(argv []string) (int, bool, []string, error) {
	port, explicit := 0, false
	var rest []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			if len(rest) == 0 {
				rest = append(rest, argv[i+1:]...)
			} else {
				rest = append(rest, argv[i:]...)
			}
			break
		}
		if a == "--port" && i+1 < len(argv) {
			p, err := parsePort(argv[i+1])
			if err != nil {
				return 0, false, nil, fmt.Errorf("argument --port: %v", err)
			}
			port, explicit = p, true
			i++
			continue
		}
		if v, ok := strings.CutPrefix(a, "--port="); ok {
			p, err := parsePort(v)
			if err != nil {
				return 0, false, nil, fmt.Errorf("argument --port: %v", err)
			}
			port, explicit = p, true
			continue
		}
		rest = append(rest, a)
	}
	if !explicit {
		p, err := environmentPort()
		if err != nil {
			return 0, false, nil, fmt.Errorf("SPLASH_PORT: %v", err)
		}
		port = p
	}
	return port, explicit, rest, nil
}

func runAgent(p Paths, name string, argv []string) error {
	port, explicit, clientArgs, err := splitAgentArgs(argv)
	if err != nil {
		return err
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("%s is not installed or is not on PATH. Install it first: %s", name, installURLs[name])
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	listing, err := requestJSON(port, "/v1/models")
	if err != nil {
		return err
	}
	if listing == nil {
		msg := fmt.Sprintf("No ready Splash server at %s. Run 'splash serve --model <HF_REPO_ID>' in another terminal first.", base)
		if explicit {
			msg += fmt.Sprintf(" If --port was meant for %s itself, put it after --.", name)
		}
		return errors.New(msg)
	}
	lo, _ := listing.(*ojson.Object)
	data, _ := lo.Value("data").([]any)
	var first *ojson.Object
	if len(data) > 0 {
		first, _ = data[0].(*ojson.Object)
	}
	ctx, ok := first.Value("context_length").(int64)
	if first == nil || first.Value("owned_by") != "splash" || !ok || ctx <= 0 {
		return errors.New("Could not identify the local Splash server")
	}
	model, _ := first.Value("id").(string)
	if model == "" {
		return errors.New("The server did not report a model name")
	}
	var modalities []string
	rawModalities, _ := first.Value("input_modalities").([]any)
	hasText := false
	for _, m := range rawModalities {
		s, isStr := m.(string)
		if !isStr {
			hasText = false
			break
		}
		modalities = append(modalities, s)
		if s == "text" {
			hasText = true
		}
	}
	if !hasText {
		return errors.New("The server did not report its input modalities; it predates this launcher, so restart it with this version of Splash")
	}
	env := environMap(os.Environ())
	key := env["SPLASH_API_KEY"]
	if key == "" {
		key = "local"
	}
	srv := agentServer{baseURL: base, model: model, context: int(ctx), modalities: modalities, apiKey: key}
	if name == "pi" {
		srv.laterSystem = rendersLaterSystem(port)
	}
	var command []string
	switch name {
	case "claude":
		command = claudeCommand(path, srv, env, clientArgs)
	case "opencode":
		command, err = opencodeCommand(path, srv, env, clientArgs, probeMajorVersion(path))
	case "codex":
		command, err = codexCommand(path, srv, env, clientArgs)
	case "hermes":
		command, err = hermesCommand(path, srv, env, clientArgs)
	case "pi":
		command, err = piCommand(path, srv, env, clientArgs)
	}
	if err != nil {
		return err
	}
	reachLoopbackDirectly(env)
	fmt.Printf("Starting %s: %s · %s context tokens\n", name, model, commas(int(ctx)))
	switch name {
	case "claude":
		fmt.Println("Claude hosted WebSearch is unavailable. WebFetch, local tools and MCP are unchanged.")
	case "codex":
		fmt.Println("Codex hosted WebSearch is disabled: Splash does not provide OpenAI's search service. Local tools and MCP are unchanged.")
	}
	return syscall.Exec(path, command, environList(env))
}

func commas(n int) string {
	s := strconv.Itoa(n)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

func environMap(list []string) map[string]string {
	m := map[string]string{}
	for _, kv := range list {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func environList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func rendersLaterSystem(port int) bool {
	status, _ := requestJSON(port, "/status")
	so, _ := status.(*ojson.Object)
	tpl, _ := so.Value("chat_template").(*ojson.Object)
	outcome := tpl.Value("later_system")
	var outcomes []any
	if o, ok := outcome.(*ojson.Object); ok {
		for _, k := range o.Keys() {
			outcomes = append(outcomes, o.Value(k))
		}
	} else {
		outcomes = []any{outcome}
	}
	for _, o := range outcomes {
		if o != "native" && o != "patched" {
			return false
		}
	}
	return true
}

var versionRE = regexp.MustCompile(`^(?:opencode\s+)?v?(\d+)\.\d+(?:\.\d+)?(?:[-+\s]|$)`)

// probeMajorVersion is the major version a client's --version names; -1
// for unknown.
func probeMajorVersion(path string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return -1
	}
	m := versionRE.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return -1
	}
	v, _ := strconv.Atoi(m[1])
	return v
}

var loopbackHosts = []string{"127.0.0.1", "localhost", "::1"}

// reachLoopbackDirectly exempts loopback from an environment proxy
// (clients._reach_loopback_directly).
func reachLoopbackDirectly(env map[string]string) {
	proxied := false
	for k, v := range env {
		switch strings.ToLower(k) {
		case "http_proxy", "https_proxy", "all_proxy":
			if v != "" {
				proxied = true
			}
		}
	}
	if !proxied {
		return
	}
	found := map[string]string{}
	for _, pair := range [][2]string{{"NO_PROXY", "no_proxy"}, {"no_proxy", "NO_PROXY"}} {
		v := env[pair[0]]
		if v == "" {
			v = env[pair[1]]
		}
		found[pair[0]] = v
	}
	for name, value := range found {
		var hosts []string
		for _, h := range strings.Split(value, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hosts = append(hosts, h)
			}
		}
		var missing []string
		for _, h := range loopbackHosts {
			present := false
			for _, x := range hosts {
				if x == h {
					present = true
				}
			}
			if !present {
				missing = append(missing, h)
			}
		}
		if len(missing) > 0 && !(len(hosts) == 1 && hosts[0] == "*") {
			env[name] = strings.Join(append(hosts, missing...), ",")
		}
	}
}

func claudeCommand(path string, s agentServer, env map[string]string, args []string) []string {
	for k, v := range map[string]string{
		"ANTHROPIC_BASE_URL": s.baseURL, "ANTHROPIC_AUTH_TOKEN": s.apiKey, "ANTHROPIC_MODEL": s.model,
		"ANTHROPIC_DEFAULT_OPUS_MODEL": s.model, "ANTHROPIC_DEFAULT_SONNET_MODEL": s.model,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL": s.model, "ANTHROPIC_SMALL_FAST_MODEL": s.model,
		"CLAUDE_CODE_MAX_CONTEXT_TOKENS": strconv.Itoa(s.context), "CLAUDE_CODE_AUTO_COMPACT_WINDOW": strconv.Itoa(s.context),
		"CLAUDE_CODE_USE_BEDROCK": "0", "CLAUDE_CODE_USE_VERTEX": "0", "CLAUDE_CODE_USE_FOUNDRY": "0",
	} {
		env[k] = v
	}
	delete(env, "ANTHROPIC_API_KEY")
	return append([]string{path, "--disallowedTools", "WebSearch", "--model", s.model, "--permission-mode", "default"}, args...)
}

func objectAt(v *ojson.Object, keys ...string) (*ojson.Object, error) {
	for _, k := range keys {
		next, has := v.Get(k)
		if !has {
			return ojson.NewObject(), nil
		}
		o, ok := next.(*ojson.Object)
		if !ok {
			return nil, fmt.Errorf("OPENCODE_CONFIG_CONTENT: '%s' must be a JSON object", k)
		}
		v = o
	}
	return v, nil
}

func opencodeCommand(path string, s agentServer, env map[string]string, args []string, version int) ([]string, error) {
	text := env["OPENCODE_CONFIG_CONTENT"]
	if text == "" {
		text = "{}"
	}
	v, err := ojson.DecodeString(text)
	config, ok := v.(*ojson.Object)
	if err != nil || !ok {
		return nil, errors.New(opencodeConfigError)
	}
	served := "splash/" + s.model
	config.Set("model", served)
	config.Set("small_model", served)
	agentsObj, err := objectAt(config, "agent")
	if err != nil {
		return nil, err
	}
	config.Set("agent", agentsObj)
	for _, a := range opencodeAgents {
		existing, err := objectAt(agentsObj, a)
		if err != nil {
			return nil, err
		}
		merged := existing.Copy()
		merged.Set("model", served)
		agentsObj.Set(a, merged)
	}
	provider, err := objectAt(config, "provider")
	if err != nil {
		return nil, err
	}
	config.Set("provider", provider)
	variants := ojson.NewObject()
	for _, e := range opencodeEfforts {
		variants.Set(e, ojson.O("reasoningEffort", e))
	}
	userVariants, err := objectAt(provider, "splash", "models", s.model, "variants")
	if err != nil {
		return nil, err
	}
	for _, k := range userVariants.Keys() {
		variants.Set(k, userVariants.Value(k))
	}
	modalities := make([]any, len(s.modalities))
	for i, m := range s.modalities {
		modalities[i] = m
	}
	output := s.responseTokens()
	input := s.context - output
	if input < 1 {
		input = 1
	}
	provider.Set("splash", ojson.O(
		"npm", "@ai-sdk/openai-compatible",
		"name", "Splash",
		"options", ojson.O("baseURL", s.endpoint(), "apiKey", s.apiKey),
		"models", ojson.O(s.model, ojson.O(
			"name", s.model,
			"reasoning", true,
			"variants", variants,
			"attachment", !(len(s.modalities) == 1 && s.modalities[0] == "text"),
			"modalities", ojson.O("input", modalities, "output", []any{"text"}),
			"limit", ojson.O("context", int64(s.context), "input", int64(input), "output", int64(output)),
		)),
	))
	env["OPENCODE_CONFIG_CONTENT"] = pyDumps(config)
	argv := append([]string{path}, args...)
	if version >= 2 && !selectsOpencodeServer(args) {
		at := len(argv)
		for i, a := range argv {
			if a == "--" {
				at = i
				break
			}
		}
		argv = append(argv[:at], append([]string{"--standalone"}, argv[at:]...)...)
	}
	return argv, nil
}

func selectsOpencodeServer(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--standalone" || a == "--server" || strings.HasPrefix(a, "--server=") {
			return true
		}
	}
	return false
}

func codexCommand(path string, s agentServer, env map[string]string, args []string) ([]string, error) {
	env["SPLASH_API_KEY"] = s.apiKey
	settings := [][2]string{
		{"model", pyDumps(s.model)},
		{"web_search", pyDumps("disabled")},
		{"model_provider", pyDumps("splash")},
		{"model_providers.splash", `{name="Splash",base_url=` + pyDumps(s.endpoint()) + `,env_key="SPLASH_API_KEY",wire_api="responses"}`},
		{"model_context_window", strconv.Itoa(s.context)},
		{"model_auto_compact_token_limit", strconv.Itoa(s.context * 9 / 10)},
	}
	argv := []string{path}
	for _, kv := range settings {
		argv = append(argv, "-c", kv[0]+"="+kv[1])
	}
	var overrides, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			rest = append(rest, args[i:]...)
			i = len(args)
		case a == "-c" || a == "--config":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s requires a config override", a)
			}
			overrides = append(overrides, "-c", args[i+1])
			i++
		case strings.HasPrefix(a, "--config="):
			overrides = append(overrides, "-c", strings.SplitN(a, "=", 2)[1])
		case strings.HasPrefix(a, "-c") && len(a) > 2:
			overrides = append(overrides, "-c", strings.TrimPrefix(a[2:], "="))
		default:
			rest = append(rest, a)
		}
	}
	return append(append(argv, overrides...), rest...), nil
}

// hermesProfileHome is clients.hermes_profile_home.
func hermesProfileHome(env map[string]string, name string) string {
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".hermes")
	if value := strings.TrimSpace(env["HERMES_HOME"]); value != "" {
		h := os.Expand(value, func(k string) string { return env[k] })
		if strings.HasPrefix(h, "~") {
			h = home + h[1:]
		}
		resolved, err := filepath.EvalSymlinks(h)
		if err != nil {
			resolved, _ = filepath.Abs(h)
		}
		rootResolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			rootResolved = root
		}
		if resolved != rootResolved && !strings.HasPrefix(resolved, rootResolved+string(os.PathSeparator)) {
			if filepath.Base(filepath.Dir(h)) == "profiles" {
				root = filepath.Dir(filepath.Dir(h))
			} else {
				root = h
			}
		}
	}
	return filepath.Join(root, "profiles", name)
}

func hermesCommand(path string, s agentServer, env map[string]string, args []string) ([]string, error) {
	home := hermesProfileHome(env, s.name())
	created := false
	if _, err := os.Stat(filepath.Join(home, "config.yaml")); err != nil {
		cmd := exec.Command(path, "profile", "create", filepath.Base(home), "--no-alias")
		cmd.Env = environList(env)
		out, err := cmd.CombinedOutput()
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return nil, fmt.Errorf("Could not run %s: %v", path, err)
			}
			if info, serr := os.Stat(home); serr != nil || !info.IsDir() {
				text := strings.TrimSpace(string(out))
				if len(text) > 2000 {
					text = text[len(text)-2000:]
				}
				return nil, fmt.Errorf("hermes profile create %s failed: %s", filepath.Base(home), text)
			}
		} else {
			created = true
		}
	}
	if err := writeHermesProfile(home, s, env, created); err != nil {
		return nil, err
	}
	env["HERMES_HOME"] = home
	env["CUSTOM_BASE_URL"] = s.endpoint()
	env["OPENAI_BASE_URL"] = s.endpoint()
	env["OPENAI_API_KEY"] = s.apiKey
	return append([]string{path, "--provider", "custom", "--model", s.model}, args...), nil
}

func yamlScalar(v any) *yaml.Node {
	n := &yaml.Node{}
	_ = n.Encode(v)
	return n
}

// setMapping sets key in a YAML mapping node, in place or appended.
func setMapping(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func writeHermesProfile(home string, s agentServer, env map[string]string, created bool) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	path := filepath.Join(home, "config.yaml")
	invalid := fmt.Errorf("Invalid Hermes profile: %s", path)
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
		var parsed yaml.Node
		if err := yaml.Unmarshal(data, &parsed); err != nil {
			return invalid
		}
		if len(parsed.Content) == 1 {
			root := parsed.Content[0]
			if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
				// An empty profile.
			} else if root.Kind != yaml.MappingNode {
				return invalid
			} else {
				doc = &parsed
			}
		}
	}
	root := doc.Content[0]
	var model *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "model" {
			model = root.Content[i+1]
		}
	}
	if created || model == nil || (model.Kind == yaml.ScalarNode && model.Tag == "!!null") {
		model = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	if model.Kind != yaml.MappingNode {
		return invalid
	}
	apiKey := "local"
	if env["SPLASH_API_KEY"] != "" {
		apiKey = "${SPLASH_API_KEY}"
	}
	for _, kv := range []struct {
		k string
		v any
	}{{"default", s.model}, {"provider", "custom"}, {"base_url", s.endpoint()}, {"api_key", apiKey},
		{"api_mode", "chat_completions"}, {"supports_vision", s.hasModality("image")},
		{"context_length", s.context}, {"max_tokens", s.responseTokens()}} {
		setMapping(model, kv.k, yamlScalar(kv.v))
	}
	setMapping(root, "model", model)
	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return replaceFile(path, out)
}

func piCommand(path string, s agentServer, env map[string]string, args []string) ([]string, error) {
	provider := s.name()
	dir := env["PI_CODING_AGENT_DIR"]
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".pi/agent")
	} else if strings.HasPrefix(dir, "~") {
		home, _ := os.UserHomeDir()
		dir = home + dir[1:]
	}
	if err := writePiProvider(filepath.Join(dir, "models.json"), provider, s, env); err != nil {
		return nil, err
	}
	return append([]string{path, "--provider", provider, "--model", s.model}, args...), nil
}

func writePiProvider(path, provider string, s agentServer, env map[string]string) error {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(dir.Fd()), syscall.LOCK_UN)
	invalid := fmt.Errorf("Invalid Pi models.json: %s", path)
	config := ojson.NewObject()
	if data, err := os.ReadFile(path); err == nil {
		v, err := ojson.Decode(data)
		if err != nil {
			return invalid
		}
		o, ok := v.(*ojson.Object)
		if !ok {
			return invalid
		}
		config = o
	}
	providers := ojson.NewObject()
	if v, has := config.Get("providers"); has {
		p, ok := v.(*ojson.Object)
		if !ok {
			return invalid
		}
		providers = p.Copy()
	}
	apiKey := "local"
	if env["SPLASH_API_KEY"] != "" {
		apiKey = "!printenv SPLASH_API_KEY"
	}
	input := []any{"text"}
	if s.hasModality("image") {
		input = []any{"text", "image"}
	}
	model := ojson.O("id", s.model, "reasoning", true, "thinkingLevelMap", ojson.O("off", "none"),
		"input", input, "contextWindow", int64(s.context), "maxTokens", int64(clientResponseTokens))
	if s.laterSystem {
		model.Set("compat", ojson.O("supportsMidConvoSystemMessages", true))
	}
	providers.Set(provider, ojson.O("baseUrl", s.endpoint(), "api", "openai-completions", "apiKey", apiKey, "models", []any{model}))
	config.Set("providers", providers)
	indent := 2
	text, err := ojson.Dumps(config, ojson.Options{EnsureASCII: true, Indent: &indent, ItemSep: ",", KeySep: ": "})
	if err != nil {
		return err
	}
	return replaceFile(path, []byte(text+"\n"))
}

// replaceFile writes path only once the text is complete, keeping its mode;
// a new file is private.
func replaceFile(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
