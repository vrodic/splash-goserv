// Package launcher is the Go port of install/launcher.py: `splash serve`,
// which prepares or finds the model and runs the server in this process,
// and the agent commands, which connect an installed coding agent to it.
//
// A model already prepared is served without Python. One not prepared yet,
// or whose files changed, is prepared by the Python installer
// (install/models.py, install/local.py), as `./splash serve` would.
package launcher

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"splash/goserver/ojson"
	"splash/goserver/server"
)

const defaultPort = 8000

var agents = []string{"claude", "opencode", "codex", "hermes", "pi"}

const usage = `usage: splash-go {serve,claude,opencode,codex,hermes,pi} ...

Serve in the foreground, or connect an installed agent to the local server.

  splash-go serve --model OWNER/REPO[:VARIANT] [--model-dir PATH]
                  [--draft-model DRAFT] [--mmproj FILE] [--revision REV]
                  [--language-only] [--offline] [--port PORT] [server options]
  splash-go claude|opencode|codex|hermes|pi [--port PORT] [agent arguments]

serve takes splash serve's options. A model it has prepared before is served
without Python; preparing a new one, or one whose files changed, runs the
Python installer once (with --offline, an installed Hub model is not checked
for updates, and is served without Python too).
`

type serveArgs struct {
	model, modelDir, draft, mmproj, revision string
	languageOnly, offline                    bool
	port                                     int
	explicitPort                             bool
	host                                     string
	passthrough                              []string
}

func environmentPort() (int, error) {
	v := os.Getenv("SPLASH_PORT")
	if v == "" {
		return defaultPort, nil
	}
	return parsePort(v)
}

func parsePort(v string) (int, error) {
	p, err := strconv.Atoi(v)
	if err != nil {
		return 0, errors.New("port must be an integer from 1 to 65535")
	}
	if p < 1 || p > 65535 {
		return 0, errors.New("port must be between 1 and 65535")
	}
	return p, nil
}

func parseServe(argv []string) (*serveArgs, error) {
	a := &serveArgs{host: "127.0.0.1"}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			return nil, errors.New("arguments after -- are only supported for coding clients")
		}
		if !strings.HasPrefix(arg, "--") {
			return nil, fmt.Errorf("unrecognized arguments: %s", arg)
		}
		name, value, hasValue := strings.Cut(arg, "=")
		launcherValue := map[string]bool{"--model": true, "--model-dir": true, "--draft-model": true,
			"--mmproj": true, "--revision": true, "--port": true}
		switch name {
		case "--language-only":
			a.languageOnly = true
			continue
		case "--offline":
			a.offline = true
			continue
		case "--binary", "--serve-record":
			return nil, fmt.Errorf("unrecognized arguments: %s", arg)
		}
		takes, known := server.FlagKind(name)
		if launcherValue[name] {
			takes, known = true, true
		}
		if !known {
			return nil, fmt.Errorf("unrecognized arguments: %s", arg)
		}
		if !takes {
			a.passthrough = append(a.passthrough, arg)
			continue
		}
		if !hasValue {
			if i+1 >= len(argv) {
				return nil, fmt.Errorf("argument %s: expected one argument", name)
			}
			i++
			value = argv[i]
		}
		switch name {
		case "--model":
			if _, _, err := splitModelID(value); err != nil {
				return nil, fmt.Errorf("argument --model: %v", err)
			}
			a.model = value
		case "--model-dir":
			a.modelDir = localPath(value)
		case "--mmproj":
			a.mmproj = localPath(value)
		case "--revision":
			a.revision = value
		case "--draft-model":
			d, err := draftModel(value)
			if err != nil {
				return nil, fmt.Errorf("argument --draft-model: %v", err)
			}
			a.draft = d
		case "--port":
			p, err := parsePort(value)
			if err != nil {
				return nil, fmt.Errorf("argument --port: %v", err)
			}
			a.port, a.explicitPort = p, true
		default:
			if name == "--host" {
				a.host = value
			}
			a.passthrough = append(a.passthrough, name+"="+value)
		}
	}
	if a.model == "" {
		return nil, errors.New("the following arguments are required: --model")
	}
	if !a.explicitPort {
		p, err := environmentPort()
		if err != nil {
			return nil, fmt.Errorf("SPLASH_PORT: %v", err)
		}
		a.port = p
	}
	if a.modelDir != "" {
		if a.revision != "" {
			return nil, errors.New("--revision cannot be used with --model-dir")
		}
		if a.draft != "" {
			if info, err := os.Stat(a.draft); err != nil || !info.IsDir() {
				return nil, errors.New("--draft-model must be a local DFlash2 draft directory with --model-dir")
			}
		}
	} else if a.mmproj != "" {
		return nil, errors.New("--mmproj requires --model-dir")
	}
	if a.mmproj != "" && a.languageOnly {
		return nil, errors.New("--mmproj cannot be used with --language-only")
	}
	return a, nil
}

// Main runs the launcher and returns its exit status.
func Main(argv []string) int {
	if len(argv) == 0 || argv[0] == "-h" || argv[0] == "--help" {
		fmt.Print(usage)
		if len(argv) == 0 {
			return 2
		}
		return 0
	}
	p := FindPaths()
	if argv[0] == "--version" {
		fmt.Println(version(p))
		return 0
	}
	command := argv[0]
	if command == "server" {
		// The server alone, on a prepared model directory, as the launcher
		// runs it after its own steps: python -m server.server's command line.
		return server.Main(argv[1:])
	}
	for _, name := range agents {
		if command == name {
			if err := runAgent(p, command, argv[1:]); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				return 1
			}
			return 0
		}
	}
	if command != "serve" {
		fmt.Fprintf(os.Stderr, "%serror: argument command: invalid choice: %q\n", usage, command)
		return 2
	}
	for _, a := range argv[1:] {
		if a == "-h" || a == "--help" {
			fmt.Print(usage)
			return 0
		}
	}
	args, err := parseServe(argv[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "splash-go serve: error:", err)
		if port, ok := servePort(argv[1:]); ok {
			recordRefusal(p, port, ojson.O("kind", "configuration", "message", err.Error()))
		}
		return 2
	}
	code, err := serve(p, args)
	if err != nil {
		var f *Failure
		if !errors.As(err, &f) || !f.Said {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		return 1
	}
	return code
}

func version(p Paths) string {
	if !p.Packaged {
		return "Splash (source checkout, Go launcher)"
	}
	data, _ := os.ReadFile(filepath.Join(p.Root, "release.json"))
	v, _ := ojson.Decode(data)
	if o, ok := v.(*ojson.Object); ok {
		if s, ok := o.Value("version").(string); ok {
			return "Splash " + s + " (Go launcher)"
		}
	}
	return "Splash (Go launcher)"
}

// servePort is the port a refused serve command line names.
func servePort(argv []string) (int, bool) {
	for i, a := range argv {
		if a == "--port" && i+1 < len(argv) {
			p, err := parsePort(argv[i+1])
			return p, err == nil
		}
		if v, ok := strings.CutPrefix(a, "--port="); ok {
			p, err := parsePort(v)
			return p, err == nil
		}
	}
	p, err := environmentPort()
	return p, err == nil
}

func (p Paths) serveLock(port int) string {
	return filepath.Join(p.Runtime, fmt.Sprintf("serve-%d.lock", port))
}
func (p Paths) record(port int) string {
	return filepath.Join(p.Runtime, fmt.Sprintf("serve-%d.json", port))
}

func writeRecord(path string, record *ojson.Object) error {
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

func recordFailed(path string, failure *ojson.Object) {
	if err := writeRecord(path, ojson.O("pid", int64(os.Getpid()), "phase", "failed", "failure", failure)); err != nil {
		fmt.Fprintln(os.Stderr, time.Now().Format("15:04:05")+" Warning · "+err.Error())
	}
}

func recordRefusal(p Paths, port int, failure *ojson.Object) {
	if os.MkdirAll(p.Runtime, 0o755) != nil {
		return
	}
	lock, err := os.OpenFile(p.serveLock(port), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return
	}
	recordFailed(p.record(port), failure)
}

func lockOwner(lock *os.File) string {
	data := make([]byte, 4096)
	n, _ := lock.ReadAt(data, 0)
	v, err := ojson.Decode(data[:n])
	if err != nil {
		return ""
	}
	o, _ := v.(*ojson.Object)
	pid, _ := o.Value("pid").(int64)
	model, _ := o.Value("model").(string)
	port, _ := o.Value("port").(int64)
	if pid <= 0 || model == "" || port < 1 || port > 65535 {
		return ""
	}
	return fmt.Sprintf(" (PID %d, model %s, port %d)", pid, model, port)
}

// checkPort fails when another process owns host:port (launcher._check_port).
func checkPort(host string, port int) error {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		_ = c.Control(func(fd uintptr) { serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1) })
		return serr
	}}
	l, err := lc.Listen(nil, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	address := l.Addr().(*net.TCPAddr).IP.String()
	l.Close()
	if address == "0.0.0.0" || address == "::" {
		address = "127.0.0.1"
	}
	if c, err := net.DialTimeout("tcp", net.JoinHostPort(address, strconv.Itoa(port)), time.Second); err == nil {
		c.Close()
		return syscall.EADDRINUSE
	}
	return nil
}

// prepare returns the model directory to serve: a prepared selection that
// still verifies, else what the Python installer prepares.
func prepare(p Paths, a *serveArgs) (string, error) {
	if err := checkPort(a.host, a.port); err != nil {
		return "", fail("port", "cannot bind %s:%d: %v", a.host, a.port, err)
	}
	if err := p.deviceCheck(); err != nil {
		return "", err
	}
	models, err := filepath.Abs(p.Models)
	if err == nil {
		if r, rerr := filepath.EvalSymlinks(models); rerr == nil {
			models = r
		}
	}
	offline := a.offline || os.Getenv("HF_HUB_OFFLINE") == "1"
	if a.modelDir != "" {
		link := localSelectionLink(models, a.modelDir, a.draft, a.mmproj, a.languageOnly)
		// Without a draft, the family's comes from the Hub cache at the
		// commit its default branch last had, which only the installer reads.
		if a.draft != "" || offline {
			if err := verifyAssembly(link); err == nil {
				return link, nil
			}
		}
		fmt.Println(time.Now().Format("15:04:05") + " Preparing · " + a.modelDir + " (Python installer)")
		return p.prepareLocal(a.modelDir, a.draft, a.mmproj, a.languageOnly)
	}
	link := hubSelectionLink(models, a.model, a.revision, a.draft, a.languageOnly)
	if offline {
		if err := verifyAssembly(link); err == nil {
			return link, nil
		}
	}
	fmt.Println(time.Now().Format("15:04:05") + " Preparing · " + a.model + " (Python installer)")
	if err := p.prepareHub(a.model, a.revision, a.draft, a.languageOnly); err != nil {
		return "", err
	}
	return link, nil
}

func serve(p Paths, a *serveArgs) (int, error) {
	if a.offline {
		os.Setenv("HF_HUB_OFFLINE", "1")
	}
	if err := os.MkdirAll(p.Runtime, 0o755); err != nil {
		return 1, err
	}
	record := p.record(a.port)
	lock, err := os.OpenFile(p.serveLock(a.port), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return 1, err
	}
	defer lock.Close()
	installation, err := os.OpenFile(filepath.Join(p.Runtime, "serve.lock"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return 1, err
	}
	defer installation.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return 1, fmt.Errorf("Splash is already serving%s; stop it with Ctrl+C first", lockOwner(lock))
	}
	_ = lock.Truncate(0)
	_, _ = lock.WriteAt(ojson.Marshal(ojson.O("pid", int64(os.Getpid()), "model", a.model, "port", int64(a.port))), 0)
	if err := writeRecord(record, ojson.O("pid", int64(os.Getpid()), "phase", "starting")); err != nil {
		return 1, err
	}
	failed := func(err error) (int, error) {
		kind := "internal"
		var f *Failure
		if errors.As(err, &f) {
			kind = f.Kind
		}
		recordFailed(record, ojson.O("kind", kind, "message", err.Error()))
		return 1, err
	}
	if syscall.Flock(int(installation.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil {
		return failed(fail("upgrade", "Splash is being upgraded; wait for the upgrade to finish"))
	}
	modelDir, err := prepare(p, a)
	if err != nil {
		return failed(err)
	}
	argv := append([]string{modelDir, "--model", a.model, "--binary", p.Binary, "--port", strconv.Itoa(a.port),
		"--serve-record", record}, a.passthrough...)
	// The key reaches the server in its environment, as the launcher passes
	// secrets, never on a command line.
	for i, arg := range argv {
		if v, ok := strings.CutPrefix(arg, "--api-key="); ok {
			os.Setenv("SPLASH_API_KEY", v)
			argv = append(argv[:i:i], argv[i+1:]...)
			break
		}
	}
	return server.Main(argv), nil
}
