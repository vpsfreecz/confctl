package kexecnetboot

import (
	"bufio"
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
	"strings"
	"sync"
)

type Config struct{ MachineFQDN, Kexec string }
type Temp struct {
	Path   string
	Writer io.Writer
	Close  func() error
}
type Dependencies struct {
	BeginActions func(context.Context) (context.Context, context.CancelFunc)
	ReadCmdline  func() ([]byte, error)
	Request      func(context.Context, string) (*http.Response, error)
	CreateTemp   func(string) (Temp, error)
	Remove       func(string) error
	Child        func(context.Context, []string) error
	In           io.Reader
	Out, Err     io.Writer
}

func OSDependencies() Dependencies {
	var once sync.Once
	var client *http.Client
	var clientErr error
	return Dependencies{ReadCmdline: func() ([]byte, error) { return os.ReadFile("/proc/cmdline") }, Request: func(ctx context.Context, raw string) (*http.Response, error) {
		once.Do(func() { client, clientErr = newClient(transportDeps{}) })
		if clientErr != nil {
			return nil, clientErr
		}
		return get(ctx, client, raw)
	}, CreateTemp: func(prefix string) (Temp, error) {
		f, err := os.CreateTemp("", prefix+"-*")
		if err != nil {
			return Temp{}, err
		}
		return Temp{f.Name(), f, f.Close}, nil
	}, Remove: os.Remove, Child: directChild, In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
}
func directChild(ctx context.Context, argv []string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start kexec: %w", err)
	}
	err := cmd.Wait()
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

var httprootPattern = regexp.MustCompile(`\bhttproot=([^\t\n\v\f\r ]+)`)

func machinesURL(cmdline []byte, override *string) (string, error) {
	m := httprootPattern.FindStringSubmatch(strip(string(cmdline)))
	if m == nil {
		return "", fmt.Errorf("ERROR: Could not find 'httproot=' parameter in /proc/cmdline")
	}
	if override != nil {
		return strings.TrimRight(*override, "/") + "/machines.json", nil
	}
	u, err := endpoint(m[1])
	if err != nil {
		return "", err
	}
	var parts []string
	for _, p := range strings.Split(u.EscapedPath(), "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) >= 3 {
		parts = parts[:len(parts)-3]
	}
	rawPath := "/" + strings.Join(parts, "/")
	if !strings.HasSuffix(rawPath, "/") {
		rawPath += "/"
	}
	rawPath += "machines.json"
	path, err := url.PathUnescape(rawPath)
	if err != nil {
		return "", fmt.Errorf("invalid escaped HTTP path")
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: path, RawPath: rawPath}).String(), nil
}
func fetchIndex(ctx context.Context, raw string, d Dependencies) (record, error) {
	if _, err := endpoint(raw); err != nil {
		return nil, err
	}
	r, err := d.Request(ctx, raw)
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return nil, fmt.Errorf("missing HTTP response body")
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return nil, fmt.Errorf("ERROR: Could not download %s, HTTP %d", publicURL(raw), r.StatusCode)
	}
	return readIndex(r.Body)
}

type ownedTemp struct {
	Temp
	closed bool
}

func (t *ownedTemp) close() error {
	if t.closed {
		return nil
	}
	t.closed = true
	return t.Close()
}

func Run(ctx context.Context, c Config, o Options, d Dependencies) (status int, err error) {
	if o.Exec && o.Unload {
		fmt.Fprintln(d.Err, "ERROR: use either --unload or --exec, not both")
		return 1, nil
	}
	if o.Exec || o.Unload {
		ctx, stop := beginActions(ctx, d)
		defer stop()
		if o.Exec {
			return action(ctx, c.Kexec, "--exec", nil, d)
		}
		return action(ctx, c.Kexec, "--unload", nil, d)
	}
	cmdline, err := d.ReadCmdline()
	if err != nil {
		return 1, err
	}
	raw, err := machinesURL(cmdline, o.Server)
	if err != nil {
		return 1, err
	}
	index, err := fetchIndex(ctx, raw, d)
	if err != nil {
		return 1, err
	}
	s := selector{bufio.NewReader(d.In), d.Out, d.Err, o.Interactive, c.MachineFQDN}
	m, err := s.machine(index, o.Machine)
	if err != nil {
		return 1, err
	}
	if m == nil {
		fmt.Fprintln(d.Err, "ERROR: No suitable machine found.")
		return 1, nil
	}
	g, err := s.generation(m, o.Generation)
	if err != nil {
		return 1, err
	}
	if g == nil {
		fmt.Fprintln(d.Err, "ERROR: No generation found/selected.")
		return 1, nil
	}
	v, err := s.variant(g, o.Variant)
	if err != nil {
		return 1, err
	}
	combined, err := params(g["kernel_params"])
	if err != nil {
		return 1, err
	}
	if v != nil && truthy(v["kernel_params"]) {
		p, err := params(v["kernel_params"])
		if err != nil {
			return 1, err
		}
		combined = append(combined, p...)
	}
	combined = append(combined, o.Append)
	joined := strings.Join(combined, " ")
	if o.Interactive {
		fmt.Fprintln(d.Out, "\nSelected configuration:")
		fmt.Fprintf(d.Out, "  Machine:       %s (spin=%s)\n", display(m["fqdn"]), display(m["spin"]))
		fmt.Fprintf(d.Out, "  Generation:    %s (%s, rev=%s, kernel=%s)\n", display(g["generation"]), display(g["time_s"]), display(g["shortrev"]), display(g["kernel_version"]))
		if v != nil {
			fmt.Fprintf(d.Out, "  Variant:       %s (%s)\n", display(v["name"]), display(v["label"]))
		} else {
			fmt.Fprintln(d.Out, "  Variant:       none")
		}
		fmt.Fprintf(d.Out, "  Kernel params: %s\n\n", joined)
		for {
			fmt.Fprint(d.Out, "Continue? [y/N]: ")
			line, ok, err := s.line()
			if err != nil {
				return 1, err
			}
			if !ok {
				return 1, fmt.Errorf("EOF at final confirmation")
			}
			switch strings.ToLower(strip(line)) {
			case "y":
				fmt.Fprintln(d.Out)
				goto confirmed
			case "n":
				fmt.Fprintln(d.Err, "Aborting")
				return 1, nil
			}
		}
	}
confirmed:
	ctx, stop := beginActions(ctx, d)
	defer stop()
	var temps []*ownedTemp
	defer func() {
		for _, t := range temps {
			if e := t.close(); e != nil {
				fmt.Fprintf(d.Err, "Warning: Could not close tmp file %s: %v\n", t.Path, e)
			}
			if e := d.Remove(t.Path); e != nil {
				fmt.Fprintf(d.Err, "Warning: Could not remove tmp file %s: %v\n", t.Path, e)
			}
		}
	}()
	download := func(name string) (string, error) {
		boot, ok := g["boot_files"].(map[string]any)
		if !ok {
			return "", fmt.Errorf("boot_files must be an object")
		}
		raw, ok := boot[name].(string)
		if !ok {
			return "", fmt.Errorf("missing boot URL for %s", name)
		}
		u, err := endpoint(raw)
		if err != nil {
			return "", err
		}
		file, err := d.CreateTemp(filepath.Base(u.EscapedPath()))
		if err != nil {
			return "", err
		}
		t := &ownedTemp{Temp: file}
		temps = append(temps, t)
		if _, err := fmt.Fprintf(d.Out, "Downloading %s -> %s\n", raw, t.Path); err != nil {
			return "", err
		}
		r, err := d.Request(ctx, raw)
		if err != nil {
			return "", err
		}
		if r == nil || r.Body == nil {
			return "", fmt.Errorf("missing HTTP response body")
		}
		if r.StatusCode < 200 || r.StatusCode >= 300 {
			_ = r.Body.Close()
			return "", fmt.Errorf("ERROR: Could not download %s, HTTP %d", publicURL(raw), r.StatusCode)
		}
		_, copyErr := io.Copy(t.Writer, r.Body)
		bodyErr := r.Body.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if bodyErr != nil {
			return "", bodyErr
		}
		if err := t.close(); err != nil {
			return "", err
		}
		return t.Path, nil
	}
	kernel, err := download("bzImage")
	if err != nil {
		return 1, err
	}
	initrd, err := download("initrd")
	if err != nil {
		return 1, err
	}
	return action(ctx, c.Kexec, "--load", []string{kernel, "--initrd=" + initrd, "--append=" + joined}, d)
}

// Prompts retain the process's default signal disposition. Only the action
// stage needs catchable cancellation through temporary-file and child cleanup.
func beginActions(ctx context.Context, d Dependencies) (context.Context, context.CancelFunc) {
	if d.BeginActions != nil {
		return d.BeginActions(ctx)
	}
	return ctx, func() {}
}

func action(ctx context.Context, kexec, action string, args []string, d Dependencies) (int, error) {
	argv := append([]string{kexec, action}, args...)
	argv = append(argv, "--no-sync")
	shown := append([]string(nil), argv...)
	if action == "--load" {
		for i, a := range shown {
			if strings.HasPrefix(a, "--append=") {
				shown[i] = "--append=\"" + strings.TrimPrefix(a, "--append=") + "\""
			}
		}
	}
	if _, err := fmt.Fprintf(d.Out, "Executing: %s\n", strings.Join(shown, " ")); err != nil {
		return 1, err
	}
	err := d.Child(ctx, argv)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			fmt.Fprintf(d.Err, "ERROR: kexec %s failed!\n", action)
			return 1, nil
		}
		return 1, err
	}
	fmt.Fprintf(d.Out, "kexec %s completed successfully.\n", action)
	return 0, nil
}
