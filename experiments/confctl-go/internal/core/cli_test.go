package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/experimental/internal/cli"
)

func TestCLIStage1Subprocess(t *testing.T) {
	if os.Getenv("CONFCTL_STAGE1_SUBPROCESS") != "1" {
		return
	}
	var argv []string
	if err := json.Unmarshal([]byte(os.Getenv("CONFCTL_STAGE1_ARGV")), &argv); err != nil {
		panic(err)
	}
	os.Exit(Main(context.Background(), argv))
}

func actualCLI(t *testing.T, argv []string, registry string) (int, string, string, string) {
	t.Helper()
	root := t.TempDir()
	configuration := filepath.Join(root, "configuration")
	tools := filepath.Join(root, "tools")
	for _, p := range []string{configuration, tools} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal("declared Bash required for process sentinels", err)
	}
	marker := filepath.Join(root, "unexpected-process")
	for _, tool := range []string{"nix", "git", "ssh", "extension"} {
		if err := os.WriteFile(filepath.Join(tools, tool), []byte("#!"+bash+"\nprintf 'unexpected child\\n' >> \"$CONFCTL_STAGE1_SENTINEL\"\nexit 97\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(argv)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIStage1Subprocess$")
	cmd.Dir = configuration
	cmd.Env = append(os.Environ(), "CONFCTL_STAGE1_SUBPROCESS=1", "CONFCTL_STAGE1_ARGV="+string(b), "CONFCTL_EXTENSION_REGISTRY="+registry, "CONFCTL_STAGE1_SENTINEL="+marker, "PATH="+tools+":"+os.Getenv("PATH"), "PAGER=", "HOME="+root)
	var out, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err = cmd.Run()
	code := 0
	if err != nil {
		var ok bool
		var exit *exec.ExitError
		exit, ok = err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	if _, err = os.Stat(marker); err == nil {
		t.Fatal("pre-execution path started an external tool", argv)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	files, err := os.ReadDir(configuration)
	if err != nil || len(files) != 0 {
		t.Fatal("pre-execution path changed configuration", argv, files, err)
	}
	return code, out.String(), stderr.String(), configuration
}

type stage1Reference struct {
	Exit                 int
	Stdout, Stderr, Text string
	StdoutAsset          string `json:"stdout_asset"`
	StdoutSha256         string `json:"stdout_sha256"`
}

func referenceRecord(t *testing.T, id string) stage1Reference {
	t.Helper()
	b, err := os.ReadFile("testdata/stage1-reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var references struct {
		ContractRevision string `json:"contract_revision"`
		Records          map[string]stage1Reference
	}
	if err = json.Unmarshal(b, &references); err != nil {
		t.Fatal(err)
	}
	o, ok := references.Records[id]
	if !ok || references.ContractRevision != "cc40679d267165aecfa569128438bb55fe910268" {
		t.Fatal("missing original Ruby reference", id)
	}
	if o.StdoutAsset != "" {
		b, err = os.ReadFile(o.StdoutAsset)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(b)) != o.StdoutSha256 {
			t.Fatal("original help asset changed", o.StdoutAsset, err)
		}
		o.Stdout = string(b)
	}
	return o
}

func TestActualCLIReferenceHelpVersionAndParserError(t *testing.T) {
	for _, tc := range []struct {
		id       string
		argv     []string
		registry string
	}{
		{"parser-help", []string{"--help"}, ""},
		{"parser-help", []string{"-h"}, ""},
		{"parser-help", []string{"help"}, ""},
		{"parser-version", []string{"--version"}, "/no/such/registry"},
		{"list-invalid-managed", []string{"ls", "--managed", "bad"}, ""},
	} {
		code, out, stderr, _ := actualCLI(t, tc.argv, tc.registry)
		want := referenceRecord(t, tc.id)
		if code != want.Exit || out != want.Stdout || stderr != want.Stderr {
			t.Fatalf("%s: exit%d stdout%q stderr%q", tc.id, code, out, stderr)
		}
	}
}

func TestActualCLICommandHelpPrecedenceHasNoEffects(t *testing.T) {
	// Reuse the original ls help bytes, including their captured provenance.
	want := referenceRecord(t, "list-invalid-managed")
	for _, argv := range [][]string{
		{"ls", "--help", "--managed", "bad"},
		{"ls", "-h", "--unknown"},
		{"ls", "-HhZ"},
		{"ls", "-h", "--output"},
	} {
		code, out, stderr, _ := actualCLI(t, argv, "")
		if code != 0 || stderr != "" || out != want.Stdout {
			t.Fatal(argv, code, out, stderr)
		}
	}
	code, out, stderr, _ := actualCLI(t, []string{"ls", "--managed", "bad", "--help"}, "")
	if code != want.Exit || out != want.Stdout || stderr != want.Stderr {
		t.Fatal("earlier parse error lost", code, out, stderr)
	}
}

func TestActualCLIIntegerParsingHasNoEffects(t *testing.T) {
	help, err := cli.BuiltinRegistry().Help([]string{"health-check"}, 80)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		argv        []string
		code        int
		out, stderr string
	}{
		{[]string{"health-check", "-j9223372036854775808"}, 2, "", "confctl-go-prototype: health-check is unavailable in the measurement prototype\n"},
		{[]string{"health-check", "-j-9223372036854775809"}, 2, "", "confctl-go-prototype: health-check is unavailable in the measurement prototype\n"},
		{[]string{"health-check", "-j0x10"}, 2, "", "confctl-go-prototype: health-check is unavailable in the measurement prototype\n"},
		{[]string{"health-check", "-j08"}, 64, help, "error: invalid argument: -j 08\n\n"},
		{[]string{"health-check", "-j08", "--help"}, 64, help, "error: invalid argument: -j 08\n\n"},
		{[]string{"health-check", "--help", "-j08"}, 0, help, ""},
		{[]string{"health-check", "-j0_10", "--help"}, 0, help, ""},
	} {
		code, out, stderr, _ := actualCLI(t, tc.argv, "")
		if code != tc.code || out != tc.out || stderr != tc.stderr {
			t.Fatal(tc.argv, code, out, stderr)
		}
	}
}

func TestActualCLIRegistryFailuresHaveNoEffects(t *testing.T) {
	for _, body := range []string{
		`not JSON`,
		`{"schema":2,"extensions":[]}`,
		`{"schema":1,"extensions":[],"discovery":true}`,
		`{"schema":1,"extensions":[]} {"schema":1}`,
	} {
		p := filepath.Join(t.TempDir(), "registry.json")
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		code, out, stderr, _ := actualCLI(t, []string{"--help"}, p)
		if code != 1 || out != "" || stderr == "" {
			t.Fatal(body, code, out, stderr)
		}
	}
}

func TestActualCLIUnavailableLeavesHaveNoEffects(t *testing.T) {
	r := cli.BuiltinRegistry()
	for _, c := range r.Leaves() {
		if c.Availability.Mode != cli.Unavailable {
			continue
		}
		t.Run(strings.Join(c.Path, "/"), func(t *testing.T) {
			code, out, stderr, _ := actualCLI(t, c.Path, "")
			if code != 2 || out != "" || !strings.Contains(stderr, "unavailable") {
				t.Fatal(code, out, stderr)
			}
		})
	}
	for _, argv := range [][]string{{"status"}, {"status", "-gcurrent"}, {"status", "-g0"}, {"build", "pattern", "--help"}, {"build", "--", "--help"}} {
		code, out, stderr, _ := actualCLI(t, argv, "")
		if code != 2 || out != "" || !strings.Contains(stderr, "unavailable") {
			t.Fatal(argv, code, out, stderr)
		}
	}
}

func TestActualCLINestedAndExtensionHelp(t *testing.T) {
	r := packagedRegistry(t)
	for i := range r.Extensions {
		r.Extensions[i].Argv = []string{"/no/such/extension"}
	}
	p := filepath.Join(t.TempDir(), "registry.json")
	b, _ := json.Marshal(r)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		argv     []string
		contains string
	}{
		{[]string{"help", "inputs", "channel", "set"}, "Unavailable in confctl-go-prototype."},
		{[]string{"inputs", "channel", "--help"}, "Unavailable"},
		{[]string{"status", "-h"}, "Execution requires --generation none."},
		{[]string{"help", "runtime-kernels", "update"}, "Update runtime kernels"},
		{[]string{"runtime-kernels", "--help"}, "Available"},
		{[]string{"runtime-kernels", "update", "--help"}, "--[no-]show-trace"},
		{[]string{"runtime-kernels", "update", "-h", "--managed", "all"}, "--[no-]show-trace"},
	} {
		code, out, stderr, _ := actualCLI(t, tc.argv, p)
		if code != 0 || stderr != "" || !strings.Contains(out, tc.contains) {
			t.Fatal(tc, code, out, stderr)
		}
	}
	code, out, _, _ := actualCLI(t, []string{"runtime-kernels", "update", "first", "second"}, p)
	if code != 64 || out != "" {
		t.Fatal("extension arity guard changed", code, out)
	}
	for _, argv := range [][]string{{"help", "missing"}, {"help", "--availability"}, {"inputs", "missing"}, {"runtime-kernels", "update", "--managed", "all"}} {
		code, _, _, _ := actualCLI(t, argv, p)
		if code != 64 {
			t.Fatal(argv, code)
		}
	}
}

func TestRegistryCollisionUsesBuiltinTree(t *testing.T) {
	for _, c := range cli.BuiltinRegistry().Children(nil) {
		r := packagedRegistry(t)
		r.Extensions[1].Commands[0].Path = []string{c.Path[0], "update"}
		b, _ := json.Marshal(r)
		p := filepath.Join(t.TempDir(), "registry.json")
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadRegistry(p); err == nil || !strings.Contains(err.Error(), "collision") {
			t.Fatal(c.Path, err)
		}
		code, _, _, _ := actualCLI(t, []string{"--help"}, p)
		if code != 1 {
			t.Fatal(c.Path, code)
		}
	}
}

func TestParsedAdapterPreservesLogAndExtensionValues(t *testing.T) {
	registrations := packagedRegistry(t)
	r, registered, err := commandRegistry(registrations)
	if err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"ls", "-o", "", "-a", "one", "-a", "two", "target", "-o", "ignored"}, {"runtime-kernels", "update", "-a", "one", "-t", "two", "-y", "--no-yes", "--show-trace", "target"}} {
		inv, err := r.Parse(argv)
		if err != nil {
			t.Fatal(err)
		}
		o := handlerOptions(inv)
		if argv[0] == "ls" {
			if !o.HasOutput || o.Output != "" || !reflect.DeepEqual(o.Attrs, []string{"one", "two"}) || !reflect.DeepEqual(o.Args, []string{"target", "-o", "ignored"}) {
				t.Fatal(o)
			}
		} else if o.Yes || !o.Trace || !reflect.DeepEqual(o.Attrs, []string{"one"}) || !reflect.DeepEqual(o.Tags, []string{"two"}) || !reflect.DeepEqual(o.Args, []string{"target"}) {
			t.Fatal(o)
		}
		if argv[0] == "runtime-kernels" {
			bound := registered[strings.Join(inv.Command.Path, " ")]
			in := extensionInvocation("/disposable", bound.Registration, bound.Command, o, argv)
			wantOptions := map[string]any{"yes": false, "attr": []string{"one"}, "tag": []string{"two"}, "show-trace": true}
			if in.Root != "/disposable" || in.ExtensionID != bound.Registration.ID || !reflect.DeepEqual(in.CommandPath, []string{"runtime-kernels", "update"}) || !reflect.DeepEqual(in.OriginCommand, in.CommandPath) || !reflect.DeepEqual(in.Options, wantOptions) || !reflect.DeepEqual(in.Arguments, []string{"target"}) || !reflect.DeepEqual(in.RawArgv, argv) {
				t.Fatal("schema1 adapter changed", in)
			}
		}
		f, err := os.Create(filepath.Join(t.TempDir(), "log"))
		if err != nil {
			t.Fatal(err)
		}
		e := &Engine{Log: f, Color: "auto"}
		e.LogCLI(argv[0], o, argv)
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if argv[0] == "runtime-kernels" {
			if !strings.Contains(string(b), "#<GLI::Command::ParentKey:0x0000000000000000> => {}") || !strings.Contains(string(b), "\"show-trace\" => true") {
				t.Fatal(string(b))
			}
		} else if !strings.Contains(string(b), "\"o\" => \"\",\n   \"output\" => \"\"") || !strings.Contains(string(b), "arguments: [\"target\", \"-o\", \"ignored\"]") {
			t.Fatal(string(b))
		}
	}
}

func TestLogCLIOriginalPPReference(t *testing.T) {
	r, registered, err := commandRegistry(packagedRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	argv := []string{"runtime-kernels", "update", "-y"}
	inv, err := r.Parse(argv)
	if err != nil {
		t.Fatal(err)
	}
	o := handlerOptions(inv)
	bound := registered[strings.Join(inv.Command.Path, " ")]
	in := extensionInvocation("/disposable", bound.Registration, bound.Command, o, argv)
	// Empty arrays remain the existing adapter's nil slices, rather than becoming
	// new protocol values. The wire schema and the aliases in LogCLI stay intact.
	if !o.Yes || o.Attrs != nil || o.Tags != nil || o.Args != nil || !reflect.DeepEqual(in.Options["attr"], []string(nil)) || !reflect.DeepEqual(in.Options["tag"], []string(nil)) || in.Arguments != nil {
		t.Fatal(o, in)
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Log: f, Color: inv.Globals["color"].Value.String}
	e.LogCLI("runtime-kernels", o, argv)
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	want := referenceRecord(t, "site-kernels-corrupt-log").Text
	want = strings.ReplaceAll(want, "${PARENT_ADDRESS:1}", "0x0000000000000000")
	if string(b) != want {
		t.Fatalf("PP log changed:\n%s\nwant:\n%s", b, want)
	}
}
