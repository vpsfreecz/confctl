//go:build healthsource

package health

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type sourceFileIdentity struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256"`
}

func sourceIdentity(t *testing.T, path string) sourceFileIdentity {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	return sourceFileIdentity{path, info.Size(), fmt.Sprintf("%04o", info.Mode().Perm()), hex.EncodeToString(hash[:])}
}

type rubyHealthCase struct {
	Name           string     `json:"name"`
	Errors         []string   `json:"errors"`
	Attempts       int        `json:"attempts"`
	FailedAttempts []int      `json:"failed_attempts"`
	Sleeps         []int      `json:"sleeps"`
	KeepCalls      int        `json:"keep_calls"`
	Argv           [][]string `json:"argv"`
	Success        bool       `json:"success"`
}

func TestRubyHealthSourceAdapter(t *testing.T) {
	ruby := os.Getenv("CONFCTL_TEST_HEALTH_RUBY")
	gems := os.Getenv("CONFCTL_TEST_HEALTH_GEMS")
	if ruby == "" || gems == "" || !filepath.IsAbs(ruby) || !filepath.IsAbs(gems) {
		t.Fatal("healthsource requires absolute CONFCTL_TEST_HEALTH_RUBY and CONFCTL_TEST_HEALTH_GEMS from the declared environment")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	private := t.TempDir()
	script := filepath.Join(private, "adapter.rb")
	if err := os.WriteFile(script, []byte(rubyHealthAdapter), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	argv := []string{ruby, "-I", filepath.Join(root, "lib"), script, root}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = private
	// Only the explicitly supplied declared provider is selected. No bundler
	// bootstrap, executable lookup, network or fallback command is involved.
	for _, env := range os.Environ() {
		key := strings.SplitN(env, "=", 2)[0]
		if key != "GEM_HOME" && key != "GEM_PATH" && key != "RUBYLIB" && !strings.HasPrefix(key, "BUNDLE_") && key != "RUBYOPT" {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "GEM_HOME="+gems, "GEM_PATH="+gems, "RUBYLIB="+filepath.Join(root, "lib"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	status := -1
	if cmd.ProcessState != nil {
		status = cmd.ProcessState.ExitCode()
	}
	var output struct {
		Ruby       string           `json:"ruby"`
		TTYVersion string           `json:"tty_version"`
		TTYResult  string           `json:"tty_result"`
		Separator  string           `json:"separator"`
		Cases      []rubyHealthCase `json:"cases"`
	}
	decodeErr := json.Unmarshal(stdout.Bytes(), &output)
	identities := []sourceFileIdentity{sourceIdentity(t, ruby), sourceIdentity(t, script), sourceIdentity(t, filepath.Join(root, "Gemfile.lock"))}
	for _, path := range []string{"base.rb", "run_command.rb", "systemd/properties.rb", "systemd/property_check.rb", "systemd/property_list.rb"} {
		identities = append(identities, sourceIdentity(t, filepath.Join(root, "lib/confctl/health_checks", path)))
	}
	if output.TTYResult != "" {
		identities = append(identities, sourceIdentity(t, output.TTYResult))
	}
	receipt := struct {
		Argv    []string             `json:"argv"`
		CWD     string               `json:"cwd"`
		Gems    string               `json:"gem_home_and_path"`
		Status  int                  `json:"status"`
		Sources []sourceFileIdentity `json:"identities"`
		Limit   string               `json:"limitations"`
	}{argv, private, gems, status, identities, "Actual Ruby health methods and tty-command Result enumeration with fake machine/command/clock/logger callbacks; no original CLI, real sleep, systemctl, SSH, fleet or log-close proof."}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if evidence := os.Getenv("CONFCTL_TEST_HEALTH_EVIDENCE"); evidence != "" {
		if !filepath.IsAbs(evidence) {
			t.Fatal("evidence destination must be absolute")
		}
		if err := os.Mkdir(evidence, 0700); err != nil {
			t.Fatal("evidence root must be absent", err)
		}
		for name, content := range map[string][]byte{"receipt.json": append(data, '\n'), "stdout.json": stdout.Bytes(), "stderr.txt": stderr.Bytes(), "adapter.rb": []byte(rubyHealthAdapter)} {
			if err := os.WriteFile(filepath.Join(evidence, name), content, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Logf("source receipt: %s", data)
	if runErr != nil || status != 0 || decodeErr != nil || stderr.Len() != 0 {
		t.Fatalf("Ruby source adapter: status=%d error=%v decode=%v stdout=%s stderr=%s", status, runErr, decodeErr, stdout.String(), stderr.String())
	}
	if output.TTYVersion != "0.10.1" || output.Separator != "\n" {
		t.Fatalf("unselected result provider/line separator: %#v", output)
	}
	want := []rubyHealthCase{
		{"stdout-match", []string{`fixture failed with 2 (fixture)`, `fixture: standard output does not match "out"`}, 1, []int{1}, []int{1}, 1, [][]string{{"fixture"}}, false},
		{"empty-match", []string{`fixture: standard output does not match ""`}, 1, []int{1}, []int{1}, 1, [][]string{{"fixture"}}, false},
		{"stdout-include", []string{`fixture: standard output does not include "first"`}, 1, []int{1}, []int{1}, 1, [][]string{{"fixture"}}, false},
		{"empty-exclude", []string{`fixture: standard output includes ""`}, 1, []int{1}, []int{1}, 1, [][]string{{"fixture"}}, false},
		{"stderr-match", []string{`fixture: standard error does not match "err"`}, 1, []int{1}, []int{1}, 1, [][]string{{"fixture"}}, false},
		{"stderr-include", []string{`fixture: standard error does not include "first"`}, 1, []int{1}, []int{1}, 1, [][]string{{"fixture"}}, false},
		{"stderr-exclude", []string{`fixture: standard error includes ""`}, 1, []int{1}, []int{1}, 1, [][]string{{"fixture"}}, false},
		{"systemd-lines", []string{"P=last=value\r, expected first", `property "missing" not found`, `property "" not found`}, 1, []int{1}, []int{1}, 1, [][]string{{"systemctl", "show", "unit*"}}, false},
		{"systemd-status", []string{`systemctl show unit* failed with 7`}, 1, []int{1}, []int{1}, 1, [][]string{{"systemctl", "show", "unit*"}}, false},
		{"strict-expiry", []string{}, 3, []int{1, 2}, []int{1, 1}, 2, [][]string{{"fixture"}, {"fixture"}, {"fixture"}}, true},
		{"failed-then-success", []string{}, 2, []int{1}, []int{3}, 1, [][]string{{"fixture"}, {"fixture"}}, true},
	}
	if !reflect.DeepEqual(output.Cases, want) {
		got, _ := json.MarshalIndent(output.Cases, "", "  ")
		t.Fatalf("original-method results differ from literal source predictions:\n%s", got)
	}
}

const rubyHealthAdapter = `require 'json'
gem 'tty-command', '0.10.1'
require 'tty-command'
module ConfCtl
  module HealthChecks
    module Systemd; end
  end
  module Logger
    def self.keep
      Fixture.keeps += 1
    end
  end
  module SystemCommand
    def self.new
      Fixture::Runner.new
    end
  end
  class MachineControl
    def initialize(_machine); end
    def execute!(*argv)
      Fixture.command(argv)
    end
  end
end
require 'confctl/health_checks/base'
require 'confctl/health_checks/run_command'
require 'confctl/health_checks/systemd/properties'
require 'confctl/health_checks/systemd/property_check'
require 'confctl/health_checks/systemd/property_list'
module Fixture
  class << self
    attr_accessor :now, :results, :argv, :sleeps, :keeps
  end
  class Runner
    def run!(*argv)
      Fixture.command(argv)
    end
  end
  class Machine
    def [](key)
      key == 'name' ? 'fixture-machine' : nil
    end
  end
  def self.command(argv)
    self.argv << argv
    raise 'unplanned fake command' if results.empty?
    TTY::Command::Result.new(*results.shift)
  end
  def self.sleep(seconds)
    self.sleeps << seconds
    self.now += seconds
  end
  def self.run(name, check, results)
    self.now = Time.at(100)
    self.results = results
    self.argv = []
    self.sleeps = []
    self.keeps = 0
    failed = []
    check.run do |attempt, _errors|
      failed << attempt
      ConfCtl::Logger.keep
    end
    raise 'unused fake results' unless self.results.empty?
    {name: name, errors: check.errors.dup, attempts: argv.length,
     failed_attempts: failed, sleeps: sleeps, keep_calls: keeps,
     argv: argv, success: check.successful?}
  end
  def self.output(match: nil, include: [], exclude: [])
    {'match' => match, 'include' => include, 'exclude' => exclude}
  end
  def self.command_check(timeout: 0, cooldown: 1, stdout: output, stderr: output)
    opts = {'description' => 'fixture', 'command' => ['fixture'], 'exitStatus' => 0,
            'standardOutput' => stdout, 'standardError' => stderr,
            'timeout' => timeout, 'cooldown' => cooldown}
    machine = Machine.new
    cmd = ConfCtl::HealthChecks::RunCommand::Command.new(machine, opts)
    ConfCtl::HealthChecks::RunCommand.new(machine, cmd, remote: false)
  end
  def self.property(name, value, timeout, cooldown)
    ConfCtl::HealthChecks::Systemd::PropertyCheck.new(
      'property' => name, 'value' => value, 'timeout' => timeout, 'cooldown' => cooldown)
  end
end
class << Time
  def now
    Fixture.now
  end
end
class ConfCtl::HealthChecks::Base
  private
  def sleep(seconds)
    Fixture.sleep(seconds)
  end
end
cases = []
cases << Fixture.run('stdout-match', Fixture.command_check(
  stdout: Fixture.output(match: 'out', include: ['missing']),
  stderr: Fixture.output(match: 'wrong')), [[2, "out
", 'bad']])
cases << Fixture.run('empty-match', Fixture.command_check(
  stdout: Fixture.output(match: '')), [[0, 'x', '']])
cases << Fixture.run('stdout-include', Fixture.command_check(
  stdout: Fixture.output(include: ['', 'first', 'second'], exclude: ['out']),
  stderr: Fixture.output(match: 'wrong')), [[0, 'out', 'bad']])
cases << Fixture.run('empty-exclude', Fixture.command_check(
  stdout: Fixture.output(exclude: ['absent', '']), stderr: Fixture.output(match: 'wrong')), [[0, '', 'bad']])
cases << Fixture.run('stderr-match', Fixture.command_check(
  stderr: Fixture.output(match: 'err', include: ['missing'])), [[0, '', "err
"]])
cases << Fixture.run('stderr-include', Fixture.command_check(
  stderr: Fixture.output(include: ['first', 'second'], exclude: ['err'])), [[0, '', 'err']])
cases << Fixture.run('stderr-exclude', Fixture.command_check(
  stderr: Fixture.output(exclude: ['absent', ''])), [[0, '', 'err']])
properties = [Fixture.property('P', 'first', 0, 3), Fixture.property('missing', '', 60, 1),
              Fixture.property('Empty', '', 2, 2), Fixture.property('=x', 'x', 10, 9),
              Fixture.property('', '', 10, 9), Fixture.property(' Space ', " kept \r", 10, 9)]
check = ConfCtl::HealthChecks::Systemd::Properties.new(Fixture::Machine.new, property_checks: properties, pattern: 'unit*')
cases << Fixture.run('systemd-lines', check, [[0, "ignored
P=first
Empty=
P=last=value\r
=x
 Space = kept \r

", '']])
check = ConfCtl::HealthChecks::Systemd::Properties.new(Fixture::Machine.new, property_checks: properties, pattern: 'unit*')
cases << Fixture.run('systemd-status', check, [[7, 'P=wrong', 'diagnostic']])
cases << Fixture.run('strict-expiry', Fixture.command_check(timeout: 2, cooldown: 1), [[1, '', ''], [1, '', ''], [0, '', '']])
cases << Fixture.run('failed-then-success', Fixture.command_check(timeout: 60, cooldown: 3), [[1, '', ''], [0, '', '']])
puts JSON.generate(ruby: RUBY_VERSION, tty_version: Gem.loaded_specs.fetch('tty-command').version.to_s,
                   tty_result: TTY::Command::Result.instance_method(:each).source_location.first,
                   separator: TTY::Command.record_separator, cases: cases)
`
