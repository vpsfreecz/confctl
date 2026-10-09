package core

import (
	"context"
	_ "embed"
	"fmt"
	ext "github.com/vpsfreecz/confctl/experimental/extension"
	"os"
	"strings"
)

// These help bytes are captured from the immutable packaged Ruby CLI. Keeping
// logical program name confctl is deliberate; executable remains separately named.
//
//go:embed help_root.txt
var RootHelp string

//go:embed help_ls.txt
var LSHelp string

//go:embed help_status.txt
var StatusHelp string
var Builtins = map[string]bool{"init": true, "add": true, "rename": true, "rediscover": true, "inputs": true, "ls": true, "build": true, "deploy": true, "health-check": true, "status": true, "changelog": true, "diff": true, "test-connection": true, "ssh": true, "cssh": true, "generation": true, "collect-garbage": true, "gen-data": true, "help": true}
var Inventory = []string{"init", "add", "rename", "rediscover", "inputs ls", "inputs update", "inputs set", "inputs channel ls", "inputs channel update", "inputs channel set", "inputs machine update", "inputs machine set", "ls", "build", "deploy", "health-check", "status", "changelog", "diff", "test-connection", "ssh", "cssh", "generation ls", "generation rm", "generation rotate", "collect-garbage", "gen-data vpsadmin all", "gen-data vpsadmin containers", "gen-data vpsadmin network"}

type Options struct {
	Attrs, Tags, Args                       []string
	Managed, Output, Generation             string
	HasOutput, HideHeader, List, Trace, Yes bool
}

func parse(cmd string, a []string) (Options, error) {
	o := Options{}
	for i := 0; i < len(a); i++ {
		s := a[i]
		if s == "--" {
			o.Args = append(o.Args, a[i+1:]...)
			break
		}
		if !strings.HasPrefix(s, "-") || s == "-" {
			o.Args = append(o.Args, a[i:]...)
			break
		}
		key, value, eq := strings.Cut(s, "=")
		flag := func() (string, error) {
			if eq {
				return value, nil
			}
			i++
			if i == len(a) {
				return "", fmt.Errorf("missing argument: %s", key)
			}
			return a[i], nil
		}
		switch key {
		case "--attr", "-a":
			v, e := flag()
			if e != nil {
				return o, e
			}
			o.Attrs = append(o.Attrs, v)
		case "--tag", "-t":
			v, e := flag()
			if e != nil {
				return o, e
			}
			o.Tags = append(o.Tags, v)
		case "--generation", "-g":
			if cmd != "status" {
				return o, fmt.Errorf("invalid option: %s", key)
			}
			v, e := flag()
			if e != nil {
				return o, e
			}
			o.Generation = v
		case "--yes", "-y":
			if cmd == "ls" {
				return o, fmt.Errorf("invalid option: %s", key)
			}
			o.Yes = true
		case "--no-yes":
			if cmd == "ls" {
				return o, fmt.Errorf("invalid option: %s", key)
			}
			o.Yes = false
		case "--managed":
			if cmd != "ls" {
				return o, fmt.Errorf("invalid option: %s", key)
			}
			v, e := flag()
			if e != nil {
				return o, e
			}
			if v != "y" && v != "yes" && v != "n" && v != "no" && v != "a" && v != "all" {
				return o, fmt.Errorf("invalid argument: --managed %s", v)
			}
			o.Managed = v
		case "--output", "-o":
			if cmd != "ls" {
				return o, fmt.Errorf("invalid option: %s", key)
			}
			v, e := flag()
			if e != nil {
				return o, e
			}
			o.Output = v
			o.HasOutput = true
		case "--hide-header", "-H":
			if cmd != "ls" {
				return o, fmt.Errorf("invalid option: %s", key)
			}
			o.HideHeader = true
		case "--no-hide-header":
			if cmd != "ls" {
				return o, fmt.Errorf("invalid option: %s", key)
			}
			o.HideHeader = false

		case "--list", "-L":
			if cmd != "ls" {
				return o, fmt.Errorf("invalid option: %s", key)
			}
			o.List = true
		case "--show-trace":
			if cmd == "status" {
				return o, fmt.Errorf("invalid option: %s", key)
			}
			o.Trace = true
		case "--no-show-trace":
			if cmd == "status" {
				return o, fmt.Errorf("invalid option: %s", key)
			}
			o.Trace = false
		default:
			// Expand short clusters/attached values into this same ordered pass.
			if len(key) > 2 && key[0] == '-' && key[1] != '-' {
				expanded := []string{}
				for j := 1; j < len(key); j++ {
					k := "-" + string(key[j])
					expanded = append(expanded, k)
					if strings.ContainsRune("atog", rune(key[j])) {
						if j+1 < len(key) {
							expanded = append(expanded, key[j+1:])
						}
						break
					}
				}
				next := append([]string(nil), a[:i]...)
				next = append(next, expanded...)
				next = append(next, a[i+1:]...)
				a = next
				i--
				continue
			}

			return o, fmt.Errorf("invalid option: %s", key)
		}
	}
	return o, nil
}
func help(cmd string) string {
	switch cmd {
	case "ls":
		return LSHelp
	case "status":
		return StatusHelp
	default:
		return RootHelp
	}
}
func Main(ctx context.Context, argv []string) int {
	color := "auto"
	i := 0
	for i < len(argv) {
		if argv[i] == "--color" || argv[i] == "-c" {
			i++
			if i == len(argv) {
				fmt.Fprintln(os.Stderr, "error: missing argument: --color")
				return 64
			}
			color = argv[i]
			i++
			continue
		}
		if strings.HasPrefix(argv[i], "-c") && !strings.HasPrefix(argv[i], "--") && len(argv[i]) > 2 {
			color = strings.TrimPrefix(argv[i], "-c")
			i++
			continue
		}
		if strings.HasPrefix(argv[i], "--color=") {
			color = strings.TrimPrefix(argv[i], "--color=")
			i++
			continue
		}
		break
	}
	argv = argv[i:]
	if color != "auto" && color != "never" && color != "always" {
		fmt.Fprintln(os.Stderr, "error: invalid argument: --color "+color)
		return 64
	}
	if len(argv) == 1 && argv[0] == "--version" {
		fmt.Fprint(os.Stderr, "confctl: version unknown\nerror: confctl: version unknown\n")
		return 1
	}
	r, err := ReadRegistry(os.Getenv("CONFCTL_EXTENSION_REGISTRY"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if len(argv) == 0 || argv[0] == "--help" || argv[0] == "help" {
		cmd := ""
		if len(argv) > 1 {
			cmd = argv[1]
		}
		text := help(cmd)
		if cmd == "" && r.hasRuntimeCommand() {
			text = strings.Replace(text, "    ssh             - Run command over SSH\n", "    runtime-kernels - Manage node runtime kernel versions\n    ssh             - Run command over SSH\n", 1)
		}
		fmt.Print(text)
		return 0
	}
	cmd := argv[0]
	for _, s := range argv[1:] {
		if s == "--help" {
			fmt.Print(help(cmd))
			return 0
		}
	}
	extensionCommand := false
	var registration Registration
	var command Command
	for _, x := range r.Extensions {
		for _, c := range x.Commands {
			if len(argv) >= len(c.Path) && strings.Join(argv[:len(c.Path)], " ") == strings.Join(c.Path, " ") {
				extensionCommand = true
				registration = x
				command = c
			}
		}
	}
	if cmd != "ls" && cmd != "status" && !extensionCommand {
		fmt.Fprintf(os.Stderr, "confctl-go-prototype: %s is unavailable in the measurement prototype\n", cmd)
		return 2
	}
	args := argv[1:]
	parserName := cmd
	if extensionCommand {
		args = argv[len(command.Path):]
		parserName = "extension"
	}
	opts, err := parse(parserName, args)
	if err != nil {
		fmt.Fprint(os.Stderr, "error: "+err.Error()+"\n\n")
		fmt.Print(help(cmd))
		return 64
	}
	if cmd == "status" && opts.Generation != "none" {
		fmt.Fprintln(os.Stderr, "confctl-go-prototype: status requires --generation none; other modes are unavailable")
		return 2
	}
	if extensionCommand && len(opts.Args) > 1 {
		fmt.Fprintln(os.Stderr, "error: unknown argument: "+strings.Join(opts.Args[1:], " "))
		return 64
	}
	e, err := New(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	e.Color = color
	e.Yes = opts.Yes
	e.ShowTrace = opts.Trace
	logName := cmd
	if extensionCommand {
		logName = strings.Join(command.Path, "-")
	}
	if err = e.OpenLog(logName); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	e.LogCLI(cmd, opts, argv)
	fail := func(err error, code int) int {
		fmt.Fprintf(os.Stderr, "\nLog file: %s\n", e.Log.Name())
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		return code
	}
	success := false
	defer func() { e.CloseLog(success) }()
	if extensionCommand {
		in := ext.Invocation{Root: e.Root, ExtensionID: registration.ID, CommandPath: command.Path, OriginCommand: command.Path, Options: map[string]any{"yes": opts.Yes, "attr": opts.Attrs, "tag": opts.Tags, "show-trace": opts.Trace}, Arguments: opts.Args, RawArgv: argv}
		code, err := e.Invoke(registration, command.Handler, in)
		if err != nil {
			return fail(err, 1)
		}
		success = code == 0
		return code
	}
	if cmd == "ls" && opts.List {
		e.ShowTrace = false
		n, er := e.evaluator()
		if er == nil {
			var v any
			v, er = n.eval(".#confctl.moduleOptions")
			if er == nil {
				fmt.Println("name")
				a, _ := v.([]any)
				for _, x := range a {
					h, _ := x.(map[string]any)
					name := str(h["name"])
					if strings.HasPrefix(name, "cluster.<name>.") {
						fmt.Println(strings.TrimPrefix(name, "cluster.<name>."))
					}
				}
			}
		}
		if er != nil {
			return fail(er, 1)
		}
		success = true
		return 0
	}
	all, err := e.Machines(false)
	if err != nil {
		return fail(err, 1)
	}
	var pattern *string
	if len(opts.Args) > 0 {
		pattern = &opts.Args[0]
	}
	managed := opts.Managed
	if cmd == "status" || managed == "" {
		managed = "yes"
	}
	ms, err := Select(all, pattern, opts.Attrs, opts.Tags, managed)
	if err != nil {
		return fail(err, 64)
	}
	if cmd == "ls" {
		var cols []string
		if opts.HasOutput {
			cols = strings.Split(opts.Output, ",")
			for len(cols) > 0 && cols[len(cols)-1] == "" {
				cols = cols[:len(cols)-1]
			}
		}
		text, err := e.List(ms, cols, !opts.HideHeader)
		if err != nil {
			return fail(err, 1)
		}
		fmt.Print(text)
		success = true
		return 0
	}
	selected := []Machine{}
	for _, m := range ms {
		if m.Target.Host != nil || m.CarrierName != nil {
			selected = append(selected, m)
		}
	}
	if len(selected) == 0 {
		return fail(fmt.Errorf("No machines to check"), 1)
	}
	if !opts.Yes {
		text, err := e.List(selected, nil, true)
		if err != nil {
			return fail(err, 1)
		}
		fmt.Print("The following machines will be checked:\n" + text + "\nGeneration: none\n")
		ok, err := e.Confirm(os.Stdin, "", false)
		if err != nil || !ok {
			if err != nil {
				return fail(err, 1)
			}
			return fail(fmt.Errorf("Aborted"), 1)
		}
	}
	text, err := e.Status(selected, all)
	if err != nil {
		return fail(err, 1)
	}
	fmt.Print(text)
	success = true
	return 0
}
