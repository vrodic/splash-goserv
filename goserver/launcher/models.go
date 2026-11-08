package launcher

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"splash/goserver/ojson"
)

// Paths are install/paths.py's: the checkout, or a packaged release.
type Paths struct {
	Root, Models, Runtime, Python, Binary string
	Packaged                              bool
}

// FindPaths locates the installation from the running executable, which
// lives in build/ of a checkout or engine/ of a release.
func FindPaths() Paths {
	exe, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	root := filepath.Dir(filepath.Dir(exe))
	if r := os.Getenv("SPLASH_ROOT"); r != "" {
		root = r
	}
	p := Paths{Root: root}
	_, err := os.Stat(filepath.Join(root, "release.json"))
	p.Packaged = err == nil
	if p.Packaged {
		home, _ := os.UserHomeDir()
		data := filepath.Join(home, "Library/Application Support/Splash")
		p.Models, p.Runtime = filepath.Join(data, "models"), filepath.Join(data, "runtime")
		p.Python, p.Binary = filepath.Join(root, "python/bin/python3"), filepath.Join(root, "engine/splash")
	} else {
		p.Models, p.Runtime = filepath.Join(root, "install/models"), filepath.Join(root, "build/runtime")
		p.Python, p.Binary = filepath.Join(root, ".venv/bin/python"), filepath.Join(root, "build/splash")
	}
	return p
}

// Failure is a refusal of a serve record kind.
type Failure struct {
	Kind    string
	Message string
	// Said: the program that failed printed it already.
	Said bool
}

func (f *Failure) Error() string { return f.Message }

func fail(kind, format string, args ...any) *Failure {
	return &Failure{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

var repoID = regexp.MustCompile(`^[A-Za-z0-9_](?:[A-Za-z0-9._-]*[A-Za-z0-9_])?/[A-Za-z0-9_](?:[A-Za-z0-9._-]{0,94}[A-Za-z0-9_])?$`)
var variantRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func checkRepoID(v string) error {
	if !repoID.MatchString(v) || strings.Contains(v, "--") || strings.Contains(v, "..") || strings.HasSuffix(v, ".git") {
		return errors.New("model must be a full Hugging Face repository ID (owner/repo)")
	}
	return nil
}

// splitModelID is serve_options.split_model_id.
func splitModelID(v string) (string, string, error) {
	repo, variant, hasVariant := strings.Cut(v, ":")
	if err := checkRepoID(repo); err != nil {
		return "", "", err
	}
	if hasVariant && (!variantRE.MatchString(variant) || strings.Contains(variant, "..")) {
		return "", "", errors.New("model variant must be a short name such as UD-Q4_K_M (owner/repo:VARIANT)")
	}
	return repo, variant, nil
}

func pyCompact(v any) string {
	s, _ := ojson.Dumps(v, ojson.Options{EnsureASCII: true, ItemSep: ",", KeySep: ":"})
	return s
}

func hashedLink(models string, selection any) string {
	sum := sha256.Sum256([]byte(pyCompact(selection)))
	return filepath.Join(models, ".selections", hex.EncodeToString(sum[:]))
}

func optional(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// hubSelectionLink is models.selection_link.
func hubSelectionLink(models, model, revision, draft string, languageOnly bool) string {
	repo, variant, _ := splitModelID(model)
	if revision != "" || languageOnly || draft != "" {
		return hashedLink(models, []any{model, optional(revision), languageOnly, optional(draft)})
	}
	if variant == "" {
		return filepath.Join(models, repo)
	}
	return filepath.Join(models, repo+":"+variant)
}

// localSelectionLink is models.local_selection_link.
func localSelectionLink(models, target, draft, mmproj string, languageOnly bool) string {
	return hashedLink(models, ojson.O("target", target, "draft", optional(draft), "mmproj", optional(mmproj), "language_only", languageOnly))
}

// localPath is the launcher's _local_path: absolute, its parent resolved,
// its own name kept.
func localPath(v string) string {
	if strings.HasPrefix(v, "~") {
		home, _ := os.UserHomeDir()
		v = home + v[1:]
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return v
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		parent = filepath.Dir(abs)
	}
	return filepath.Join(parent, filepath.Base(abs))
}

// draftModel is models.parse_draft_model: a local directory resolved, else
// a repository ID.
func draftModel(v string) (string, error) {
	local := v
	if strings.HasPrefix(local, "~") {
		home, _ := os.UserHomeDir()
		local = home + local[1:]
	}
	if info, err := os.Stat(local); v != "" && err == nil && info.IsDir() {
		abs, _ := filepath.Abs(local)
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			return r, nil
		}
		return abs, nil
	}
	if err := checkRepoID(v); err != nil {
		return "", errors.New("must be a local DFlash2 draft directory or a Hugging Face repository ID (owner/repo)")
	}
	return v, nil
}

// verifyAssembly is assembly.verify: the selection's model.json, and each
// linked file still the one it records, by size and times.
func verifyAssembly(link string) error {
	data, err := os.ReadFile(filepath.Join(link, "model.json"))
	if err != nil {
		return err
	}
	v, err := ojson.Decode(data)
	if err != nil {
		return err
	}
	record, _ := v.(*ojson.Object)
	files, _ := record.Value("files").(*ojson.Object)
	family, _ := record.Value("family").(string)
	if files == nil || files.Len() == 0 || family == "" {
		return errors.New("invalid resolved model record")
	}
	for _, name := range files.Keys() {
		entry, _ := files.Value(name).(*ojson.Object)
		if entry == nil {
			return errors.New("invalid resolved model record")
		}
		path := filepath.Join(link, name)
		target, err := os.Readlink(path)
		if err != nil || target != entry.Value("path") {
			return errors.New("resolved model file changed: " + name)
		}
		var st syscall.Stat_t
		if err := syscall.Stat(path, &st); err != nil {
			return errors.New("resolved model file changed: " + name)
		}
		size, _ := entry.Value("bytes").(int64)
		mtime, _ := entry.Value("mtime_ns").(int64)
		ctime, _ := entry.Value("ctime_ns").(int64)
		if st.Size != size || st.Mtimespec.Nano() != mtime || st.Ctimespec.Nano() != ctime {
			return errors.New("resolved model file changed: " + name)
		}
	}
	return nil
}

// deviceCheck runs the engine's check of this Mac before any model work.
func (p Paths) deviceCheck() error {
	replace := "rebuild it with make"
	if p.Packaged {
		replace = "reinstall Splash"
	}
	cmd := exec.Command(p.Binary, "device-check")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return fail("device", "cannot run the engine %s (%v); %s", p.Binary, err, replace)
	}
	if exitErr.ExitCode() == 64 {
		return fail("device", "the engine %s does not know this device check; %s", p.Binary, replace)
	}
	report := strings.TrimSpace(stderr.String())
	var last string
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(line, "error: ") {
			last = strings.TrimPrefix(line, "error: ")
		}
	}
	if last == "" {
		last = "the engine's device check failed: " + report
	}
	return fail("device", "%s", last)
}

func (p Paths) needPython(what string) error {
	if _, err := os.Stat(p.Python); err != nil {
		return fail("install", "%s needs Splash's Python environment (%s), which is not installed: run make install-environment, or ./splash serve once", what, p.Python)
	}
	return nil
}

// prepareHub runs the Python installer for a Hub selection
// (launcher._ensure_installed).
func (p Paths) prepareHub(model, revision, draft string, languageOnly bool) error {
	if err := p.needPython("installing " + model); err != nil {
		return err
	}
	args := []string{filepath.Join(p.Root, "install/models.py"), "--models", p.Models, "--model", model}
	if revision != "" {
		args = append(args, "--revision", revision)
	}
	if draft != "" {
		args = append(args, "--draft-model", draft)
	}
	if languageOnly {
		args = append(args, "--language-only")
	}
	args = append(args, "prepare")
	cmd := exec.Command(p.Python, args...)
	cmd.Dir, cmd.Stderr = p.Root, os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var failure string
	reader := bufio.NewReader(out)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			if v, jerr := ojson.DecodeString(strings.TrimSpace(line)); jerr == nil {
				if o, ok := v.(*ojson.Object); ok && o.Value("event") == "error" {
					failure, _ = o.Value("message").(string)
					continue
				}
			}
			os.Stdout.WriteString(line)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		if failure == "" {
			return fail("install", "model download or verification failed")
		}
		return &Failure{Kind: "install", Message: failure, Said: true}
	}
	return nil
}

// prepareLocal runs the Python helper for local files
// (launcher._prepare_local_model) and returns the directory it prepared.
func (p Paths) prepareLocal(target, draft, mmproj string, languageOnly bool) (string, error) {
	if err := p.needPython("preparing " + target); err != nil {
		return "", err
	}
	args := []string{"-P", "-m", "install.local", "--models", p.Models}
	if draft != "" {
		args = append(args, "--draft-model", draft)
	}
	if mmproj != "" {
		args = append(args, "--mmproj", mmproj)
	}
	if languageOnly {
		args = append(args, "--language-only")
	}
	args = append(args, "--", target)
	cmd := exec.Command(p.Python, args...)
	cmd.Dir, cmd.Stderr = p.Root, os.Stderr
	cmd.Env = append(os.Environ(), "PYTHONPATH="+p.Root)
	output, err := cmd.Output()
	result := strings.TrimSpace(string(output))
	if err != nil {
		if v, jerr := ojson.DecodeString(result); jerr == nil {
			if o, ok := v.(*ojson.Object); ok {
				msg, _ := o.Value("message").(string)
				kind, _ := o.Value("kind").(string)
				if msg != "" {
					return "", &Failure{Kind: kind, Message: "cannot prepare " + target + ": " + msg}
				}
			}
		}
		return "", fail("internal", "local model preparation failed")
	}
	if !filepath.IsAbs(result) {
		return "", fail("internal", "local model preparation returned no absolute directory")
	}
	return result, nil
}
