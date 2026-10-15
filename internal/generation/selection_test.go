package generation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func selectedNames(list *UnifiedList) []string {
	names := []string{}
	for _, g := range list.Generations {
		names = append(names, g.Name)
	}
	return names
}

func TestSelectionProjection(t *testing.T) {
	list := &UnifiedList{}
	for _, spec := range []struct {
		name, date string
		current    bool
	}{
		{"alpha", "2026-10-09T00:00:00.123456789012344Z", false},
		{"beta", "2026-10-09T00:00:00.123456789012345Z", true},
		{"9", "2026-10-09T00:00:00.123456789012346Z", false},
	} {
		list.AddBuild(testBuild(t, "a", spec.name, "/"+spec.name, spec.date, Object{}, nil, spec.current))
	}
	before := append([]*Unified{}, list.Generations...)
	for _, tc := range []struct {
		pattern    *string
		want       []string
		match, now int
	}{
		{nil, []string{"alpha", "beta", "9"}, 0, 0},
		{stringPointer(""), []string{}, 3, 0},
		{stringPointer("all"), []string{}, 3, 0},
		{stringPointer("a*"), []string{"alpha"}, 3, 0},
		{stringPointer("{alpha,beta}"), []string{"alpha", "beta"}, 3, 0},
		{stringPointer("current"), []string{"beta"}, 0, 0},
		{stringPointer("old"), []string{"alpha", "9"}, 0, 0},
		{stringPointer("1d"), []string{"alpha"}, 0, 1},
		{stringPointer("1d\n"), []string{"alpha"}, 0, 1},
		{stringPointer("0d"), []string{"alpha", "beta", "9"}, 0, 1},
		{stringPointer("1d\n\n"), []string{}, 3, 0},
		{stringPointer("-1d"), []string{}, 3, 0},
		{stringPointer("0"), []string{"9"}, 0, 0},
		{stringPointer("-0"), []string{"9"}, 0, 0},
		{stringPointer("01"), []string{"alpha"}, 0, 0},
		{stringPointer("2"), []string{"beta"}, 0, 0},
		{stringPointer("3"), []string{"9"}, 0, 0},
		{stringPointer("-1"), []string{"beta"}, 0, 0},
		{stringPointer("-2"), []string{"alpha"}, 0, 0},
		{stringPointer("-3"), []string{}, 3, 0},
		{stringPointer("9"), []string{"9"}, 3, 0},
		{stringPointer("999999999999999999999999"), []string{}, 3, 0},
		{stringPointer("-999999999999999999999999"), []string{}, 3, 0},
		{stringPointer("1\n"), []string{}, 3, 0},
		{stringPointer("+1"), []string{}, 3, 0},
	} {
		label := "nil"
		if tc.pattern != nil {
			label = *tc.pattern
		}
		t.Run(label, func(t *testing.T) {
			matches, clocks := 0, 0
			result, err := SelectList(list, SelectionOptions{Pattern: tc.pattern, Match: func(pattern, name string) bool {
				matches++
				if pattern != *tc.pattern {
					t.Fatal("matcher input changed")
				}
				// Deliberately fake: these assertions verify delegation to the
				// injected matcher, not a second glob/brace implementation.
				switch pattern {
				case "a*":
					return name == "alpha"
				case "{alpha,beta}":
					return name == "alpha" || name == "beta"
				default:
					return pattern == name
				}
			}, Now: func() Timestamp { clocks++; return stamp(t, "2026-10-10T00:00:00.123456789012345Z") }})
			if err != nil || !reflect.DeepEqual(selectedNames(result), tc.want) || matches != tc.match || clocks != tc.now {
				t.Fatalf("result%v err%v matches%d clocks%d", selectedNames(result), err, matches, clocks)
			}
			if !reflect.DeepEqual(list.Generations, before) {
				t.Fatal("projection mutated original list")
			}
		})
	}
	// Exact enormous age arithmetic, including a fractional strict boundary.
	days := integer("999999999999999999999999999999999")
	threshold := new(big.Int).Neg(new(big.Int).Mul(days, big.NewInt(86400)))
	huge := &UnifiedList{Generations: []*Unified{
		{Name: "before", Date: Timestamp{seconds: new(big.Int).Sub(threshold, big.NewInt(1)), fraction: new(big.Rat)}},
		{Name: "equal", Date: Timestamp{seconds: new(big.Int).Set(threshold), fraction: new(big.Rat).SetFrac64(1, 3)}},
		{Name: "after", Date: Timestamp{seconds: new(big.Int).Set(threshold), fraction: new(big.Rat).SetFrac64(1, 2)}},
	}}
	got, err := SelectList(huge, SelectionOptions{Pattern: stringPointer(days.String() + "d"), Now: func() Timestamp { return Timestamp{seconds: new(big.Int), fraction: new(big.Rat).SetFrac64(1, 3)} }})
	if err != nil || !reflect.DeepEqual(selectedNames(got), []string{"before"}) {
		t.Fatal(got, err)
	}
}

func TestSelectionScopesAndErrorBoundaries(t *testing.T) {
	hosts := []string{"b", "a"}
	for _, tc := range []struct {
		local, remote bool
		want          []string
	}{
		{false, false, []string{"local:b", "local:a"}},
		{true, false, []string{"local:b", "local:a"}},
		{false, true, []string{"remote:b", "remote:a"}},
		{true, true, []string{"remote:b", "remote:a", "local:b", "local:a"}},
	} {
		calls := []string{}
		result, err := SelectGenerations(hosts, SelectionOptions{Local: tc.local, Remote: tc.remote}, func(host string) (*HostList, error) {
			calls = append(calls, "remote:"+host)
			return &HostList{Generations: []*Host{testHost(t, host, "1", stringPointer("/remote"), "2000-01-01T00:00:00Z", nil, false)}}, nil
		}, func(host string) (*BuildList, error) {
			if tc.remote && !tc.local {
				t.Fatal("remote-only loaded local state")
			}
			calls = append(calls, "local:"+host)
			return &BuildList{Builds: []*Build{testBuild(t, host, "local", "/local", "1900-01-01T00:00:00Z", Object{}, nil, false)}}, nil
		})
		if err != nil || !reflect.DeepEqual(calls, tc.want) || len(result.Generations) != len(tc.want) {
			t.Fatal(calls, result, err)
		}
	}
	problem := errors.New("infrastructure")
	calls := []string{}
	_, err := SelectGenerations(hosts, SelectionOptions{Local: true, Remote: true}, func(host string) (*HostList, error) { calls = append(calls, host); return nil, problem }, func(string) (*BuildList, error) { t.Fatal("local after remote error"); return nil, nil })
	if err != problem || !reflect.DeepEqual(calls, []string{"b"}) {
		t.Fatal(calls, err)
	}
	// Missing remote list is ordinary query failure; continue with other hosts.
	calls = nil
	result, err := SelectGenerations(hosts, SelectionOptions{Remote: true}, func(host string) (*HostList, error) { calls = append(calls, host); return nil, nil }, nil)
	if err != nil || len(result.Generations) != 0 || !reflect.DeepEqual(calls, hosts) {
		t.Fatal(result, err, calls)
	}
	calls = nil
	_, err = SelectGenerations(hosts, SelectionOptions{}, nil, func(host string) (*BuildList, error) { calls = append(calls, host); return nil, problem })
	if err != problem || !reflect.DeepEqual(calls, []string{"b"}) {
		t.Fatal(err, calls)
	}
	// The clock is sampled after every included list has been collected.
	calls = nil
	_, err = SelectGenerations(hosts, SelectionOptions{Remote: true, Local: true, Pattern: stringPointer("1d"), Now: func() Timestamp { calls = append(calls, "now"); return stamp(t, "2026-10-10T00:00:00Z") }}, func(host string) (*HostList, error) { calls = append(calls, "remote:"+host); return &HostList{}, nil }, func(host string) (*BuildList, error) { calls = append(calls, "local:"+host); return &BuildList{}, nil })
	if err != nil || !reflect.DeepEqual(calls, []string{"remote:b", "remote:a", "local:b", "local:a", "now"}) {
		t.Fatal(err, calls)
	}
}

func mixedSelectionTree(t *testing.T) string {
	root := t.TempDir()
	seed(t, root, "a", "alpha", record(root+"/system-a", "2026-10-08T00:00:00Z"))
	seed(t, root, "a", "beta", `{"mode":"flakes","date":"2026-10-09T00:00:00.123456789012345Z","toplevel":"`+root+`/system-b","inputs":{"role":"/role"},"inputs_info":{"role":{"rev":"new","extra":9007199254740993}},"newer":{"unknown":true}}`)
	seed(t, root, "a", "legacy", `{"mode":"swpins"}`)
	seed(t, root, "a", "partial", `{"mode":"flakes","date":`)
	seed(t, root, "b", "good", record(root+"/system-c", "2026-10-07T00:00:00Z"))
	if err := os.Symlink("absent", filepath.Join(root, ".confctl", "generations", "b", "current")); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSelectionPrivateRecordsAndNoWrites(t *testing.T) {
	root := mixedSelectionTree(t)
	readOnly(t, root, func() {
		remoteCalls, localCalls := []string{}, []string{}
		remote := func(host string) (*HostList, error) {
			remoteCalls = append(remoteCalls, host)
			return ParseHostList(host, "/private/profile", []byte(root+"/system-b\n/private/profile-1-link;"+root+"/system-b;0"), Options{}), nil
		}
		local := func(host string) (*BuildList, error) {
			localCalls = append(localCalls, host)
			return LoadBuildList(root, host, Options{})
		}
		for _, pattern := range []string{"0", "1", "999999999999999999999", "legacy", "partial"} {
			remoteCalls, localCalls = nil, nil
			_, err := SelectGenerations([]string{"a", "b"}, SelectionOptions{Remote: true, Local: true, Pattern: &pattern, Match: func(p, n string) bool { return p == n }}, remote, local)
			if err == nil || !reflect.DeepEqual(remoteCalls, []string{"a", "b"}) || !reflect.DeepEqual(localCalls, []string{"a"}) {
				t.Fatal("remote bypassed rejection/ambiguity", pattern, err, remoteCalls, localCalls)
			}
		}
		for _, pattern := range []string{"old", "current"} {
			_, err := SelectGenerations([]string{"a", "b"}, SelectionOptions{Remote: true, Local: true, Pattern: &pattern}, remote, local)
			if err == nil || !strings.Contains(err.Error(), "Cannot select current generation for b") {
				t.Fatal(pattern, err)
			}
		}
		// Nil/explicit valid names remain readable with rejected/broken-current records.
		result, err := SelectGenerations([]string{"a", "b"}, SelectionOptions{Local: true, Remote: true}, remote, local)
		if err != nil || len(result.Generations) != 4 || result.Generations[0].Name != "beta" || result.Generations[0].PresenceString() != "build+host" {
			t.Fatal(result, err)
		}
		result, err = SelectGenerations([]string{"b"}, SelectionOptions{Pattern: stringPointer("good"), Match: func(p, n string) bool { return p == n }}, remote, local)
		if err != nil || !reflect.DeepEqual(selectedNames(result), []string{"good"}) {
			t.Fatal(result, err)
		}
		result, err = SelectGenerations([]string{"a", "b"}, SelectionOptions{Remote: true, Pattern: stringPointer("old")}, remote, func(string) (*BuildList, error) { t.Fatal("remote-only entered rejected local tree"); return nil, nil })
		if err != nil || len(result.Generations) != 0 {
			t.Fatal(result, err)
		}
	})
}

// Optional ORIGINAL-ADAPTER uses real Ruby local readers and CLI selection,
// with completed fake status lists/executor. No machine control is constructed.
const originalSelectionAdapter = `
require 'json'
require 'time'
abort 'expected json2.21.2' unless JSON::VERSION == '2.21.2'
$LOAD_PATH.unshift(ARGV.fetch(0))
module ConfCtl
  module Generation; end
  module Utils; end
  module Cli
    class Command
      def initialize(opts); @opts = opts; end
      attr_reader :opts
    end
  end
  class Error < StandardError; end
  class ConfDir
    class << self; attr_accessor :generation_dir; end
  end
  class Logger
    def self.instance; self; end
    def self.open?; false; end
  end
  class ParallelExecutor
    def initialize(_); @jobs = []; end
    def add(&block); @jobs << block; end
    def run; @jobs.each(&:call); end
  end
  class MachineStatus
    def initialize(machine); @generations = machine; end
    attr_reader :generations
    def query(**); end
  end
  def self.safe_host_name(host); host.gsub('/', ':'); end
end
require 'confctl/generation/build'
require 'confctl/generation/build_list'
require 'confctl/generation/host'
require 'confctl/generation/host_list'
require 'confctl/generation/unified'
require 'confctl/generation/unified_list'
require 'confctl/pattern'
require 'confctl/cli/generation'
fixed_now = Time.iso8601('2026-10-10T00:00:00.123456789012345Z')
Time.define_singleton_method(:now) { fixed_now }
ConfCtl::ConfDir.generation_dir = File.join(ARGV.fetch(1), '.confctl', 'generations')
puts JSON.generate(JSON.parse(STDIN.read).map do |f|
  machines = {}
  f['hosts'].each do |host|
    fake = Object.new
    def fake.bash_script(_); [@output, '']; end
    fake.instance_variable_set(:@output, f['outputs'][host])
    machines[host] = ConfCtl::Generation::HostList.fetch(Struct.new(:name).new(host), fake, profile: '/private/profile')
  end
  cli = ConfCtl::Cli::Generation.new(local: f['local'], remote: f['remote'])
  begin
    list = cli.send(:select_generations, machines, f['pattern'])
    {names: list.map(&:name), hosts: list.map(&:host), presence: list.map(&:presence_str)}
  rescue ConfCtl::Error => e
    {error: e.message}
  rescue RangeError => e
    {range_error: e.message}
  end
end)
`

func TestOriginalRubySelection(t *testing.T) {
	ruby := rubyAdapter(t)
	lib, err := filepath.Abs("../../lib")
	if err != nil {
		t.Fatal(err)
	}
	jsonLib, err := filepath.Abs("../../.gems/ruby/3.4.0/gems/json-2.21.2/lib")
	if err != nil {
		t.Fatal(err)
	}
	root := mixedSelectionTree(t)
	outputs := map[string]string{
		"a": root + "/system-b\n/private/profile-2-link;" + root + "/system-b;0\n/private/profile-1-link;/remote-a;1",
		"b": "/none\n/private/profile-999999999999999999999-link;/remote-b;2",
	}
	fixtures := []map[string]any{}
	for _, pattern := range []*string{nil, stringPointer(""), stringPointer("all"), stringPointer("alpha"), stringPointer("{alpha,beta}"), stringPointer("a*"), stringPointer("current"), stringPointer("old"), stringPointer("1d"), stringPointer("1d\n"), stringPointer("1\n"), stringPointer("0"), stringPointer("-1"), stringPointer("999999999999999999999"), stringPointer("legacy")} {
		for _, scopes := range [][2]bool{{false, false}, {false, true}, {true, true}} {
			fixtures = append(fixtures, map[string]any{"pattern": pattern, "local": scopes[0], "remote": scopes[1], "hosts": []string{"a", "b"}, "outputs": outputs})
		}
	}
	input, _ := json.Marshal(fixtures)
	readOnly(t, root, func() {
		cmd := exec.Command(ruby, "-I", jsonLib, "-e", originalSelectionAdapter, lib, root)
		cmd.Env = append(cmd.Environ(), "TZ=UTC")
		cmd.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("original selection %v %s", err, stderr.Bytes())
		}
		var results []struct {
			Names, Hosts, Presence []string
			Error                  string
			RangeError             string `json:"range_error"`
		}
		if err := json.Unmarshal(out, &results); err != nil {
			t.Fatalf("%s %v", out, err)
		}
		if len(results) != len(fixtures) {
			t.Fatal("missing selections")
		}
		for i, r := range results {
			f := fixtures[i]
			pattern := f["pattern"].(*string)
			result, err := SelectGenerations([]string{"a", "b"}, SelectionOptions{Local: f["local"].(bool), Remote: f["remote"].(bool), Pattern: pattern, Now: func() Timestamp { return stamp(t, "2026-10-10T00:00:00.123456789012345Z") }, Match: func(p, n string) bool {
				// Predetermined finite matcher results; parser ownership remains core.
				switch p {
				case "a*":
					return n == "alpha"
				case "{alpha,beta}":
					return n == "alpha" || n == "beta"
				default:
					return p == n
				}
			}}, func(host string) (*HostList, error) {
				return ParseHostList(host, "/private/profile", []byte(outputs[host]), Options{}), nil
			}, func(host string) (*BuildList, error) { return LoadBuildList(root, host, Options{}) })
			if r.RangeError != "" {
				// Actual Ruby Array indexing rejects huge offsets before name
				// fallback. The accepted R2 brief explicitly requires bounded
				// big integers instead. Keep this finite difference visible.
				if pattern == nil || *pattern != "999999999999999999999" || !f["remote"].(bool) || f["local"].(bool) || err != nil || len(result.Generations) != 0 || !strings.Contains(r.RangeError, "bignum too big") {
					t.Fatalf("unexpected range difference: fixture%d %+v Go%v %v", i, r, result, err)
				}
				continue
			}
			if (err != nil) != (r.Error != "") {
				t.Fatalf("fixture%d error original%q Go%v", i, r.Error, err)
			}
			if err != nil {
				// Ruby/Go system-call wording differs for dangling current. The
				// local error class/host/prefix is the contract at this adapter.
				if strings.HasPrefix(r.Error, "Cannot select current generation for b:") {
					if !strings.HasPrefix(err.Error(), "Cannot select current generation for b:") {
						t.Fatal(err)
					}
				} else if err.Error() != r.Error {
					t.Fatalf("fixture%d original%q Go%q", i, r.Error, err)
				}
				continue
			}
			hosts, presence := []string{}, []string{}
			for _, g := range result.Generations {
				hosts = append(hosts, g.Host)
				presence = append(presence, g.PresenceString())
			}
			if !reflect.DeepEqual(selectedNames(result), r.Names) || !reflect.DeepEqual(hosts, r.Hosts) || !reflect.DeepEqual(presence, r.Presence) {
				t.Fatalf("fixture%d original%+v Go%v %v %v", i, r, selectedNames(result), hosts, presence)
			}
		}
	})
}

func TestOriginalRubyCreatedUnifiedSelection(t *testing.T) {
	ruby := rubyAdapter(t)
	lib, err := filepath.Abs("../../lib")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	// Existing finite writer adapter injects only private ConfDir/GCRoot roots.
	// Fixture creation precedes the reader no-writes snapshot.
	cmd := exec.Command(ruby, "-e", originalWriterAdapter, lib, root)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if _, err := cmd.Output(); err != nil {
		t.Fatalf("private original writer %v %s", err, stderr.Bytes())
	}
	seed(t, root, "nested/host", "legacy", `{"mode":"swpins"}`)
	seed(t, root, "nested/host", "partial", `{"mode":"flakes"`)
	seed(t, root, "broken", "extra", `{"mode":"flakes","date":"2026-10-08T00:00:00Z","toplevel":"`+root+`/system","inputs":{},"newer":{"unknown":true}}`)
	if err := os.Symlink("missing", filepath.Join(root, ".confctl", "generations", "broken", "current")); err != nil {
		t.Fatal(err)
	}
	readOnly(t, root, func() {
		remote := func(host string) (*HostList, error) {
			return FetchHostList(context.Background(), host, "/private/profile", Options{}, func(context.Context, []string, []byte) ([]byte, bool, error) {
				return []byte(root + "/system\n/private/profile-123456789012345678901234567890-link;" + root + "/system;0;/nix/store/hash-linux-remote/bzImage"), true, nil
			})
		}
		local := func(host string) (*BuildList, error) { return LoadBuildList(root, host, Options{}) }
		list, err := SelectGenerations([]string{"nested/host", "broken"}, SelectionOptions{Remote: true, Local: true}, remote, local)
		if err != nil || len(list.Generations) != 3 {
			t.Fatal(list, err)
		}
		first := list.Generations[0]
		if first.Name != "2026-10-08--12-00-00" || first.ID.String() != "123456789012345678901234567890" || first.PresenceString() != "build+host" || first.CurrentString() != "build+host" || *first.KernelVersion != "remote" {
			t.Fatal(first)
		}
		metadata := first.InputsInfo().value("z").(Object)
		if metadata.value("large").(json.Number).String() != "9007199254740993" {
			t.Fatal(metadata)
		}
		if list.Generations[1].Name != "extra" || list.Generations[2].Name != "2026-10-08--12-01-00" {
			t.Fatal("inventory insertion changed")
		}
		_, err = SelectGenerations([]string{"nested/host"}, SelectionOptions{Remote: true, Local: true, Pattern: stringPointer("0")}, remote, local)
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatal("Ruby-created valid record bypassed exclusions", err)
		}
		_, err = SelectGenerations([]string{"broken"}, SelectionOptions{Remote: true, Local: true, Pattern: stringPointer("old")}, remote, local)
		if err == nil || !strings.Contains(err.Error(), "Cannot select current generation for broken:") {
			t.Fatal(err)
		}
	})
}
