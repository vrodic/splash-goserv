package server

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"splash/goserver/jinja"
	"splash/goserver/llg"
	"splash/goserver/ojson"
	"splash/goserver/tokenizer"
	"splash/goserver/wire"
)

// Args are the server's command line (server.server.parse_args and
// serve_options.SERVE_OPTIONS).
type Args struct {
	ModelRoot, Model, Binary, ServeRecord string
	Port                                  int
	Host                                  string
	AllowedHosts, AllowedOrigins          []string
	APIKey                                string
	NoWebUI                               bool
	MaxRequest                            int64
	QueueSize                             int
	RequestTimeout                        float64
	ServedNames                           []string
	AnnounceServedName                    bool
	DefaultEffort                         *string
	ThinkingBudget                        *int
	ThinkingEndText                       *string
	MaxImagePixels                        int64
	// The engine's arguments after MODEL_DIRECTORY.
	maxContext, maxMemory, maxCacheDisk string
	engineFlags                         []string
}

const maxPixels = 4_194_304
const minPixels = 65_536

var sizeRE = regexp.MustCompile(`^\d+(\.\d+)?$`)

func parseSize(v string) (int64, error) {
	n := strings.ToUpper(strings.TrimSpace(v))
	mult := int64(1)
	for _, u := range []struct {
		s string
		m int64
	}{{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}} {
		if strings.HasSuffix(n, u.s) {
			n, mult = strings.TrimSuffix(n, u.s), u.m
			break
		}
	}
	m := sizeRE.FindStringSubmatch(n)
	if m == nil || (mult == 1 && m[1] != "") {
		return 0, errors.New("must be 'auto' or a positive byte count such as 32G or 2.5G")
	}
	f, _ := strconv.ParseFloat(n, 64)
	size := int64(f * float64(mult))
	if mult == 1 {
		size, _ = strconv.ParseInt(n, 10, 64)
	}
	if size < 1 {
		return 0, errors.New("must be 'auto' or a positive byte count such as 32G or 2.5G")
	}
	return size, nil
}

func parseDuration(v string) (float64, bool) {
	t := strings.ToLower(strings.TrimSpace(v))
	unit := map[byte]float64{'s': 1, 'm': 60, 'h': 3600}
	var secs float64
	var err error
	if len(t) > 0 && unit[t[len(t)-1]] != 0 {
		secs, err = strconv.ParseFloat(t[:len(t)-1], 64)
		secs *= unit[t[len(t)-1]]
	} else {
		secs, err = strconv.ParseFloat(t, 64)
	}
	if err != nil || math.IsInf(secs, 0) || math.IsNaN(secs) || secs <= 0 {
		return 0, false
	}
	return secs, true
}

// The server's options that take a value, and its switches.
var valueFlags = map[string]bool{"--model": true, "--port": true, "--binary": true, "--serve-record": true, "--host": true,
	"--allowed-host": true, "--allowed-origin": true, "--api-key": true, "--max-memory": true, "--max-context": true,
	"--ple-memory": true, "--expert-memory": true, "--sparse-kv-memory": true, "--kv-format": true,
	"--state-format": true, "--idle-release": true, "--max-cache-disk": true, "--max-cache-disk-writes": true,
	"--cache-dir": true, "--served-model-name": true, "--default-reasoning-effort": true,
	"--thinking-token-budget": true, "--thinking-end-text": true, "--max-request-size": true,
	"--max-image-pixels": true, "--request-timeout": true, "--queue-size": true, "--decode-share": true,
	"--verify-rows": true}
var switchFlags = map[string]bool{"--no-webui": true, "--persistent-cache": true, "--announce-served-name": true,
	"--disable-ane": true, "--allow-idle-sleep": true}

// FlagKind says how the server reads option name: as one taking a value, a
// switch, or not at all.
func FlagKind(name string) (takesValue, known bool) {
	if valueFlags[name] {
		return true, true
	}
	return false, switchFlags[name]
}

// ParseArgs reads argv as argparse does: --flag value or --flag=value.
func ParseArgs(argv []string) (*Args, error) {
	a := &Args{Port: 8000, Host: "127.0.0.1", MaxRequest: 128 << 20, QueueSize: 32, MaxImagePixels: maxPixels,
		maxContext: "auto", maxMemory: "auto"}
	a.APIKey = os.Getenv("SPLASH_API_KEY")
	if v, ok := os.LookupEnv("SPLASH_DEFAULT_REASONING_EFFORT"); ok {
		a.DefaultEffort = &v
	}
	if v, ok := os.LookupEnv("SPLASH_THINKING_TOKEN_BUDGET"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, errors.New("--thinking-token-budget: must be a nonnegative integer")
		}
		a.ThinkingBudget = &n
	}
	if v, ok := os.LookupEnv("SPLASH_THINKING_END_TEXT"); ok {
		a.ThinkingEndText = &v
	}
	takes, switches := valueFlags, switchFlags
	var positional []string
	persistent, cacheDir := false, ""
	var cacheDisk *int64
	cacheWrites := int64(0)
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if !strings.HasPrefix(arg, "--") {
			positional = append(positional, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if switches[name] {
			switch name {
			case "--no-webui":
				a.NoWebUI = true
			case "--persistent-cache":
				persistent = true
				a.engineFlags = append(a.engineFlags, "--persistent-cache", "on")
			case "--announce-served-name":
				a.AnnounceServedName = true
			case "--disable-ane":
				a.engineFlags = append(a.engineFlags, "--ane", "off")
			case "--allow-idle-sleep":
				a.engineFlags = append(a.engineFlags, "--idle-sleep", "allow")
			}
			continue
		}
		if !takes[name] {
			return nil, fmt.Errorf("unrecognized arguments: %s", arg)
		}
		if !hasValue {
			if i+1 >= len(argv) {
				return nil, fmt.Errorf("argument %s: expected one argument", name)
			}
			i++
			value = argv[i]
		}
		bad := func(msg string) error { return fmt.Errorf("argument %s: %s", name, msg) }
		switch name {
		case "--model":
			a.Model = value
		case "--port":
			p, err := strconv.Atoi(value)
			if err != nil || p < 0 || p > 65535 {
				return nil, errors.New("--port must be in [0, 65535]")
			}
			a.Port = p
		case "--binary":
			a.Binary = value
		case "--serve-record":
			a.ServeRecord = value
		case "--host":
			a.Host = value
		case "--allowed-host":
			a.AllowedHosts = append(a.AllowedHosts, value)
		case "--allowed-origin":
			o, err := ParseAllowedOrigin(value)
			if err != nil {
				return nil, bad(err.Error())
			}
			a.AllowedOrigins = append(a.AllowedOrigins, o)
		case "--api-key":
			a.APIKey = value
		case "--max-memory", "--max-context":
			v := strings.ToLower(strings.TrimSpace(value))
			if v != "auto" {
				if name == "--max-context" {
					u := strings.ToUpper(v)
					mult := 1
					if strings.HasSuffix(u, "K") {
						u, mult = u[:len(u)-1], 1024
					} else if strings.HasSuffix(u, "M") {
						u, mult = u[:len(u)-1], 1000000
					}
					n, err := strconv.Atoi(u)
					if err != nil || n*mult < 1 || n*mult > 1010000 {
						return nil, bad("must be 'auto' or a token count up to 1010000, such as 100K or 1M")
					}
					a.maxContext = strconv.Itoa(n * mult)
				} else {
					n, err := parseSize(value)
					if err != nil {
						return nil, bad(err.Error())
					}
					a.maxMemory = strconv.FormatInt(n, 10)
				}
			}
		case "--ple-memory", "--expert-memory", "--sparse-kv-memory":
			v := strings.ToLower(strings.TrimSpace(value))
			if v == "auto" {
				continue
			}
			if v != "off" || name == "--sparse-kv-memory" {
				n, err := parseSize(value)
				if err != nil {
					return nil, bad("use auto, off, or a size")
				}
				v = strconv.FormatInt(n, 10)
			}
			a.engineFlags = append(a.engineFlags, name, v)
		case "--kv-format":
			if value != "int8" && value != "bf16" {
				return nil, bad("invalid choice")
			}
			if value != "int8" {
				a.engineFlags = append(a.engineFlags, name, value)
			}
		case "--state-format":
			if value != "fp32" && value != "bf16" {
				return nil, bad("invalid choice")
			}
			if value != "fp32" {
				a.engineFlags = append(a.engineFlags, name, value)
			}
		case "--verify-rows":
			if value != "auto" && value != "all" {
				return nil, bad("invalid choice")
			}
			if value != "auto" {
				a.engineFlags = append(a.engineFlags, name, value)
			}
		case "--idle-release":
			if strings.ToLower(strings.TrimSpace(value)) == "off" {
				a.engineFlags = append(a.engineFlags, name, "off")
			} else {
				secs, ok := parseDuration(value)
				if !ok {
					return nil, bad("must be off or a positive duration such as 30m, 2h or 600")
				}
				a.engineFlags = append(a.engineFlags, name, ojson.FloatRepr(secs))
			}
		case "--max-cache-disk":
			if strings.TrimSpace(value) == "0" {
				z := int64(0)
				cacheDisk = &z
			} else if strings.ToLower(strings.TrimSpace(value)) != "auto" {
				n, err := parseSize(value)
				if err != nil {
					return nil, bad("use auto, 0 to disable, or a size such as 16G")
				}
				cacheDisk = &n
			}
		case "--max-cache-disk-writes":
			if strings.TrimSpace(value) != "0" {
				n, err := parseSize(value)
				if err != nil {
					return nil, bad("use 0 for no limit, or a size such as 200G")
				}
				cacheWrites = n
				a.engineFlags = append(a.engineFlags, name, strconv.FormatInt(n, 10))
			}
		case "--cache-dir":
			p, err := filepath.Abs(os.ExpandEnv(strings.Replace(value, "~", os.Getenv("HOME"), 1)))
			if err != nil || strings.TrimSpace(value) == "" {
				return nil, bad("must name a directory")
			}
			cacheDir = p
			a.engineFlags = append(a.engineFlags, name, p)
		case "--served-model-name":
			a.ServedNames = append(a.ServedNames, value)
		case "--default-reasoning-effort":
			v := value
			a.DefaultEffort = &v
		case "--thinking-token-budget":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return nil, bad("must be a nonnegative integer")
			}
			a.ThinkingBudget = &n
		case "--thinking-end-text":
			v := value
			a.ThinkingEndText = &v
		case "--max-request-size":
			n, err := parseSize(value)
			if err != nil {
				return nil, bad("must be a positive byte count such as 128M")
			}
			a.MaxRequest = n
		case "--max-image-pixels":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < minPixels || n > maxPixels {
				return nil, bad(fmt.Sprintf("must be between %d and %d pixels", minPixels, maxPixels))
			}
			a.MaxImagePixels = n
			if n != maxPixels {
				a.engineFlags = append(a.engineFlags, "--max-image-patches", strconv.FormatInt(n/(16*16)/4*4, 10))
			}
		case "--request-timeout":
			secs, ok := parseDuration(value)
			if !ok {
				return nil, bad("must be a positive duration such as 30m, 2h or 3600")
			}
			a.RequestTimeout = secs
		case "--queue-size":
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 {
				return nil, bad("must be a positive number of requests such as 32")
			}
			a.QueueSize = n
		case "--decode-share":
			f, err := strconv.ParseFloat(value, 64)
			if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
				return nil, bad("must be a nonnegative number such as 0.5")
			}
			a.engineFlags = append(a.engineFlags, name, ojson.FloatRepr(f))
		}
	}
	if len(positional) != 1 {
		return nil, errors.New("expected one MODEL_DIRECTORY")
	}
	a.ModelRoot = positional[0]
	if a.Model == "" {
		return nil, errors.New("the following arguments are required: --model")
	}
	if a.DefaultEffort != nil && !contains(reasoningEfforts, *a.DefaultEffort) {
		return nil, errors.New("--default-reasoning-effort: must be one of " + strings.Join(reasoningEfforts, ", "))
	}
	if a.AnnounceServedName && len(a.ServedNames) == 0 {
		return nil, errors.New("--announce-served-name needs --served-model-name")
	}
	if cacheDisk != nil {
		a.maxCacheDisk = strconv.FormatInt(*cacheDisk, 10)
		if *cacheDisk == 0 && (persistent || cacheWrites > 0) {
			return nil, errors.New("--persistent-cache and --max-cache-disk-writes need the SSD cache --max-cache-disk 0 turns off")
		}
	}
	if cacheDir != "" && !persistent {
		return nil, errors.New("--cache-dir needs --persistent-cache")
	}
	if a.Binary == "" {
		exe, _ := os.Executable()
		a.Binary = filepath.Join(filepath.Dir(exe), "splash")
		if _, err := os.Stat(a.Binary); err != nil {
			// A release keeps the engine in engine/.
			if alt := filepath.Join(filepath.Dir(filepath.Dir(exe)), "engine", "splash"); fileExists(alt) {
				a.Binary = alt
			}
		}
	}
	return a, nil
}

func (a *Args) engineCommand(modelRoot string, first *wire.Ready) []string {
	cmd := []string{a.Binary, "serve-native", modelRoot, a.maxContext, a.maxMemory}
	if a.maxCacheDisk != "" {
		cmd = append(cmd, a.maxCacheDisk)
	}
	cmd = append(cmd, a.engineFlags...)
	if first != nil {
		cmd = append(cmd, "--relaunch-context", strconv.Itoa(int(first.MaxContext)))
	}
	return cmd
}

// ---------------------------------------------------------------- serve record

func writeRecord(path string, record *ojson.Object) error {
	if path == "" {
		return nil
	}
	sibling := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%d", filepath.Base(path), os.Getpid()))
	if err := os.WriteFile(sibling, ojson.Marshal(record), 0o644); err != nil {
		return fmt.Errorf("cannot write the serve record %s: %v", path, err)
	}
	if err := os.Rename(sibling, path); err != nil {
		os.Remove(sibling)
		return fmt.Errorf("cannot write the serve record %s: %v", path, err)
	}
	return nil
}

func recordPhase(path, phase string, mustWrite bool) error {
	err := writeRecord(path, ojson.O("pid", int64(os.Getpid()), "phase", phase))
	if err != nil && !mustWrite {
		printStatus("Warning · "+err.Error(), true)
		return nil
	}
	return err
}

func recordFailure(path string, failure *ojson.Object) {
	if err := writeRecord(path, ojson.O("pid", int64(os.Getpid()), "phase", "failed", "failure", failure)); err != nil {
		printStatus("Warning · "+err.Error(), true)
	}
}

// ---------------------------------------------------------------- model

// holdModel resolves the assembly and holds its model.json under a shared
// lock for the server's life (install/assembly.hold).
func holdModel(path string) (string, *os.File, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, &ModelError{fmt.Sprintf("%s names no model Splash prepared: prepare the model again", path)}
	}
	if filepath.Base(filepath.Dir(resolved)) != ".resolved" {
		return "", nil, &ModelError{fmt.Sprintf("%s is not a model directory Splash prepared", path)}
	}
	modelsRoot := filepath.Dir(filepath.Dir(resolved))
	lock, err := os.OpenFile(filepath.Join(modelsRoot, ".install.lock"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return "", nil, &ModelError{fmt.Sprintf("cannot hold %s in %s: %v", path, modelsRoot, err)}
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Println("Another Splash model installation is running; waiting...")
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	record, err := os.Open(filepath.Join(resolved, "model.json"))
	if err != nil {
		return "", nil, &ModelError{fmt.Sprintf("%s names no model Splash prepared: prepare the model again", path)}
	}
	if err := syscall.Flock(int(record.Fd()), syscall.LOCK_SH); err != nil {
		record.Close()
		return "", nil, &ModelError{fmt.Sprintf("cannot hold %s: %v", path, err)}
	}
	return resolved, record, nil
}

func modelFamily(root string) *Family {
	data, err := os.ReadFile(filepath.Join(root, "model.json"))
	if err != nil {
		return nil
	}
	v, err := ojson.Decode(data)
	if err != nil {
		return nil
	}
	o, _ := v.(*ojson.Object)
	name, _ := o.Value("family").(string)
	return familyNamed(name)
}

func thinkingEndTokens(tok *tokenizer.Tokenizer, text *string, cf *ChatFormat) ([]uint32, error) {
	if text == nil {
		return nil, nil
	}
	ids := tok.Encode(*text)
	bad := len(ids) == 0 || ids[len(ids)-1] != cf.ThinkEndID
	for _, id := range ids[:max(0, len(ids)-1)] {
		if tok.IsAdded(id) {
			bad = true
		}
	}
	if bad {
		return nil, &ModelError{fmt.Sprintf("the thinking end %q must end with %s and hold no other special token", *text, thinkEnd)}
	}
	if len(ids) > wire.MaxThinkingEnd {
		return nil, &ModelError{fmt.Sprintf("the thinking end %q takes %d tokens; the most is %d", *text, len(ids), wire.MaxThinkingEnd)}
	}
	out := make([]uint32, len(ids))
	for i, id := range ids {
		out[i] = uint32(id)
	}
	return out, nil
}

// Main runs the server; it returns the process's exit code.
func Main(argv []string) int {
	args, err := ParseArgs(argv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	if pid := os.Getenv("SPLASH_APP_PID"); pid != "" {
		if n, err := strconv.Atoi(pid); err == nil {
			go watchProcess(n)
		}
	}
	raiseDescriptorLimit()
	failed := func(kind string, err error) int {
		printStatus("Error · "+err.Error(), true)
		recordFailure(args.ServeRecord, ojson.O("kind", kind, "message", err.Error()))
		return 1
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(args.Host, strconv.Itoa(args.Port)))
	if err != nil {
		return failed("port", fmt.Errorf("unable to start HTTP server: %v", err))
	}
	key, err := LoadThinkingKey()
	if err != nil {
		listener.Close()
		return failed("internal", err)
	}
	codec, _ := NewThinkingCodec(key)
	printStatus("Loading · "+args.Model, false)
	_ = recordPhase(args.ServeRecord, "loading", false)
	root, held, err := holdModel(args.ModelRoot)
	if err != nil {
		listener.Close()
		return failed("model", err)
	}
	defer held.Close()
	family := modelFamily(root)
	if family == nil {
		listener.Close()
		return failed("model", fmt.Errorf("%s is not prepared as a model of a family this release serves: prepare it again", args.Model))
	}
	tok, err := tokenizer.Load(filepath.Join(root, "tokenizer", "tokenizer.json"))
	if err != nil {
		listener.Close()
		return failed("model", fmt.Errorf("cannot load the tokenizer of %s: %v", args.Model, err))
	}
	cf := family.Format
	for _, s := range append(append([]string{}, cf.StopTokens...), thinkEnd, toolCallOpen) {
		if _, ok := tok.TokenToID(s); !ok {
			listener.Close()
			return failed("model", fmt.Errorf("the tokenizer has no %s token the %s chat format needs", s, family.Name))
		}
	}
	if id, _ := tok.TokenToID(thinkEnd); id != cf.ThinkEndID {
		listener.Close()
		return failed("model", fmt.Errorf("the tokenizer's %s is not token %d", thinkEnd, cf.ThinkEndID))
	}
	sources, special, err := LoadTokenizerFiles(filepath.Join(root, "tokenizer"))
	if err != nil {
		listener.Close()
		return failed("model", err)
	}
	renderer := &Renderer{special: special}
	templates, err := NewChatTemplates(sources, renderer, tok)
	if err != nil {
		listener.Close()
		return failed("model", err)
	}
	printStatus("Chat template · "+templates.Describe(), false)
	endText := args.ThinkingEndText
	if endText == nil {
		endText = templates.Select(nil).ThinkingEnd
	}
	thinkingEnd, err := thinkingEndTokens(tok, endText, cf)
	if err != nil {
		listener.Close()
		return failed("model", err)
	}
	rt := NewRuntime(func(first *wire.Ready) []string { return args.engineCommand(root, first) }, args.QueueSize)
	rt.OnWarming = func() { _ = recordPhase(args.ServeRecord, "warming", false) }
	backend := NewBackend(rt, tok, thinkingEnd)
	started := make(chan error, 1)
	go func() { started <- rt.Start() }()
	select {
	case err = <-started:
	case <-signals:
		rt.Close()
		listener.Close()
		return 0
	}
	if err != nil {
		listener.Close()
		var sf *StartFailedError
		if errors.As(err, &sf) {
			f := ojson.O("kind", kindName(sf.Event.Kind))
			if sf.Event.Required > 0 || sf.Event.Budget > 0 || sf.Event.PlanContext > 0 {
				bound := "device_memory"
				if int(sf.Event.Bound) < len(wire.ShortfallBounds) {
					bound = wire.ShortfallBounds[sf.Event.Bound]
				}
				f.Set("bound", bound)
				f.Set("required_bytes", int64(sf.Event.Required))
				f.Set("budget_bytes", int64(sf.Event.Budget))
				f.Set("plan_context_tokens", int64(sf.Event.PlanContext))
			}
			f.Set("message", err.Error())
			printStatus("Error · "+err.Error(), true)
			recordFailure(args.ServeRecord, f)
			return 1
		}
		return failed("internal", err)
	}
	served := rt.Capabilities()
	if int(served.Vocabulary) > tok.VocabSize() {
		// The model's logit rows may exceed the tokenizer's ids; fine.
	}
	for _, s := range cf.StopTokens {
		id, _ := tok.TokenToID(s)
		found := false
		for _, st := range served.StopTokens {
			if int(st) == id {
				found = true
			}
		}
		if !found {
			rt.Close()
			listener.Close()
			return failed("model", fmt.Errorf("the engine's stop tokens do not include %s", s))
		}
	}
	names := []string{args.Model}
	response := args.Model
	if args.AnnounceServedName {
		response = args.ServedNames[0]
	}
	seen := map[string]bool{}
	var modelNames []string
	for _, n := range append(append([]string{response}, names...), args.ServedNames...) {
		if !seen[n] {
			seen[n] = true
			modelNames = append(modelNames, n)
		}
	}
	sampling := map[string]any{}
	for k, v := range family.Sampling {
		sampling[k] = v
	}
	app := &Frontend{Tok: tok, Backend: backend, Model: args.Model, ResponseModel: response, ModelNames: modelNames,
		Templates: templates, Renderer: renderer, Format: cf, SamplingDefault: sampling, DefaultEffort: args.DefaultEffort,
		DefaultBudget: args.ThinkingBudget, Codec: codec}
	if args.RequestTimeout > 0 {
		app.RequestTimeout = time.Duration(args.RequestTimeout * float64(time.Second))
	}
	app.init(int(served.MaxConcurrent))
	exeDir, _ := os.Executable()
	repo := filepath.Dir(filepath.Dir(exeDir))
	// llguidance's token trie takes about 100 MB, which only constrained
	// requests need: the first one loads it.
	app.loadConstraints = func() (*ConstraintFactory, error) {
		started := time.Now()
		factory, err := NewConstraintFactory(FindLLGuidance(repo), tok, served)
		debug.FreeOSMemory()
		if err == nil {
			printStatus(fmt.Sprintf("Constraints · llguidance %s loaded in %.1f s", llg.Version(), time.Since(started).Seconds()), false)
		}
		return factory, err
	}
	exe, _ := os.Executable()
	root2 := filepath.Dir(filepath.Dir(exe))
	if _, err := os.Stat(filepath.Join(root2, "server", "chat.html")); err != nil {
		root2 = filepath.Dir(filepath.Dir(args.Binary))
	}
	srv := NewServer(app, root2, args.QueueSize)
	srv.Host, srv.Port, srv.APIKey, srv.WebUI, srv.MaxRequest = args.Host, listener.Addr().(*net.TCPAddr).Port, args.APIKey, !args.NoWebUI, args.MaxRequest
	srv.AllowedHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	for _, h := range append([]string{args.Host}, args.AllowedHosts...) {
		if h != "0.0.0.0" && h != "::" {
			srv.AllowedHosts[strings.TrimRight(strings.ToLower(h), ".")] = true
		}
	}
	srv.AllowedOrigins = map[string]bool{}
	for _, o := range args.AllowedOrigins {
		srv.AllowedOrigins[o] = true
	}
	if srv.AllowedOrigins["*"] && args.APIKey == "" {
		printStatus("Warning · --allowed-origin '*' without --api-key lets every web page open in a browser that reaches this server use it", true)
	}
	if err := recordPhase(args.ServeRecord, "ready", true); err != nil {
		printStatus("Error · "+err.Error(), true)
		backend.Close()
		return 1
	}
	tokens := int(served.MaxContext)
	context := fmt.Sprintf("%dK", tokens/1024)
	if tokens%1024 != 0 {
		context = commas(tokens)
	}
	printStatus(fmt.Sprintf("Ready · %s · context %s · text only (Go server) · http://%s:%d", args.Model, context, args.Host, srv.Port), false)
	go func() { _ = srv.Serve(listener) }()
	<-signals
	signal.Ignore(syscall.SIGTERM)
	go func() {
		<-signals
		rt.Kill()
	}()
	listener.Close()
	printStatus("Stopping · releasing engine resources", false)
	backend.Close()
	return 0
}

func raiseDescriptorLimit() {
	var lim syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim) != nil {
		return
	}
	want := uint64(10240)
	if lim.Max < want {
		want = lim.Max
	}
	if lim.Cur < want {
		lim.Cur = want
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim)
	}
}

// watchProcess stops the server once process pid has ended.
func watchProcess(pid int) {
	kq, err := syscall.Kqueue()
	if err != nil {
		return
	}
	ev := syscall.Kevent_t{Ident: uint64(pid), Filter: syscall.EVFILT_PROC, Flags: syscall.EV_ADD | syscall.EV_ONESHOT, Fflags: syscall.NOTE_EXIT}
	out := make([]syscall.Kevent_t, 1)
	_, _ = syscall.Kevent(kq, []syscall.Kevent_t{ev}, out, nil)
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
}

var _ = jinja.Parse

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
