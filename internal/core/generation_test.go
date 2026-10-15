package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/generation"
)

type generationWarningWriter struct {
	body  bytes.Buffer
	count int
	err   error
}

func (w *generationWarningWriter) Write(p []byte) (int, error) {
	w.body.Write(p)
	if w.count < 0 {
		return len(p), w.err
	}
	return w.count, w.err
}
func TestGenerationWarningStderrOrder(t *testing.T) {
	failure := errors.New("warning stderr refusal")
	for _, row := range []struct {
		count     int
		err, want error
	}{{0, failure, failure}, {1, nil, io.ErrShortWrite}, {-1, nil, nil}} {
		root := t.TempDir()
		f, err := os.Create(filepath.Join(root, "log"))
		if err != nil {
			t.Fatal(err)
		}
		e := &Engine{Log: f, logOwner: nativeEngineLog(f)}
		w := &generationWarningWriter{count: row.count, err: row.err}
		got := e.generationWarning(w)("warning")
		if got != row.want || w.body.String() != "warning\n" {
			t.Fatal("warning stderr count/error changed")
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		if row.want == nil {
			want = "warning\n"
		}
		if string(b) != want {
			t.Fatal("log reached before failed stderr or warning bytes changed")
		}
	}
}

func seedCoreGeneration(t *testing.T, root, host, name, payload string) string {
	t.Helper()
	dir := filepath.Join(root, ".confctl", "generations", generation.SafeHost(host), name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "generation.json"), []byte(payload), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}
func coreGenerationRecord(top, date, info string) string {
	return fmt.Sprintf(`{"mode":"flakes","toplevel":%q,"date":%q,"inputs":{},"inputs_info":%s}`, top, date, info)
}
func generationInvocation(t *testing.T, args ...string) cli.Invocation {
	t.Helper()
	inv, err := cli.BuiltinRegistry().Parse(append([]string{"generation", "ls"}, args...))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestGenerationTableMetadataOrderAndPresentation(t *testing.T) {
	root := t.TempDir()
	seedCoreGeneration(t, root, "z", "zzz", coreGenerationRecord(root+"/z", "2000-01-01T00:00:00Z", `{"second":{"short_rev":"S","shortRev":"later","rev":"second-rev","lastModified":9007199254740993},"first":"123456789abc","partial":false}`))
	seedCoreGeneration(t, root, "a", "aaa", coreGenerationRecord(root+"/a", "2020-01-01T00:00:00Z", `{"third":{"shortRev":"","rev":"not-used"},"second":{"shortRev":false,"rev":"abcdefghij"}}`))
	before := testConfigurationState(t, root)
	list := &generation.UnifiedList{}
	for _, host := range []string{"z", "a"} {
		builds, err := generation.LoadBuildList(root, host, generation.Options{})
		if err != nil {
			t.Fatal(err)
		}
		list.AddBuilds(builds)
	}
	text := generationTable(list)
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if !reflect.DeepEqual(strings.Fields(lines[0]), []string{"HOST", "NAME", "ID", "PRESENCE", "CURRENT", "KERNEL", "SECOND", "FIRST", "PARTIAL", "THIRD"}) {
		t.Fatal(text)
	}
	if strings.Fields(lines[1])[0] != "a" || strings.Fields(lines[2])[0] != "z" || !strings.Contains(lines[2], "later") || !strings.Contains(lines[2], "12345678") || !strings.Contains(lines[1], "abcdefgh") {
		t.Fatal(text)
	}
	if list.Generations[0].Name != "zzz" || list.AtOffset(big.NewInt(1)).Name != "zzz" {
		t.Fatal("display sort changed insertion offsets")
	}
	info := generationInputInfo(list.Generations[0].InputsInfo()[0].Value)
	if info["lastModified"].(json.Number).String() != "9007199254740993" || info["shortRev"] != "later" {
		t.Fatal(info)
	}
	if !reflect.DeepEqual(before, testConfigurationState(t, root)) {
		t.Fatal("table mutated records")
	}
	// Source input columns can overwrite row/sort keys; preserve the duplicate
	// column and resulting display sort rather than reserving new role names.
	seedCoreGeneration(t, root, "z", "collision", coreGenerationRecord(root+"/collision", "2030-01-01T00:00:00Z", `{"name":"000000000","host":"overwritten"}`))
	builds, err := generation.LoadBuildList(root, "z", generation.Options{})
	if err != nil {
		t.Fatal(err)
	}
	collision := &generation.UnifiedList{}
	collision.AddBuilds(builds)
	text = generationTable(collision)
	if !strings.HasPrefix(strings.Fields(strings.Split(text, "\n")[2])[1], "00000000") || strings.Fields(strings.Split(text, "\n")[1])[1] != "-" {
		t.Fatal(text)
	}
}

func TestGenerationLocalScopesSelectorsAndNoWrites(t *testing.T) {
	root := t.TempDir()
	z := connectionMachine("z", "key-z", "unused.invalid", false)
	z.Target.Host = nil
	a := connectionMachine("a", "key-a", "unused.invalid", true)
	e := &Engine{Root: root, Context: context.Background(), Inventory: []Machine{z, a}}
	seedCoreGeneration(t, root, "z", "old", coreGenerationRecord(root+"/old", "2000-01-01T00:00:00Z", `{}`))
	seedCoreGeneration(t, root, "z", "new", coreGenerationRecord(root+"/new", "2001-01-01T00:00:00Z", `{}`))
	seedCoreGeneration(t, root, "a", "9", coreGenerationRecord(root+"/a", "2002-01-01T00:00:00Z", `{}`))
	if err := os.Symlink("old", filepath.Join(root, ".confctl", "generations", "z", "current")); err != nil {
		t.Fatal(err)
	}
	before := testConfigurationState(t, root)
	for _, tc := range []struct {
		args  []string
		names []string
	}{
		{nil, []string{"9", "new", "old"}},
		{[]string{"-l"}, []string{"9", "new", "old"}},
		{[]string{"*", "current"}, []string{"9", "old"}},
		{[]string{"*", "old"}, []string{"new"}},
		{[]string{"*", "0"}, []string{"9"}},
		{[]string{"*", "1"}, []string{"old"}},
		{[]string{"*", "-1"}, []string{"new"}},
		{[]string{"*", "9"}, []string{"9"}},
		{[]string{"*", "{old,new}"}, []string{"new", "old"}},
		{[]string{"*", ""}, []string{}},
		{[]string{"*", "all"}, []string{}},
		{[]string{"key-z"}, []string{"new", "old"}},
		{[]string{"-a", "managed=false"}, []string{"new", "old"}},
		{[]string{"-t", "node", "*"}, []string{"9", "new", "old"}},
		{[]string{"missing"}, []string{}},
		{[]string{"*", "999999999999999999999"}, []string{}},
	} {
		text, err := e.ListGenerations(generationInvocation(t, tc.args...))
		if err != nil {
			t.Fatal(tc.args, err)
		}
		got := []string{}
		for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n")[1:] {
			got = append(got, strings.Fields(line)[1])
		}
		if !reflect.DeepEqual(got, tc.names) {
			t.Fatal(tc.args, got, tc.names, text)
		}
	}
	if !reflect.DeepEqual(before, testConfigurationState(t, root)) {
		t.Fatal("local listing wrote files")
	}
}

func TestGenerationRemoteMergeInventoryAndLocalErrors(t *testing.T) {
	e, z, root := queryFixture(t, `case "$remote" in
 'cat /proc/uptime') printf '1000 0\n' ;;
 'bash --norc')
   if [[ " $* " == *' fake.invalid '* ]]; then printf '%s/z\n/private/profile-2-link;%s/z;2\n/private/profile-1-link;%s/z1;10\n' "$PWD" "$PWD" "$PWD"; else printf '%s/a\n/private/profile-1-link;%s/a;1\n' "$PWD" "$PWD"; fi ;;
 *) exit 99 ;;
esac
`)
	z.Name = "z"
	a := connectionMachine("a", "a", "second.invalid", false)
	a.Profile = "/private/profile"
	e.Inventory = []Machine{z, a}
	seedCoreGeneration(t, root, "z", "zzz", coreGenerationRecord(root+"/z", "2020-01-01T00:00:00Z", `{"zrole":"zzzzzzzzzz"}`))
	seedCoreGeneration(t, root, "a", "aaa", coreGenerationRecord(root+"/a", "1900-01-01T00:00:00Z", `{"arole":"aaaaaaaaaa"}`))
	before := testConfigurationState(t, filepath.Join(root, ".confctl", "generations"))
	text, err := e.ListGenerations(generationInvocation(t, "-lr", "*", "1"))
	if err != nil || !strings.Contains(text, "1970-") || strings.Contains(text, "zzz") || strings.Contains(text, "aaa") {
		t.Fatal(text, err)
	}
	text, err = e.ListGenerations(generationInvocation(t, "-lr"))
	if err != nil || !strings.Contains(text, "build+host") || !strings.HasSuffix(strings.Fields(strings.Split(text, "\n")[0])[6], "ZROLE") {
		t.Fatal(text, err)
	}
	// A rejected local record still refuses numeric selection after all remotes.
	seedCoreGeneration(t, root, "z", "legacy", `{"mode":"swpins"}`)
	log, err := os.Create(filepath.Join(root, "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	e.Log, e.logOwner = log, nativeEngineLog(log)
	_, err = e.ListGenerations(generationInvocation(t, "-lr", "*", "0"))
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatal(err)
	}
	if !strings.Contains(fixtureFile(t, root, "log"), "Records and GC roots were left untouched") {
		t.Fatal("warnings missing from log")
	}
	text, err = e.ListGenerations(generationInvocation(t, "-r", "*", "0"))
	if err != nil || strings.Contains(text, "zzz") || strings.Contains(text, "aaa") {
		t.Fatal("remote-only loaded local", text, err)
	}
	if err := os.Symlink("absent", filepath.Join(root, ".confctl", "generations", "a", "current")); err != nil {
		t.Fatal(err)
	}
	_, err = e.ListGenerations(generationInvocation(t, "-lr", "*", "old"))
	if err == nil || !strings.Contains(err.Error(), "Cannot select current generation for a") {
		t.Fatal(err)
	}
	// Check the accepted records before adding deliberate error fixtures above.
	after := testConfigurationState(t, filepath.Join(root, ".confctl", "generations"))
	for path, value := range before {
		if after[path] != value {
			t.Fatal("record changed", path)
		}
	}
}

func TestGenerationStatusNoneLocalReaderCountAndWarnings(t *testing.T) {
	e, m, root := queryFixture(t, `case "$remote" in
 'cat /proc/uptime') printf '600.5 0\n' ;;
 'bash --norc') printf '/system\n/private/profile-999999999999999999999-link;/system;0\n/private/profile-1-link;\ninvalid;/system;0\n' ;;
 'cat /etc/confctl/inputs-info.json') printf '{"role":"abcdefghij"}' ;;
 *) exit 99 ;;
esac
`)
	// R1 accepts local zones/signed years/missing inputs; the removed counter
	// rejected these. Explicit null inputs remains rejected, with source warnings.
	seedCoreGeneration(t, root, m.Name, "signed", fmt.Sprintf(`{"mode":"flakes","date":"0-01-01T00:00:00","toplevel":%q}`, root+"/missing"))
	seedCoreGeneration(t, root, m.Name, "good", coreGenerationRecord(root+"/missing", "2026-01-01T00:00:00Z", `{}`))
	seedCoreGeneration(t, root, m.Name, "bad", fmt.Sprintf(`{"mode":"flakes","date":"2026-01-01T00:00:00Z","toplevel":%q,"inputs":null}`, root+"/missing"))
	if err := os.Symlink("absent", filepath.Join(root, ".confctl", "generations", m.Name, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "flake.nix"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(root, "tools", "nix")
	ssh, err := os.ReadFile(filepath.Join(root, "tools", "ssh"))
	if err != nil {
		t.Fatal(err)
	}
	shebang := strings.Split(string(ssh), "\n")[0]
	nix := shebang + "\n" + `case "${@: -1}" in
 '.#confctl.settings') printf '{"nix":{"maxJobs":"7"}}' ;;
 '.#confctl.machineKeys') printf '{"logical":"key"}' ;;
 '.#confctl.inputsInfo.key') printf '{"role":"abcdefghij"}' ;;
 *) exit 99 ;;
esac
`
	if err := os.WriteFile(tool, []byte(nix), 0700); err != nil {
		t.Fatal(err)
	}
	e.SettingsCache = map[string]any{"nix": map[string]any{"maxJobs": "7"}}
	log, err := os.Create(filepath.Join(root, "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	e.Log, e.logOwner = log, nativeEngineLog(log)
	e.Color = "never"
	before := testConfigurationState(t, filepath.Join(root, ".confctl", "generations"))
	text, err := e.Status([]Machine{m}, []Machine{m})
	if err != nil || !strings.Contains(text, "2:2") || !strings.Contains(text, "ok") || strings.Contains(fixtureFile(t, root, "trace"), "realpath") {
		t.Fatal(text, err, fixtureFile(t, root, "trace"))
	}
	if !strings.Contains(fixtureFile(t, root, "log"), "Skipping generation") || !strings.Contains(fixtureFile(t, root, "log"), "Invalid profile generation link 'invalid'") {
		t.Fatal("warning log missing")
	}
	if !reflect.DeepEqual(before, testConfigurationState(t, filepath.Join(root, ".confctl", "generations"))) {
		t.Fatal("status repaired state")
	}
}

func TestGenerationClockContextAndEmptyRemote(t *testing.T) {
	if opts := (&Engine{}).generationOptions(); opts.Location != nil {
		t.Fatal("changed default timezone")
	}
	// R1's default location is the caller's local context; there is no remote
	// timezone query or added operational option.
	old := time.Local
	time.Local = time.FixedZone("fixture", 3600)
	defer func() { time.Local = old }()
	root := t.TempDir()
	seedCoreGeneration(t, root, "host", "local", coreGenerationRecord(root+"/missing", "2026-01-01T00:00:00", `{}`))
	list, err := generation.LoadBuildList(root, "host", (&Engine{}).generationOptions())
	if err != nil || list.Builds[0].Date.OffsetSeconds != 3600 {
		t.Fatal(list, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := &Engine{Root: root, Context: ctx, Inventory: []Machine{{Name: "host"}}}
	before := testConfigurationState(t, root)
	if _, err := e.ListGenerations(generationInvocation(t)); err != context.Canceled {
		t.Fatal("local cancellation lost", err)
	}
	if !reflect.DeepEqual(before, testConfigurationState(t, root)) {
		t.Fatal("canceled read changed state")
	}
}

// Finite optional ORIGINAL-ADAPTER. It reads only the supplied private records
// using original BuildList/Unified/CLI/OutputFormatter; no query or writer runs.
const originalGenerationTableAdapter = `
require 'json'
require 'time'
abort 'expected json2.21.2' unless JSON::VERSION == '2.21.2'
$LOAD_PATH.unshift(ARGV.fetch(0))
module ConfCtl
 module Generation; end
 module Utils; end
 module Cli; class Command; end; end
 class Error < StandardError; end
 class ConfDir; class << self; attr_accessor :generation_dir; end; end
 class Logger
  def self.instance; self; end
  def self.open?; false; end
 end
 def self.safe_host_name(host); host.gsub('/', ':'); end
end
require 'confctl/generation/build'
require 'confctl/generation/build_list'
require 'confctl/generation/unified'
require 'confctl/generation/unified_list'
require 'confctl/inputs_info'
require 'confctl/cli/output_formatter'
require 'confctl/cli/generation'
ConfCtl::ConfDir.generation_dir = File.join(ARGV.fetch(1), '.confctl', 'generations')
list = ConfCtl::Generation::UnifiedList.new
%w[z a].each { |host| list.add_build_generations(ConfCtl::Generation::BuildList.new(host)) }
ConfCtl::Cli::Generation.new.send(:list_generations, list)
`

func TestGenerationOriginalTableAdapter(t *testing.T) {
	ruby := os.Getenv("CONFCTL_GENERATION_RUBY")
	if ruby == "" {
		t.Skip("finite original table adapter requires pinned Ruby3.4.9")
	}
	version, err := exec.Command(ruby, "--version").Output()
	if err != nil || !strings.HasPrefix(string(version), "ruby 3.4.9 ") {
		t.Fatal("pinned Ruby required", err, string(version))
	}
	lib, err := filepath.Abs("../../lib")
	if err != nil {
		t.Fatal(err)
	}
	gemRoot, err := filepath.Abs("../../.gems/ruby/3.4.0")
	if err != nil {
		t.Fatal(err)
	}
	jsonLib := filepath.Join(gemRoot, "gems", "json-2.21.2", "lib")
	root := t.TempDir()
	seedCoreGeneration(t, root, "z", "zzz", coreGenerationRecord(root+"/z", "2000-01-01T00:00:00Z", `{"second":{"short_rev":"first","shortRev":"later","rev":"second-rev","lastModified":9007199254740993},"first":"123456789abc","partial":false}`))
	seedCoreGeneration(t, root, "a", "aaa", coreGenerationRecord(root+"/a", "2020-01-01T00:00:00Z", `{"third":{"shortRev":"","rev":"not-used"},"second":{"shortRev":false,"rev":"abcdefghij"}}`))
	for _, collision := range []bool{false, true} {
		if collision {
			seedCoreGeneration(t, root, "z", "collision", coreGenerationRecord(root+"/collision", "2030-01-01T00:00:00Z", `{"name":"000000000","host":"overwritten"}`))
		}
		before := testConfigurationState(t, root)
		list := &generation.UnifiedList{}
		for _, host := range []string{"z", "a"} {
			builds, err := generation.LoadBuildList(root, host, generation.Options{})
			if err != nil {
				t.Fatal(err)
			}
			list.AddBuilds(builds)
		}
		cmd := exec.Command(ruby, "-I", jsonLib, "-e", originalGenerationTableAdapter, lib, root)
		cmd.Env = append(cmd.Environ(), "GEM_HOME="+gemRoot, "GEM_PATH="+gemRoot, "TZ=UTC")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("original table: %v %s", err, stderr.Bytes())
		}
		if got := generationTable(list); string(out) != got {
			t.Fatalf("original table%q Go%q", out, got)
		}
		if !reflect.DeepEqual(before, testConfigurationState(t, root)) {
			t.Fatal("adapter wrote state")
		}
	}
}
